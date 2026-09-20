package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestMapCommands_HistoryCapturesSemanticDeltas(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Pilot", Outcome: "Prove it", Owner: "A"}})
	askInput := WorkstreamItemInput{Kind: "ask", Title: "Security review", Counterpart: "Security", AskStatus: "open", FollowUpAt: "2026-01-03T00:00:00Z"}
	_, ask := commandHTTP(t, h, WorkstreamCommand{OperationID: "ask", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: askInput})
	askInput.AskStatus, askInput.FollowUpAt, askInput.LastContactAt = "waiting", "2026-01-04T00:00:00Z", "2026-01-02T12:00:00Z"
	if code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "ask-edit", Action: "edit_item", ItemID: ask.ItemID, ExpectedRevision: ask.Revision, Item: askInput}); code != http.StatusOK {
		t.Fatalf("ask edit=%d", code)
	}
	signal := WorkstreamItemInput{Kind: "signal", Title: "Errors", Value: "2", Unit: "%", Assessment: "unknown", ObservedAt: "2026-01-01T00:00:00Z"}
	_, sig := commandHTTP(t, h, WorkstreamCommand{OperationID: "signal", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: signal})
	signal.Value, signal.Assessment, signal.ObservedAt = "7", "concerning", "2026-01-02T00:00:00Z"
	if code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "signal-edit", Action: "edit_item", ItemID: sig.ItemID, ExpectedRevision: sig.Revision, Item: signal}); code != http.StatusOK {
		t.Fatalf("signal edit=%d", code)
	}
	view := mapDataHTTP(t, h, w.WorkstreamID)
	all := ""
	for _, e := range view.Timeline.Rows {
		all += e.Description + "\n"
	}
	for _, want := range []string{"Created ask: Security review", "ask status open → waiting", "follow-up 2026-01-03T00:00:00Z → 2026-01-04T00:00:00Z", "value 2 → 7", "assessment unknown → concerning", "observed at 2026-01-01T00:00:00Z → 2026-01-02T00:00:00Z"} {
		if !strings.Contains(all, want) {
			t.Fatalf("timeline missing %q:\n%s", want, all)
		}
	}
}

func TestMapCommands_WorkstreamHistoryIncludesMetadataAndCompletionJudgment(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Pilot", Outcome: "Prove it", Owner: "A"}})
	code, edited := commandHTTP(t, h, WorkstreamCommand{OperationID: "metadata", Action: "edit_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: w.Revision, Workstream: WorkstreamInput{Name: "Pilot", Outcome: "Ship it", Owner: "B", Sponsor: "VP", TargetDate: "2026-02-01", Notes: "Narrow rollout"}})
	if code != http.StatusOK {
		t.Fatalf("metadata edit=%d", code)
	}
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "complete", Action: "complete_workstream", WorkstreamID: w.WorkstreamID, ExpectedRevision: edited.Revision, Confirm: true, Workstream: WorkstreamInput{Notes: "Released to all users"}})
	if code != http.StatusOK {
		t.Fatalf("complete=%d", code)
	}
	view := mapDataHTTP(t, h, w.WorkstreamID)
	all := ""
	for _, e := range view.Timeline.Rows {
		all += e.Description + "\n"
	}
	for _, want := range []string{"outcome Prove it → Ship it", "owner A → B", "target date none → 2026-02-01", "Completed workstream: outcome Released to all users"} {
		if !strings.Contains(all, want) {
			t.Fatalf("timeline missing %q:\n%s", want, all)
		}
	}
}

func TestMapCommands_NormalizesEditsAndProtectsLifecycleAndRestore(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, a := commandHTTP(t, h, WorkstreamCommand{OperationID: "a", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "A"}})
	_, b := commandHTTP(t, h, WorkstreamCommand{OperationID: "b", Action: "create_workstream", Workstream: WorkstreamInput{Name: "B", Outcome: "B"}})
	in := WorkstreamItemInput{Kind: "task", Title: "Task", TrackingState: "blocked", BlockerReason: "API unavailable"}
	_, item := commandHTTP(t, h, WorkstreamCommand{OperationID: "task", Action: "create_item", WorkstreamID: a.WorkstreamID, Item: in})
	in.TrackingState, in.BlockerReason, in.Value, in.Unit, in.Assessment, in.ObservedAt = "in_progress", "stale", "irrelevant", "x", "concerning", "2026-01-01T00:00:00Z"
	code, edited := commandHTTP(t, h, WorkstreamCommand{OperationID: "unblock", Action: "edit_item", ItemID: item.ItemID, ExpectedRevision: item.Revision, Item: in})
	if code != http.StatusOK {
		t.Fatalf("edit=%d", code)
	}
	view := mapDataHTTP(t, h, a.WorkstreamID)
	got := view.Categories[0].Items.Rows[0]
	if got.BlockerReason != "" || got.Value != "" || got.Assessment != "" {
		t.Fatalf("irrelevant state persisted: %+v", got.WorkstreamItem)
	}
	code, noop := commandHTTP(t, h, WorkstreamCommand{OperationID: "detach", Action: "detach_item", ItemID: item.ItemID, ExpectedRevision: edited.Revision, Confirm: true})
	if code != http.StatusOK {
		t.Fatalf("detach=%d", code)
	}
	code, again := commandHTTP(t, h, WorkstreamCommand{OperationID: "detach-again", Action: "detach_item", ItemID: item.ItemID, ExpectedRevision: noop.Revision, Confirm: true})
	if code != http.StatusOK || !again.Idempotent || again.Revision != noop.Revision {
		t.Fatalf("repeat detach=%d %+v", code, again)
	}
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "wrong-restore", Action: "restore_item", ItemID: item.ItemID, WorkstreamID: b.WorkstreamID, ExpectedRevision: noop.Revision})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-parent restore=%d", code)
	}
	code, restored := commandHTTP(t, h, WorkstreamCommand{OperationID: "restore", Action: "restore_item", ItemID: item.ItemID, ExpectedRevision: noop.Revision})
	if code != http.StatusOK || restored.WorkstreamID != a.WorkstreamID {
		t.Fatalf("restore=%d %+v", code, restored)
	}
	code, complete := commandHTTP(t, h, WorkstreamCommand{OperationID: "complete", Action: "complete_workstream", WorkstreamID: a.WorkstreamID, ExpectedRevision: 5, Confirm: true, OverrideReason: "Work continues elsewhere", Workstream: WorkstreamInput{Notes: "Delivered"}})
	if code != http.StatusOK {
		t.Fatalf("complete=%d %+v", code, complete)
	}
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "complete-again", Action: "complete_workstream", WorkstreamID: a.WorkstreamID, ExpectedRevision: complete.Revision, Confirm: true, Workstream: WorkstreamInput{Notes: "Overwrite"}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("repeat complete=%d", code)
	}
	view = mapDataHTTP(t, h, a.WorkstreamID)
	if view.Selected.CompletionNote != "Delivered" {
		t.Fatalf("completion note overwritten: %q", view.Selected.CompletionNote)
	}
}

func TestMapCommands_CanonicalGitHubPRIdentityKeepsGenericQueryIdentity(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "w", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "B"}})
	add := func(op, raw string) CommandResult {
		_, r := commandHTTP(t, h, WorkstreamCommand{OperationID: op, Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "Ref", SourceURL: raw}})
		return r
	}
	first := add("pr-one", "HTTPS://github.com/Octo/Repo/pull/42?diff=split#discussion")
	second := add("pr-two", "https://github.com/octo/repo/pull/042")
	if first.ItemID == "" || second.ItemID != first.ItemID || second.Revision != first.Revision {
		t.Fatalf("PR identity did not dedupe: %#v %#v", first, second)
	}
	g1 := add("generic-one", "https://example.test/doc?view=one#first")
	g2 := add("generic-two", "https://example.test/doc?view=two#first")
	if g1.ItemID == g2.ItemID {
		t.Fatalf("generic URL query was incorrectly discarded: %#v %#v", g1, g2)
	}
}

func TestMapCommands_ReferenceNeedsSourceAndDuplicateReturnsCurrentRevision(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, w := commandHTTP(t, h, WorkstreamCommand{OperationID: "ref-parent", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Context", Outcome: "Keep labelled evidence"}})
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "empty-ref", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "No source"}})
	if code != 422 {
		t.Fatalf("contextless reference returned %d", code)
	}
	input := WorkstreamItemInput{Kind: "reference", Title: "Human-readable evidence", SourceURL: "https://example.test/evidence"}
	_, ref := commandHTTP(t, h, WorkstreamCommand{OperationID: "real-ref", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: input})
	input.Description = "Updated local context"
	_, edited := commandHTTP(t, h, WorkstreamCommand{OperationID: "edit-ref-context", Action: "edit_item", WorkstreamID: w.WorkstreamID, ItemID: ref.ItemID, ExpectedRevision: ref.Revision, Item: input})
	_, again := commandHTTP(t, h, WorkstreamCommand{OperationID: "duplicate-ref", Action: "create_item", WorkstreamID: w.WorkstreamID, Item: input})
	if again.ItemID != ref.ItemID || again.Revision != edited.Revision {
		t.Fatalf("duplicate returned stale identity/revision: %+v vs %+v", again, edited)
	}
	view := mapDataHTTP(t, h, w.WorkstreamID)
	if view.Categories[0].Items.Rows[0].Source.Label != "Human-readable evidence" {
		t.Fatal("reference was not labelled from the supplied item title")
	}
}
