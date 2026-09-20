package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapBoundaryRejectsCrossSiteAndMalformedCommands(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db, baseCtx: context.Background()}
	mux := http.NewServeMux()
	s.routes(mux)
	for _, path := range []string{"/map/commands", "/map/action", "/map/mirror/retry"} {
		for _, useFetchMetadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("cross-site %s metadata=%t", path, useFetchMetadata), func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(`{"operation_id":"hostile","action":"create_workstream","workstream":{"name":"Injected","outcome":"Must not save"}}`))
				r.Header.Set("Origin", "https://untrusted.example")
				if useFetchMetadata {
					r.Header.Set("Sec-Fetch-Site", "cross-site")
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"unknown field", `{"action":"create_workstream","untrusted":true}`, 400},
		{"second command", `{} {}`, 400},
		{"invalid JSON", `{`, 400},
		{"oversized", `{"operation_id":"large","workstream":{"notes":"` + strings.Repeat("x", mapRequestLimit) + `"}}`, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/map/commands", strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	view := (&acceptanceMapClient{t: t, handler: mux}).read("", "")
	if view.Workstreams.Total != 0 {
		t.Fatal("rejected requests saved workstreams")
	}
}

func TestMapBoundaryEscapesTextAndRejectsUnsafeSources(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "escaping.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db, baseCtx: context.Background()}
	mux := http.NewServeMux()
	s.routes(mux)
	client := &acceptanceMapClient{t: t, handler: mux}
	created := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: `<script>alert("name")</script>`, Outcome: `<img src=x onerror=alert(1)>`, Notes: `<svg onload=alert(2)>`}})
	for i, source := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://user:password@example.com/private", "https:///missing-host"} {
		body, err := json.Marshal(WorkstreamCommand{OperationID: fmt.Sprintf("unsafe-%d", i), Action: "create_item", WorkstreamID: created.WorkstreamID, ExpectedRevision: created.Revision, Item: WorkstreamItemInput{Kind: "reference", Title: "Unsafe", SourceURL: source}})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/map/commands", strings.NewReader(string(body))))
		if w.Code != 422 {
			t.Fatalf("source %q: status=%d body=%s", source, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map?workstream="+created.WorkstreamID, nil))
	if w.Code != 200 {
		t.Fatalf("render=%d: %s", w.Code, w.Body.String())
	}
	for _, unsafe := range []string{`<script>alert("name")</script>`, `<img src=x onerror=alert(1)>`, `<svg onload=alert(2)>`} {
		if strings.Contains(w.Body.String(), unsafe) {
			t.Fatalf("unescaped user text: %s", unsafe)
		}
	}
	view := client.read(created.WorkstreamID, "")
	for _, category := range view.Categories {
		if category.Total != 0 {
			t.Fatal("unsafe source mutation partially committed")
		}
	}
}

func TestMapMigrationPreservesExistingReviewsAndSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// This is the pre-Map portion of the existing schema, with no new tables.
	legacy, _, found := strings.Cut(schemaSQL, "-- Map/workstreams")
	if !found {
		t.Fatal("legacy schema boundary missing")
	}
	if _, err = db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	reviewID := seedReview(t, db)
	if _, err = db.Exec(`INSERT INTO sessions(agent,machine,session_key,title,last_active,message_count) VALUES('codex','local','before-map','Existing session','2026-09-20 12:00:00.000',10)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	var workstreamID string
	for reopening := 0; reopening < 2; reopening++ {
		db, err = OpenDB(path)
		if err != nil {
			t.Fatal(err)
		}
		s := &server{db: db, baseCtx: context.Background()}
		mux := http.NewServeMux()
		s.routes(mux)
		for _, tc := range []struct{ path, want string }{{"/", "Add foo to bar"}, {fmt.Sprintf("/pr/%d", reviewID), "Private review brief"}, {"/sessions", "Existing session"}} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("reopen %d %s: status=%d missing %q", reopening, tc.path, w.Code, tc.want)
			}
		}
		client := &acceptanceMapClient{t: t, handler: mux}
		if reopening == 0 {
			workstreamID = client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "Migrated local work", Outcome: "Preserve the existing app"}}).WorkstreamID
		}
		view := client.read(workstreamID, "")
		if view.Selected == nil || view.Selected.Name != "Migrated local work" {
			t.Fatal("new workstream not durable across reopening")
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
