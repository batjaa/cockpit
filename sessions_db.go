package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Staleness thresholds: a session is "stale" when it went quiet mid-work.
// Computed at query time, never stored. See docs/sessions-spec.md.
const (
	staleMinIdle     = 24 * time.Hour
	staleMaxIdle     = 14 * 24 * time.Hour
	staleMinMessages = 10
)

// SessionRecord is the metadata a scanner extracts for one agent session.
// No transcript bodies — titles, timestamps, counts, references only.
type SessionRecord struct {
	Agent        string // "claude" | "codex" | "cursor"
	Machine      string // "local" or remote name
	SessionKey   string
	ProjectDir   string
	Title        string
	Subtitle     string
	Branch       string
	ResumeCmd    string
	StartedAt    time.Time // zero allowed
	LastActive   time.Time
	MessageCount int
}

// UpsertSession inserts or updates a session keyed on
// (agent, machine, session_key). On conflict it refreshes title, subtitle,
// project_dir, resume_cmd, last_active, and message_count; started_at and
// archived are preserved. Returns the row id.
func UpsertSession(ctx context.Context, db *sql.DB, rec SessionRecord) (int64, error) {
	var startedAt any
	if !rec.StartedAt.IsZero() {
		startedAt = dbTime(rec.StartedAt)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO sessions
			(agent, machine, session_key, project_dir, title, subtitle, branch,
			 started_at, last_active, message_count, resume_cmd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent, machine, session_key) DO UPDATE SET
			project_dir   = excluded.project_dir,
			title         = excluded.title,
			subtitle      = excluded.subtitle,
			branch        = excluded.branch,
			last_active   = excluded.last_active,
			message_count = excluded.message_count,
			resume_cmd    = excluded.resume_cmd
	`, rec.Agent, rec.Machine, rec.SessionKey, rec.ProjectDir, rec.Title,
		rec.Subtitle, rec.Branch, startedAt, dbTime(rec.LastActive), rec.MessageCount, rec.ResumeCmd)
	if err != nil {
		return 0, fmt.Errorf("upsert session: %w", err)
	}

	var id int64
	err = db.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE agent=? AND machine=? AND session_key=?`,
		rec.Agent, rec.Machine, rec.SessionKey).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("session id: %w", err)
	}
	return id, nil
}

// ReplaceSessionTickets replaces the full set of tickets for a session in a
// single transaction: it deletes the existing rows and inserts the new set
// (deduplicated). Passing an empty slice clears all tickets.
func ReplaceSessionTickets(ctx context.Context, db *sql.DB, sessionID int64, tickets []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_tickets WHERE session_id=?`, sessionID); err != nil {
		return fmt.Errorf("clear tickets: %w", err)
	}

	seen := map[string]bool{}
	for _, ticket := range tickets {
		if ticket == "" || seen[ticket] {
			continue
		}
		seen[ticket] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_tickets (session_id, ticket) VALUES (?, ?)`,
			sessionID, ticket); err != nil {
			return fmt.Errorf("insert ticket: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tickets: %w", err)
	}
	return nil
}

// SessionFilter narrows a session listing. Empty string fields mean "no
// filter". Search matches title or project_dir as a case-insensitive
// substring.
type SessionFilter struct {
	Agent, Machine, Ticket, Search string
	Repo, Branch                   string // Repo matches the basename of project_dir
	IncludeArchived                bool
	ShowAll                        bool // UI-only: include trivial sessions; ignored by ListSessions
}

// SessionRow is one row in a session listing: the stored record plus
// derived state (archived flag, tickets, and the computed stale flag).
type SessionRow struct {
	ID int64
	SessionRecord
	Archived bool
	Tickets  []string
	Stale    bool // last_active 24h–14d ago AND message_count >= 10 AND not archived
}

// ListSessions returns sessions matching the filter, newest last_active
// first. Tickets are populated per row. The stale flag is computed in Go
// relative to time.Now().
func ListSessions(ctx context.Context, db *sql.DB, f SessionFilter) ([]SessionRow, error) {
	var (
		where []string
		args  []any
	)
	if !f.IncludeArchived {
		where = append(where, "s.archived = 0")
	}
	if f.Agent != "" {
		where = append(where, "s.agent = ?")
		args = append(args, f.Agent)
	}
	if f.Machine != "" {
		where = append(where, "s.machine = ?")
		args = append(args, f.Machine)
	}
	if f.Search != "" {
		where = append(where, "(s.title LIKE ? ESCAPE '\\' OR s.project_dir LIKE ? ESCAPE '\\')")
		pat := "%" + escapeLike(f.Search) + "%"
		args = append(args, pat, pat)
	}
	if f.Repo != "" {
		// Match the project_dir basename: exact dir or any path ending /<repo>.
		where = append(where, "(s.project_dir = ? OR s.project_dir LIKE ? ESCAPE '\\')")
		args = append(args, f.Repo, "%/"+escapeLike(f.Repo))
	}
	if f.Branch != "" {
		where = append(where, "s.branch = ?")
		args = append(args, f.Branch)
	}
	if f.Ticket != "" {
		where = append(where,
			"EXISTS (SELECT 1 FROM session_tickets t WHERE t.session_id = s.id AND t.ticket = ?)")
		args = append(args, f.Ticket)
	}

	query := `
		SELECT s.id, s.agent, s.machine, s.session_key, s.project_dir,
		       s.title, s.subtitle, s.branch, s.resume_cmd, s.started_at, s.last_active,
		       s.message_count, s.archived
		FROM sessions s`
	if len(where) > 0 {
		query += "\n\t\tWHERE " + strings.Join(where, " AND ")
	}
	query += "\n\t\tORDER BY s.last_active DESC, s.id DESC"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	var out []SessionRow
	var ids []int64
	idx := map[int64]int{}
	for rows.Next() {
		var (
			r        SessionRow
			started  sql.NullTime
			archived int
		)
		if err := rows.Scan(
			&r.ID, &r.Agent, &r.Machine, &r.SessionKey, &r.ProjectDir,
			&r.Title, &r.Subtitle, &r.Branch, &r.ResumeCmd, &started, &r.LastActive,
			&r.MessageCount, &archived,
		); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		if started.Valid {
			r.StartedAt = started.Time
		}
		r.Archived = archived != 0
		r.Stale = isStale(r.LastActive, r.MessageCount, r.Archived, now)
		idx[r.ID] = len(out)
		ids = append(ids, r.ID)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}

	if err := attachTickets(ctx, db, ids, idx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachTickets loads tickets for the given session ids and assigns them to
// the matching rows via idx (id -> slice index).
func attachTickets(ctx context.Context, db *sql.DB, ids []int64, idx map[int64]int, out []SessionRow) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	tRows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT session_id, ticket FROM session_tickets
		WHERE session_id IN (%s)
		ORDER BY session_id, ticket
	`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return fmt.Errorf("list session tickets: %w", err)
	}
	defer tRows.Close()
	for tRows.Next() {
		var sid int64
		var ticket string
		if err := tRows.Scan(&sid, &ticket); err != nil {
			return fmt.Errorf("scan session ticket: %w", err)
		}
		if i, ok := idx[sid]; ok {
			out[i].Tickets = append(out[i].Tickets, ticket)
		}
	}
	return tRows.Err()
}

// isStale applies the spec's staleness heuristic: last_active between 24h
// and 14d ago, message_count >= 10, and not archived.
func isStale(lastActive time.Time, messageCount int, archived bool, now time.Time) bool {
	if archived || messageCount < staleMinMessages {
		return false
	}
	idle := now.Sub(lastActive)
	return idle >= staleMinIdle && idle <= staleMaxIdle
}

// escapeLike escapes LIKE wildcards so user search text matches literally.
// Used with `ESCAPE '\'` in the query.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SetSessionArchived sets the archived flag on a session. Returns
// sql.ErrNoRows if no session with that id exists.
func SetSessionArchived(ctx context.Context, db *sql.DB, id int64, archived bool) error {
	val := 0
	if archived {
		val = 1
	}
	res, err := db.ExecContext(ctx,
		`UPDATE sessions SET archived=? WHERE id=?`, val, id)
	if err != nil {
		return fmt.Errorf("set session archived: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetScanHighWater returns the high-water mtime for a scan source. If no row
// exists it returns the zero time.Time and a nil error.
func GetScanHighWater(ctx context.Context, db *sql.DB, source string) (time.Time, error) {
	var hw time.Time
	err := db.QueryRowContext(ctx,
		`SELECT high_water FROM scan_state WHERE source=?`, source).Scan(&hw)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get scan high water: %w", err)
	}
	return hw, nil
}

// SetScanHighWater upserts the high-water mtime for a scan source.
func SetScanHighWater(ctx context.Context, db *sql.DB, source string, t time.Time) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO scan_state (source, high_water) VALUES (?, ?)
		ON CONFLICT(source) DO UPDATE SET high_water = excluded.high_water
	`, source, dbTime(t))
	if err != nil {
		return fmt.Errorf("set scan high water: %w", err)
	}
	return nil
}

// CountStaleSessions returns the number of non-archived sessions matching
// the staleness heuristic. Counts in SQL relative to the current time.
func CountStaleSessions(ctx context.Context, db *sql.DB) (int, error) {
	now := time.Now()
	minActive := dbTime(now.Add(-staleMaxIdle)) // older bound (>= this)
	maxActive := dbTime(now.Add(-staleMinIdle)) // newer bound (<= this)
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE archived = 0
		  AND message_count >= ?
		  AND last_active >= ?
		  AND last_active <= ?
	`, staleMinMessages, minActive, maxActive).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count stale sessions: %w", err)
	}
	return n, nil
}
