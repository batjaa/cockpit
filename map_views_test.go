package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapViewsRenderSavedGraphHistoryAndMarkdownWithoutMutating(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "views.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db, baseCtx: context.Background()}
	mux := http.NewServeMux()
	s.routes(mux)
	client := acceptanceMapClient{t: t, handler: mux}
	w := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits <safe>", Outcome: "Ship the pilot"}})
	it := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: "Review the pilot", TrackingState: "blocked", BlockerReason: "Need policy approval"}})
	view := client.read(w.WorkstreamID, it.ItemID)
	client.command(WorkstreamCommand{Action: "record_decision", WorkstreamID: w.WorkstreamID, ExpectedRevision: view.Selected.Revision, Decision: DecisionInput{Note: "Keep the pilot small"}})
	before := client.read(w.WorkstreamID, it.ItemID)
	for _, tc := range []struct {
		tab  string
		want []string
	}{
		{"overview", []string{"Need policy approval", "Keep the pilot small", "Detached items"}},
		{"graph", []string{"ROOT → CATEGORY → ITEM", "Text alternative", "Review the pilot", "<svg"}},
		{"timeline", []string{"Keep the pilot small", "local events"}},
		{"obsidian", []string{"Generated Markdown preview", "Markdown location", "## Outcome", "Review the pilot"}},
	} {
		t.Run(tc.tab, func(t *testing.T) {
			q := url.Values{"workstream": {w.WorkstreamID}, "item": {it.ItemID}, "tab": {tc.tab}, "q": {"Credits"}}
			rw := httptest.NewRecorder()
			mux.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/map?"+q.Encode(), nil))
			if rw.Code != 200 {
				t.Fatalf("status=%d: %s", rw.Code, rw.Body.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(rw.Body.String(), want) {
					t.Errorf("missing %q", want)
				}
			}
			if !strings.Contains(rw.Body.String(), "q=Credits") {
				t.Error("navigation lost search state")
			}
		})
	}
	after := client.read(w.WorkstreamID, it.ItemID)
	if before.Selected.Revision != after.Selected.Revision || before.Timeline.Total != after.Timeline.Total || before.Selected.Mirror.Status != after.Selected.Mirror.Status {
		t.Fatal("view reads mutated saved records or mirror state")
	}
}

func TestMapFormErrorEscapesOperationIdentity(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "form.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db, baseCtx: context.Background()}
	mux := http.NewServeMux()
	s.routes(mux)
	v := url.Values{"action": {"create_workstream"}, "operation_id": {`" autofocus onfocus="alert(1)`}, "outcome": {"Preserved draft"}}
	r := httptest.NewRequest(http.MethodPost, "/map/action", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 422 {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("incorrect content type: %s", w.Header().Get("Content-Type"))
	}
	if strings.Contains(w.Body.String(), `value="" autofocus`) {
		t.Fatal("operation identity escaped the HTML attribute")
	}
	if !strings.Contains(w.Body.String(), "Preserved draft") {
		t.Fatal("validation lost draft")
	}
}

func TestMapViews_DecisionLinksFollowMovesAndExposeExportStatus(t *testing.T) {
	h, _ := newCommandHTTP(t)
	client := acceptanceMapClient{t: t, handler: h}
	a := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "Decision context"}})
	b := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "B", Outcome: "Current task owner"}})
	it := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: a.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: "Related task", TrackingState: "open"}})
	view := client.read(a.WorkstreamID, it.ItemID)
	d := client.command(WorkstreamCommand{Action: "record_decision", WorkstreamID: a.WorkstreamID, ExpectedRevision: view.Selected.Revision, Decision: DecisionInput{Note: "Retain the evidence", ItemID: it.ItemID}})
	client.command(WorkstreamCommand{Action: "move_item", ItemID: it.ItemID, ExpectedRevision: it.Revision, DestinationWorkstreamID: b.WorkstreamID})
	for _, tab := range []string{"timeline", "obsidian"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/map?workstream="+a.WorkstreamID+"&tab="+tab, nil))
		if rw.Code != http.StatusOK {
			t.Fatalf("%s status: %d", tab, rw.Code)
		}
		if tab == "timeline" {
			want := `href="/map?filter=active&amp;item=` + it.ItemID + `&amp;tab=timeline&amp;workstream=` + b.WorkstreamID + `"`
			if !strings.Contains(rw.Body.String(), want) {
				t.Error("decision link does not follow its moved item to the current parent")
			}
		} else {
			for _, want := range []string{"Decision exports", `value="decision"`, `value="` + d.DecisionID + `"`, "decisions/" + d.DecisionID + ".md"} {
				if !strings.Contains(rw.Body.String(), want) {
					t.Errorf("missing decision export control %q", want)
				}
			}
		}
	}
}
