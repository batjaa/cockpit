package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This exercises the agreed boundary: public HTTP commands commit to a real
// temporary SQLite database, then the deterministic worker control exports to
// a real temporary vault. No real home directory or provider is involved.
func TestMapMirror_HTTPCommandDrainsAndPreservesExternalEdit(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "map.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := NewWorkstreamStore(db, func() time.Time { return now }, time.UTC)
	vault := t.TempDir()
	worker := NewMirrorWorker(store, MirrorWorkerConfig{Root: vault, Now: func() time.Time { return now }})
	store.SetAfterCommit(worker.Wake)
	s := &server{db: db, baseCtx: context.Background(), workstreams: store, mirror: worker}
	mux := http.NewServeMux()
	s.routes(mux)

	create := postMapCommand(t, mux, WorkstreamCommand{
		OperationID: "mirror-create", Action: "create_workstream",
		Workstream: WorkstreamInput{Name: "Credits v1", Outcome: "Ship credits safely"},
	})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatalf("drain initial export: %v", err)
	}
	path := filepath.Join(vault, "workstreams", create.WorkstreamID+".md")
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read initial Markdown: %v", err)
	}
	for _, want := range []string{`id: "` + create.WorkstreamID + `"`, "# Credits v1", "Ship credits safely"} {
		if !strings.Contains(string(initial), want) {
			t.Errorf("initial Markdown missing %q:\n%s", want, initial)
		}
	}

	const external = "# My edited vault note\n"
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	postMapCommand(t, mux, WorkstreamCommand{
		OperationID: "mirror-edit", Action: "edit_workstream", WorkstreamID: create.WorkstreamID, ExpectedRevision: create.Revision,
		Workstream: WorkstreamInput{Name: "Credits v2", Outcome: "Ship credits safely"},
	})
	historyAfterEdit := getMapData(t, mux, create.WorkstreamID).Timeline.Total
	// A prior successful attempt starts the durable five-second retry floor;
	// this is the latest permitted first retry, with no sleep in the test.
	now = now.Add(mirrorRetryFloor)
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatalf("drain conflicting export: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != external {
		t.Fatalf("external content was overwritten: %q", got)
	}
	view := getMapData(t, mux, create.WorkstreamID)
	if view.Selected == nil || view.Selected.Mirror.Status != "conflict" {
		t.Fatalf("mirror status=%+v, want conflict", view.Selected)
	}
	if view.Timeline.Total != historyAfterEdit+1 {
		t.Fatalf("conflict history=%d, want one transition after %d", view.Timeline.Total, historyAfterEdit)
	}
	// Restoring Cockpit's last known bytes and using the public retry endpoint
	// reconciles without force-overwriting the prior external edit.
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	retryMirrorHTTP(t, mux, MirrorKey{EntityType: "workstream", EntityID: create.WorkstreamID})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustReadFile(t, path)), "# Credits v2") {
		t.Fatal("retry did not export latest saved revision")
	}
	view = getMapData(t, mux, create.WorkstreamID)
	if view.Selected == nil || view.Selected.Mirror.Status != "synced" {
		t.Fatalf("mirror did not recover: %+v", view.Selected)
	}
	if view.Timeline.Total != historyAfterEdit+2 {
		t.Fatalf("recovery history=%d, want one additional transition", view.Timeline.Total)
	}
}

func retryMirrorHTTP(t *testing.T, mux *http.ServeMux, key MirrorKey) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"entity_type": key.EntityType, "entity_id": key.EntityID})
	req := httptest.NewRequest(http.MethodPost, "/map/mirror/retry", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("retry: status=%d body=%s", w.Code, w.Body.String())
	}
}
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestMapMirror_HTTPCommandsCoalesceToLatestRevision(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "map.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := NewWorkstreamStore(db, func() time.Time { return now }, time.UTC)
	vault := t.TempDir()
	worker := NewMirrorWorker(store, MirrorWorkerConfig{Root: vault, Now: func() time.Time { return now }})
	store.SetAfterCommit(worker.Wake)
	s := &server{db: db, baseCtx: context.Background(), workstreams: store, mirror: worker}
	mux := http.NewServeMux()
	s.routes(mux)
	created := postMapCommand(t, mux, WorkstreamCommand{OperationID: "coalesce-create", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits v1", Outcome: "Initial outcome"}})
	postMapCommand(t, mux, WorkstreamCommand{OperationID: "coalesce-edit", Action: "edit_workstream", WorkstreamID: created.WorkstreamID, ExpectedRevision: created.Revision, Workstream: WorkstreamInput{Name: "Credits v2", Outcome: "Latest outcome"}})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(vault, "workstreams", created.WorkstreamID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "Credits v1") || !strings.Contains(string(contents), "Credits v2") || !strings.Contains(string(contents), "Latest outcome") {
		t.Fatalf("mirror did not coalesce to latest committed revision:\n%s", contents)
	}
	view := getMapData(t, mux, created.WorkstreamID)
	if view.Selected == nil || view.Selected.Mirror.Status != "synced" || view.Selected.Mirror.LastSuccessRevision != 2 {
		t.Fatalf("mirror state=%+v, want revision 2 synced", view.Selected)
	}
}

func TestMapMirror_HTTPCommandRejectsSymlinkTarget(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "map.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := NewWorkstreamStore(db, func() time.Time { return now }, time.UTC)
	vault := t.TempDir()
	worker := NewMirrorWorker(store, MirrorWorkerConfig{Root: vault, Now: func() time.Time { return now }})
	store.SetAfterCommit(worker.Wake)
	s := &server{db: db, baseCtx: context.Background(), workstreams: store, mirror: worker}
	mux := http.NewServeMux()
	s.routes(mux)
	created := postMapCommand(t, mux, WorkstreamCommand{OperationID: "symlink-create", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship safely"}})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(vault, "workstreams", created.WorkstreamID+".md")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("do not touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	postMapCommand(t, mux, WorkstreamCommand{OperationID: "symlink-edit", Action: "edit_workstream", WorkstreamID: created.WorkstreamID, ExpectedRevision: created.Revision, Workstream: WorkstreamInput{Name: "Credits v2", Outcome: "Ship safely"}})
	now = now.Add(mirrorRetryFloor)
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "do not touch\n" {
		t.Fatalf("mirror followed target symlink: %q", got)
	}
	view := getMapData(t, mux, created.WorkstreamID)
	if view.Selected == nil || view.Selected.Mirror.Status != "conflict" {
		t.Fatalf("mirror status=%+v, want conflict", view.Selected)
	}
}

func TestMapMirror_HTTPExportsAllEntityKindsWithStablePaths(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "map.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := NewWorkstreamStore(db, func() time.Time { return now }, time.UTC)
	vault := t.TempDir()
	worker := NewMirrorWorker(store, MirrorWorkerConfig{Root: vault, Now: func() time.Time { return now }})
	store.SetAfterCommit(worker.Wake)
	s := &server{db: db, baseCtx: context.Background(), workstreams: store, mirror: worker}
	mux := http.NewServeMux()
	s.routes(mux)
	ws := postMapCommand(t, mux, WorkstreamCommand{OperationID: "kinds-ws", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Archive", Outcome: "Keep an offline record", Notes: "Local-only."}})
	ref := postMapCommand(t, mux, WorkstreamCommand{OperationID: "kinds-ref", Action: "create_item", WorkstreamID: ws.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "RFC", SourceURL: "https://example.com/rfc", SourceKind: "rfc"}})
	task := postMapCommand(t, mux, WorkstreamCommand{OperationID: "kinds-task", Action: "create_item", WorkstreamID: ws.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: "Review", TrackingState: "blocked", BlockerReason: "Awaiting input"}})
	ask := postMapCommand(t, mux, WorkstreamCommand{OperationID: "kinds-ask", Action: "create_item", WorkstreamID: ws.WorkstreamID, Item: WorkstreamItemInput{Kind: "ask", Title: "Confirm", Counterpart: "Billing", AskStatus: "waiting", FollowUpAt: "2026-09-21T12:00:00Z"}})
	signal := postMapCommand(t, mux, WorkstreamCommand{OperationID: "kinds-signal", Action: "create_item", WorkstreamID: ws.WorkstreamID, Item: WorkstreamItemInput{Kind: "signal", Title: "Latency", Assessment: "concerning", ObservedAt: "2026-09-20T11:00:00Z", Value: "210", Unit: "ms"}})
	current := getMapData(t, mux, ws.WorkstreamID).Selected
	decision := postMapCommand(t, mux, WorkstreamCommand{OperationID: "kinds-decision", Action: "record_decision", WorkstreamID: ws.WorkstreamID, ExpectedRevision: current.Revision, Decision: DecisionInput{Note: "Proceed with safeguards."}})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"workstreams/" + ws.WorkstreamID + ".md", "engineering/" + ref.ItemID + ".md", "engineering/" + task.ItemID + ".md", "asks/" + ask.ItemID + ".md", "signals/" + signal.ItemID + ".md", "decisions/" + decision.DecisionID + ".md"} {
		if _, err := os.ReadFile(filepath.Join(vault, file)); err != nil {
			t.Fatalf("missing mirror %s: %v", file, err)
		}
	}
	workstreamBytes, _ := os.ReadFile(filepath.Join(vault, "workstreams", ws.WorkstreamID+".md"))
	for _, want := range []string{"../engineering/" + ref.ItemID + ".md", "../asks/" + ask.ItemID + ".md", "../signals/" + signal.ItemID + ".md", "../decisions/" + decision.DecisionID + ".md"} {
		if !strings.Contains(string(workstreamBytes), want) {
			t.Errorf("workstream link missing %q", want)
		}
	}
	archiveRevision := getMapData(t, mux, ws.WorkstreamID).Selected.Revision
	postMapCommand(t, mux, WorkstreamCommand{OperationID: "kinds-archive", Action: "archive_workstream", WorkstreamID: ws.WorkstreamID, ExpectedRevision: archiveRevision, Confirm: true})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	archived, _ := os.ReadFile(filepath.Join(vault, "workstreams", ws.WorkstreamID+".md"))
	if !strings.Contains(string(archived), `archived: "true"`) {
		t.Fatalf("archive did not retain stable workstream path/state:\n%s", archived)
	}
}

func TestMapMirror_HTTPChildDetachRestoreMoveRefreshesAllParentExports(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "map.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := NewWorkstreamStore(db, func() time.Time { return now }, time.UTC)
	vault := t.TempDir()
	worker := NewMirrorWorker(st, MirrorWorkerConfig{Root: vault, Now: func() time.Time { return now }})
	st.SetAfterCommit(worker.Wake)
	s := &server{db: db, baseCtx: context.Background(), workstreams: st, mirror: worker}
	mux := http.NewServeMux()
	s.routes(mux)
	a := postMapCommand(t, mux, WorkstreamCommand{OperationID: "move-a", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "a"}})
	b := postMapCommand(t, mux, WorkstreamCommand{OperationID: "move-b", Action: "create_workstream", Workstream: WorkstreamInput{Name: "B", Outcome: "b"}})
	child := postMapCommand(t, mux, WorkstreamCommand{OperationID: "move-child", Action: "create_item", WorkstreamID: a.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: "Child", TrackingState: "open"}})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(vault, "engineering", child.ItemID+".md")
	parentA := filepath.Join(vault, "workstreams", a.WorkstreamID+".md")
	parentB := filepath.Join(vault, "workstreams", b.WorkstreamID+".md")
	edit := postMapCommand(t, mux, WorkstreamCommand{OperationID: "move-edit", Action: "edit_item", WorkstreamID: a.WorkstreamID, ItemID: child.ItemID, ExpectedRevision: child.Revision, Item: WorkstreamItemInput{Kind: "task", Title: "Child updated", TrackingState: "in_progress"}})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustReadFile(t, childPath)), "Child updated") || !strings.Contains(string(mustReadFile(t, parentA)), child.ItemID) {
		t.Fatal("child edit did not refresh item and parent export")
	}
	postMapCommand(t, mux, WorkstreamCommand{OperationID: "move-detach", Action: "detach_item", WorkstreamID: a.WorkstreamID, ItemID: child.ItemID, ExpectedRevision: edit.Revision, Confirm: true})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mustReadFile(t, parentA)), child.ItemID) {
		t.Fatal("detached child remained in parent mirror")
	}
	postMapCommand(t, mux, WorkstreamCommand{OperationID: "move-restore", Action: "restore_item", WorkstreamID: a.WorkstreamID, ItemID: child.ItemID, ExpectedRevision: edit.Revision + 1, Confirm: true})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	postMapCommand(t, mux, WorkstreamCommand{OperationID: "move-to-b", Action: "move_item", WorkstreamID: a.WorkstreamID, ItemID: child.ItemID, DestinationWorkstreamID: b.WorkstreamID, ExpectedRevision: edit.Revision + 2, Confirm: true})
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mustReadFile(t, parentA)), child.ItemID) || !strings.Contains(string(mustReadFile(t, parentB)), child.ItemID) {
		t.Fatal("move did not refresh both parent mirrors")
	}
}

func postMapCommand(t *testing.T, mux *http.ServeMux, command WorkstreamCommand) CommandResult {
	t.Helper()
	command = commandWithCurrentCreateRevision(t, mux, command)
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/map/commands", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("command %s: status=%d body=%s", command.Action, w.Code, w.Body.String())
	}
	var result CommandResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func getMapData(t *testing.T, mux *http.ServeMux, workstreamID string) MapPageView {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map/data?workstream="+workstreamID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("map data: status=%d body=%s", w.Code, w.Body.String())
	}
	var view MapPageView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}
