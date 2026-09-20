package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newCommandHTTP(t *testing.T) (http.Handler, *WorkstreamStore) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "map.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewWorkstreamStore(db, func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }, time.UTC)
	s := &server{db: db, baseCtx: context.Background(), workstreams: store}
	mux := http.NewServeMux()
	s.routes(mux)
	return mux, store
}
func commandHTTP(t *testing.T, h http.Handler, c WorkstreamCommand) (int, CommandResult) {
	t.Helper()
	c = commandWithCurrentCreateRevision(t, h, c)
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest(http.MethodPost, "/map/commands", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out CommandResult
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// Fixture clients model opening a fresh create form. Explicit revisions are
// never replaced, so stale-revision tests exercise the actual submitted value.
func commandWithCurrentCreateRevision(t *testing.T, h http.Handler, c WorkstreamCommand) WorkstreamCommand {
	t.Helper()
	if c.Action == "create_item" && c.ExpectedRevision == 0 {
		view := (&acceptanceMapClient{t: t, handler: h}).read(c.WorkstreamID, "")
		if view.Selected != nil {
			c.ExpectedRevision = view.Selected.Revision
		}
	}
	return c
}

func TestMapCommands_CreateValidatesAndRetriesIdempotently(t *testing.T) {
	h, _ := newCommandHTTP(t)
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "bad", Action: "create_workstream", Workstream: WorkstreamInput{Name: " ", Outcome: " "}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create=%d want 422", code)
	}
	cmd := WorkstreamCommand{OperationID: "one", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship credits"}}
	code, first := commandHTTP(t, h, cmd)
	if code != http.StatusOK || first.WorkstreamID == "" {
		t.Fatalf("create=%d %#v", code, first)
	}
	code, again := commandHTTP(t, h, cmd)
	if code != http.StatusOK || !again.Idempotent || again.WorkstreamID != first.WorkstreamID {
		t.Fatalf("retry=%d %#v", code, again)
	}
}

func TestMapCommands_DuplicateReferenceReturnsExistingAndDetachRestoreRetainsParent(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship"}})
	newRef := func(id string) CommandResult {
		_, r := commandHTTP(t, h, WorkstreamCommand{OperationID: id, Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "RFC", SourceURL: "https://example.test/rfc#one"}})
		return r
	}
	first := newRef("r1")
	second := newRef("r2")
	if first.ItemID == "" || second.ItemID != first.ItemID {
		t.Fatalf("duplicate refs %#v %#v", first, second)
	}
	code, detached := commandHTTP(t, h, WorkstreamCommand{OperationID: "detach", Action: "detach_item", ItemID: first.ItemID, ExpectedRevision: first.Revision, Confirm: true})
	if code != http.StatusOK {
		t.Fatalf("detach=%d", code)
	}
	code, restored := commandHTTP(t, h, WorkstreamCommand{OperationID: "restore", Action: "restore_item", ItemID: first.ItemID, ExpectedRevision: detached.Revision})
	if code != http.StatusOK || restored.WorkstreamID != w.WorkstreamID {
		t.Fatalf("restore=%d %#v", code, restored)
	}
}

func TestMapCommands_RejectUnsafeSourcesAndInvalidKindState(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship"}})
	for _, item := range []WorkstreamItemInput{
		{Kind: "reference", Title: "unsafe", SourceURL: "https://user:password@example.test/private"},
		{Kind: "reference", Title: "unsafe", SourceURL: "file:///tmp/private"},
		{Kind: "task", Title: "bad state", TrackingState: "invented"},
		{Kind: "task", Title: "blocked", TrackingState: "blocked"},
		{Kind: "signal", Title: "unknown", Assessment: "invented", ObservedAt: "2026-01-02T03:04:05Z"},
	} {
		code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: opaqueID(), Action: "create_item", WorkstreamID: w.WorkstreamID, Item: item})
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%+v status=%d want 422", item, code)
		}
	}
}

func TestMapCommands_StaleEditAndLifecycleReadOnly(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship"}})
	code, completed := commandHTTP(t, h, WorkstreamCommand{OperationID: "complete", Action: "complete_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: w.Revision, Confirm: true, Workstream: WorkstreamInput{Notes: "Shipped"}})
	if code != http.StatusOK {
		t.Fatalf("complete=%d", code)
	}
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "edit-completed", Action: "edit_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: completed.Revision, Workstream: WorkstreamInput{Name: "Changed", Outcome: "Changed"}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("edit completed=%d want 422", code)
	}
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "stale", Action: "reopen_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: w.Revision})
	if code != http.StatusConflict {
		t.Fatalf("stale lifecycle=%d want 409", code)
	}
	code, reopened := commandHTTP(t, h, WorkstreamCommand{OperationID: "reopen", Action: "reopen_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: completed.Revision})
	if code != http.StatusOK || reopened.Revision != completed.Revision+1 {
		t.Fatalf("reopen=%d %#v", code, reopened)
	}
}

func TestMapCommands_NoOpEditDoesNotAdvanceRevisionOrHistory(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship"}})
	in := WorkstreamItemInput{Kind: "task", Title: "Review", TrackingState: "open", DueDate: "2026-01-03"}
	_, task := commandHTTP(t, h, WorkstreamCommand{OperationID: "task", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: in})
	before := mapDataHTTP(t, h, w.WorkstreamID)
	code, result := commandHTTP(t, h, WorkstreamCommand{OperationID: "same", Action: "edit_item", WorkstreamID: w.WorkstreamID, ItemID: task.ItemID, ExpectedRevision: task.Revision, Item: in})
	if code != http.StatusOK || result.Revision != task.Revision {
		t.Fatalf("no-op result=%d %#v", code, result)
	}
	after := mapDataHTTP(t, h, w.WorkstreamID)
	if after.Selected.Revision != before.Selected.Revision || after.Timeline.Total != before.Timeline.Total {
		t.Fatalf("no-op changed state: before=%+v after=%+v", before.Selected, after.Selected)
	}
}

func TestMapCommands_ConcurrentSameRevisionHasOneSaveAndOneConflict(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship"}})
	in := WorkstreamItemInput{Kind: "task", Title: "Review", TrackingState: "open"}
	_, task := commandHTTP(t, h, WorkstreamCommand{OperationID: "task", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: in})
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, title := range []string{"One", "Two"} {
		wg.Add(1)
		go func(title string) {
			defer wg.Done()
			next := in
			next.Title = title
			code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "edit-" + title, Action: "edit_item", ItemID: task.ItemID, ExpectedRevision: task.Revision, Item: next})
			codes <- code
		}(title)
	}
	wg.Wait()
	close(codes)
	ok, conflict := 0, 0
	for c := range codes {
		if c == http.StatusOK {
			ok++
		}
		if c == http.StatusConflict {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("saves=%d conflicts=%d", ok, conflict)
	}
}

func TestMapCommands_BoundsOperationIdentity(t *testing.T) {
	h, _ := newCommandHTTP(t)
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: strings.Repeat("x", 201), Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship"}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("long operation=%d", code)
	}
}

func TestMapCommands_PastedCachedPRReusesLocalIdentity(t *testing.T) {
	h, store := newCommandHTTP(t)
	if _, err := UpsertPR(context.Background(), store.db, GHPR{Number: 42, Title: "Credits", URL: "https://github.com/octo/repo/pull/42", HeadRefOid: "abc", Author: GHAuthor{Login: "octo"}}, store.now()); err != nil {
		t.Fatal(err)
	}
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship"}})
	_, item := commandHTTP(t, h, WorkstreamCommand{OperationID: "pasted", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "PR", SourceURL: "https://github.com/octo/repo/pull/42?plain=1#discussion"}})
	v := mapDataHTTP(t, h, w.WorkstreamID)
	if len(v.Categories) == 0 || len(v.Categories[0].Items.Rows) == 0 {
		t.Fatal("reference absent")
	}
	got := v.Categories[0].Items.Rows[0].Source
	if got == nil || got.PRID == nil || *got.PRID == 0 {
		t.Fatalf("pasted cached PR not linked: %+v", got)
	}
	_, again := commandHTTP(t, h, WorkstreamCommand{OperationID: "again", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "PR", SourceURL: "https://github.com/octo/repo/pull/42?plain=1#discussion"}})
	if again.ItemID != item.ItemID {
		t.Fatalf("same canonical URL duplicated %q %q", item.ItemID, again.ItemID)
	}
}

func TestMapCommands_MoveRejectsInactiveDestinationAndDecisionRequiresNote(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, a := commandHTTP(t, h, WorkstreamCommand{OperationID: "a", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "Ship A"}})
	_, b := commandHTTP(t, h, WorkstreamCommand{OperationID: "b", Action: "create_workstream", Workstream: WorkstreamInput{Name: "B", Outcome: "Ship B"}})
	_, task := commandHTTP(t, h, WorkstreamCommand{OperationID: "task", Action: "create_item", WorkstreamID: a.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: "Task", TrackingState: "open"}})
	_, done := commandHTTP(t, h, WorkstreamCommand{OperationID: "done", Action: "complete_workstream", WorkstreamID: b.WorkstreamID, ExpectedRevision: b.Revision, Confirm: true, Workstream: WorkstreamInput{Notes: "Done"}})
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "move", Action: "move_item", ItemID: task.ItemID, ExpectedRevision: task.Revision, DestinationWorkstreamID: done.WorkstreamID})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("move inactive=%d", code)
	}
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "decision", Action: "record_decision", WorkstreamID: a.WorkstreamID, ExpectedRevision: 2, Decision: DecisionInput{Note: " "}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("empty decision=%d", code)
	}
}

func TestMapCommands_DetachRequiresConfirmationAndBoundsSourceFields(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "B"}})
	_, task := commandHTTP(t, h, WorkstreamCommand{OperationID: "task", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: "T", TrackingState: "open"}})
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "detach", Action: "detach_item", ItemID: task.ItemID, ExpectedRevision: task.Revision})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed detach=%d", code)
	}
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "url", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "R", SourceURL: "https://x.test/" + strings.Repeat("a", 4097)}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized url=%d", code)
	}
}

func TestMapCommands_TimestampNoOpAndClearAreDistinct(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "B"}})
	in := WorkstreamItemInput{Kind: "ask", Title: "Ask", Counterpart: "X", AskStatus: "waiting", FollowUpAt: "2026-01-03T00:00:00Z"}
	_, item := commandHTTP(t, h, WorkstreamCommand{OperationID: "i", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: in})
	code, same := commandHTTP(t, h, WorkstreamCommand{OperationID: "same", Action: "edit_item", ItemID: item.ItemID, ExpectedRevision: item.Revision, Item: in})
	if code != 200 || same.Revision != item.Revision {
		t.Fatalf("same timestamp %#v", same)
	}
	in.FollowUpAt = ""
	code, cleared := commandHTTP(t, h, WorkstreamCommand{OperationID: "clear", Action: "edit_item", ItemID: item.ItemID, ExpectedRevision: item.Revision, Item: in})
	if code != 200 || cleared.Revision != item.Revision+1 {
		t.Fatalf("clear timestamp %#v", cleared)
	}
}

func TestMapCommands_LeavingBlockedClearsCurrentReason(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "B"}})
	in := WorkstreamItemInput{Kind: "task", Title: "T", TrackingState: "blocked", BlockerReason: "Waiting on API"}
	_, item := commandHTTP(t, h, WorkstreamCommand{OperationID: "i", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: in})
	in.TrackingState = "in_progress"
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "unblock", Action: "edit_item", ItemID: item.ItemID, ExpectedRevision: item.Revision, Item: in})
	if code != 200 {
		t.Fatalf("unblock=%d", code)
	}
	v := mapDataHTTP(t, h, w.WorkstreamID)
	got := v.Categories[0].Items.Rows[0]
	if got.BlockerReason != "" {
		t.Fatalf("blocker retained %q", got.BlockerReason)
	}
}

func TestMapCommands_EditReferenceToExistingSourceReturnsValidationNotPartial(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "B"}})
	_, oneRef := commandHTTP(t, h, WorkstreamCommand{OperationID: "one", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "One", SourceURL: "https://example.test/one"}})
	_, two := commandHTTP(t, h, WorkstreamCommand{OperationID: "two", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "Two", SourceURL: "https://example.test/two"}})
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "change", Action: "edit_item", ItemID: two.ItemID, ExpectedRevision: two.Revision, Item: WorkstreamItemInput{Kind: "reference", Title: "Two", SourceURL: "https://example.test/one"}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate edit status=%d", code)
	}
	v := mapDataHTTP(t, h, w.WorkstreamID)
	if len(v.Categories[0].Items.Rows) != 2 || v.Categories[0].Items.Rows[0].ID == "" || oneRef.ItemID == two.ItemID {
		t.Fatalf("edit partially changed references")
	}
}

func TestMapCommands_CompletionOutcomeSurvivesArchiveRestoreAndNoOpEdit(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "B"}})
	_, done := commandHTTP(t, h, WorkstreamCommand{OperationID: "done", Action: "complete_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: w.Revision, Confirm: true, Workstream: WorkstreamInput{Notes: "Shipped safely"}})
	_, arch := commandHTTP(t, h, WorkstreamCommand{OperationID: "archive", Action: "archive_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: done.Revision, Confirm: true})
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "bad", Action: "reopen_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: arch.Revision})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("reopen archived=%d", code)
	}
	_, rest := commandHTTP(t, h, WorkstreamCommand{OperationID: "restore", Action: "restore_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: arch.Revision})
	v := mapDataHTTP(t, h, w.WorkstreamID)
	if v.Selected.CompletionNote != "Shipped safely" {
		t.Fatalf("completion note lost %q", v.Selected.CompletionNote)
	}
	code, same := commandHTTP(t, h, WorkstreamCommand{OperationID: "same", Action: "restore_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: rest.Revision})
	if code != 200 || same.Revision != rest.Revision {
		t.Fatalf("restore no-op %#v", same)
	}
}

func mapDataHTTP(t *testing.T, h http.Handler, id string) MapPageView {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map/data?workstream="+id, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("read=%d %s", w.Code, w.Body.String())
	}
	var v MapPageView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
