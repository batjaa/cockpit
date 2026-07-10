package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexTitleScanLines bounds how many leading lines we inspect for a title.
const codexTitleScanLines = 80

// codexTitleMaxLen is the title truncation length.
const codexTitleMaxLen = 120

// codexEnvelope is the top-level shape of every rollout JSONL line.
type codexEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// codexMeta is the payload of the line-1 session_meta record.
type codexMeta struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Git       struct {
		Branch string `json:"branch"`
	} `json:"git"`
}

// codexPayload covers the payload shapes we read for titles. Codex uses two
// relevant forms (verified against a real rollout):
//
//   - event_msg / user_message: {"type":"user_message","message":"<text>"} —
//     the cleanest source for the actual user prompt.
//   - response_item / message:  {"type":"message","role":"user",
//     "content":[{"type":"input_text","text":"<text>"}]} — the same prompt,
//     but the first user-role message is usually an injected AGENTS.md /
//     instructions block, so we filter those out.
//
// Unknown payload types are tolerated and skipped.
type codexPayload struct {
	Type    string       `json:"type"`
	Role    string       `json:"role"`
	Message string       `json:"message"`
	Content []codexBlock `json:"content"`
}

type codexBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ScanCodexStore parses Codex rollout files under root (normally
// ~/.codex/sessions). Files with mtime <= since are skipped (zero since =
// scan all). A missing root returns (nil, nil): the store may not exist.
//
// Layout: <root>/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl. Line 1 is a
// session_meta record carrying id, started-at timestamp, and cwd. If line 1
// isn't session_meta the file is skipped with no error.
func ScanCodexStore(root, machine string, since time.Time) ([]SessionRecord, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat codex root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, nil
	}

	var records []SessionRecord
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable subtrees without aborting the whole walk.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		mtime := fi.ModTime()
		if !since.IsZero() && !mtime.After(since) {
			return nil
		}
		rec, ok, err := parseCodexFile(path, machine, mtime)
		if err != nil || !ok {
			// Bad/headerless files are skipped silently per the spec.
			return nil
		}
		records = append(records, rec)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk codex root %q: %w", root, walkErr)
	}
	return records, nil
}

// parseCodexFile builds a SessionRecord from one rollout. ok=false (no error)
// means the file's first line wasn't a session_meta record and should be
// skipped. MessageCount is the total line count of the file (cheap; matches
// the spec).
func parseCodexFile(path, machine string, mtime time.Time) (SessionRecord, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionRecord{}, false, fmt.Errorf("open codex rollout %q: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// Line 1 must be session_meta.
	if !sc.Scan() {
		return SessionRecord{}, false, nil
	}
	var first codexEnvelope
	if err := json.Unmarshal(sc.Bytes(), &first); err != nil || first.Type != "session_meta" {
		return SessionRecord{}, false, nil
	}
	var meta codexMeta
	if err := json.Unmarshal(first.Payload, &meta); err != nil {
		return SessionRecord{}, false, nil
	}
	if meta.ID == "" {
		return SessionRecord{}, false, nil
	}

	rec := SessionRecord{
		Agent:      "codex",
		Machine:    machine,
		SessionKey: meta.ID,
		ProjectDir: meta.Cwd,
		Branch:     meta.Git.Branch,
		LastActive: mtime,
		ResumeCmd:  fmt.Sprintf("codex resume %s", meta.ID),
	}
	if meta.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, meta.Timestamp); err == nil {
			rec.StartedAt = t
		}
	}

	// Subsequent lines: find the first usable user message for the title and
	// count every line.
	lineCount := 1 // the session_meta line
	var title string
	scanned := 0
	for sc.Scan() {
		lineCount++
		if title != "" {
			continue // keep counting lines; stop title work
		}
		if scanned >= codexTitleScanLines {
			continue
		}
		scanned++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var env codexEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // tolerate malformed lines
		}
		if env.Type != "event_msg" && env.Type != "response_item" {
			continue
		}
		var p codexPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		if txt, ok := usableCodexUserText(&p); ok {
			title = txt
		}
	}

	if title == "" {
		title = meta.ID
	}
	rec.Title = truncateTitle(collapseWhitespace(title), codexTitleMaxLen)
	rec.MessageCount = lineCount
	return rec, true, nil
}

// usableCodexUserText returns the title-worthy text of a payload, or ok=false
// when the payload isn't a real user prompt. It accepts:
//   - user_message events (payload.message)
//   - message records with role "user" whose first text block isn't an
//     injected instructions block (AGENTS.md, permissions, environment, etc.)
func usableCodexUserText(p *codexPayload) (string, bool) {
	switch p.Type {
	case "user_message":
		txt := strings.TrimSpace(p.Message)
		if txt == "" || looksInjected(txt) {
			return "", false
		}
		return txt, true
	case "message":
		if p.Role != "user" {
			return "", false
		}
		for _, b := range p.Content {
			if b.Type != "input_text" && b.Type != "text" {
				continue
			}
			txt := strings.TrimSpace(b.Text)
			if txt == "" || looksInjected(txt) {
				continue
			}
			return txt, true
		}
	}
	return "", false
}

// looksInjected reports whether text is a system-injected block rather than a
// real user prompt. Codex prepends AGENTS.md, permissions, and environment
// context as user-role messages; these are not titles.
func looksInjected(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "# AGENTS.md") ||
		strings.HasPrefix(t, "<permissions") ||
		strings.HasPrefix(t, "<environment_context") ||
		strings.HasPrefix(t, "<user_instructions") ||
		strings.Contains(t, "<INSTRUCTIONS>") {
		return true
	}
	return false
}
