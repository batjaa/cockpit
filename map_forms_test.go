package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newMapFormsServer(t *testing.T, location *time.Location) (*server, *http.ServeMux) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "map-forms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &server{db: db, workstreams: NewWorkstreamStore(db, nil, location)}
	mux := http.NewServeMux()
	s.workstreamRoutes(mux)
	s.mapRoutes(mux)
	return s, mux
}

func TestMapForms_EditWorkstreamIncludesMetadataAndCancel(t *testing.T) {
	s, mux := newMapFormsServer(t, time.UTC)
	created, err := s.workstreamStore().Execute(t.Context(), WorkstreamCommand{OperationID: "create-form-metadata", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship credits", Owner: "Ada", Sponsor: "Grace", TargetDate: "2026-10-01", Notes: "Keep scope narrow"}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map?workstream="+created.WorkstreamID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`name="owner" value="Ada"`, `name="sponsor" value="Grace"`, `name="target_date" value="2026-10-01"`, `name="notes"`, "Cancel"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestMapForms_EditItemUsesKindSpecificFieldsAndLocalDateTime(t *testing.T) {
	location := time.FixedZone("PDT", -7*60*60)
	s, mux := newMapFormsServer(t, location)
	workstream, err := s.workstreamStore().Execute(t.Context(), WorkstreamCommand{OperationID: "create-form-item-parent", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship credits"}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.workstreamStore().Execute(t.Context(), WorkstreamCommand{OperationID: "create-form-signal", Action: "create_item", WorkstreamID: workstream.WorkstreamID, ExpectedRevision: workstream.Revision, Item: WorkstreamItemInput{Kind: "signal", Title: "Latency", ObservedAt: "2026-10-01T17:30:00Z", Assessment: "concerning", Value: "120", Unit: "ms", SourceURL: "https://example.test/latency", SourceLabel: "dashboard"}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map?workstream="+workstream.WorkstreamID+"&item="+item.ItemID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`name="source_url"`, `value="https://example.test/latency"`, `name="value" value="120"`, `name="unit" value="ms"`, `name="observed_at" value="2026-10-01T10:30"`, `data-map-kind="signal"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestMapForms_PostNormalizesLocalDateTimeAndPreservesErrors(t *testing.T) {
	location := time.FixedZone("PDT", -7*60*60)
	s, mux := newMapFormsServer(t, location)
	workstream, err := s.workstreamStore().Execute(t.Context(), WorkstreamCommand{OperationID: "create-form-ask-parent", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship credits"}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.workstreamStore().Execute(t.Context(), WorkstreamCommand{OperationID: "create-form-ask", Action: "create_item", WorkstreamID: workstream.WorkstreamID, ExpectedRevision: workstream.Revision, Item: WorkstreamItemInput{Kind: "ask", Title: "Ask Billing", Counterpart: "Billing", AskStatus: "waiting"}})
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"action": {"edit_item"}, "operation_id": {"edit-form-ask"}, "workstream_id": {workstream.WorkstreamID}, "item_id": {item.ItemID}, "expected_revision": {"1"}, "kind": {"ask"}, "title": {"Ask Billing"}, "counterpart": {"Billing"}, "ask_status": {"waiting"}, "follow_up_at": {"2026-10-02T09:15"}}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/map/action?tab=overview", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	var follow string
	if err := s.db.QueryRow(`SELECT follow_up_at FROM workstream_items WHERE id=?`, item.ItemID).Scan(&follow); err != nil {
		t.Fatal(err)
	}
	if follow != "2026-10-02T16:15:00Z" {
		t.Fatalf("follow_up_at=%q", follow)
	}

	values.Set("operation_id", "invalid-form-ask")
	values.Set("expected_revision", "2")
	values.Set("counterpart", "")
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/map/action?workstream="+workstream.WorkstreamID+"&item="+item.ItemID, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `name="counterpart" value=""`) || !strings.Contains(w.Body.String(), "Review the marked fields") {
		t.Fatalf("field error/input not preserved: %s", w.Body.String())
	}
}
