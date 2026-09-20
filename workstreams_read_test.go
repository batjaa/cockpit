package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMapReadCachedSourceOpensTheActualReview(t *testing.T) {
	handler, store := newCommandHTTP(t)
	// Make PR IDs different from review IDs; the established /pr route is
	// keyed by review identity, not the upstream PR row.
	_, err := store.db.Exec(`INSERT INTO prs(id,owner,repo,number,url,title,author,head_sha,first_seen,last_seen) VALUES(99,'other','repo',1,'https://github.com/other/repo/pull/1','Other','person','sha',?,?)`, dbTime(store.now()), dbTime(store.now()))
	if err != nil {
		t.Fatal(err)
	}
	reviewID := seedReview(t, store.db)
	client := acceptanceMapClient{t: t, handler: handler}
	w := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "Cached review", Outcome: "Review existing work"}})
	ref := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "Review context", PRID: 100}})
	view := client.read(w.WorkstreamID, ref.ItemID)
	if view.SelectedItem == nil || view.SelectedItem.Source == nil {
		t.Fatal("cached source context missing")
	}
	want := fmt.Sprintf("/pr/%d", reviewID)
	if view.SelectedItem.Source.LocalReviewURL != want {
		t.Fatalf("local review URL=%q want %q", view.SelectedItem.Source.LocalReviewURL, want)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, want, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("review link=%d", response.Code)
	}
	// Merely attaching/reading cannot change the upstream source.
	if view.SelectedItem.Source.CachedState != "OPEN" {
		t.Fatalf("cached source was mutated: %q", view.SelectedItem.Source.CachedState)
	}
}

func TestMapReadLargeDatasetHasBoundedPagesAndHiddenAttention(t *testing.T) {
	handler, store := newCommandHTTP(t)
	// Fixture preparation is bulk SQL; all observations go through the public
	// HTTP read seam. This tests scale without 20,000 fsync-heavy API writes.
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for n := 0; n < 100; n++ {
		id := fmt.Sprintf("fixture-workstream-%03d", n)
		_, err = tx.Exec(`INSERT INTO workstreams(id,name,outcome,created_at,updated_at) VALUES(?,?,?,?,?)`, id, fmt.Sprintf("Workstream %03d", n), "Track a local outcome", dbTime(store.now()), dbTime(store.now()))
		if err != nil {
			t.Fatal(err)
		}
	}
	wid := "fixture-workstream-099"
	items, err := tx.Prepare(`INSERT INTO workstream_items(id,workstream_id,kind,title,tracking_state,blocker_reason,created_at,updated_at) VALUES(?,?,'task',?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer items.Close()
	events, err := tx.Prepare(`INSERT INTO timeline_events(id,workstream_id,entity_type,entity_id,event_type,description,revision,created_at) VALUES(?,?,'engineering_item',?,'created','Fixture task',1,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	for n := 0; n < 10_000; n++ {
		id := fmt.Sprintf("fixture-item-%05d", n)
		title, state, blocker := fmt.Sprintf("Task %05d", n), "done", ""
		if n == 9_999 {
			title, state, blocker = "Needle approval PLAT-4242", "blocked", "Needs an explicit decision"
		}
		if _, err = items.Exec(id, wid, title, state, blocker, dbTime(store.now()), dbTime(store.now())); err != nil {
			t.Fatal(err)
		}
		if _, err = events.Exec(fmt.Sprintf("fixture-event-%05d", n), wid, id, dbTime(store.now())); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	client := acceptanceMapClient{t: t, handler: handler}
	started := time.Now()
	view := client.read(wid, "")
	t.Logf("100 workstreams / 10,000 items + events: full selected read %s", time.Since(started))
	if len(view.Workstreams.Rows) != 50 || view.Workstreams.Total != 100 || view.Workstreams.NextCursor != "50" {
		t.Fatalf("workstream pagination %+v", view.Workstreams)
	}
	if view.Workstreams.Rows[0].ID != wid {
		t.Fatal("hidden attention must rank before quiet workstreams")
	}
	if len(view.Categories[0].Items.Rows) != 50 || view.Categories[0].Total != 10_000 || view.Categories[0].AttentionCount != 1 {
		t.Fatal("bounded item page must retain full-dataset totals and attention")
	}
	if view.Selected.OpenTaskCount != 1 || len(view.Selected.AttentionReasons) != 1 || view.Selected.AttentionReasons[0].EntityID != "fixture-item-09999" {
		t.Fatal("last-page blocked task missing from attention")
	}
	if len(view.Graph.Nodes) != 104 || view.Graph.Omitted != 9_900 {
		t.Fatalf("graph must have root, three branches, 100 items and omitted count: nodes=%d omitted=%d", len(view.Graph.Nodes), view.Graph.Omitted)
	}
	if len(view.Timeline.Rows) != 50 || view.Timeline.Total != 10_000 {
		t.Fatal("timeline not bounded")
	}
	readQuery := func(query string) MapPageView {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/map/data?"+query, nil))
		if response.Code != 200 {
			t.Fatalf("query %q status=%d %s", query, response.Code, response.Body.String())
		}
		var page MapPageView
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	next := readQuery("workstreams_cursor=50")
	seen := map[string]bool{}
	for _, row := range view.Workstreams.Rows {
		seen[row.ID] = true
	}
	for _, row := range next.Workstreams.Rows {
		if seen[row.ID] {
			t.Fatal("duplicate workstream across stable pages")
		}
		seen[row.ID] = true
	}
	if len(seen) != 100 || next.Workstreams.NextCursor != "" {
		t.Fatal("workstreams omitted by pagination")
	}
	search := readQuery("q=PLAT-4242")
	if search.Workstreams.Total != 1 || len(search.Workstreams.Rows) != 1 || search.Workstreams.Rows[0].ID != wid {
		t.Fatal("search missed hidden child")
	}
	second := readQuery("workstream=" + wid + "&category=engineering&items_cursor=50&timeline_cursor=50")
	if second.Categories[0].Items.Rows[0].ID == view.Categories[0].Items.Rows[0].ID || second.Timeline.Rows[0].ID == view.Timeline.Rows[0].ID {
		t.Fatal("item/history cursor did not advance")
	}
}

func TestMapReadDateOnlyDeadlineUsesLocalDayAcrossDST(t *testing.T) {
	handler, store := newCommandHTTP(t)
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 7, 59, 59, 0, time.UTC)
	store.now = func() time.Time { return now }
	store.location = location
	client := acceptanceMapClient{t: t, handler: handler}
	w := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "DST release", Outcome: "Respect the entire local deadline day", TargetDate: "2026-11-01"}})
	task := client.command(WorkstreamCommand{Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: "Review", DueDate: "2026-11-01", TrackingState: "blocked", BlockerReason: "Waiting for fixture"}})
	before := client.read(w.WorkstreamID, task.ItemID)
	if before.Selected.AttentionCount != 1 || len(before.Selected.AttentionReasons) != 1 {
		t.Fatalf("before local midnight only blocker counts: %+v", before.Selected)
	}
	now = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	after := client.read(w.WorkstreamID, task.ItemID)
	if after.Selected.AttentionCount != 2 || len(after.Selected.AttentionReasons) != 3 {
		t.Fatalf("midnight: blocked+overdue task counts once, target counts once, all three reasons retained: %+v", after.Selected)
	}
	if after.Selected.Revision != before.Selected.Revision || after.SelectedItem.Revision != before.SelectedItem.Revision {
		t.Fatal("time transition mutated local tracking")
	}
	if after.Categories[0].AttentionCount != 1 || len(after.SelectedItem.Attention) != 2 {
		t.Fatal("branch must count distinct entities while showing both reasons")
	}
}
