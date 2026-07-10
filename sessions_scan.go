package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// remoteHostTimeout bounds the total time spent scanning a single remote host
// (both claude and codex). A slow or wedged host stalls only its own goroutine.
const remoteHostTimeout = 30 * time.Second

// sourcePaths holds the local store roots for one machine. Factored out (and
// passed in) so tests drive scanSessionsFrom with fixture dirs instead of the
// real home directory — there is no env var a test could be fooled by.
type sourcePaths struct {
	claudeRoot string // ~/.claude/projects
	codexRoot  string // ~/.codex/sessions
	cursorDB   string // ~/Library/.../state.vscdb
}

// localSourcePaths builds the real local store roots from the user's home
// directory.
func localSourcePaths() (sourcePaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sourcePaths{}, fmt.Errorf("resolve home dir: %w", err)
	}
	return sourcePaths{
		claudeRoot: filepath.Join(home, ".claude", "projects"),
		codexRoot:  filepath.Join(home, ".codex", "sessions"),
		cursorDB:   filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"),
	}, nil
}

// ScanSessions runs every enabled source: local claude/codex/cursor, then
// devbox-discovered + configured remotes (claude+codex only). Each source is
// independent — a failure logs and the scan continues. Records and their
// extracted tickets are persisted and per-source high-water marks advance.
// Returns an error only if every attempted source failed.
func ScanSessions(ctx context.Context, db *sql.DB, cfg Config) error {
	if !cfg.Sessions.Enabled {
		slog.Info("sessions scan skipped; disabled in config")
		return nil
	}
	paths, err := localSourcePaths()
	if err != nil {
		return err
	}
	remotes := gatherRemotes(ctx, cfg)
	return scanSessionsFrom(ctx, db, cfg, paths, remotes)
}

// gatherRemotes unions devbox-discovered hosts (when enabled) with the
// statically configured remotes. Configured remotes win on name collisions and
// are deduped by Host. Discovery failure logs a warning and falls back to the
// configured list only.
func gatherRemotes(ctx context.Context, cfg Config) []RemoteConfig {
	var discovered []RemoteConfig
	if cfg.Sessions.DevboxDiscovery {
		d, err := DiscoverDevboxes(ctx)
		if err != nil {
			slog.Warn("devbox discovery failed; using configured remotes only", "err", err)
		} else {
			discovered = d
			slog.Info("devbox discovery", "running", len(d))
		}
	}

	// Configured remotes win on name collisions, so insert them first and let
	// discovered hosts fill in only the hosts not already present.
	seen := make(map[string]bool)
	var out []RemoteConfig
	for _, r := range cfg.Sessions.Remotes {
		if r.Host == "" || seen[r.Host] {
			continue
		}
		seen[r.Host] = true
		out = append(out, r)
	}
	for _, r := range discovered {
		if r.Host == "" || seen[r.Host] {
			continue
		}
		seen[r.Host] = true
		out = append(out, r)
	}
	return out
}

// scanSessionsFrom is the orchestration core: it scans local stores from the
// given paths and the given remotes, persisting records and advancing
// high-water marks. ScanSessions wraps it with real paths; tests drive it
// directly with fixture dirs. Returns an error only when every attempted
// source failed.
func scanSessionsFrom(ctx context.Context, db *sql.DB, cfg Config, paths sourcePaths, remotes []RemoteConfig) error {
	var (
		mu        sync.Mutex
		attempted int
		failed    int
	)
	record := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		attempted++
		if err != nil {
			failed++
		}
	}

	// Local sources run sequentially: they're cheap (filesystem only) and the
	// SQLite writer is single-threaded anyway.
	if cfg.Sessions.ScanClaude {
		record(persistSource(ctx, db, "local:claude", func(since time.Time) ([]SessionRecord, error) {
			return ScanClaudeStore(paths.claudeRoot, "local", since)
		}))
	}
	if cfg.Sessions.ScanCodex {
		record(persistSource(ctx, db, "local:codex", func(since time.Time) ([]SessionRecord, error) {
			return ScanCodexStore(paths.codexRoot, "local", since)
		}))
	}
	if cfg.Sessions.ScanCursor {
		record(persistSource(ctx, db, "local:cursor", func(since time.Time) ([]SessionRecord, error) {
			return ScanCursorStore(paths.cursorDB, "local", since)
		}))
	}

	// Remote sources: one goroutine per host, each with its own 30s budget.
	// Within a host claude and codex run sequentially (they share the same ssh
	// connection limits and timeout). Persistence (DB writes) is serialized via
	// the writer being inside the goroutine but SQLite handles concurrent
	// callers; the attempt counters are mutex-guarded.
	var wg sync.WaitGroup
	for _, r := range remotes {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			hctx, cancel := context.WithTimeout(ctx, remoteHostTimeout)
			defer cancel()
			if cfg.Sessions.ScanClaude {
				record(persistSource(hctx, db, r.Name+":claude", func(since time.Time) ([]SessionRecord, error) {
					return ScanRemoteClaude(hctx, r.Host, r.Name, since)
				}))
			}
			if cfg.Sessions.ScanCodex {
				record(persistSource(hctx, db, r.Name+":codex", func(since time.Time) ([]SessionRecord, error) {
					return ScanRemoteCodex(hctx, r.Host, r.Name, since)
				}))
			}
		}()
	}
	wg.Wait()

	slog.Info("sessions scan done", "sources", attempted, "failed", failed)
	if attempted > 0 && failed == attempted {
		return fmt.Errorf("all %d session sources failed", attempted)
	}
	return nil
}

// persistSource runs one scan source end-to-end: read its high-water mark,
// scan since that mark, upsert each record with its extracted tickets, and
// advance the high-water mark to the newest LastActive seen (never regressing).
// A scan error logs and returns the error without advancing the mark, so the
// next run retries the same window. Returns nil on success.
func persistSource(ctx context.Context, db *sql.DB, source string, scan func(since time.Time) ([]SessionRecord, error)) error {
	since, err := GetScanHighWater(ctx, db, source)
	if err != nil {
		slog.Error("sessions scan: read high water", "source", source, "err", err)
		return err
	}

	records, err := scan(since)
	if err != nil {
		slog.Warn("sessions scan source failed", "source", source, "err", err)
		return err
	}

	var maxActive time.Time
	persisted := 0
	for _, rec := range records {
		id, err := UpsertSession(ctx, db, rec)
		if err != nil {
			slog.Error("sessions scan: upsert", "source", source, "key", rec.SessionKey, "err", err)
			continue
		}
		tickets := ExtractTickets(rec.Title + " " + rec.Subtitle + " " + rec.ProjectDir)
		if err := ReplaceSessionTickets(ctx, db, id, tickets); err != nil {
			slog.Error("sessions scan: tickets", "source", source, "key", rec.SessionKey, "err", err)
			// The session row is persisted; a ticket failure shouldn't drop it.
		}
		persisted++
		if rec.LastActive.After(maxActive) {
			maxActive = rec.LastActive
		}
	}

	// Advance the high-water mark only forward. Records older than the current
	// mark can't appear (the scanners filter by since), but guard anyway so a
	// stale clock or out-of-order remote can never rewind the mark.
	if maxActive.After(since) {
		if err := SetScanHighWater(ctx, db, source, maxActive); err != nil {
			slog.Error("sessions scan: set high water", "source", source, "err", err)
			return err
		}
	}

	slog.Info("sessions scan source", "source", source, "scanned", len(records), "persisted", persisted)
	return nil
}
