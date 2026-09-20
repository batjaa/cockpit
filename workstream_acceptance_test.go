package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

type acceptanceMapClient struct {
	t        *testing.T
	handler  http.Handler
	sequence int
}

func (c *acceptanceMapClient) command(cmd WorkstreamCommand) CommandResult {
	c.t.Helper()
	c.sequence++
	cmd.OperationID = fmt.Sprintf("acceptance-%d", c.sequence)
	cmd = commandWithCurrentCreateRevision(c.t, c.handler, cmd)
	body, err := json.Marshal(cmd)
	if err != nil {
		c.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/map/commands", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	if w.Code < 200 || w.Code >= 300 {
		c.t.Fatalf("%s: status %d: %s", cmd.Action, w.Code, w.Body.String())
	}
	var result CommandResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		c.t.Fatal(err)
	}
	return result
}

func (c *acceptanceMapClient) read(id, item string) MapPageView {
	c.t.Helper()
	q := url.Values{"workstream": {id}, "item": {item}}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map/data?"+q.Encode(), nil))
	if w.Code != http.StatusOK {
		c.t.Fatalf("read Map: %d: %s", w.Code, w.Body.String())
	}
	var view MapPageView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		c.t.Fatal(err)
	}
	return view
}

func TestMapAcceptanceAttentionChangesWithTimeWithoutResolvingWork(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "acceptance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 19, 0, 0, 0, time.UTC)
	store := NewWorkstreamStore(db, func() time.Time { return now }, location)
	s := &server{db: db, baseCtx: context.Background(), workstreams: store}
	mux := http.NewServeMux()
	s.routes(mux)
	client := acceptanceMapClient{t: t, handler: mux}
	created := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{
		Name: "Credits v1", Outcome: "Ship credits without increasing customer latency", TargetDate: "2026-09-21",
	}})
	reference := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: created.WorkstreamID, Item: WorkstreamItemInput{
		Kind: "reference", Title: "Credits design", SourceURL: "https://example.com/credits#decision", SourceKind: "rfc", SourceLabel: "Credits RFC",
	}})
	taskInput := WorkstreamItemInput{Kind: "task", Title: "Review credits change", TrackingState: "open", DueDate: "2026-09-20"}
	task := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: created.WorkstreamID, Item: taskInput})
	askInput := WorkstreamItemInput{Kind: "ask", Title: "Confirm billing compatibility", Counterpart: "Billing", AskStatus: "waiting", FollowUpAt: "2026-09-20T21:00:00Z"}
	ask := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: created.WorkstreamID, Item: askInput})
	signalInput := WorkstreamItemInput{Kind: "signal", Title: "Credit latency", Value: "210", Unit: "ms", Assessment: "concerning", ObservedAt: "2026-09-20T18:00:00Z", ReviewBy: "2026-09-20T21:00:00Z"}
	signal := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: created.WorkstreamID, Item: signalInput})
	view := client.read(created.WorkstreamID, "")
	if view.Selected == nil {
		t.Fatal("created workstream not selected")
	}
	if len(view.Selected.AttentionReasons) != 1 || view.Selected.AttentionReasons[0].EntityID != signal.ItemID {
		t.Fatalf("only the concerning signal should initially need attention: %+v", view.Selected.AttentionReasons)
	}
	if view.Selected.OpenTaskCount != 1 || view.Selected.UnresolvedAskCount != 1 {
		t.Fatalf("references are not obligations: %+v", view.Selected)
	}
	beforeRevision := view.Selected.Revision
	beforeHistory := view.Timeline.Total
	now = time.Date(2026, 9, 20, 21, 0, 0, 0, time.UTC)
	view = client.read(created.WorkstreamID, ask.ItemID)
	if len(view.Selected.AttentionReasons) != 2 {
		t.Fatalf("follow-up due exactly now should add a reason: %+v", view.Selected.AttentionReasons)
	}
	if view.Selected.Revision != beforeRevision || view.Timeline.Total != beforeHistory {
		t.Fatal("passage of time or a read must not mutate revisions/history")
	}
	if view.SelectedItem == nil || view.SelectedItem.AskStatus != "waiting" {
		t.Fatal("reading an overdue ask must not resolve it")
	}
	askInput.AskStatus = "resolved"
	client.command(WorkstreamCommand{Action: "edit_item", WorkstreamID: created.WorkstreamID, ItemID: ask.ItemID, ExpectedRevision: ask.Revision, Item: askInput})
	taskInput.TrackingState = "done"
	client.command(WorkstreamCommand{Action: "edit_item", WorkstreamID: created.WorkstreamID, ItemID: task.ItemID, ExpectedRevision: task.Revision, Item: taskInput})
	view = client.read(created.WorkstreamID, reference.ItemID)
	if len(view.Selected.AttentionReasons) != 1 || view.Selected.AttentionReasons[0].EntityID != signal.ItemID {
		t.Fatal("resolving task and ask must leave the concerning signal unresolved")
	}
	if view.SelectedItem == nil || view.SelectedItem.Kind != "reference" || view.SelectedItem.TrackingState != "" {
		t.Fatal("reference must remain context, not a completed task")
	}
	signalInput.Assessment = "normal"
	signalInput.ObservedAt = "2026-09-20T21:00:00Z"
	signalInput.ReviewBy = "2026-09-21T21:00:00Z"
	client.command(WorkstreamCommand{Action: "edit_item", WorkstreamID: created.WorkstreamID, ItemID: signal.ItemID, ExpectedRevision: signal.Revision, Item: signalInput})
	view = client.read(created.WorkstreamID, "")
	if len(view.Selected.AttentionReasons) != 0 || view.Selected.Lifecycle != "active" {
		t.Fatalf("cleared concerns must not auto-complete the workstream: %+v", view.Selected)
	}
}
