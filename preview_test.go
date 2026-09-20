package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMapPreview runs the real application against explicitly isolated data for
// browser acceptance. It is opt-in, never opens the normal Cockpit database, and
// disables discovery/review/session jobs. Compile with go test -c, then run with
// COCKPIT_PREVIEW_DIR and -test.run '^TestMapPreview$' -test.timeout 0.
func TestMapPreview(t *testing.T) {
	dir := os.Getenv("COCKPIT_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set COCKPIT_PREVIEW_DIR to an isolated preview directory")
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("COCKPIT_PREVIEW_DIR must be absolute")
	}
	db, err := OpenDB(filepath.Join(dir, "cockpit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if os.Getenv("COCKPIT_PREVIEW_SEED") == "1" {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM prs`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			seedReview(t, db)
		}
		for n := 100; n < 125; n++ {
			if _, err := UpsertPR(context.Background(), db, GHPR{Number: n, Title: fmt.Sprintf("Preview cached change %d", n), URL: fmt.Sprintf("https://github.com/octo/repo/pull/%d", n), HeadRefOid: "preview", Author: GHAuthor{Login: "preview"}}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := DefaultConfig()
	cfg.Schedule = ScheduleConfig{}
	cfg.Sessions.Enabled = false
	cfg.Workstreams.Mirror.Root = filepath.Join(dir, "vault")
	cfg.HTTP.Addr = "127.0.0.1:8766"
	if addr := os.Getenv("COCKPIT_PREVIEW_ADDR"); addr != "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			t.Fatal("COCKPIT_PREVIEW_ADDR must use a loopback IP address")
		}
		cfg.HTTP.Addr = addr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t.Logf("isolated preview: http://%s (data: %s)", cfg.HTTP.Addr, dir)
	if err := Serve(ctx, db, cfg); err != nil {
		t.Fatal(err)
	}
}
