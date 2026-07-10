package main

import (
	"context"
	"testing"
)

// TestEnqueue_CoalesceDedupOrder exercises the queue's admission rules with a
// simulated in-flight discovery (active=true so enqueue never spawns a real
// worker): discovers coalesce, reviews dedupe by URL, and distinct reviews
// stack in order with the right "ahead" count.
func TestEnqueue_CoalesceDedupOrder(t *testing.T) {
	s := &server{baseCtx: context.Background()}
	s.runMu.Lock()
	s.active = true
	s.current = &runJob{kind: runDiscover, trigger: "in-flight"}
	s.runMu.Unlock()

	// A second discovery coalesces with the active one.
	if st, _ := s.enqueue(runJob{kind: runDiscover, trigger: "schedule"}); st != enqDuplicate {
		t.Errorf("discover during discover: got %q want %q", st, enqDuplicate)
	}

	// First review queues behind the active discovery.
	if st, ahead := s.enqueue(runJob{kind: runReview, url: "u1"}); st != enqQueued || ahead != 1 {
		t.Errorf("review u1: got (%q,%d) want (%q,1)", st, ahead, enqQueued)
	}

	// Same URL dedupes against the pending one.
	if st, _ := s.enqueue(runJob{kind: runReview, url: "u1"}); st != enqDuplicate {
		t.Errorf("review u1 again: got %q want %q", st, enqDuplicate)
	}

	// A different URL stacks behind current + u1.
	if st, ahead := s.enqueue(runJob{kind: runReview, url: "u2"}); st != enqQueued || ahead != 2 {
		t.Errorf("review u2: got (%q,%d) want (%q,2)", st, ahead, enqQueued)
	}

	s.runMu.Lock()
	got := len(s.queue)
	s.runMu.Unlock()
	if got != 2 {
		t.Errorf("queue depth = %d want 2 (u1, u2)", got)
	}
}

// TestEnqueue_RefusedDuringShutdown: once baseCtx is cancelled, new work is
// refused so the worker can drain and exit.
func TestEnqueue_RefusedDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &server{baseCtx: ctx}
	if st, _ := s.enqueue(runJob{kind: runReview, url: "u1"}); st != enqShutdown {
		t.Errorf("enqueue after shutdown: got %q want %q", st, enqShutdown)
	}
	if s.isRunning() {
		t.Error("no worker should start during shutdown")
	}
}
