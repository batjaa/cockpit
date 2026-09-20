package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMapMirrorHTTPFailureBackoffRestartAndRecovery(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	vault := filepath.Join(dir, "blocked-vault")
	// A regular file where the root directory belongs fails even as root.
	if err := os.WriteFile(vault, []byte("unrelated file"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store := NewWorkstreamStore(db, func() time.Time { return now }, time.UTC)
	worker := NewMirrorWorker(store, MirrorWorkerConfig{Root: vault, Now: func() time.Time { return now }})
	s := &server{db: db, baseCtx: context.Background(), workstreams: store, mirror: worker}
	mux := http.NewServeMux()
	s.routes(mux)
	client := acceptanceMapClient{t: t, handler: mux}
	w := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "Saved without a vault", Outcome: "Local saves must survive export failure"}})
	initial := client.read(w.WorkstreamID, "")
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := client.read(w.WorkstreamID, "")
	if failed.Selected.Mirror.Status != "error" || failed.Selected.Mirror.LastSuccessRevision != 0 || failed.Timeline.Total != initial.Timeline.Total+1 {
		t.Fatalf("saved/error state: %+v", failed.Selected)
	}
	firstAttempt := *failed.Selected.Mirror.LastAttemptAt
	for _, advance := range []time.Duration{0, 4 * time.Second} {
		now = firstAttempt.Add(advance)
		if err := worker.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		view := client.read(w.WorkstreamID, "")
		if !view.Selected.Mirror.LastAttemptAt.Equal(firstAttempt) || view.Timeline.Total != failed.Timeline.Total {
			t.Fatal("retry floor or event dedup violated")
		}
	}
	now = firstAttempt.Add(5 * time.Second)
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := client.read(w.WorkstreamID, "")
	if !second.Selected.Mirror.LastAttemptAt.Equal(now) || second.Timeline.Total != failed.Timeline.Total {
		t.Fatal("second attempt missing or duplicated history")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store = NewWorkstreamStore(db, func() time.Time { return now }, time.UTC)
	worker = NewMirrorWorker(store, MirrorWorkerConfig{Root: vault, Now: func() time.Time { return now }})
	s = &server{db: db, baseCtx: context.Background(), workstreams: store, mirror: worker}
	mux = http.NewServeMux()
	s.routes(mux)
	client.handler = mux
	if err := os.Rename(vault, filepath.Join(dir, "preserved-unrelated-file")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(9 * time.Second)
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vault); !os.IsNotExist(err) {
		t.Fatal("restart lost durable 10-second backoff")
	}
	now = now.Add(time.Second)
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := client.read(w.WorkstreamID, "")
	if view.Selected.Mirror.Status != "synced" || view.Selected.Mirror.LastSuccessRevision != view.Selected.Revision || view.Timeline.Total != failed.Timeline.Total+1 {
		t.Fatalf("restart recovery: %+v", view.Selected)
	}
	if !strings.Contains(string(mustReadFile(t, filepath.Join(vault, "workstreams", w.WorkstreamID+".md"))), "Saved without a vault") {
		t.Fatal("recovered content missing")
	}
	if string(mustReadFile(t, filepath.Join(dir, "preserved-unrelated-file"))) != "unrelated file" {
		t.Fatal("unrelated file changed")
	}
}

func TestMapMirrorHTTPConcurrentSavesAndPublisher(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewWorkstreamStore(db, time.Now, time.UTC)
	worker := NewMirrorWorker(store, MirrorWorkerConfig{Root: t.TempDir()})
	s := &server{db: db, baseCtx: context.Background(), workstreams: store, mirror: worker}
	mux := http.NewServeMux()
	s.routes(mux)
	client := acceptanceMapClient{t: t, handler: mux}
	w := client.command(WorkstreamCommand{Action: "create_workstream", Workstream: WorkstreamInput{Name: "Concurrent writes", Outcome: "Publish every latest revision"}})
	var wg sync.WaitGroup
	problems := make(chan string, 100)
	for lane := 0; lane < 3; lane++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if lane == 0 {
					if err := worker.Drain(context.Background()); err != nil {
						problems <- err.Error()
					}
					continue
				}
				if lane == 1 {
					code, _ := commandHTTP(t, mux, WorkstreamCommand{OperationID: fmt.Sprintf("parallel-%d", i), Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "task", Title: fmt.Sprintf("Task %d", i), TrackingState: "open"}})
					if code != 200 {
						problems <- fmt.Sprintf("command returned %d", code)
					}
					continue
				}
				// PR/run activity uses the same SQLite file but not the Map mutex.
				if _, err := db.Exec(`INSERT INTO runs(trigger,status,started_at) VALUES('manual','success',?)`, dbTime(time.Now())); err != nil {
					problems <- err.Error()
				}
			}
		}(lane)
	}
	wg.Wait()
	close(problems)
	for problem := range problems {
		t.Error(problem)
	}
	if err := worker.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := client.read(w.WorkstreamID, "")
	if view.Selected.OpenTaskCount != 25 {
		t.Fatalf("saved %d tasks", view.Selected.OpenTaskCount)
	}
	if view.Selected.Mirror.Status != "synced" || view.Selected.Mirror.LastSuccessRevision != view.Selected.Revision {
		t.Fatalf("latest revision not synced: %+v", view.Selected.Mirror)
	}
}
