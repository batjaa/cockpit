package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeTitleScanLines bounds how many leading lines we inspect for a title /
// started_at. Transcripts can be large, so we never read the whole file just
// to pick a title.
const claudeTitleScanLines = 50

// claudeHeadBytes bounds how many bytes we read from the front of each file
// for title / started_at extraction.
const claudeHeadBytes = 64 * 1024

// claudeTitleMaxLen is the title truncation length.
const claudeTitleMaxLen = 120

// claudeLine is the subset of a transcript JSONL record we care about.
// Claude writes one JSON object per line; many keys vary by version, so we
// decode only what we need and tolerate everything else.
type claudeLine struct {
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	IsMeta    bool           `json:"isMeta"`
	GitBranch string         `json:"gitBranch"`
	Message   *claudeMessage `json:"message"`
}

type claudeMessage struct {
	Role string `json:"role"`
	// Content is either a string or an array of blocks. Decoded lazily.
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ScanClaudeStore parses Claude Code session transcripts under root
// (normally ~/.claude/projects). Files with mtime <= since are skipped
// (zero since = scan all). machine fills SessionRecord.Machine.
//
// Layout: <root>/<project-slug>/<session-uuid>.jsonl. The project slug
// de-slugs to a directory ("-Users-foo-git-bar" -> "/Users/foo/git/bar").
// NOTE the de-slug is lossy: a directory containing a literal '-' is
// indistinguishable from a path separator, so such paths round-trip wrong.
// This is accepted; the resume command still works for the common case.
//
// A missing root returns (nil, nil): the store may not exist on a machine.
func ScanClaudeStore(root, machine string, since time.Time) ([]SessionRecord, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat claude root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, nil
	}

	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read claude root %q: %w", root, err)
	}

	var records []SessionRecord
	for _, proj := range projects {
		if !proj.IsDir() {
			continue
		}
		slug := proj.Name()
		projectDir := deslugClaudeProject(slug)
		projPath := filepath.Join(root, slug)

		files, err := os.ReadDir(projPath)
		if err != nil {
			// A project dir we can't read shouldn't abort the whole scan.
			continue
		}
		for _, f := range files {
			// agent-*.jsonl are subagent transcripts spawned by a parent
			// session (Agent tool / workflows) — not resumable sessions
			// of their own, so they'd be pure noise in the list.
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") || strings.HasPrefix(f.Name(), "agent-") {
				continue
			}
			fi, err := f.Info()
			if err != nil {
				continue
			}
			mtime := fi.ModTime()
			if !since.IsZero() && !mtime.After(since) {
				continue
			}
			path := filepath.Join(projPath, f.Name())
			rec, err := parseClaudeFile(path, projectDir, machine, mtime)
			if err != nil {
				// Tolerate a single bad file; keep scanning the rest.
				continue
			}
			records = append(records, rec)
		}
	}
	return records, nil
}

// parseClaudeFile builds a SessionRecord from one transcript. It reads only
// the head of the file (claudeHeadBytes) for the title / started_at, then
// counts lines across the whole file cheaply (no JSON parse) for the message
// count. MessageCount is therefore an approximation: the count of user /
// assistant lines in the head, plus — if the file is larger than the head —
// the remaining line count is added without classifying each line. Documented
// as acceptable per the spec.
func parseClaudeFile(path, projectDir, machine string, mtime time.Time) (SessionRecord, error) {
	sessionKey := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	rec := SessionRecord{
		Agent:      "claude",
		Machine:    machine,
		SessionKey: sessionKey,
		ProjectDir: projectDir,
		LastActive: mtime,
		ResumeCmd:  fmt.Sprintf("cd %s && claude --resume %s", projectDir, sessionKey),
	}

	f, err := os.Open(path)
	if err != nil {
		return SessionRecord{}, fmt.Errorf("open claude transcript %q: %w", path, err)
	}
	defer f.Close()

	var (
		title       string
		lastUser    string
		startedAt   time.Time
		headMsgs    int
		linesInHead int
	)

	head := io.LimitReader(f, claudeHeadBytes)
	sc := bufio.NewScanner(head)
	// Allow long JSONL lines (transcripts embed large tool payloads).
	sc.Buffer(make([]byte, 0, 64*1024), claudeHeadBytes)
	scanned := 0
	for sc.Scan() {
		linesInHead++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var line claudeLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue // tolerate malformed lines
		}
		if startedAt.IsZero() && line.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, line.Timestamp); err == nil {
				startedAt = t
			}
		}
		if rec.Branch == "" && line.GitBranch != "" {
			rec.Branch = line.GitBranch
		}
		if line.Type == "user" || line.Type == "assistant" {
			headMsgs++
		}
		if scanned < claudeTitleScanLines {
			scanned++
			if txt, ok := usableClaudeUserText(&line); ok {
				if title == "" {
					title = txt
				}
				lastUser = txt
			}
		}
	}
	// A scanner error (e.g. an over-long line) shouldn't fail the whole
	// record; we keep whatever we extracted from the head.

	// Count any remaining whole-file lines beyond the head cheaply. The head
	// classification gave us user/assistant counts; tail lines are added
	// unclassified (acceptable approximation per the spec).
	totalLines := linesInHead
	if _, err := f.Seek(claudeHeadBytes, io.SeekStart); err == nil {
		tail := bufio.NewScanner(f)
		tail.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		// The first tail token may be the remainder of a line split by the
		// head boundary; counting it as a line is close enough.
		for tail.Scan() {
			totalLines++
		}
	}

	msgCount := headMsgs
	if totalLines > linesInHead {
		msgCount += totalLines - linesInHead
	}

	if title == "" {
		title = sessionKey
	}
	rec.Title = truncateTitle(title, claudeTitleMaxLen)
	if lastUser != "" && lastUser != title {
		rec.Subtitle = truncateTitle(lastUser, claudeTitleMaxLen)
	}
	rec.StartedAt = startedAt
	rec.MessageCount = msgCount
	return rec, nil
}

// usableClaudeUserText returns the title-worthy text of a user line, or
// ok=false if the line is not a usable user prompt. It handles both string
// and array-of-blocks content and skips slash-command wrappers, local-command
// output, meta lines, and tool-result-only lines.
func usableClaudeUserText(line *claudeLine) (string, bool) {
	if line.Type != "user" || line.IsMeta || line.Message == nil {
		return "", false
	}
	text, ok := claudeContentText(line.Message.Content)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	// Skip slash-command wrappers and local-command sentinels.
	if strings.HasPrefix(trimmed, "<command-") ||
		strings.Contains(trimmed, "<command-message>") ||
		strings.HasPrefix(trimmed, "<local-command-") {
		return "", false
	}
	return collapseWhitespace(trimmed), true
}

// claudeContentText extracts plain text from a message content value, which
// is either a JSON string or an array of blocks. It returns the first text
// block's text for arrays; ok=false when there's no usable text (e.g. a
// tool_result-only array, or a non-text shape).
func claudeContentText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	// String content.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	// Array-of-blocks content.
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return b.Text, true
			}
		}
	}
	return "", false
}

// deslugClaudeProject converts a project slug into a directory path. A leading
// dash maps to the filesystem root and each subsequent dash becomes a path
// separator: "-Users-foo-git-bar" -> "/Users/foo/git/bar". Lossy for paths
// containing literal dashes (see ScanClaudeStore docs).
func deslugClaudeProject(slug string) string {
	if slug == "" {
		return ""
	}
	if strings.HasPrefix(slug, "-") {
		return "/" + strings.ReplaceAll(strings.TrimPrefix(slug, "-"), "-", "/")
	}
	return strings.ReplaceAll(slug, "-", "/")
}

// collapseWhitespace replaces runs of any whitespace (including newlines and
// tabs) with a single space and trims the ends.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateTitle trims s to at most max runes, appending an ellipsis when cut.
func truncateTitle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
