package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const mapPageSize = 50

func opaqueID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
func cleanRequired(s, field string, max int, fields map[string]string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		fields[field] = "is required"
	}
	if utf8.RuneCountInString(s) > max {
		fields[field] = fmt.Sprintf("must be at most %d characters", max)
	}
	return s
}
func cleanOptional(s, field string, max int, fields map[string]string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		fields[field] = fmt.Sprintf("must be at most %d characters", max)
	}
	return s
}
func validDate(s string) bool {
	if s == "" {
		return true
	}
	_, e := time.Parse("2006-01-02", s)
	return e == nil
}
func parseInstant(s string, fields map[string]string, field string) *time.Time {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	t, e := time.Parse(time.RFC3339, s)
	if e != nil {
		fields[field] = "must be an RFC3339 timestamp"
		return nil
	}
	return &t
}

// normalizeItemInput is shared by create and edit. A full-replacement edit
// must not retain stale controls from a different kind, and normalization must
// happen before no-op detection (not after it).
func normalizeItemInput(in *WorkstreamItemInput, fields map[string]string) {
	in.BlockerReason = cleanOptional(in.BlockerReason, "blocker_reason", 4000, fields)
	in.Value = cleanOptional(in.Value, "value", 200, fields)
	in.Unit = cleanOptional(in.Unit, "unit", 200, fields)
	switch in.Kind {
	case "task":
		in.Counterpart, in.AskStatus, in.FollowUpAt, in.LastContactAt = "", "", "", ""
		in.Value, in.Unit, in.ObservedAt, in.Assessment, in.ReviewBy = "", "", "", "", ""
		if in.TrackingState != "blocked" {
			in.BlockerReason = ""
		}
	case "ask":
		in.DueDate, in.TrackingState, in.BlockerReason = "", "", ""
		in.Value, in.Unit, in.ObservedAt, in.Assessment, in.ReviewBy = "", "", "", "", ""
	case "signal":
		in.DueDate, in.TrackingState, in.BlockerReason = "", "", ""
		in.Counterpart, in.AskStatus, in.FollowUpAt, in.LastContactAt = "", "", "", ""
	default: // reference and invalid kinds persist no local obligation fields.
		in.DueDate, in.TrackingState, in.BlockerReason = "", "", ""
		in.Counterpart, in.AskStatus, in.FollowUpAt, in.LastContactAt = "", "", "", ""
		in.Value, in.Unit, in.ObservedAt, in.Assessment, in.ReviewBy = "", "", "", "", ""
	}
}

func timelineValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "none"
	}
	if utf8.RuneCountInString(s) > 160 {
		return string([]rune(s)[:157]) + "…"
	}
	return s
}
func timelineTime(t sql.NullTime) string {
	if !t.Valid {
		return "none"
	}
	return t.Time.UTC().Format(time.RFC3339)
}
func timelineNextTime(t *time.Time) string {
	if t == nil {
		return "none"
	}
	return t.UTC().Format(time.RFC3339)
}
func itemEditDescription(kind string, changes []string) string {
	d := "Edited " + kind
	if len(changes) > 0 {
		d += ": " + strings.Join(changes, "; ")
	}
	if utf8.RuneCountInString(d) > 1200 {
		return string([]rune(d)[:1199]) + "…"
	}
	return d
}

func canonicalSource(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("URL must use http or https")
	}
	if u.Host == "" || u.User != nil {
		return "", errors.New("URL must have a host and no credentials")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" && !((u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443")) {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") { // URL.Host needs brackets around a bare IPv6 literal.
		host = "[" + host + "]"
	}
	u.Host = host
	u.User = nil
	// GitHub PR URLs have a documented identity. Their query and fragment are
	// presentation details, unlike generic URLs where both remain significant.
	if strings.EqualFold(u.Hostname(), "github.com") {
		parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
		if len(parts) == 4 && strings.EqualFold(parts[2], "pull") {
			if n, e := strconv.ParseInt(parts[3], 10, 64); e == nil && n > 0 {
				return "github-pr://" + u.Host + "/" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1]) + "/" + strconv.FormatInt(n, 10), nil
			}
		}
	}
	return u.String(), nil
}

func (st *WorkstreamStore) Execute(ctx context.Context, c WorkstreamCommand) (CommandResult, error) {
	// A store is process-local and SQLite's deferred WAL transactions can
	// otherwise surface a busy snapshot after both requests read a revision.
	// Serialize commands so the second request observes a domain ConflictError.
	st.mu.Lock()
	defer st.mu.Unlock()
	if strings.TrimSpace(c.OperationID) == "" || len(c.OperationID) > 200 {
		return CommandResult{}, &ValidationError{Fields: map[string]string{"operation_id": "is required"}}
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return CommandResult{}, err
	}
	digest := sha256.Sum256(payload)
	requestHash := hex.EncodeToString(digest[:])
	tx, err := st.beginWrite(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	defer tx.Rollback()
	var old CommandResult
	var oldHash string
	err = tx.QueryRowContext(ctx, `SELECT operation_id,workstream_id,item_id,decision_id,revision,request_hash FROM operation_receipts WHERE operation_id=?`, c.OperationID).Scan(&old.OperationID, &old.WorkstreamID, &old.ItemID, &old.DecisionID, &old.Revision, &oldHash)
	if err == nil {
		if oldHash != requestHash {
			return CommandResult{}, &ValidationError{Fields: map[string]string{"operation_id": "This submission was already used. Reload the form and review your changes before saving."}}
		}
		old.Idempotent = true
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CommandResult{}, err
	}
	var out CommandResult
	switch c.Action {
	case "create_workstream":
		out, err = st.createWorkstream(ctx, tx, c)
	case "edit_workstream", "complete_workstream", "reopen_workstream", "archive_workstream", "restore_workstream":
		out, err = st.changeWorkstream(ctx, tx, c)
	case "create_item":
		out, err = st.createItem(ctx, tx, c)
	case "edit_item", "detach_item", "restore_item", "move_item":
		out, err = st.changeItem(ctx, tx, c)
	case "record_decision":
		out, err = st.recordDecision(ctx, tx, c)
	default:
		err = &ValidationError{Fields: map[string]string{"action": "unsupported action"}}
	}
	if err != nil {
		return CommandResult{}, err
	}
	out.OperationID = c.OperationID
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_receipts(operation_id,action,workstream_id,item_id,decision_id,revision,created_at,request_hash) VALUES(?,?,?,?,?,?,?,?)`, out.OperationID, c.Action, out.WorkstreamID, out.ItemID, out.DecisionID, out.Revision, dbTime(st.now()), requestHash); err != nil {
		return CommandResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return CommandResult{}, err
	}
	if st.afterCommit != nil {
		st.afterCommit()
	}
	return out, nil
}

func (st *WorkstreamStore) createWorkstream(ctx context.Context, tx *sql.Tx, c WorkstreamCommand) (CommandResult, error) {
	f := map[string]string{}
	in := c.Workstream
	in.Name = cleanRequired(in.Name, "name", 200, f)
	in.Outcome = cleanRequired(in.Outcome, "outcome", 4000, f)
	in.Owner = cleanOptional(in.Owner, "owner", 200, f)
	in.Sponsor = cleanOptional(in.Sponsor, "sponsor", 200, f)
	in.Notes = cleanOptional(in.Notes, "notes", 32000, f)
	if !validDate(in.TargetDate) {
		f["target_date"] = "must be YYYY-MM-DD"
	}
	if len(f) > 0 {
		return CommandResult{}, &ValidationError{Fields: f}
	}
	if in.Owner == "" {
		in.Owner = "me"
	}
	now := st.now()
	id := opaqueID()
	_, err := tx.ExecContext(ctx, `INSERT INTO workstreams(id,name,outcome,owner,sponsor,target_date,notes,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, in.Name, in.Outcome, in.Owner, in.Sponsor, in.TargetDate, in.Notes, dbTime(now), dbTime(now))
	if err != nil {
		return CommandResult{}, err
	}
	if err = st.event(ctx, tx, id, "workstream", id, "created", "Created workstream", 1); err != nil {
		return CommandResult{}, err
	}
	if err = st.schedule(ctx, tx, MirrorKey{"workstream", id}, 1); err != nil {
		return CommandResult{}, err
	}
	return CommandResult{WorkstreamID: id, Revision: 1}, nil
}

func (st *WorkstreamStore) loadWorkstream(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Workstream, error) {
	var w Workstream
	var archived int
	err := q.QueryRowContext(ctx, `SELECT id,name,outcome,owner,sponsor,target_date,notes,lifecycle,archived,completion_note,revision,created_at,updated_at FROM workstreams WHERE id=?`, id).Scan(&w.ID, &w.Name, &w.Outcome, &w.Owner, &w.Sponsor, &w.TargetDate, &w.Notes, &w.Lifecycle, &archived, &w.CompletionNote, &w.Revision, &w.CreatedAt, &w.UpdatedAt)
	w.Archived = archived != 0
	if errors.Is(err, sql.ErrNoRows) {
		return w, &NotFoundError{"workstream", id}
	}
	return w, err
}
func (st *WorkstreamStore) changeWorkstream(ctx context.Context, tx *sql.Tx, c WorkstreamCommand) (CommandResult, error) {
	w, err := st.loadWorkstream(ctx, tx, c.WorkstreamID)
	if err != nil {
		return CommandResult{}, err
	}
	if c.ExpectedRevision != w.Revision {
		return CommandResult{}, &ConflictError{w.ID, c.ExpectedRevision, w.Revision}
	}
	fields := map[string]string{}
	event := "edited"
	desc := "Edited workstream"
	now := st.now()
	lifecycle, archived, note := w.Lifecycle, w.Archived, ""
	switch c.Action {
	case "edit_workstream":
		if w.Archived || w.Lifecycle == "completed" {
			fields["workstream"] = "restore or reopen before editing"
		} else {
			in := c.Workstream
			in.Name = cleanRequired(in.Name, "name", 200, fields)
			in.Outcome = cleanRequired(in.Outcome, "outcome", 4000, fields)
			in.Owner = cleanOptional(in.Owner, "owner", 200, fields)
			in.Sponsor = cleanOptional(in.Sponsor, "sponsor", 200, fields)
			in.Notes = cleanOptional(in.Notes, "notes", 32000, fields)
			if !validDate(in.TargetDate) {
				fields["target_date"] = "must be YYYY-MM-DD"
			}
			if len(fields) == 0 {
				if in.Owner == "" {
					in.Owner = "me"
				}
				if in.Name == w.Name && in.Outcome == w.Outcome && in.Owner == w.Owner && in.Sponsor == w.Sponsor && in.TargetDate == w.TargetDate && in.Notes == w.Notes {
					return CommandResult{WorkstreamID: w.ID, Revision: w.Revision, Idempotent: true}, nil
				}
				changes := make([]string, 0, 6)
				add := func(label, old, next string) {
					if old != next {
						changes = append(changes, label+" "+timelineValue(old)+" → "+timelineValue(next))
					}
				}
				add("name", w.Name, in.Name)
				add("outcome", w.Outcome, in.Outcome)
				add("owner", w.Owner, in.Owner)
				add("sponsor", w.Sponsor, in.Sponsor)
				add("target date", w.TargetDate, in.TargetDate)
				add("notes", w.Notes, in.Notes)
				desc = itemEditDescription("workstream", changes)
				_, err = tx.ExecContext(ctx, `UPDATE workstreams SET name=?,outcome=?,owner=?,sponsor=?,target_date=?,notes=?,revision=revision+1,updated_at=? WHERE id=?`, in.Name, in.Outcome, in.Owner, in.Sponsor, in.TargetDate, in.Notes, dbTime(now), w.ID)
			}
		}
	case "complete_workstream":
		if w.Archived {
			fields["workstream"] = "restore before completing"
		}
		if w.Lifecycle == "completed" {
			fields["lifecycle"] = "workstream is already completed"
		}
		note = cleanRequired(c.Workstream.Notes, "outcome_note", 32000, fields)
		if !c.Confirm {
			fields["confirm"] = "confirmation is required"
		}
		lifecycle = "completed"
		event = "completed"
		desc = "Completed workstream: outcome " + timelineValue(note)
		if len(fields) == 0 && !c.Confirm {
			return CommandResult{}, &ValidationError{Fields: fields}
		}
		if len(fields) == 0 {
			var n int
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM workstream_items WHERE workstream_id=? AND detached_at IS NULL AND ((kind='task' AND tracking_state IN ('open','in_progress','blocked')) OR (kind='ask' AND ask_status IN ('open','waiting')) OR (kind='signal' AND assessment='concerning'))`, w.ID).Scan(&n)
			if err == nil && n > 0 && strings.TrimSpace(c.OverrideReason) == "" {
				fields["override_reason"] = "is required while unresolved items remain"
			}
		}
		c.OverrideReason = cleanOptional(c.OverrideReason, "override_reason", 32000, fields)
		if strings.TrimSpace(c.OverrideReason) != "" {
			desc += "; override reason " + timelineValue(c.OverrideReason)
		}
	case "reopen_workstream":
		if w.Archived {
			fields["workstream"] = "restore before reopening"
		}
		if w.Lifecycle != "completed" {
			fields["lifecycle"] = "only completed workstreams can reopen"
		}
		lifecycle = "active"
		event = "reopened"
		desc = "Reopened workstream"
	case "archive_workstream":
		if w.Archived {
			return CommandResult{WorkstreamID: w.ID, Revision: w.Revision, Idempotent: true}, nil
		}
		if !c.Confirm {
			fields["confirm"] = "confirmation is required"
		}
		archived = true
		event = "archived"
		desc = "Archived workstream"
	case "restore_workstream":
		if !w.Archived {
			return CommandResult{WorkstreamID: w.ID, Revision: w.Revision, Idempotent: true}, nil
		}
		archived = false
		event = "restored"
		desc = "Restored workstream"
	}
	if len(fields) > 0 {
		return CommandResult{}, &ValidationError{Fields: fields}
	}
	if c.Action != "edit_workstream" {
		if c.Action == "complete_workstream" {
			_, err = tx.ExecContext(ctx, `UPDATE workstreams SET lifecycle=?,archived=?,archived_at=?,completion_note=?,revision=revision+1,updated_at=? WHERE id=?`, lifecycle, boolInt(archived), nullableTime(archived, now), note, dbTime(now), w.ID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE workstreams SET lifecycle=?,archived=?,archived_at=?,revision=revision+1,updated_at=? WHERE id=?`, lifecycle, boolInt(archived), nullableTime(archived, now), dbTime(now), w.ID)
		}
	}
	if err != nil {
		return CommandResult{}, err
	}
	rev := w.Revision + 1
	if err = st.event(ctx, tx, w.ID, "workstream", w.ID, event, desc, rev); err != nil {
		return CommandResult{}, err
	}
	if err = st.schedule(ctx, tx, MirrorKey{"workstream", w.ID}, rev); err != nil {
		return CommandResult{}, err
	}
	return CommandResult{WorkstreamID: w.ID, Revision: rev}, nil
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func nullableTime(ok bool, t time.Time) any {
	if ok {
		return dbTime(t)
	}
	return nil
}

func (st *WorkstreamStore) ensureActive(ctx context.Context, tx *sql.Tx, id string) (Workstream, error) {
	w, e := st.loadWorkstream(ctx, tx, id)
	if e != nil {
		return w, e
	}
	if w.Archived || w.Lifecycle != "active" {
		return w, &ValidationError{Fields: map[string]string{"workstream": "workstream is read-only"}}
	}
	return w, nil
}
func (st *WorkstreamStore) source(ctx context.Context, tx *sql.Tx, in WorkstreamItemInput) (string, error) {
	f := map[string]string{}
	if utf8.RuneCountInString(in.SourceURL) > 4096 {
		f["source_url"] = "must be at most 4096 characters"
	}
	if utf8.RuneCountInString(in.SourceLabel) > 200 {
		f["source_label"] = "must be at most 200 characters"
	}
	if utf8.RuneCountInString(in.SourceKind) > 200 {
		f["source_kind"] = "must be at most 200 characters"
	}
	if len(f) > 0 {
		return "", &ValidationError{Fields: f}
	}
	if in.PRID == 0 && strings.TrimSpace(in.SourceURL) == "" {
		if in.Kind == "reference" {
			return "", &ValidationError{Fields: map[string]string{"source_url": "Choose a cached PR or enter a source URL for a reference."}}
		}
		return "", nil
	}
	now := st.now()
	var raw, canon, kind, label string
	var pr any
	if in.PRID != 0 {
		var url string
		err := tx.QueryRowContext(ctx, `SELECT url FROM prs WHERE id=?`, in.PRID).Scan(&url)
		if errors.Is(err, sql.ErrNoRows) {
			return "", &ValidationError{Fields: map[string]string{"pr_id": "cached PR not found"}}
		}
		if err != nil {
			return "", err
		}
		raw = url
		pr = in.PRID
		kind = "pr"
	} else {
		raw = in.SourceURL
		kind = cleanOptional(in.SourceKind, "source_kind", 200, map[string]string{})
		if kind == "" {
			kind = "url"
		}
	}
	var err error
	canon, err = canonicalSource(raw)
	if err != nil {
		return "", &ValidationError{Fields: map[string]string{"source_url": err.Error()}}
	}
	label = strings.TrimSpace(in.SourceLabel)
	if label == "" {
		label = strings.TrimSpace(in.Title)
	}
	// A pasted GitHub PR URL is still only a local reference, but when its
	// canonical host/owner/repo/number already exists in Cockpit we reuse the
	// cached identity. Query/fragment are preserved for generic source identity
	// and never trigger discovery.
	if in.PRID == 0 {
		if id, ok := cachedPRIDForURL(ctx, tx, raw); ok {
			pr = id
			kind = "pr"
		}
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM sources WHERE canonical_id=?`, canon).Scan(&id)
	if err == nil {
		if pr != nil {
			_, err = tx.ExecContext(ctx, `UPDATE sources SET pr_id=COALESCE(pr_id,?),kind=CASE WHEN pr_id IS NULL THEN 'pr' ELSE kind END,updated_at=? WHERE id=?`, pr, dbTime(now), id)
			if err != nil {
				return "", err
			}
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = opaqueID()
	_, err = tx.ExecContext(ctx, `INSERT INTO sources(id,kind,url,canonical_id,label,pr_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, kind, raw, canon, label, pr, dbTime(now), dbTime(now))
	return id, err
}

func cachedPRIDForURL(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, raw string) (int64, bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
		return 0, false
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) != 4 || !strings.EqualFold(parts[2], "pull") {
		return 0, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return 0, false
	}
	var id int64
	var prURL string
	err = q.QueryRowContext(ctx, `SELECT id,url FROM prs WHERE lower(owner)=lower(?) AND lower(repo)=lower(?) AND number=?`, parts[0], parts[1], n).Scan(&id, &prURL)
	if err != nil {
		return 0, false
	}
	inputCanonical, inputErr := canonicalSource(raw)
	prCanonical, prErr := canonicalSource(prURL)
	return id, inputErr == nil && prErr == nil && inputCanonical == prCanonical
}

func (st *WorkstreamStore) createItem(ctx context.Context, tx *sql.Tx, c WorkstreamCommand) (CommandResult, error) {
	w, err := st.ensureActive(ctx, tx, c.WorkstreamID)
	if err != nil {
		return CommandResult{}, err
	}
	if c.ExpectedRevision != w.Revision {
		return CommandResult{}, &ConflictError{w.ID, c.ExpectedRevision, w.Revision}
	}
	in := c.Item
	f := map[string]string{}
	in.Kind = cleanRequired(in.Kind, "kind", 20, f)
	in.Title = cleanRequired(in.Title, "title", 200, f)
	in.Description = cleanOptional(in.Description, "description", 32000, f)
	if in.Kind != "task" && in.Kind != "reference" && in.Kind != "ask" && in.Kind != "signal" {
		f["kind"] = "must be task, reference, ask, or signal"
	}
	normalizeItemInput(&in, f)
	if in.Kind == "task" {
		if in.TrackingState == "" {
			in.TrackingState = "open"
		}
		if !one(in.TrackingState, "open", "in_progress", "blocked", "done", "cancelled") {
			f["tracking_state"] = "invalid state"
		}
		if in.TrackingState == "blocked" && strings.TrimSpace(in.BlockerReason) == "" {
			f["blocker_reason"] = "is required when blocked"
		}
		if !validDate(in.DueDate) {
			f["due_date"] = "must be YYYY-MM-DD"
		}
	}
	if in.Kind == "ask" {
		in.Counterpart = cleanRequired(in.Counterpart, "counterpart", 200, f)
		if in.AskStatus == "" {
			in.AskStatus = "open"
		}
		if !one(in.AskStatus, "open", "waiting", "resolved", "cancelled") {
			f["ask_status"] = "invalid state"
		}
	}
	if in.Kind == "signal" {
		if !one(in.Assessment, "normal", "concerning", "unknown") {
			f["assessment"] = "must be normal, concerning, or unknown"
		}
		if strings.TrimSpace(in.ObservedAt) == "" {
			f["observed_at"] = "is required"
		}
	}
	if len(f) > 0 {
		return CommandResult{}, &ValidationError{Fields: f}
	}
	sourceID, err := st.source(ctx, tx, in)
	if err != nil {
		return CommandResult{}, err
	}
	if in.Kind == "reference" && sourceID != "" {
		var existing string
		var existingRevision int64
		err = tx.QueryRowContext(ctx, `SELECT id,revision FROM workstream_items WHERE workstream_id=? AND source_id=? AND kind='reference' AND detached_at IS NULL`, w.ID, sourceID).Scan(&existing, &existingRevision)
		if err == nil {
			return CommandResult{WorkstreamID: w.ID, ItemID: existing, Revision: existingRevision, Idempotent: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return CommandResult{}, err
		}
		var detached string
		var detachedRev int64
		err = tx.QueryRowContext(ctx, `SELECT id,revision FROM workstream_items WHERE workstream_id=? AND source_id=? AND kind='reference' AND detached_at IS NOT NULL ORDER BY detached_at DESC LIMIT 1`, w.ID, sourceID).Scan(&detached, &detachedRev)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE workstream_items SET detached_at=NULL,revision=revision+1,updated_at=? WHERE id=?`, dbTime(st.now()), detached)
			if err != nil {
				return CommandResult{}, err
			}
			newRev := detachedRev + 1
			if err = st.event(ctx, tx, w.ID, "engineering_item", detached, "reattached", "Reattached reference", newRev); err != nil {
				return CommandResult{}, err
			}
			if err = st.schedule(ctx, tx, MirrorKey{"engineering_item", detached}, newRev); err != nil {
				return CommandResult{}, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE workstreams SET revision=revision+1,updated_at=? WHERE id=?`, dbTime(st.now()), w.ID); err != nil {
				return CommandResult{}, err
			}
			if err = st.schedule(ctx, tx, MirrorKey{"workstream", w.ID}, w.Revision+1); err != nil {
				return CommandResult{}, err
			}
			return CommandResult{WorkstreamID: w.ID, ItemID: detached, Revision: newRev}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return CommandResult{}, err
		}
	}
	follow := parseInstant(in.FollowUpAt, f, "follow_up_at")
	contact := parseInstant(in.LastContactAt, f, "last_contact_at")
	observed := parseInstant(in.ObservedAt, f, "observed_at")
	review := parseInstant(in.ReviewBy, f, "review_by")
	if len(f) > 0 {
		return CommandResult{}, &ValidationError{Fields: f}
	}
	id := opaqueID()
	now := st.now()
	_, err = tx.ExecContext(ctx, `INSERT INTO workstream_items(id,workstream_id,kind,title,description,source_id,due_date,tracking_state,blocker_reason,counterpart,ask_status,follow_up_at,last_contact_at,value,unit,observed_at,assessment,review_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, w.ID, in.Kind, in.Title, in.Description, nilIfEmpty(sourceID), in.DueDate, in.TrackingState, in.BlockerReason, in.Counterpart, in.AskStatus, timeArg(follow), timeArg(contact), in.Value, in.Unit, timeArg(observed), in.Assessment, timeArg(review), dbTime(now), dbTime(now))
	if err != nil {
		return CommandResult{}, err
	}
	if err = st.event(ctx, tx, w.ID, itemType(in.Kind), id, "created", "Created "+in.Kind+": "+timelineValue(in.Title), 1); err != nil {
		return CommandResult{}, err
	}
	if err = st.schedule(ctx, tx, MirrorKey{itemType(in.Kind), id}, 1); err != nil {
		return CommandResult{}, err
	}
	if err = st.schedule(ctx, tx, MirrorKey{"workstream", w.ID}, w.Revision+1); err != nil {
		return CommandResult{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE workstreams SET revision=revision+1,updated_at=? WHERE id=?`, dbTime(now), w.ID)
	return CommandResult{WorkstreamID: w.ID, ItemID: id, Revision: 1}, err
}
func one(v string, xs ...string) bool {
	for _, x := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func itemType(k string) string {
	if k == "task" || k == "reference" {
		return "engineering_item"
	}
	return k
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return dbTime(*t)
}
func sameDBTime(old sql.NullTime, next *time.Time) bool {
	if !old.Valid {
		return next == nil
	}
	return next != nil && old.Time.UTC().Equal(next.UTC())
}

// changeItem deliberately takes a full replacement input; callers must send
// the current revision and kind is immutable, preventing cross-kind coercion.
func (st *WorkstreamStore) changeItem(ctx context.Context, tx *sql.Tx, c WorkstreamCommand) (CommandResult, error) {
	var it WorkstreamItem
	var detached sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT id,COALESCE(workstream_id,''),kind,title,revision,detached_at FROM workstream_items WHERE id=?`, c.ItemID).Scan(&it.ID, &it.WorkstreamID, &it.Kind, &it.Title, &it.Revision, &detached)
	if errors.Is(err, sql.ErrNoRows) {
		return CommandResult{}, &NotFoundError{"item", c.ItemID}
	}
	if err != nil {
		return CommandResult{}, err
	}
	if detached.Valid {
		it.DetachedAt = &detached.Time
	}
	if c.ExpectedRevision != it.Revision {
		return CommandResult{}, &ConflictError{it.ID, c.ExpectedRevision, it.Revision}
	}
	now := st.now()
	parent := it.WorkstreamID
	event, desc := "edited", "Edited "+it.Kind
	switch c.Action {
	case "edit_item":
		if _, e := st.ensureActive(ctx, tx, it.WorkstreamID); e != nil {
			return CommandResult{}, e
		}
		in := c.Item
		f := map[string]string{}
		if in.Kind != "" && in.Kind != it.Kind {
			f["kind"] = "item kind cannot change"
		}
		if in.Kind == "" {
			in.Kind = it.Kind
		}
		in.Title = cleanRequired(in.Title, "title", 200, f)
		in.Description = cleanOptional(in.Description, "description", 32000, f)
		normalizeItemInput(&in, f)
		if it.Kind == "task" {
			if !one(in.TrackingState, "open", "in_progress", "blocked", "done", "cancelled") {
				f["tracking_state"] = "invalid state"
			}
			if in.TrackingState == "blocked" && strings.TrimSpace(in.BlockerReason) == "" {
				f["blocker_reason"] = "is required when blocked"
			}
			if !validDate(in.DueDate) {
				f["due_date"] = "must be YYYY-MM-DD"
			}
		}
		if it.Kind == "ask" {
			in.Counterpart = cleanRequired(in.Counterpart, "counterpart", 200, f)
			if !one(in.AskStatus, "open", "waiting", "resolved", "cancelled") {
				f["ask_status"] = "invalid state"
			}
		}
		if it.Kind == "signal" {
			if !one(in.Assessment, "normal", "concerning", "unknown") {
				f["assessment"] = "invalid assessment"
			}
			if strings.TrimSpace(in.ObservedAt) == "" {
				f["observed_at"] = "is required"
			}
		}
		follow := parseInstant(in.FollowUpAt, f, "follow_up_at")
		contact := parseInstant(in.LastContactAt, f, "last_contact_at")
		observed := parseInstant(in.ObservedAt, f, "observed_at")
		review := parseInstant(in.ReviewBy, f, "review_by")
		if len(f) > 0 {
			return CommandResult{}, &ValidationError{Fields: f}
		}
		var replacementSource any = nil
		if strings.TrimSpace(in.SourceURL) != "" || in.PRID != 0 {
			sid, e := st.source(ctx, tx, in)
			if e != nil {
				return CommandResult{}, e
			}
			replacementSource = nilIfEmpty(sid)
		}
		var oldTitle, oldDescription, oldSource, oldDue, oldTracking, oldBlocker, oldCounterpart, oldAsk, oldValue, oldUnit, oldAssessment string
		var oldFollow, oldContact, oldObserved, oldReview sql.NullTime
		if err = tx.QueryRowContext(ctx, `SELECT title,description,COALESCE(source_id,''),due_date,tracking_state,blocker_reason,counterpart,ask_status,value,unit,assessment,follow_up_at,last_contact_at,observed_at,review_by FROM workstream_items WHERE id=?`, it.ID).Scan(&oldTitle, &oldDescription, &oldSource, &oldDue, &oldTracking, &oldBlocker, &oldCounterpart, &oldAsk, &oldValue, &oldUnit, &oldAssessment, &oldFollow, &oldContact, &oldObserved, &oldReview); err != nil {
			return CommandResult{}, err
		}
		// No-op saves are acknowledged through the operation receipt but do not
		// invent a revision, timeline event, or mirror export.
		newSource := oldSource
		if replacementSource != nil {
			newSource = replacementSource.(string)
		}
		if it.Kind == "reference" && newSource != oldSource {
			var n int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM workstream_items WHERE workstream_id=? AND kind='reference' AND source_id=? AND detached_at IS NULL AND id<>?`, it.WorkstreamID, newSource, it.ID).Scan(&n); err != nil {
				return CommandResult{}, err
			}
			if n > 0 {
				return CommandResult{}, &ValidationError{Fields: map[string]string{"source_url": "this workstream already has that reference"}}
			}
		}
		if oldTitle == in.Title && oldDescription == in.Description && oldSource == newSource && oldDue == in.DueDate && oldTracking == in.TrackingState && oldBlocker == in.BlockerReason && oldCounterpart == in.Counterpart && oldAsk == in.AskStatus && oldValue == in.Value && oldUnit == in.Unit && oldAssessment == in.Assessment && sameDBTime(oldFollow, follow) && sameDBTime(oldContact, contact) && sameDBTime(oldObserved, observed) && sameDBTime(oldReview, review) {
			return CommandResult{WorkstreamID: it.WorkstreamID, ItemID: it.ID, Revision: it.Revision, Idempotent: true}, nil
		}
		changes := make([]string, 0, 12)
		add := func(label, old, next string) {
			if old != next {
				changes = append(changes, label+" "+timelineValue(old)+" → "+timelineValue(next))
			}
		}
		add("title", oldTitle, in.Title)
		add("description", oldDescription, in.Description)
		if oldSource != newSource {
			changes = append(changes, "source reference updated")
		}
		add("due date", oldDue, in.DueDate)
		add("state", oldTracking, in.TrackingState)
		add("blocker", oldBlocker, in.BlockerReason)
		add("counterpart", oldCounterpart, in.Counterpart)
		add("ask status", oldAsk, in.AskStatus)
		add("value", oldValue, in.Value)
		add("unit", oldUnit, in.Unit)
		add("assessment", oldAssessment, in.Assessment)
		add("follow-up", timelineTime(oldFollow), timelineNextTime(follow))
		add("last contact", timelineTime(oldContact), timelineNextTime(contact))
		add("observed at", timelineTime(oldObserved), timelineNextTime(observed))
		add("review by", timelineTime(oldReview), timelineNextTime(review))
		desc = itemEditDescription(it.Kind, changes)
		_, err = tx.ExecContext(ctx, `UPDATE workstream_items SET title=?,description=?,source_id=CASE WHEN ? IS NULL THEN source_id ELSE ? END,due_date=?,tracking_state=?,blocker_reason=?,counterpart=?,ask_status=?,follow_up_at=?,last_contact_at=?,value=?,unit=?,observed_at=?,assessment=?,review_by=?,revision=revision+1,updated_at=? WHERE id=?`, in.Title, in.Description, replacementSource, replacementSource, in.DueDate, in.TrackingState, in.BlockerReason, in.Counterpart, in.AskStatus, timeArg(follow), timeArg(contact), in.Value, in.Unit, timeArg(observed), in.Assessment, timeArg(review), dbTime(now), it.ID)
	case "detach_item":
		if !c.Confirm {
			return CommandResult{}, &ValidationError{Fields: map[string]string{"confirm": "confirmation is required"}}
		}
		if it.DetachedAt != nil {
			return CommandResult{WorkstreamID: it.WorkstreamID, ItemID: it.ID, Revision: it.Revision, Idempotent: true}, nil
		}
		if _, e := st.ensureActive(ctx, tx, it.WorkstreamID); e != nil {
			return CommandResult{}, e
		}
		_, err = tx.ExecContext(ctx, `UPDATE workstream_items SET detached_at=?,revision=revision+1,updated_at=? WHERE id=?`, dbTime(now), dbTime(now), it.ID)
		event = "detached"
		desc = "Detached " + it.Kind
	case "restore_item":
		if it.DetachedAt == nil {
			return CommandResult{}, &ValidationError{Fields: map[string]string{"item": "item is already attached"}}
		}
		// Detach retains the original parent. Restore is deliberately not a move:
		// make the item attached first, then use the audited move operation.
		if c.WorkstreamID != "" && c.WorkstreamID != it.WorkstreamID {
			return CommandResult{}, &ValidationError{Fields: map[string]string{"workstream_id": "restore must use the original workstream; move after restoring"}}
		}
		_, err = st.ensureActive(ctx, tx, it.WorkstreamID)
		if err == nil {
			if it.Kind == "reference" {
				var n int
				err = tx.QueryRowContext(ctx, `SELECT count(*) FROM workstream_items WHERE workstream_id=? AND kind='reference' AND source_id=(SELECT source_id FROM workstream_items WHERE id=?) AND detached_at IS NULL`, it.WorkstreamID, it.ID).Scan(&n)
				if err == nil && n > 0 {
					return CommandResult{}, &ValidationError{Fields: map[string]string{"item": "this workstream already has that reference"}}
				}
			}
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE workstream_items SET detached_at=NULL,revision=revision+1,updated_at=? WHERE id=?`, dbTime(now), it.ID)
			}
		}
		event = "restored"
		desc = "Restored " + it.Kind
	case "move_item":
		if it.DetachedAt != nil {
			return CommandResult{}, &ValidationError{Fields: map[string]string{"item": "restore detached item before moving"}}
		}
		if _, e := st.ensureActive(ctx, tx, it.WorkstreamID); e != nil {
			return CommandResult{}, e
		}
		if c.DestinationWorkstreamID == "" {
			return CommandResult{}, &ValidationError{Fields: map[string]string{"destination_workstream_id": "is required"}}
		}
		_, err = st.ensureActive(ctx, tx, c.DestinationWorkstreamID)
		if err == nil && it.Kind == "reference" {
			var n int
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM workstream_items WHERE workstream_id=? AND kind='reference' AND source_id=(SELECT source_id FROM workstream_items WHERE id=?) AND detached_at IS NULL`, c.DestinationWorkstreamID, it.ID).Scan(&n)
			if err == nil && n > 0 {
				return CommandResult{}, &ValidationError{Fields: map[string]string{"destination_workstream_id": "already has this reference"}}
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE workstream_items SET workstream_id=?,revision=revision+1,updated_at=? WHERE id=?`, c.DestinationWorkstreamID, dbTime(now), it.ID)
			parent = c.DestinationWorkstreamID
		}
		event = "moved"
		desc = "Moved " + it.Kind
	default:
		return CommandResult{}, &ValidationError{Fields: map[string]string{"action": "editing item fields is not yet supported"}}
	}
	if err != nil {
		return CommandResult{}, err
	}
	if it.WorkstreamID != "" {
		if err = st.event(ctx, tx, it.WorkstreamID, itemType(it.Kind), it.ID, event, desc, it.Revision+1); err != nil {
			return CommandResult{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE workstreams SET revision=revision+1,updated_at=? WHERE id=?`, dbTime(now), it.WorkstreamID); err != nil {
			return CommandResult{}, err
		}
		var parentRevision int64
		if err = tx.QueryRowContext(ctx, `SELECT revision FROM workstreams WHERE id=?`, it.WorkstreamID).Scan(&parentRevision); err != nil {
			return CommandResult{}, err
		}
		if err = st.schedule(ctx, tx, MirrorKey{"workstream", it.WorkstreamID}, parentRevision); err != nil {
			return CommandResult{}, err
		}
	}
	if parent != "" && parent != it.WorkstreamID {
		if err = st.event(ctx, tx, parent, itemType(it.Kind), it.ID, event, desc, it.Revision+1); err != nil {
			return CommandResult{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE workstreams SET revision=revision+1,updated_at=? WHERE id=?`, dbTime(now), parent); err != nil {
			return CommandResult{}, err
		}
		var parentRevision int64
		if err = tx.QueryRowContext(ctx, `SELECT revision FROM workstreams WHERE id=?`, parent).Scan(&parentRevision); err != nil {
			return CommandResult{}, err
		}
		if err = st.schedule(ctx, tx, MirrorKey{"workstream", parent}, parentRevision); err != nil {
			return CommandResult{}, err
		}
	}
	if err = st.schedule(ctx, tx, MirrorKey{itemType(it.Kind), it.ID}, it.Revision+1); err != nil {
		return CommandResult{}, err
	}
	return CommandResult{WorkstreamID: parent, ItemID: it.ID, Revision: it.Revision + 1}, nil
}

func (st *WorkstreamStore) recordDecision(ctx context.Context, tx *sql.Tx, c WorkstreamCommand) (CommandResult, error) {
	w, err := st.ensureActive(ctx, tx, c.WorkstreamID)
	if err != nil {
		return CommandResult{}, err
	}
	if c.ExpectedRevision != w.Revision {
		return CommandResult{}, &ConflictError{w.ID, c.ExpectedRevision, w.Revision}
	}
	f := map[string]string{}
	note := cleanRequired(c.Decision.Note, "note", 32000, f)
	// Decision references must describe this workstream, not merely satisfy
	// global foreign keys. Shared sources are valid only through local items.
	if c.Decision.ItemID != "" {
		var parent string
		var source sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT workstream_id,source_id FROM workstream_items WHERE id=?`, c.Decision.ItemID).Scan(&parent, &source)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && parent != w.ID) {
			f["decision_item_id"] = "Choose an item from this workstream."
		} else if err != nil {
			return CommandResult{}, err
		} else if c.Decision.SourceID != "" && c.Decision.SourceID != source.String {
			f["decision_source_id"] = "Choose the source associated with the related item."
		}
	} else if c.Decision.SourceID != "" {
		var associated bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workstream_items WHERE workstream_id=? AND source_id=? AND detached_at IS NULL)`, w.ID, c.Decision.SourceID).Scan(&associated)
		if err != nil {
			return CommandResult{}, err
		}
		if !associated {
			f["decision_source_id"] = "Choose a source associated with this workstream."
		}
	}
	if len(f) > 0 {
		return CommandResult{}, &ValidationError{Fields: f}
	}
	id := opaqueID()
	now := st.now()
	_, err = tx.ExecContext(ctx, `INSERT INTO decisions(id,workstream_id,item_id,source_id,note,created_at) VALUES(?,?,?,?,?,?)`, id, w.ID, nilIfEmpty(c.Decision.ItemID), nilIfEmpty(c.Decision.SourceID), note, dbTime(now))
	if err != nil {
		return CommandResult{}, err
	}
	if err = st.event(ctx, tx, w.ID, "decision", id, "recorded", "Recorded decision", 1); err != nil {
		return CommandResult{}, err
	}
	if err = st.schedule(ctx, tx, MirrorKey{"decision", id}, 1); err != nil {
		return CommandResult{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE workstreams SET revision=revision+1,updated_at=? WHERE id=?`, dbTime(now), w.ID)
	if err == nil {
		err = st.schedule(ctx, tx, MirrorKey{"workstream", w.ID}, w.Revision+1)
	}
	return CommandResult{WorkstreamID: w.ID, DecisionID: id, Revision: 1}, err
}
func (st *WorkstreamStore) event(ctx context.Context, tx *sql.Tx, wid, typ, id, event, desc string, rev int64) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO timeline_events(id,workstream_id,entity_type,entity_id,event_type,description,revision,created_at) VALUES(?,?,?,?,?,?,?,?)`, opaqueID(), wid, typ, id, event, desc, rev, dbTime(st.now()))
	return e
}
func (st *WorkstreamStore) schedule(ctx context.Context, tx *sql.Tx, k MirrorKey, rev int64) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO mirror_states(entity_type,entity_id,desired_revision,status) VALUES(?,?,?,'pending') ON CONFLICT(entity_type,entity_id) DO UPDATE SET desired_revision=MAX(mirror_states.desired_revision,excluded.desired_revision),last_attempt_at=CASE WHEN excluded.desired_revision>mirror_states.desired_revision THEN NULL ELSE mirror_states.last_attempt_at END,attempt_count=CASE WHEN excluded.desired_revision>mirror_states.desired_revision THEN 0 ELSE mirror_states.attempt_count END,next_attempt_at=CASE WHEN excluded.desired_revision>mirror_states.desired_revision THEN NULL ELSE mirror_states.next_attempt_at END,status=CASE WHEN mirror_states.status='conflict' THEN 'conflict' ELSE 'pending' END,error=CASE WHEN mirror_states.status='conflict' THEN mirror_states.error ELSE '' END`, k.EntityType, k.EntityID, rev)
	return e
}
