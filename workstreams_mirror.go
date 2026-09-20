package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NextPendingMirror returns one eligible export in a stable order.
func (st *WorkstreamStore) NextPendingMirror(ctx context.Context, now time.Time) (MirrorStateView, bool, error) {
	var state MirrorStateView
	var attempted, succeeded sql.NullTime
	err := st.db.QueryRowContext(ctx, `
		SELECT entity_type,entity_id,desired_revision,last_written_revision,last_success_revision,
		       last_checksum,status,error,last_attempt_at,last_success_at
		FROM mirror_states
		WHERE status IN ('pending','error')
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		ORDER BY COALESCE(next_attempt_at, '1970-01-01 00:00:00.000'), entity_type, entity_id
		LIMIT 1`, dbTime(now)).Scan(
		&state.EntityType, &state.EntityID, &state.DesiredRevision, &state.LastWrittenRevision,
		&state.LastSuccessRevision, &state.LastChecksum, &state.Status, &state.Error, &attempted, &succeeded,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorStateView{}, false, nil
	}
	if err != nil {
		return MirrorStateView{}, false, err
	}
	if attempted.Valid {
		state.LastAttemptAt = &attempted.Time
	}
	if succeeded.Valid {
		state.LastSuccessAt = &succeeded.Time
	}
	return state, true, nil
}

// AckMirrorWrite records every known filesystem write, but only marks an
// export synced when it is still the desired revision. This lets an old write
// supply the checksum needed for the next conflict check without incorrectly
// acknowledging a newer local mutation.
func (st *WorkstreamStore) AckMirrorWrite(ctx context.Context, key MirrorKey, writtenRevision int64, sum string, at time.Time) error {
	tx, err := st.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prior, operational string
	var desired int64
	if err := tx.QueryRowContext(ctx, `SELECT status,desired_revision,last_operational_state FROM mirror_states WHERE entity_type=? AND entity_id=?`, key.EntityType, key.EntityID).Scan(&prior, &desired, &operational); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &NotFoundError{EntityType: "mirror_state", EntityID: key.EntityType + ":" + key.EntityID}
		}
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE mirror_states
		SET last_written_revision=CASE WHEN last_written_revision > ? THEN last_written_revision ELSE ? END,
		    last_checksum=CASE WHEN last_written_revision > ? THEN last_checksum ELSE ? END,
		    last_attempt_at=?,
		    last_success_revision=CASE WHEN last_success_revision > ? THEN last_success_revision ELSE ? END,
		    last_success_at=CASE WHEN last_success_revision > ? THEN last_success_at ELSE ? END,
		    status=CASE WHEN desired_revision=? THEN 'synced' ELSE 'pending' END,
		    error=CASE WHEN desired_revision=? THEN '' ELSE error END,
		    attempt_count=0,next_attempt_at=NULL,
		    last_operational_state=CASE WHEN desired_revision=? THEN 'synced' ELSE last_operational_state END
		WHERE entity_type=? AND entity_id=?`,
		writtenRevision, writtenRevision, writtenRevision, sum, dbTime(at),
		writtenRevision, writtenRevision, writtenRevision, dbTime(at), writtenRevision, writtenRevision, writtenRevision,
		key.EntityType, key.EntityID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{EntityType: "mirror_state", EntityID: key.EntityType + ":" + key.EntityID}
	}
	if (operational == "error" || operational == "conflict") && desired == writtenRevision {
		if err := st.mirrorTimeline(ctx, tx, key, "recovered", "Mirror export recovered"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordMirrorFailure stores a retryable I/O failure or a terminal conflict.
// It intentionally does not schedule or write content revisions/timeline rows:
// operational history is deduplicated by state transition in the caller's
// durable state, not by each retry.
func (st *WorkstreamStore) RecordMirrorFailure(ctx context.Context, key MirrorKey, _ int64, message string, conflict bool, at time.Time) error {
	status := "error"
	if conflict {
		status = "conflict"
	}
	tx, err := st.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prior, operational string
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT status,last_operational_state,attempt_count FROM mirror_states WHERE entity_type=? AND entity_id=?`, key.EntityType, key.EntityID).Scan(&prior, &operational, &attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &NotFoundError{EntityType: "mirror_state", EntityID: key.EntityType + ":" + key.EntityID}
		}
		return err
	}
	delay := mirrorRetryDelay(attempts)
	next := at.Add(delay)
	if conflict {
		next = time.Time{}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE mirror_states
		SET status=?, error=?, last_attempt_at=?, attempt_count=attempt_count+1,
		    next_attempt_at=?, last_operational_state=CASE WHEN last_operational_state=? THEN last_operational_state ELSE ? END
		WHERE entity_type=? AND entity_id=?`, status, message, dbTime(at), nullableTime(!next.IsZero(), next), status, status, key.EntityType, key.EntityID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{EntityType: "mirror_state", EntityID: key.EntityType + ":" + key.EntityID}
	}
	if operational != status {
		event, desc := "mirror_error", "Mirror export needs retry"
		if conflict {
			event, desc = "mirror_conflict", "Mirror export conflict needs reconciliation"
		}
		if err := st.mirrorTimeline(ctx, tx, key, event, desc); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func mirrorRetryDelay(attempt int) time.Duration {
	delay := mirrorRetryFloor
	for i := 0; i < attempt && delay < time.Minute; i++ {
		delay *= 2
	}
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

// mirrorTimeline records only operational state transitions. It never
// schedules an export or changes a content revision, preventing retry loops.
func (st *WorkstreamStore) mirrorTimeline(ctx context.Context, tx *sql.Tx, key MirrorKey, event, description string) error {
	workstreamID, err := st.mirrorWorkstreamID(ctx, tx, key)
	if err != nil || workstreamID == "" {
		return err
	}
	return st.event(ctx, tx, workstreamID, key.EntityType, key.EntityID, event, description, 0)
}

func (st *WorkstreamStore) mirrorWorkstreamID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key MirrorKey) (string, error) {
	if key.EntityType == "workstream" {
		return key.EntityID, nil
	}
	var id string
	var err error
	switch key.EntityType {
	case "decision":
		err = q.QueryRowContext(ctx, `SELECT workstream_id FROM decisions WHERE id=?`, key.EntityID).Scan(&id)
	case "engineering_item", "ask", "signal":
		err = q.QueryRowContext(ctx, `SELECT COALESCE(workstream_id,'') FROM workstream_items WHERE id=?`, key.EntityID).Scan(&id)
	default:
		return "", nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RetryMirror rechecks a durable row after the user has reconciled the vault.
// It grants no overwrite authority: the publisher repeats checksum and
// symlink/collision checks, so an unresolved conflict immediately conflicts
// again rather than being force-replaced.
func (st *WorkstreamStore) RetryMirror(ctx context.Context, key MirrorKey, at time.Time) error {
	result, err := st.db.ExecContext(ctx, `
		UPDATE mirror_states
		SET status='pending', error='', last_attempt_at=NULL, next_attempt_at=NULL
		WHERE entity_type=? AND entity_id=?`, key.EntityType, key.EntityID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{EntityType: "mirror_state", EntityID: key.EntityType + ":" + key.EntityID}
	}
	_ = at // explicit retry is immediately eligible; no wall-clock state is needed.
	return nil
}

// RenderMirrorSnapshot reads a coherent committed snapshot. It refuses to
// render when the requested entity revision is no longer current; callers then
// leave the durable desired revision pending for the next pass.
func (st *WorkstreamStore) RenderMirrorSnapshot(ctx context.Context, key MirrorKey, revision int64) (MirrorDocument, error) {
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MirrorDocument{}, err
	}
	defer tx.Rollback()
	var doc MirrorDocument
	switch key.EntityType {
	case "workstream":
		doc, err = st.renderWorkstreamMirror(ctx, tx, key, revision)
	case "engineering_item", "ask", "signal":
		doc, err = st.renderItemMirror(ctx, tx, key, revision)
	case "decision":
		doc, err = st.renderDecisionMirror(ctx, tx, key, revision)
	default:
		err = ErrRevisionUnavailable
	}
	if err != nil {
		return MirrorDocument{}, err
	}
	if err := tx.Commit(); err != nil {
		return MirrorDocument{}, err
	}
	return doc, nil
}

func (st *WorkstreamStore) renderWorkstreamMirror(ctx context.Context, tx *sql.Tx, key MirrorKey, revision int64) (MirrorDocument, error) {
	w, err := st.loadWorkstream(ctx, tx, key.EntityID)
	if err != nil || w.Revision != revision {
		return MirrorDocument{}, ErrRevisionUnavailable
	}
	doc := newMirrorDocument(key, revision, w.Name, "workstreams/"+w.ID+".md", w.CreatedAt, w.UpdatedAt)
	doc.Frontmatter["lifecycle"] = w.Lifecycle
	doc.Frontmatter["archived"] = fmt.Sprintf("%t", w.Archived)
	var b strings.Builder
	b.WriteString("## Outcome\n\n")
	b.WriteString(w.Outcome)
	b.WriteString("\n\n## Context\n\n")
	b.WriteString("- Owner: ")
	b.WriteString(w.Owner)
	if w.Sponsor != "" {
		b.WriteString("\n- Sponsor: ")
		b.WriteString(w.Sponsor)
	}
	if w.TargetDate != "" {
		b.WriteString("\n- Target date: ")
		b.WriteString(w.TargetDate)
	}
	if w.Notes != "" {
		b.WriteString("\n\n## Notes\n\n")
		b.WriteString(w.Notes)
	}
	if w.CompletionNote != "" {
		b.WriteString("\n\n## Recorded completion outcome\n\n")
		b.WriteString(w.CompletionNote)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,title FROM workstream_items WHERE workstream_id=? AND detached_at IS NULL ORDER BY kind,title,id`, w.ID)
	if err != nil {
		return MirrorDocument{}, err
	}
	defer rows.Close()
	if rows.Next() {
		b.WriteString("\n\n## Attached items\n")
		for {
			var id, kind, title string
			if err := rows.Scan(&id, &kind, &title); err != nil {
				return MirrorDocument{}, err
			}
			kindDir, _ := mirrorDirectory(itemType(kind))
			doc.Links = append(doc.Links, MirrorLink{Label: title, RelativePath: "../" + kindDir + "/" + id + ".md"})
			if !rows.Next() {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return MirrorDocument{}, err
	}
	decisions, err := tx.QueryContext(ctx, `SELECT id,note FROM decisions WHERE workstream_id=? ORDER BY created_at,id`, w.ID)
	if err != nil {
		return MirrorDocument{}, err
	}
	defer decisions.Close()
	for decisions.Next() {
		var id, note string
		if err := decisions.Scan(&id, &note); err != nil {
			return MirrorDocument{}, err
		}
		doc.Links = append(doc.Links, MirrorLink{Label: "Decision: " + firstLine(note), RelativePath: "../decisions/" + id + ".md"})
	}
	if err := decisions.Err(); err != nil {
		return MirrorDocument{}, err
	}
	doc.Body = b.String()
	return doc, nil
}

func (st *WorkstreamStore) renderItemMirror(ctx context.Context, tx *sql.Tx, key MirrorKey, revision int64) (MirrorDocument, error) {
	item, err := loadMirrorItem(ctx, tx, key.EntityID)
	if err != nil || item.Revision != revision || itemType(item.Kind) != key.EntityType {
		return MirrorDocument{}, ErrRevisionUnavailable
	}
	doc := newMirrorDocument(key, revision, item.Title, mirrorPath(key), item.CreatedAt, item.UpdatedAt)
	doc.Frontmatter["kind"] = item.Kind
	doc.Frontmatter["workstream_id"] = item.WorkstreamID
	doc.Frontmatter["detached"] = fmt.Sprintf("%t", item.DetachedAt != nil)
	if item.WorkstreamID != "" {
		doc.Links = append(doc.Links, MirrorLink{Label: "Workstream", RelativePath: "../workstreams/" + item.WorkstreamID + ".md"})
	}
	var b strings.Builder
	if item.Description != "" {
		b.WriteString(item.Description)
		b.WriteString("\n\n")
	}
	b.WriteString("## Local state\n\n")
	switch item.Kind {
	case "task":
		b.WriteString("- Tracking: ")
		b.WriteString(item.TrackingState)
		if item.DueDate != "" {
			b.WriteString("\n- Due date: ")
			b.WriteString(item.DueDate)
		}
		if item.BlockerReason != "" {
			b.WriteString("\n- Blocker: ")
			b.WriteString(item.BlockerReason)
		}
	case "reference":
		b.WriteString("- Context reference; not a local completion obligation.")
	case "ask":
		b.WriteString("- Status: ")
		b.WriteString(item.AskStatus)
		b.WriteString("\n- Counterpart: ")
		b.WriteString(item.Counterpart)
		if item.FollowUpAt != nil {
			b.WriteString("\n- Follow up: ")
			b.WriteString(item.FollowUpAt.UTC().Format(time.RFC3339))
		}
		if item.LastContactAt != nil {
			b.WriteString("\n- Last contact: ")
			b.WriteString(item.LastContactAt.UTC().Format(time.RFC3339))
		}
	case "signal":
		b.WriteString("- Manual assessment: ")
		b.WriteString(item.Assessment)
		if item.ObservedAt != nil {
			b.WriteString("\n- Observed at: ")
			b.WriteString(item.ObservedAt.UTC().Format(time.RFC3339))
		}
		if item.Value != "" {
			b.WriteString("\n- Value: ")
			b.WriteString(item.Value)
			if item.Unit != "" {
				b.WriteByte(' ')
				b.WriteString(item.Unit)
			}
		}
		if item.ReviewBy != nil {
			b.WriteString("\n- Review by: ")
			b.WriteString(item.ReviewBy.UTC().Format(time.RFC3339))
		}
	}
	if item.DetachedAt != nil {
		b.WriteString("\n- Detached at: ")
		b.WriteString(item.DetachedAt.UTC().Format(time.RFC3339))
	}
	if item.SourceID != "" {
		source, err := loadMirrorSource(ctx, tx, item.SourceID)
		if err != nil {
			return MirrorDocument{}, err
		}
		b.WriteString("\n\n## Source snapshot\n\n- Kind: ")
		b.WriteString(source.Kind)
		if source.PRID != nil {
			b.WriteString("\n- Provenance: cached local PR snapshot")
		} else {
			b.WriteString("\n- Provenance: manual URL reference")
		}
		if source.Label != "" {
			b.WriteString("\n- Label: ")
			b.WriteString(source.Label)
		}
		b.WriteString("\n- URL: ")
		b.WriteString(source.URL)
		if source.CachedTitle != "" {
			b.WriteString("\n- Cached title: ")
			b.WriteString(source.CachedTitle)
		}
		if source.CachedState != "" {
			b.WriteString("\n- Cached state: ")
			b.WriteString(source.CachedState)
		}
		if source.LastObservedAt != nil {
			b.WriteString("\n- Last observed: ")
			b.WriteString(source.LastObservedAt.UTC().Format(time.RFC3339))
		}
	}
	doc.Body = b.String()
	return doc, nil
}

func (st *WorkstreamStore) renderDecisionMirror(ctx context.Context, tx *sql.Tx, key MirrorKey, revision int64) (MirrorDocument, error) {
	var d Decision
	err := tx.QueryRowContext(ctx, `SELECT id,workstream_id,COALESCE(item_id,''),COALESCE(source_id,''),note,revision,created_at FROM decisions WHERE id=?`, key.EntityID).Scan(&d.ID, &d.WorkstreamID, &d.ItemID, &d.SourceID, &d.Note, &d.Revision, &d.CreatedAt)
	if err != nil || d.Revision != revision {
		return MirrorDocument{}, ErrRevisionUnavailable
	}
	doc := newMirrorDocument(key, revision, "Decision", "decisions/"+d.ID+".md", d.CreatedAt, d.CreatedAt)
	doc.Frontmatter["workstream_id"] = d.WorkstreamID
	doc.Links = append(doc.Links, MirrorLink{Label: "Workstream", RelativePath: "../workstreams/" + d.WorkstreamID + ".md"})
	if d.ItemID != "" {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM workstream_items WHERE id=?`, d.ItemID).Scan(&kind); err == nil {
			dir, _ := mirrorDirectory(itemType(kind))
			doc.Links = append(doc.Links, MirrorLink{Label: "Related item", RelativePath: "../" + dir + "/" + d.ItemID + ".md"})
		}
	}
	doc.Body = d.Note
	return doc, nil
}

func newMirrorDocument(key MirrorKey, revision int64, title, relative string, created, updated time.Time) MirrorDocument {
	return MirrorDocument{Key: key, RelativePath: relative, Revision: revision, Title: title, Frontmatter: map[string]string{
		"id": key.EntityID, "type": key.EntityType, "revision": fmt.Sprintf("%d", revision),
		"created_at": created.UTC().Format(time.RFC3339), "updated_at": updated.UTC().Format(time.RFC3339),
	}}
}

func mirrorPath(key MirrorKey) string {
	dir, _ := mirrorDirectory(key.EntityType)
	return dir + "/" + key.EntityID + ".md"
}
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func loadMirrorItem(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (WorkstreamItem, error) {
	var it WorkstreamItem
	var workstream, source, due, tracking, blocker, counterpart, ask, value, unit, assessment sql.NullString
	var follow, contact, observed, review, detached sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT id,COALESCE(workstream_id,''),kind,title,description,COALESCE(source_id,''),due_date,tracking_state,blocker_reason,counterpart,ask_status,follow_up_at,last_contact_at,value,unit,observed_at,assessment,review_by,detached_at,revision,created_at,updated_at FROM workstream_items WHERE id=?`, id).Scan(&it.ID, &workstream, &it.Kind, &it.Title, &it.Description, &source, &due, &tracking, &blocker, &counterpart, &ask, &follow, &contact, &value, &unit, &observed, &assessment, &review, &detached, &it.Revision, &it.CreatedAt, &it.UpdatedAt)
	if err != nil {
		return it, err
	}
	it.WorkstreamID = workstream.String
	it.SourceID = source.String
	it.DueDate = due.String
	it.TrackingState = tracking.String
	it.BlockerReason = blocker.String
	it.Counterpart = counterpart.String
	it.AskStatus = ask.String
	it.Value = value.String
	it.Unit = unit.String
	it.Assessment = assessment.String
	if follow.Valid {
		it.FollowUpAt = &follow.Time
	}
	if contact.Valid {
		it.LastContactAt = &contact.Time
	}
	if observed.Valid {
		it.ObservedAt = &observed.Time
	}
	if review.Valid {
		it.ReviewBy = &review.Time
	}
	if detached.Valid {
		it.DetachedAt = &detached.Time
	}
	return it, nil
}

func loadMirrorSource(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Source, error) {
	var s Source
	var pr sql.NullInt64
	var seen sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT s.id,s.kind,s.url,s.canonical_id,s.label,s.pr_id,s.revision,s.created_at,s.updated_at,COALESCE(p.title,''),COALESCE(p.state,''),p.last_seen FROM sources s LEFT JOIN prs p ON p.id=s.pr_id WHERE s.id=?`, id).Scan(&s.ID, &s.Kind, &s.URL, &s.CanonicalID, &s.Label, &pr, &s.Revision, &s.CreatedAt, &s.UpdatedAt, &s.CachedTitle, &s.CachedState, &seen)
	if err != nil {
		return s, err
	}
	if pr.Valid {
		s.PRID = &pr.Int64
	}
	if seen.Valid {
		s.LastObservedAt = &seen.Time
	}
	return s, nil
}
