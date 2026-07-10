package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// cursorComposer is a tolerant view of a Cursor composerData:<uuid> value.
// Every field is optional: Cursor's store is undocumented and the schema
// version (_v) drifts (observed across this user's DB: null..16). We only
// read what we need and treat anything missing as zero. See the "Cursor —
// local only" section and the Risks section of docs/sessions-spec.md.
type cursorComposer struct {
	// IsBestOfNSubcomposer marks parallel best-of-N children of a parent
	// composer — subagent runs, not user sessions.
	IsBestOfNSubcomposer bool   `json:"isBestOfNSubcomposer"`
	Version              int    `json:"_v"`
	ComposerID           string `json:"composerId"`
	Name                 string `json:"name"`
	Subtitle             string `json:"subtitle"`
	CreatedAt            int64  `json:"createdAt"`     // epoch ms
	LastUpdatedAt        int64  `json:"lastUpdatedAt"` // epoch ms

	// fullConversationHeadersOnly is a list of message headers; its length
	// is the message count. We only need the count, so decode into a slice
	// of raw messages to avoid coupling to the (drifting) header shape.
	Headers []json.RawMessage `json:"fullConversationHeadersOnly"`

	// Project-dir sources, in preference order. trackedGitRepos[0].repoPath
	// is a plain filesystem path (e.g. "/home/ubuntu/co/backend") and is the
	// cleanest signal; ~248/743 records in the real DB carry it.
	TrackedGitRepos []struct {
		RepoPath string `json:"repoPath"`
		Branches []struct {
			BranchName        string `json:"branchName"`
			LastInteractionAt int64  `json:"lastInteractionAt"`
		} `json:"branches"`
	} `json:"trackedGitRepos"`

	// workspaceIdentifier.configPath.path is a fallback. It can point at a
	// .code-workspace file and may be a vscode-remote:// path, so it is only
	// used when no repoPath is present and it looks like a real fs path.
	WorkspaceIdentifier struct {
		ConfigPath struct {
			Path   string `json:"path"`
			Scheme string `json:"scheme"`
		} `json:"configPath"`
	} `json:"workspaceIdentifier"`
}

// ScanCursorStore parses Cursor composer sessions from the state.vscdb
// SQLite database at dbPath (normally ~/Library/Application Support/
// Cursor/User/globalStorage/state.vscdb). since filters by lastUpdatedAt.
//
// It is deliberately failure-tolerant: a missing/unreadable DB or any
// open/query error returns (nil, nil), never an error, so a broken or
// absent Cursor install can never break the overall multi-source scan. A
// single record that fails to parse is skipped (logged at most). Only the
// composerData: key prefix is queried — the DB can exceed 100MB (3.5GB on
// this machine) with unrelated blob keys, so SELECT * is never used.
func ScanCursorStore(dbPath, machine string, since time.Time) ([]SessionRecord, error) {
	if dbPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		// Missing DB (no Cursor install, or path typo) is not an error.
		return nil, nil
	}

	// Cheap pre-check: if neither the DB nor its WAL has been touched since
	// the high-water mark, nothing can have changed — skip without opening.
	if !since.IsZero() && !cursorStoreTouchedAfter(dbPath, since) {
		return nil, nil
	}

	// Preferred path: read the LIVE database directly. The store is WAL-
	// journaled, so a read-only connection gets snapshot isolation and never
	// blocks Cursor's writer. This avoids copying the multi-GB file. If the
	// direct open or query fails (e.g. read-only WAL quirks when Cursor is
	// not running and no -shm exists), fall back to the old copy-to-temp.
	if out, ok := queryCursorComposers(dbPath, machine, since); ok {
		return out, nil
	}
	log.Printf("cursor: direct read failed; falling back to snapshot copy")

	tmpDir, err := os.MkdirTemp("", "cockpit-cursor-")
	if err != nil {
		// Can't make a workspace — degrade to skipping Cursor, never break.
		log.Printf("cursor: mkdir temp: %v (skipping)", err)
		return nil, nil
	}
	defer os.RemoveAll(tmpDir)

	copyPath := filepath.Join(tmpDir, "state.vscdb")
	if err := copyFile(dbPath, copyPath); err != nil {
		log.Printf("cursor: copy db: %v (skipping)", err)
		return nil, nil
	}
	// Sidecars are best-effort: their absence is normal (checkpointed DB).
	_ = copyFile(dbPath+"-wal", copyPath+"-wal")
	_ = copyFile(dbPath+"-shm", copyPath+"-shm")

	out, _ := queryCursorComposers(copyPath, machine, since)
	return out, nil
}

// cursorStoreTouchedAfter reports whether the DB or its WAL sidecar has an
// mtime after t. While Cursor runs it writes constantly (agentKv blobs), so
// this mostly helps when Cursor is closed.
func cursorStoreTouchedAfter(dbPath string, t time.Time) bool {
	for _, p := range []string{dbPath, dbPath + "-wal"} {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(t) {
			return true
		}
	}
	return false
}

// queryCursorComposers opens path read-only and returns the parsed records.
// ok=false means the open/query itself failed and the caller should try the
// copy fallback; parse-level problems still return ok=true with whatever
// parsed.
//
// Two query details matter at this DB's size (3.5GB live):
//   - The key predicate is a half-open range, NOT LIKE: SQLite's default
//     case-insensitive LIKE cannot use the key index and table-scans the
//     whole file (~6s); the range is an index scan (~10ms). ';' is the
//     successor of ':' in ASCII.
//   - The since filter is pushed into SQL via json_extract on
//     lastUpdatedAt with createdAt as fallback (mirroring the Go-side
//     rule), so unchanged rows are never transferred. The Go-side filter
//     in parseCursorComposer stays as the authority.
func queryCursorComposers(path, machine string, since time.Time) ([]SessionRecord, bool) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Printf("cursor: open db: %v", err)
		return nil, false
	}
	defer db.Close()

	query := `SELECT key, value FROM cursorDiskKV
		WHERE key >= 'composerData:' AND key < 'composerData;'`
	var args []any
	if !since.IsZero() {
		query += ` AND COALESCE(json_extract(value,'$.lastUpdatedAt'),
		                        json_extract(value,'$.createdAt'), 0) > ?`
		args = append(args, since.UnixMilli())
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		// Schema drift, corrupt page, or read-only WAL failure.
		log.Printf("cursor: query composerData: %v", err)
		return nil, false
	}
	defer rows.Close()

	var out []SessionRecord
	for rows.Next() {
		var key string
		var value []byte // BLOB column; also handles TEXT-stored JSON
		if err := rows.Scan(&key, &value); err != nil {
			log.Printf("cursor: scan row: %v (skipping record)", err)
			continue
		}
		rec, ok := parseCursorComposer(key, value, machine, since)
		if !ok {
			continue
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		// Partial results so far are still useful; return what we have.
		log.Printf("cursor: iterate rows: %v (returning partial)", err)
	}
	return out, true
}

// parseCursorComposer turns one composerData row into a SessionRecord. It
// returns ok=false when the record should be skipped: unparseable JSON, a
// draft (empty name AND zero messages), or filtered out by since. Every
// field is optional and version-agnostic — unknown _v values still parse
// best-effort.
func parseCursorComposer(key string, value []byte, machine string, since time.Time) (SessionRecord, bool) {
	var c cursorComposer
	if err := json.Unmarshal(value, &c); err != nil {
		// Corrupt / unexpected JSON — skip silently (debug log at most).
		log.Printf("cursor: parse %s: %v (skipping record)", key, err)
		return SessionRecord{}, false
	}

	msgCount := len(c.Headers)
	name := strings.TrimSpace(c.Name)

	// Drafts: never-titled, never-messaged composers. The real DB has 354 of
	// these; they are noise, not work. Skip per the spec.
	if name == "" && msgCount == 0 {
		return SessionRecord{}, false
	}
	// Best-of-N subcomposers are spawned children of a parent session.
	if c.IsBestOfNSubcomposer {
		return SessionRecord{}, false
	}

	// SessionKey: composerId, falling back to the key's "composerData:" suffix.
	sessionKey := strings.TrimSpace(c.ComposerID)
	if sessionKey == "" {
		sessionKey = strings.TrimPrefix(key, "composerData:")
	}
	if sessionKey == "" {
		// Nothing to key on — can't dedupe/upsert this row meaningfully.
		return SessionRecord{}, false
	}

	// LastActive: lastUpdatedAt, falling back to createdAt. 443 records in
	// the real DB have a null lastUpdatedAt, so the fallback is load-bearing.
	lastMs := c.LastUpdatedAt
	if lastMs == 0 {
		lastMs = c.CreatedAt
	}
	if lastMs == 0 {
		// No usable activity timestamp; last_active is NOT NULL in the schema.
		return SessionRecord{}, false
	}
	lastActive := msToTime(lastMs)

	// since filters on effective last activity.
	if !since.IsZero() && lastActive.Before(since) {
		return SessionRecord{}, false
	}

	// Title: name, then subtitle, then the session key.
	title := name
	if title == "" {
		title = strings.TrimSpace(c.Subtitle)
	}
	if title == "" {
		title = sessionKey
	}

	var startedAt time.Time
	if c.CreatedAt != 0 {
		startedAt = msToTime(c.CreatedAt)
	}

	return SessionRecord{
		Agent:        "cursor",
		Machine:      machine,
		SessionKey:   sessionKey,
		ProjectDir:   c.projectDir(),
		Branch:       c.branch(),
		Title:        title,
		Subtitle:     strings.TrimSpace(c.Subtitle),
		ResumeCmd:    "", // Cursor is GUI-only; the UI shows "open in Cursor".
		StartedAt:    startedAt,
		LastActive:   lastActive,
		MessageCount: msgCount,
	}, true
}

// branch returns the most recently interacted-with branch across tracked
// repos, best-effort ("" when the record carries none).
func (c cursorComposer) branch() string {
	best, bestAt := "", int64(-1)
	for _, r := range c.TrackedGitRepos {
		for _, b := range r.Branches {
			if b.BranchName != "" && b.LastInteractionAt > bestAt {
				best, bestAt = b.BranchName, b.LastInteractionAt
			}
		}
	}
	return best
}

// projectDir resolves a best-effort project directory. trackedGitRepos[0]
// .repoPath is preferred (a clean absolute fs path). workspaceIdentifier's
// configPath.path is a fallback, but only when it is a local "file" path —
// a .code-workspace file is still a more useful pointer than nothing, while
// a vscode-remote:// authority path would be misleading, so non-file
// schemes are ignored. Returns "" when neither yields a usable path.
func (c cursorComposer) projectDir() string {
	for _, r := range c.TrackedGitRepos {
		if p := strings.TrimSpace(r.RepoPath); p != "" {
			return p
		}
	}
	cp := c.WorkspaceIdentifier.ConfigPath
	if p := strings.TrimSpace(cp.Path); p != "" {
		if cp.Scheme == "" || cp.Scheme == "file" {
			return p
		}
	}
	return ""
}

// msToTime converts epoch milliseconds to a time.Time in UTC.
func msToTime(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

// copyFile copies src to dst. A missing src is reported as an error so the
// caller can decide whether it matters (it does for the main DB, not for
// the optional -wal/-shm sidecars).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy to %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}
