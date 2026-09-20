package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// SQLite has no IANA timezone database. This scalar orders date-only deadlines
// using the same DST-aware Go rules as the rendered reasons, without loading
// all workstreams into memory to sort them.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("cockpit_due_at", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		date, _ := args[0].(string)
		zone, _ := args[1].(string)
		if date == "" {
			return nil, nil
		}
		location, err := time.LoadLocation(zone)
		if err != nil {
			return nil, err
		}
		day, err := time.ParseInLocation("2006-01-02", date, location)
		if err != nil {
			return nil, err
		}
		return dbTime(day.AddDate(0, 0, 1)), nil
	})
}

// A page uses one read transaction and clock snapshot. Every row collection
// has a SQL limit, while aggregate counts include hidden pages.
type mapReader struct {
	tx            *sql.Tx
	now           time.Time
	location      *time.Location
	mirrorEnabled bool
}

const itemAttentionSQL = `((i.kind='task' AND i.tracking_state IN ('open','in_progress','blocked') AND
 (i.tracking_state='blocked' OR (i.due_date<>'' AND i.due_date<s.today))) OR
 (i.kind='ask' AND i.ask_status IN ('open','waiting') AND i.follow_up_at IS NOT NULL AND i.follow_up_at<=s.now) OR
 (i.kind='signal' AND i.assessment='concerning'))`

const workstreamRollupSQL = `WITH snapshot AS (SELECT ? AS now, ? AS today, ? AS zone),
 item_rollup AS (
 SELECT i.workstream_id,
 SUM(CASE WHEN ` + itemAttentionSQL + ` THEN 1 ELSE 0 END) AS attention_count,
 SUM(CASE WHEN i.kind='ask' AND i.ask_status='waiting' THEN 1 ELSE 0 END) AS waiting_count,
 MIN(CASE WHEN i.kind='task' AND i.tracking_state IN ('open','in_progress','blocked') THEN cockpit_due_at(i.due_date,s.zone)
          WHEN i.kind='ask' AND i.ask_status IN ('open','waiting') THEN i.follow_up_at END) AS deadline
 FROM workstream_items i CROSS JOIN snapshot s WHERE i.detached_at IS NULL GROUP BY i.workstream_id
 ), ranked AS (
 SELECT w.*, COALESCE(r.attention_count,0)+(w.target_date<>'' AND w.target_date<s.today) AS attention_count,
 CASE WHEN COALESCE(r.attention_count,0)>0 OR (w.target_date<>'' AND w.target_date<s.today) THEN 0
      WHEN COALESCE(r.waiting_count,0)>0 THEN 1 ELSE 2 END AS attention_rank,
 CASE WHEN w.target_date='' THEN COALESCE(r.deadline,'9999')
      WHEN r.deadline IS NULL THEN cockpit_due_at(w.target_date,s.zone)
      ELSE MIN(cockpit_due_at(w.target_date,s.zone),r.deadline) END AS deadline
 FROM workstreams w LEFT JOIN item_rollup r ON r.workstream_id=w.id CROSS JOIN snapshot s)
`

func (st *WorkstreamStore) ListMapPage(ctx context.Context, query MapQuery) (MapPageView, error) {
	q, err := normalizeMapQuery(query)
	if err != nil {
		return MapPageView{}, err
	}
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MapPageView{}, err
	}
	defer tx.Rollback()
	r := mapReader{tx: tx, now: st.now(), location: st.location, mirrorEnabled: st.mirrorEnabled}
	v := MapPageView{Query: q}
	if v.Workstreams, err = r.workstreams(ctx, q); err != nil {
		return v, err
	}
	if v.ActiveWorkstreamCount, err = r.count(ctx, `SELECT COUNT(*) FROM workstreams WHERE archived=0 AND lifecycle='active'`); err != nil {
		return v, err
	}
	if v.Mirror, err = r.mirrorSummary(ctx); err != nil {
		return v, err
	}
	if q.WorkstreamID == "" {
		return v, nil
	}
	w, err := st.loadWorkstream(ctx, tx, q.WorkstreamID)
	if err != nil {
		var missing *NotFoundError
		if errors.As(err, &missing) {
			v.NotFound = "Workstream not found. Return to the list or clear your filters."
			return v, nil
		}
		return v, err
	}
	detail := WorkstreamDetailView{Workstream: w}
	v.Selected = &detail
	if v.Categories, err = r.categories(ctx, q, &detail); err != nil {
		return v, err
	}
	if err = r.reasons(ctx, q, &detail); err != nil {
		return v, err
	}
	if detail.Mirror, err = r.mirrorState(ctx, MirrorKey{"workstream", w.ID}); err != nil {
		return v, err
	}
	if err = r.decisions(ctx, q, &detail); err != nil {
		return v, err
	}
	if v.Timeline, err = r.timeline(ctx, q); err != nil {
		return v, err
	}
	if v.Detached, err = r.items(ctx, w.ID, "i.detached_at IS NOT NULL", q.DetachedCursor, 50); err != nil {
		return v, err
	}
	if v.Unfinished, err = r.items(ctx, w.ID, "i.detached_at IS NULL AND ((i.kind='task' AND i.tracking_state IN ('open','in_progress','blocked')) OR (i.kind='ask' AND i.ask_status IN ('open','waiting')) OR (i.kind='signal' AND i.assessment='concerning'))", q.UnfinishedCursor, 50); err != nil {
		return v, err
	}
	if v.Graph, err = r.graph(ctx, w); err != nil {
		return v, err
	}
	if q.ItemID != "" {
		items, err := r.itemRows(ctx, "i.id=? AND i.workstream_id=?", []any{q.ItemID, w.ID}, 1, 0)
		if err != nil {
			return v, err
		}
		if len(items) == 0 {
			v.NotFound = "This item is not in the selected workstream. It may have moved."
		} else {
			v.SelectedItem = &items[0]
		}
	}
	return v, nil
}

func normalizeMapQuery(q MapQuery) (MapQuery, error) {
	if q.Tab == "" {
		q.Tab = "overview"
	}
	if q.Filter == "" {
		q.Filter = "active"
	}
	if q.ItemsCategory == "" {
		q.ItemsCategory = "engineering"
	}
	fields := map[string]string{}
	if !one(q.Tab, "overview", "graph", "timeline", "obsidian") {
		fields["tab"] = "Choose a supported view."
	}
	if !one(q.Filter, "active", "completed", "archived") {
		fields["filter"] = "Choose active, completed, or archived."
	}
	if !one(q.ItemsCategory, "engineering", "asks", "signals") {
		fields["category"] = "Choose Engineering, Asks, or Signals."
	}
	if len(q.Search) > 4096 {
		fields["q"] = "Search is too long."
	}
	for name, cursor := range map[string]string{"workstreams_cursor": q.WorkstreamsCursor, "items_cursor": q.ItemsCursor, "timeline_cursor": q.TimelineCursor, "detached_cursor": q.DetachedCursor, "decisions_cursor": q.DecisionsCursor, "reasons_cursor": q.ReasonsCursor, "unfinished_cursor": q.UnfinishedCursor} {
		if _, err := pageOffset(cursor); err != nil {
			fields[name] = "This page position is invalid. Return to the first page."
		}
	}
	q.WorkstreamsLimit = pageLimit(q.WorkstreamsLimit)
	q.ItemsLimit = pageLimit(q.ItemsLimit)
	q.TimelineLimit = pageLimit(q.TimelineLimit)
	if len(fields) > 0 {
		return q, &ValidationError{Fields: fields}
	}
	return q, nil
}

func pageLimit(n int) int {
	if n < 1 || n > 50 {
		return 50
	}
	return n
}
func pageOffset(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 || n > 1_000_000_000 {
		return 0, errors.New("invalid page")
	}
	return n, nil
}
func pageFor[T any](rows []T, total, offset, limit int) Page[T] {
	p := Page[T]{Rows: rows, Total: total}
	if len(rows) > limit {
		p.Rows = rows[:limit]
		p.NextCursor = strconv.Itoa(offset + limit)
	}
	if offset > 0 {
		p.PreviousCursor = strconv.Itoa(max(0, offset-limit))
	}
	return p
}
func mapSearchPattern(s string) string {
	return "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s) + "%"
}
func (r mapReader) count(ctx context.Context, query string, args ...any) (int, error) {
	var n int
	err := r.tx.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}
func (r mapReader) clockArgs() []any {
	return []any{dbTime(r.now), r.now.In(r.location).Format("2006-01-02"), r.location.String()}
}
func attentionLabel(count, waiting int) string {
	if count > 0 {
		return "needs-attention"
	}
	if waiting > 0 {
		return "waiting"
	}
	return "no-recorded-attention"
}

func (r mapReader) workstreams(ctx context.Context, q MapQuery) (Page[WorkstreamListRow], error) {
	where := "w.archived=0 AND w.lifecycle='active'"
	if q.Filter == "completed" {
		where = "w.archived=0 AND w.lifecycle='completed'"
	}
	if q.Filter == "archived" {
		where = "w.archived=1"
	}
	args := []any{}
	if q.Search != "" {
		where += ` AND (w.name LIKE ? ESCAPE '!' OR w.outcome LIKE ? ESCAPE '!' OR w.owner LIKE ? ESCAPE '!' OR w.sponsor LIKE ? ESCAPE '!' OR EXISTS (
 SELECT 1 FROM workstream_items i LEFT JOIN sources src ON src.id=i.source_id LEFT JOIN prs p ON p.id=src.pr_id
 WHERE i.workstream_id=w.id AND (i.title LIKE ? ESCAPE '!' OR i.counterpart LIKE ? ESCAPE '!' OR i.description LIKE ? ESCAPE '!'
 OR src.canonical_id LIKE ? ESCAPE '!' OR src.url LIKE ? ESCAPE '!' OR (p.owner||'/'||p.repo||'#'||p.number) LIKE ? ESCAPE '!')))`
		for i := 0; i < 10; i++ {
			args = append(args, mapSearchPattern(q.Search))
		}
	}
	total, err := r.count(ctx, `SELECT COUNT(*) FROM workstreams w WHERE `+where, args...)
	if err != nil {
		return Page[WorkstreamListRow]{}, err
	}
	offset, _ := pageOffset(q.WorkstreamsCursor)
	bind := append(r.clockArgs(), args...)
	bind = append(bind, q.WorkstreamsLimit+1, offset)
	rows, err := r.tx.QueryContext(ctx, workstreamRollupSQL+`SELECT w.id,w.name,w.outcome,w.owner,w.sponsor,w.target_date,w.notes,w.lifecycle,w.archived,w.revision,w.created_at,w.updated_at,
 w.attention_count,w.attention_rank,w.deadline FROM ranked w WHERE `+where+` ORDER BY w.attention_rank,w.deadline,w.name COLLATE NOCASE,w.id LIMIT ? OFFSET ?`, bind...)
	if err != nil {
		return Page[WorkstreamListRow]{}, err
	}
	defer rows.Close()
	var out []WorkstreamListRow
	for rows.Next() {
		var w WorkstreamListRow
		var rank int
		if err = rows.Scan(&w.ID, &w.Name, &w.Outcome, &w.Owner, &w.Sponsor, &w.TargetDate, &w.Notes, &w.Lifecycle, &w.Archived, &w.Revision, &w.CreatedAt, &w.UpdatedAt, &w.AttentionCount, &rank, &w.EarliestDeadline); err != nil {
			return Page[WorkstreamListRow]{}, err
		}
		w.Attention = attentionLabel(w.AttentionCount, 0)
		if rank == 1 {
			w.Attention = "waiting"
		}
		if w.EarliestDeadline == "9999" {
			w.EarliestDeadline = ""
		}
		out = append(out, w)
	}
	return pageFor(out, total, offset, q.WorkstreamsLimit), rows.Err()
}

var categoryDefinitions = []struct{ key, label, predicate string }{
	{"engineering", "Engineering", "i.kind IN ('task','reference')"},
	{"asks", "Asks", "i.kind='ask'"},
	{"signals", "Signals", "i.kind='signal'"},
}

func itemCategory(kind string) string {
	if kind == "ask" {
		return "asks"
	}
	if kind == "signal" {
		return "signals"
	}
	return "engineering"
}

func (r mapReader) categories(ctx context.Context, q MapQuery, detail *WorkstreamDetailView) ([]CategoryView, error) {
	rows, err := r.tx.QueryContext(ctx, `WITH snapshot AS (SELECT ? AS now, ? AS today)
 SELECT CASE WHEN i.kind IN ('task','reference') THEN 'engineering' WHEN i.kind='ask' THEN 'asks' ELSE 'signals' END,
 COUNT(*),SUM(CASE WHEN `+itemAttentionSQL+` THEN 1 ELSE 0 END),
 SUM(CASE WHEN i.kind='ask' AND i.ask_status='waiting' THEN 1 ELSE 0 END),
 SUM(CASE WHEN i.kind='task' AND i.tracking_state IN ('open','in_progress','blocked') THEN 1 ELSE 0 END),
 SUM(CASE WHEN i.kind='ask' AND i.ask_status IN ('open','waiting') THEN 1 ELSE 0 END),
 SUM(CASE WHEN i.kind='signal' AND i.assessment='unknown' THEN 1 ELSE 0 END),
 SUM(CASE WHEN i.kind='signal' AND (i.observed_at IS NULL OR (i.review_by IS NOT NULL AND i.review_by<=s.now)) THEN 1 ELSE 0 END),
 SUM(CASE WHEN i.source_id IS NOT NULL AND (p.id IS NULL OR p.last_seen IS NULL) THEN 1 ELSE 0 END)
 FROM workstream_items i CROSS JOIN snapshot s LEFT JOIN sources src ON src.id=i.source_id LEFT JOIN prs p ON p.id=src.pr_id
 WHERE i.workstream_id=? AND i.detached_at IS NULL GROUP BY 1`, dbTime(r.now), r.now.In(r.location).Format("2006-01-02"), detail.ID)
	if err != nil {
		return nil, err
	}
	type counts struct{ total, attention, waiting int }
	byCategory := map[string]counts{}
	waitingTotal := 0
	for rows.Next() {
		var key string
		var c counts
		var tasks, asks, unknown, stale, missing int
		if err = rows.Scan(&key, &c.total, &c.attention, &c.waiting, &tasks, &asks, &unknown, &stale, &missing); err != nil {
			rows.Close()
			return nil, err
		}
		byCategory[key] = c
		waitingTotal += c.waiting
		detail.OpenTaskCount += tasks
		detail.UnresolvedAskCount += asks
		detail.AttentionCount += c.attention
		if unknown > 0 {
			detail.Warnings = append(detail.Warnings, fmt.Sprintf("%d manual signal(s) have an unknown assessment.", unknown))
		}
		if stale > 0 {
			detail.Warnings = append(detail.Warnings, fmt.Sprintf("%d manual signal(s) need a fresh observation or review.", stale))
		}
		if missing > 0 {
			detail.Warnings = append(detail.Warnings, fmt.Sprintf("%d source reference(s) have unavailable metadata or unknown freshness.", missing))
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if detail.TargetDate != "" && detail.TargetDate < r.now.In(r.location).Format("2006-01-02") {
		detail.AttentionCount++
	}
	detail.Attention = attentionLabel(detail.AttentionCount, waitingTotal)
	var out []CategoryView
	for _, def := range categoryDefinitions {
		cursor := ""
		if q.ItemsCategory == def.key {
			cursor = q.ItemsCursor
		}
		page, err := r.items(ctx, detail.ID, "i.detached_at IS NULL AND "+def.predicate, cursor, q.ItemsLimit)
		if err != nil {
			return nil, err
		}
		c := byCategory[def.key]
		out = append(out, CategoryView{Key: def.key, Label: def.label, Attention: attentionLabel(c.attention, c.waiting), AttentionCount: c.attention, Total: c.total, Items: page})
	}
	return out, nil
}

const itemSelectSQL = `SELECT i.id,COALESCE(i.workstream_id,''),i.kind,i.title,i.description,COALESCE(i.source_id,''),i.due_date,i.tracking_state,i.blocker_reason,
 i.counterpart,i.ask_status,i.follow_up_at,i.last_contact_at,i.value,i.unit,i.observed_at,i.assessment,i.review_by,i.detached_at,i.revision,i.created_at,i.updated_at,
 COALESCE(src.kind,''),COALESCE(src.url,''),COALESCE(src.canonical_id,''),COALESCE(src.label,''),src.pr_id,COALESCE(src.revision,0),src.created_at,src.updated_at,
 COALESCE(p.title,''),COALESCE(p.state,''),p.last_seen,
 COALESCE((SELECT rv.id FROM reviews rv WHERE rv.pr_id=p.id AND rv.state!='failed' ORDER BY rv.created_at DESC,rv.id DESC LIMIT 1),0),
 COALESCE(m.desired_revision,0),COALESCE(m.last_written_revision,0),COALESCE(m.last_success_revision,0),COALESCE(m.last_checksum,''),COALESCE(m.status,'pending'),m.last_attempt_at,m.last_success_at,COALESCE(m.error,'')
 FROM workstream_items i LEFT JOIN sources src ON src.id=i.source_id LEFT JOIN prs p ON p.id=src.pr_id
 LEFT JOIN mirror_states m ON m.entity_id=i.id AND m.entity_type=CASE WHEN i.kind IN ('task','reference') THEN 'engineering_item' ELSE i.kind END `

func (r mapReader) itemRows(ctx context.Context, where string, args []any, limit, offset int) ([]WorkstreamItemView, error) {
	args = append(args, limit, offset)
	rows, err := r.tx.QueryContext(ctx, itemSelectSQL+` WHERE `+where+` ORDER BY i.created_at,i.id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkstreamItemView
	for rows.Next() {
		var v WorkstreamItemView
		var src Source
		var follow, contact, observed, review, detached, sourceCreated, sourceUpdated, lastObserved, lastAttempt, lastSuccess sql.NullTime
		var prID sql.NullInt64
		var reviewID int64
		err = rows.Scan(&v.ID, &v.WorkstreamID, &v.Kind, &v.Title, &v.Description, &v.SourceID, &v.DueDate, &v.TrackingState, &v.BlockerReason, &v.Counterpart, &v.AskStatus, &follow, &contact, &v.Value, &v.Unit, &observed, &v.Assessment, &review, &detached, &v.Revision, &v.CreatedAt, &v.UpdatedAt,
			&src.Kind, &src.URL, &src.CanonicalID, &src.Label, &prID, &src.Revision, &sourceCreated, &sourceUpdated, &src.CachedTitle, &src.CachedState, &lastObserved, &reviewID,
			&v.Mirror.DesiredRevision, &v.Mirror.LastWrittenRevision, &v.Mirror.LastSuccessRevision, &v.Mirror.LastChecksum, &v.Mirror.Status, &lastAttempt, &lastSuccess, &v.Mirror.Error)
		if err != nil {
			return nil, err
		}
		v.FollowUpAt = nullTimePointer(follow)
		v.LastContactAt = nullTimePointer(contact)
		v.ObservedAt = nullTimePointer(observed)
		v.ReviewBy = nullTimePointer(review)
		v.DetachedAt = nullTimePointer(detached)
		v.Mirror.MirrorKey = MirrorKey{itemType(v.Kind), v.ID}
		v.Mirror.LastAttemptAt = nullTimePointer(lastAttempt)
		v.Mirror.LastSuccessAt = nullTimePointer(lastSuccess)
		if !r.mirrorEnabled {
			v.Mirror.Status = "disabled"
		}
		if v.SourceID != "" {
			src.ID = v.SourceID
			src.CreatedAt = sourceCreated.Time
			src.UpdatedAt = sourceUpdated.Time
			src.LastObservedAt = nullTimePointer(lastObserved)
			if prID.Valid {
				src.PRID = &prID.Int64
			}
			if reviewID > 0 {
				src.LocalReviewURL = "/pr/" + strconv.FormatInt(reviewID, 10)
			}
			v.Source = &src
		}
		v.Attention = r.itemReasons(v.WorkstreamItem)
		out = append(out, v)
	}
	return out, rows.Err()
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time
	return &t
}
func (r mapReader) items(ctx context.Context, wid, predicate, cursor string, limit int) (Page[WorkstreamItemView], error) {
	offset, err := pageOffset(cursor)
	if err != nil {
		return Page[WorkstreamItemView]{}, err
	}
	where := "i.workstream_id=? AND " + predicate
	total, err := r.count(ctx, `SELECT COUNT(*) FROM workstream_items i WHERE `+where, wid)
	if err != nil {
		return Page[WorkstreamItemView]{}, err
	}
	rows, err := r.itemRows(ctx, where, []any{wid}, limit+1, offset)
	return pageFor(rows, total, offset, limit), err
}

func (r mapReader) itemReasons(item WorkstreamItem) []AttentionReason {
	if item.DetachedAt != nil {
		return nil
	}
	var reasons []AttentionReason
	add := func(code, label string) {
		reasons = append(reasons, AttentionReason{EntityID: item.ID, EntityType: itemType(item.Kind), Category: itemCategory(item.Kind), Code: code, Label: label})
	}
	if item.Kind == "task" && one(item.TrackingState, "open", "in_progress", "blocked") {
		if item.TrackingState == "blocked" {
			add("blocked", item.Title+": blocked — "+item.BlockerReason)
		}
		if item.DueDate != "" && item.DueDate < r.now.In(r.location).Format("2006-01-02") {
			add("overdue_task", item.Title+": overdue since "+item.DueDate)
		}
	}
	if item.Kind == "ask" && one(item.AskStatus, "open", "waiting") && item.FollowUpAt != nil && !r.now.Before(*item.FollowUpAt) {
		add("overdue_ask", item.Title+": follow up with "+item.Counterpart)
	}
	if item.Kind == "signal" && item.Assessment == "concerning" {
		add("concerning_signal", item.Title+": manual observation is concerning")
	}
	return reasons
}

func (r mapReader) reasons(ctx context.Context, q MapQuery, d *WorkstreamDetailView) error {
	d.ReasonsTotal = d.AttentionCount
	if d.TargetDate != "" && d.TargetDate < r.now.In(r.location).Format("2006-01-02") {
		d.AttentionReasons = append(d.AttentionReasons, AttentionReason{EntityID: d.ID, EntityType: "workstream", Code: "target_date", Label: "Target date " + d.TargetDate + " has passed."})
	}
	offset, _ := pageOffset(q.ReasonsCursor)
	predicate := strings.ReplaceAll(strings.ReplaceAll(itemAttentionSQL, "s.today", "?"), "s.now", "?")
	items, err := r.itemRows(ctx, "i.workstream_id=? AND i.detached_at IS NULL AND "+predicate, []any{d.ID, r.now.In(r.location).Format("2006-01-02"), dbTime(r.now)}, 51, offset)
	if err != nil {
		return err
	}
	if len(items) > 50 {
		items = items[:50]
		d.ReasonsNextCursor = strconv.Itoa(offset + 50)
	}
	for _, item := range items {
		d.AttentionReasons = append(d.AttentionReasons, item.Attention...)
	}
	return nil
}

func (r mapReader) timeline(ctx context.Context, q MapQuery) (Page[TimelineEventView], error) {
	offset, _ := pageOffset(q.TimelineCursor)
	total, err := r.count(ctx, `SELECT COUNT(*) FROM timeline_events WHERE workstream_id=?`, q.WorkstreamID)
	if err != nil {
		return Page[TimelineEventView]{}, err
	}
	rows, err := r.tx.QueryContext(ctx, `SELECT id,workstream_id,entity_type,entity_id,event_type,description,revision,created_at FROM timeline_events WHERE workstream_id=? ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, q.WorkstreamID, q.TimelineLimit+1, offset)
	if err != nil {
		return Page[TimelineEventView]{}, err
	}
	defer rows.Close()
	var out []TimelineEventView
	for rows.Next() {
		var event TimelineEventView
		if err = rows.Scan(&event.ID, &event.WorkstreamID, &event.EntityType, &event.EntityID, &event.EventType, &event.Description, &event.Revision, &event.CreatedAt); err != nil {
			return Page[TimelineEventView]{}, err
		}
		out = append(out, event)
	}
	return pageFor(out, total, offset, q.TimelineLimit), rows.Err()
}

func (r mapReader) decisions(ctx context.Context, q MapQuery, d *WorkstreamDetailView) error {
	var err error
	d.DecisionsTotal, err = r.count(ctx, `SELECT COUNT(*) FROM decisions WHERE workstream_id=?`, d.ID)
	if err != nil {
		return err
	}
	offset, _ := pageOffset(q.DecisionsCursor)
	rows, err := r.tx.QueryContext(ctx, `SELECT d.id,d.workstream_id,COALESCE(d.item_id,''),COALESCE(d.source_id,''),d.note,d.revision,d.created_at,COALESCE(i.workstream_id,''),
 COALESCE(m.desired_revision,0),COALESCE(m.last_written_revision,0),COALESCE(m.last_success_revision,0),COALESCE(m.last_checksum,''),COALESCE(m.status,'pending'),m.last_attempt_at,m.last_success_at,COALESCE(m.error,'')
 FROM decisions d LEFT JOIN workstream_items i ON i.id=d.item_id
 LEFT JOIN mirror_states m ON m.entity_type='decision' AND m.entity_id=d.id
 WHERE d.workstream_id=? ORDER BY d.created_at DESC,d.id DESC LIMIT 51 OFFSET ?`, d.ID, offset)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x DecisionView
		var attempt, success sql.NullTime
		if err = rows.Scan(&x.ID, &x.WorkstreamID, &x.ItemID, &x.SourceID, &x.Note, &x.Revision, &x.CreatedAt, &x.ItemWorkstreamID,
			&x.Mirror.DesiredRevision, &x.Mirror.LastWrittenRevision, &x.Mirror.LastSuccessRevision, &x.Mirror.LastChecksum, &x.Mirror.Status, &attempt, &success, &x.Mirror.Error); err != nil {
			return err
		}
		x.Mirror.MirrorKey = MirrorKey{"decision", x.ID}
		x.Mirror.LastAttemptAt, x.Mirror.LastSuccessAt = nullTimePointer(attempt), nullTimePointer(success)
		if !r.mirrorEnabled {
			x.Mirror.Status = "disabled"
		}
		d.Decisions = append(d.Decisions, x)
	}
	if len(d.Decisions) > 50 {
		d.Decisions = d.Decisions[:50]
		d.DecisionsNextCursor = strconv.Itoa(offset + 50)
	}
	return rows.Err()
}

func (r mapReader) graph(ctx context.Context, w Workstream) (GraphView, error) {
	g := GraphView{Nodes: []GraphNode{{ID: w.ID, Label: w.Name, Kind: "workstream"}}}
	for _, d := range categoryDefinitions {
		g.Nodes = append(g.Nodes, GraphNode{ID: d.key, Label: d.label, Kind: "category", ParentID: w.ID})
	}
	total, err := r.count(ctx, `SELECT COUNT(*) FROM workstream_items WHERE workstream_id=? AND detached_at IS NULL`, w.ID)
	if err != nil {
		return g, err
	}
	rows, err := r.tx.QueryContext(ctx, `SELECT id,title,kind FROM workstream_items WHERE workstream_id=? AND detached_at IS NULL ORDER BY CASE kind WHEN 'task' THEN 0 WHEN 'reference' THEN 1 WHEN 'ask' THEN 2 ELSE 3 END,created_at,id LIMIT 100`, w.ID)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var n GraphNode
		if err = rows.Scan(&n.ID, &n.Label, &n.Kind); err != nil {
			return g, err
		}
		n.ParentID = itemCategory(n.Kind)
		g.Nodes = append(g.Nodes, n)
		count++
	}
	g.Omitted = max(0, total-count)
	return g, rows.Err()
}

func (r mapReader) mirrorState(ctx context.Context, key MirrorKey) (MirrorStateView, error) {
	v := MirrorStateView{MirrorKey: key, Status: "pending"}
	if !r.mirrorEnabled {
		v.Status = "disabled"
		return v, nil
	}
	var attempt, success sql.NullTime
	err := r.tx.QueryRowContext(ctx, `SELECT desired_revision,last_written_revision,last_success_revision,last_checksum,status,last_attempt_at,last_success_at,error FROM mirror_states WHERE entity_type=? AND entity_id=?`, key.EntityType, key.EntityID).Scan(&v.DesiredRevision, &v.LastWrittenRevision, &v.LastSuccessRevision, &v.LastChecksum, &v.Status, &attempt, &success, &v.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	v.LastAttemptAt = nullTimePointer(attempt)
	v.LastSuccessAt = nullTimePointer(success)
	return v, err
}
func (r mapReader) mirrorSummary(ctx context.Context) (MirrorSummaryView, error) {
	v := MirrorSummaryView{Disabled: !r.mirrorEnabled}
	if v.Disabled {
		return v, nil
	}
	err := r.tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='pending'),0),COALESCE(SUM(status='error'),0),COALESCE(SUM(status='conflict'),0) FROM mirror_states`).Scan(&v.Pending, &v.Errors, &v.Conflicts)
	return v, err
}

func (st *WorkstreamStore) SearchCachedPRs(ctx context.Context, q PRSearchQuery) (PRSearchPage, error) {
	q.Limit = pageLimit(q.Limit)
	offset, err := pageOffset(q.Cursor)
	if err != nil {
		return PRSearchPage{}, &ValidationError{Fields: map[string]string{"cursor": "Invalid page position."}}
	}
	if len(q.Search) > 4096 {
		return PRSearchPage{}, &ValidationError{Fields: map[string]string{"q": "Search is too long."}}
	}
	tx, err := st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PRSearchPage{}, err
	}
	defer tx.Rollback()
	where := `(owner||'/'||repo||'#'||number||' '||title||' '||url) LIKE ? ESCAPE '!'`
	pattern := mapSearchPattern(q.Search)
	var total int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM prs WHERE `+where, pattern).Scan(&total); err != nil {
		return PRSearchPage{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,owner,repo,number,url,title,state,last_seen FROM prs WHERE `+where+` ORDER BY owner COLLATE NOCASE,repo COLLATE NOCASE,number,id LIMIT ? OFFSET ?`, pattern, q.Limit+1, offset)
	if err != nil {
		return PRSearchPage{}, err
	}
	defer rows.Close()
	var out []CachedPRView
	for rows.Next() {
		var p CachedPRView
		var seen sql.NullTime
		if err = rows.Scan(&p.ID, &p.Owner, &p.Repo, &p.Number, &p.URL, &p.Title, &p.State, &seen); err != nil {
			return PRSearchPage{}, err
		}
		p.LastObservedAt = nullTimePointer(seen)
		out = append(out, p)
	}
	p := pageFor(out, total, offset, q.Limit)
	return PRSearchPage{Rows: p.Rows, NextCursor: p.NextCursor, Total: total}, rows.Err()
}
