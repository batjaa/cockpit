package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// devboxBinary is an internal remote-host CLI resolved from PATH (not a
// public tool). Tests leave it as "devbox" while a stub on PATH shadows the
// real binary.
const devboxBinary = "devbox"

// sshConnectTimeoutSeconds bounds the TCP/handshake phase of every ssh call
// so an unreachable host fails fast instead of hanging the per-host goroutine.
const sshConnectTimeoutSeconds = 10

// remoteHeadBytes is how many bytes of each remote transcript we pull for
// title / metadata extraction. Mirrors the local scanners' head-only reads:
// 16KB covers the codex session_meta line plus the early user prompt, and the
// first user line of a claude transcript.
const remoteHeadBytes = 16384

// remoteRecordSep prefixes each per-file metadata line in the streamed ssh
// output: "===FILE\t<path>\t<mtime-epoch>\t<linecount>" followed by the head
// bytes of the file. A tab-delimited sentinel keeps parsing trivial and can't
// collide with JSONL content (which never starts with this literal).
const remoteRecordSep = "===FILE\t"

// ansiOSCRe strips two classes of terminal control sequences from devbox
// output: CSI sequences (colors, e.g. "\x1b[1;32m") and OSC-8 hyperlink
// sequences ("\x1b]8;;<uri>\x1b\\" or BEL-terminated). The devbox CLI wraps
// dashboard URLs in OSC-8 hyperlinks and colorizes the table, neither of which
// survives plain-text column parsing.
var ansiOSCRe = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b\\[[0-9;?]*[ -/]*[@-~]")

// stripTerminalSequences removes ANSI CSI color codes and OSC-8 hyperlink
// wrappers, leaving the human-readable text.
func stripTerminalSequences(s string) string {
	return ansiOSCRe.ReplaceAllString(s, "")
}

// resolveDevboxBinary finds the devbox CLI. The server may run with a
// minimal PATH (launchd, nohup) that misses ~/.local/bin where the CLI
// installs itself — fall back there explicitly.
func resolveDevboxBinary() string {
	if p, err := exec.LookPath(devboxBinary); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "devbox")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return devboxBinary // let exec fail with a clear error
}

// DiscoverDevboxes parses `devbox list` output into remotes. The probe
// (no --no-probe) costs a few seconds but is required: without it the
// STATE column reads "·" and no row would ever match "running".
// Only rows with STATE == "running" are returned; the ALIAS column is the
// ssh target. Returns (nil, error) on CLI failure — caller logs and falls
// back to configured remotes.
func DiscoverDevboxes(ctx context.Context) ([]RemoteConfig, error) {
	cmd := exec.CommandContext(ctx, resolveDevboxBinary(), "list")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("devbox list: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("devbox list: %w", err)
	}
	return parseDevboxList(string(out)), nil
}

// parseDevboxList extracts running devboxes from a `devbox list` table. It
// strips terminal sequences, splits each box-drawing row on the vertical bar
// (│ / |), and matches the NAME/STATE/ALIAS columns by header position so the
// parser tolerates column reordering. Header, border, and non-running rows are
// skipped.
func parseDevboxList(raw string) []RemoteConfig {
	var (
		out                         []RemoteConfig
		nameIdx, stateIdx, aliasIdx = -1, -1, -1
		headerSeen                  bool
	)
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := stripTerminalSequences(sc.Text())
		cells := splitBoxRow(line)
		if cells == nil {
			continue // not a │-delimited table row
		}
		if !headerSeen {
			// The header row carries the column labels.
			for i, c := range cells {
				switch strings.ToUpper(strings.TrimSpace(c)) {
				case "NAME":
					nameIdx = i
				case "STATE":
					stateIdx = i
				case "ALIAS":
					aliasIdx = i
				}
			}
			if nameIdx >= 0 && stateIdx >= 0 && aliasIdx >= 0 {
				headerSeen = true
			}
			continue
		}
		if nameIdx >= len(cells) || stateIdx >= len(cells) || aliasIdx >= len(cells) {
			continue // ragged/border row
		}
		state := strings.TrimSpace(cells[stateIdx])
		if state != "running" {
			continue
		}
		name := strings.TrimSpace(cells[nameIdx])
		alias := strings.TrimSpace(cells[aliasIdx])
		if name == "" || alias == "" || alias == "·" {
			continue
		}
		out = append(out, RemoteConfig{Name: name, Host: alias})
	}
	return out
}

// splitBoxRow splits a box-drawing table row on its column separators and
// returns the inner cells, or nil when the line isn't a data/header row.
// Both the Unicode box-drawing bar (│) and the ASCII pipe (|) are accepted as
// separators; border rows (made of ├─┼┤ etc. with no separator) yield nil.
func splitBoxRow(line string) []string {
	if !strings.ContainsAny(line, "│|") {
		return nil
	}
	// Border rows use horizontal/junction runes; a row with box-drawing
	// corners or dashes and no real text is a divider, not data.
	if strings.ContainsAny(line, "─┼├┤╭╮╰╯┬┴") {
		return nil
	}
	// Normalize the Unicode bar to ASCII pipe, then split.
	norm := strings.ReplaceAll(line, "│", "|")
	parts := strings.Split(norm, "|")
	// Split produces leading/trailing empties from the outer borders; trim
	// the extremes but keep interior cells (including the marker column).
	if len(parts) >= 2 {
		parts = parts[1 : len(parts)-1]
	}
	return parts
}

// ScanRemoteClaude pulls Claude session metadata from a remote host over ssh.
// host is an ssh alias or user@host; machine fills SessionRecord.Machine.
// since filters by file mtime (GNU `find -newermt @<epoch>`). A single ssh
// invocation streams a "===FILE" record per matching transcript; titles and
// timestamps are extracted locally with the same helpers the local claude
// scanner uses. Returns (nil, error) on ssh failure; an absent store yields
// (nil, nil). Linux remotes are assumed.
func ScanRemoteClaude(ctx context.Context, host, machine string, since time.Time) ([]SessionRecord, error) {
	script := remoteClaudeScript(since)
	out, err := runSSH(ctx, host, script)
	if err != nil {
		return nil, fmt.Errorf("remote claude scan %s: %w", host, err)
	}
	return parseRemoteClaudeStream(out, host, machine), nil
}

// ScanRemoteCodex pulls Codex session metadata from a remote host over ssh.
// Same single-invocation streaming pattern as ScanRemoteClaude; the codex
// session_meta line is line 1, well within the head bytes pulled per file.
func ScanRemoteCodex(ctx context.Context, host, machine string, since time.Time) ([]SessionRecord, error) {
	script := remoteCodexScript(since)
	out, err := runSSH(ctx, host, script)
	if err != nil {
		return nil, fmt.Errorf("remote codex scan %s: %w", host, err)
	}
	return parseRemoteCodexStream(out, host, machine), nil
}

// runSSH executes script on host via ssh, passing the script on stdin so the
// remote shell interprets the pipeline (devbox exec / a single argv would
// not). BatchMode disables interactive prompts; ConnectTimeout bounds the
// handshake. Returns stdout bytes, or an error carrying stderr on non-zero
// exit.
func runSSH(ctx context.Context, host, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", sshConnectTimeoutSeconds),
		host,
		"bash -s",
	)
	cmd.Stdin = strings.NewReader(script)
	// On context cancellation, force-close the pipes shortly after the kill
	// so a stuck remote can't hold the goroutine open indefinitely.
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("ssh: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("ssh: %w", err)
	}
	return out, nil
}

// remoteClaudeScript builds the remote shell pipeline for the claude store.
// For each ~/.claude/projects/*/*.jsonl newer than since, it emits the
// "===FILE\t<path>\t<mtime-epoch>\t<linecount>" sentinel followed by the head
// bytes of the file. Missing dirs are tolerated (2>/dev/null, || true) so a
// host with no claude store exits 0 with empty output.
func remoteClaudeScript(since time.Time) string {
	newer := findNewerExpr(since)
	return fmt.Sprintf(`set -o pipefail 2>/dev/null
dir="$HOME/.claude/projects"
[ -d "$dir" ] || exit 0
find "$dir" -type f -name '*.jsonl' ! -name 'agent-*'%s 2>/dev/null | while IFS= read -r f; do
  mt=$(stat -c %%Y "$f" 2>/dev/null) || continue
  lc=$(wc -l < "$f" 2>/dev/null | tr -d ' ') || lc=0
  printf '%s%%s\t%%s\t%%s\n' "$f" "$mt" "$lc"
  head -c %d "$f" 2>/dev/null
  printf '\n'
done
true
`, newer, remoteRecordSep, remoteHeadBytes)
}

// remoteCodexScript builds the remote shell pipeline for the codex store. Same
// sentinel format as the claude script; session_meta is line 1 and well within
// the head bytes.
func remoteCodexScript(since time.Time) string {
	newer := findNewerExpr(since)
	return fmt.Sprintf(`set -o pipefail 2>/dev/null
dir="$HOME/.codex/sessions"
[ -d "$dir" ] || exit 0
find "$dir" -type f -name 'rollout-*.jsonl'%s 2>/dev/null | while IFS= read -r f; do
  mt=$(stat -c %%Y "$f" 2>/dev/null) || continue
  lc=$(wc -l < "$f" 2>/dev/null | tr -d ' ') || lc=0
  printf '%s%%s\t%%s\t%%s\n' "$f" "$mt" "$lc"
  head -c %d "$f" 2>/dev/null
  printf '\n'
done
true
`, newer, remoteRecordSep, remoteHeadBytes)
}

// findNewerExpr returns the GNU find predicate that limits results to files
// modified strictly after since. `-newermt @<epoch>` is GNU-specific (the
// devboxes are Linux). A zero since returns "" so every file matches.
func findNewerExpr(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return fmt.Sprintf(" -newermt @%d", since.Unix())
}

// remoteFile is one parsed "===FILE" record from the ssh stream: the sentinel
// metadata plus the accumulated head bytes of the transcript.
type remoteFile struct {
	path  string
	mtime time.Time
	lines int
	head  []byte
}

// parseRemoteStream splits an ssh output stream into per-file records. Each
// record starts at a remoteRecordSep line; everything until the next sentinel
// (or EOF) is that file's head bytes. Malformed sentinel lines are skipped.
func parseRemoteStream(out []byte) []remoteFile {
	var (
		files []remoteFile
		cur   *remoteFile
		buf   bytes.Buffer
	)
	flush := func() {
		if cur != nil {
			// Trim the single trailing newline the remote printf appends
			// after the head bytes.
			head := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
			cur.head = append([]byte(nil), head...)
			files = append(files, *cur)
		}
		cur = nil
		buf.Reset()
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, remoteRecordSep) {
			flush()
			meta := strings.TrimPrefix(line, remoteRecordSep)
			fields := strings.Split(meta, "\t")
			if len(fields) < 3 {
				continue // malformed sentinel; ignore until the next one
			}
			epoch, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
			if err != nil {
				continue
			}
			lines, _ := strconv.Atoi(strings.TrimSpace(fields[2]))
			cur = &remoteFile{
				path:  fields[0],
				mtime: time.Unix(epoch, 0).UTC(),
				lines: lines,
			}
			continue
		}
		if cur == nil {
			continue // content before any sentinel (e.g. shell noise)
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return files
}

// parseRemoteClaudeStream turns a claude ssh stream into SessionRecords. The
// session key is the filename stem; the project dir is de-slugged from the
// parent directory name (same scheme as the local scanner). Title / started-at
// extraction reuses the local claude helpers on the head bytes so parsing
// rules stay in one place. MessageCount is the wc -l line count.
func parseRemoteClaudeStream(out []byte, host, machine string) []SessionRecord {
	var records []SessionRecord
	for _, f := range parseRemoteStream(out) {
		base := remoteBase(f.path)
		sessionKey := strings.TrimSuffix(base, ".jsonl")
		slug := remoteBase(remoteDir(f.path))
		projectDir := deslugClaudeProject(slug)

		title, subtitle, startedAt, branch := claudeHeadTitle(f.head)
		if title == "" {
			title = sessionKey
		}
		records = append(records, SessionRecord{
			Agent:        "claude",
			Machine:      machine,
			SessionKey:   sessionKey,
			ProjectDir:   projectDir,
			Branch:       branch,
			Title:        truncateTitle(title, claudeTitleMaxLen),
			Subtitle:     subtitle,
			StartedAt:    startedAt,
			LastActive:   f.mtime,
			MessageCount: f.lines,
			ResumeCmd:    fmt.Sprintf("ssh -t %s 'cd %s && claude --resume %s'", host, projectDir, sessionKey),
		})
	}
	return records
}

// parseRemoteCodexStream turns a codex ssh stream into SessionRecords. The
// session key, started-at, and project dir come from the line-1 session_meta
// record; the title comes from the first usable user message. Both reuse the
// local codex helpers. A head whose first line isn't session_meta is skipped.
func parseRemoteCodexStream(out []byte, host, machine string) []SessionRecord {
	var records []SessionRecord
	for _, f := range parseRemoteStream(out) {
		meta, title, ok := codexHeadParse(f.head)
		if !ok {
			continue
		}
		if title == "" {
			title = meta.ID
		}
		var startedAt time.Time
		if meta.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, meta.Timestamp); err == nil {
				startedAt = t
			}
		}
		records = append(records, SessionRecord{
			Agent:        "codex",
			Machine:      machine,
			SessionKey:   meta.ID,
			ProjectDir:   meta.Cwd,
			Branch:       meta.Git.Branch,
			Title:        truncateTitle(collapseWhitespace(title), codexTitleMaxLen),
			StartedAt:    startedAt,
			LastActive:   f.mtime,
			MessageCount: f.lines,
			ResumeCmd:    fmt.Sprintf("ssh -t %s 'codex resume %s'", host, meta.ID),
		})
	}
	return records
}

// claudeHeadTitle extracts the title, subtitle (last usable user text), and
// started-at timestamp from the head bytes of a claude transcript. It mirrors
// parseClaudeFile's head loop but works on an in-memory buffer (the remote
// stream), reusing usableClaudeUserText so the title rules are not duplicated.
func claudeHeadTitle(head []byte) (title, subtitle string, startedAt time.Time, branch string) {
	var lastUser string
	sc := bufio.NewScanner(bytes.NewReader(head))
	sc.Buffer(make([]byte, 0, 64*1024), remoteHeadBytes)
	scanned := 0
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var line claudeLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue // tolerate malformed / partial trailing lines
		}
		if startedAt.IsZero() && line.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, line.Timestamp); err == nil {
				startedAt = t
			}
		}
		if branch == "" && line.GitBranch != "" {
			branch = line.GitBranch
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
	if lastUser != "" && lastUser != title {
		subtitle = truncateTitle(lastUser, claudeTitleMaxLen)
	}
	return title, subtitle, startedAt, branch
}

// codexHeadParse extracts the session_meta and first usable user title from
// the head bytes of a codex rollout. ok=false means line 1 wasn't a valid
// session_meta record (skip the file). It reuses the local codex envelope /
// payload types and usableCodexUserText so the parsing rules stay shared.
func codexHeadParse(head []byte) (meta codexMeta, title string, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(head))
	sc.Buffer(make([]byte, 0, 64*1024), remoteHeadBytes)
	if !sc.Scan() {
		return codexMeta{}, "", false
	}
	var first codexEnvelope
	if err := json.Unmarshal(sc.Bytes(), &first); err != nil || first.Type != "session_meta" {
		return codexMeta{}, "", false
	}
	if err := json.Unmarshal(first.Payload, &meta); err != nil || meta.ID == "" {
		return codexMeta{}, "", false
	}
	scanned := 0
	for sc.Scan() {
		if title != "" || scanned >= codexTitleScanLines {
			break
		}
		scanned++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var env codexEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // tolerate malformed / partial trailing lines
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
	return meta, title, true
}

// remoteBase returns the final path component of a remote (Linux) path. We use
// a literal '/' split rather than filepath.Base so cockpit running on macOS
// still parses Linux-style remote paths correctly.
func remoteBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// remoteDir returns the directory portion of a remote (Linux) path.
func remoteDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}
