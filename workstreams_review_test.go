package main

import (
	"net/http"
	"testing"
)

func TestMapCommands_CreateItemRejectsStaleParentRevision(t *testing.T) {
	h, _ := newCommandHTTP(t)
	_, created := commandHTTP(t, h, WorkstreamCommand{
		OperationID: "stale-create-parent", Action: "create_workstream",
		Workstream: WorkstreamInput{Name: "Launch", Outcome: "Ship safely"},
	})
	code, edited := commandHTTP(t, h, WorkstreamCommand{
		OperationID: "stale-create-edit", Action: "edit_workstream", WorkstreamID: created.WorkstreamID, ExpectedRevision: created.Revision,
		Workstream: WorkstreamInput{Name: "Launch updated", Outcome: "Ship safely"},
	})
	if code != http.StatusOK {
		t.Fatalf("parent edit=%d", code)
	}
	if edited.Revision == created.Revision {
		t.Fatal("parent edit did not advance revision")
	}

	code, _ = commandHTTP(t, h, WorkstreamCommand{
		OperationID: "stale-create-item", Action: "create_item", WorkstreamID: created.WorkstreamID, ExpectedRevision: created.Revision,
		Item: WorkstreamItemInput{Kind: "task", Title: "Stale obligation", TrackingState: "open"},
	})
	if code != http.StatusConflict {
		t.Fatalf("stale create item=%d want %d", code, http.StatusConflict)
	}
}

func TestMapCommands_DecisionRelationshipsStayWithinWorkstream(t *testing.T) {
	t.Run("rejects item from another workstream", func(t *testing.T) {
		h, a, _, itemA, itemB, _, _ := reviewDecisionFixtures(t)
		viewA := mapDataHTTP(t, h, a.WorkstreamID)
		code, _ := commandHTTP(t, h, WorkstreamCommand{
			OperationID: "decision-cross-item", Action: "record_decision", WorkstreamID: a.WorkstreamID, ExpectedRevision: viewA.Selected.Revision,
			Decision: DecisionInput{Note: "Incorrect cross-workstream item", ItemID: itemB.ItemID},
		})
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("cross-workstream decision item=%d want %d (local item=%s)", code, http.StatusUnprocessableEntity, itemA.ItemID)
		}
	})
	t.Run("rejects source unrelated to its item", func(t *testing.T) {
		h, a, _, itemA, _, _, sourceB := reviewDecisionFixtures(t)
		viewA := mapDataHTTP(t, h, a.WorkstreamID)
		code, _ := commandHTTP(t, h, WorkstreamCommand{
			OperationID: "decision-mismatched-source", Action: "record_decision", WorkstreamID: a.WorkstreamID, ExpectedRevision: viewA.Selected.Revision,
			Decision: DecisionInput{Note: "Incorrect unrelated source", ItemID: itemA.ItemID, SourceID: sourceB},
		})
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("decision with mismatched source=%d want %d", code, http.StatusUnprocessableEntity)
		}
	})
	t.Run("accepts matching item and source", func(t *testing.T) {
		h, a, _, itemA, _, sourceA, _ := reviewDecisionFixtures(t)
		viewA := mapDataHTTP(t, h, a.WorkstreamID)
		code, decision := commandHTTP(t, h, WorkstreamCommand{
			OperationID: "decision-valid-related", Action: "record_decision", WorkstreamID: a.WorkstreamID, ExpectedRevision: viewA.Selected.Revision,
			Decision: DecisionInput{Note: "Correct related decision", ItemID: itemA.ItemID, SourceID: sourceA},
		})
		if code != http.StatusOK || decision.DecisionID == "" {
			t.Fatalf("valid related decision=%d %#v", code, decision)
		}
	})
}

func reviewDecisionFixtures(t *testing.T) (http.Handler, CommandResult, CommandResult, CommandResult, CommandResult, string, string) {
	t.Helper()
	h, _ := newCommandHTTP(t)
	_, a := commandHTTP(t, h, WorkstreamCommand{OperationID: "decision-a", Action: "create_workstream", Workstream: WorkstreamInput{Name: "A", Outcome: "Keep A coherent"}})
	_, b := commandHTTP(t, h, WorkstreamCommand{OperationID: "decision-b", Action: "create_workstream", Workstream: WorkstreamInput{Name: "B", Outcome: "Keep B coherent"}})
	_, itemA := commandHTTP(t, h, WorkstreamCommand{OperationID: "decision-item-a", Action: "create_item", WorkstreamID: a.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "A evidence", SourceURL: "https://example.test/a"}})
	_, itemB := commandHTTP(t, h, WorkstreamCommand{OperationID: "decision-item-b", Action: "create_item", WorkstreamID: b.WorkstreamID, Item: WorkstreamItemInput{Kind: "reference", Title: "B evidence", SourceURL: "https://example.test/b"}})
	viewA := mapDataHTTP(t, h, a.WorkstreamID)
	viewB := mapDataHTTP(t, h, b.WorkstreamID)
	return h, a, b, itemA, itemB, reviewItemByID(t, viewA, itemA.ItemID).SourceID, reviewItemByID(t, viewB, itemB.ItemID).SourceID
}

func TestMapCommands_SourceOnlyDecisionRequiresAttachedContext(t *testing.T) {
	h, a, _, itemA, _, sourceA, _ := reviewDecisionFixtures(t)
	code, _ := commandHTTP(t, h, WorkstreamCommand{OperationID: "detach-decision-source", Action: "detach_item", ItemID: itemA.ItemID, ExpectedRevision: itemA.Revision, Confirm: true})
	if code != http.StatusOK {
		t.Fatalf("detach decision source: %d", code)
	}
	view := mapDataHTTP(t, h, a.WorkstreamID)
	code, _ = commandHTTP(t, h, WorkstreamCommand{OperationID: "decision-detached-source", Action: "record_decision", WorkstreamID: a.WorkstreamID, ExpectedRevision: view.Selected.Revision, Decision: DecisionInput{Note: "No attached source context", SourceID: sourceA}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("source-only decision from detached source: %d", code)
	}
}

func reviewItemByID(t *testing.T, view MapPageView, id string) WorkstreamItemView {
	t.Helper()
	for _, category := range view.Categories {
		for _, item := range category.Items.Rows {
			if item.ID == id {
				return item
			}
		}
	}
	t.Fatalf("item %q not found in map view", id)
	return WorkstreamItemView{}
}
