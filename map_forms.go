package main

type mapField struct {
	Name, Label, Value, Type, ID, Error string
	MaxLength                           int
	Required, ReadOnly                  bool
	Options                             []string
}

type mapFormViews struct {
	CreateWorkstream, EditWorkstream, ItemBase, ItemTask, ItemAsk, ItemSignal, EditItem, Completion []mapField
	CreateKind                                                                                      string
}

// Form values belong to the submitted action, never every form on the page.
// Prepared fields share escaping, bounds, labels and validation presentation.
func prepareMapForms(data mapTemplateData) mapFormViews {
	field := func(action, name, label, kind, value string, limit int, required bool, options ...string) mapField {
		f := mapField{Name: name, Label: label, Type: kind, Value: value, ID: action + "-" + name, MaxLength: limit, Required: required, Options: options}
		if data.Form.Action == action {
			if v, ok := data.Form.Values[name]; ok {
				f.Value = v
			}
			f.Error = data.Form.Errors[name]
			if name == "notes" && action == "complete_workstream" {
				f.Error = data.Form.Errors["outcome_note"]
			}
		}
		return f
	}
	workstream := func(action string, w Workstream) []mapField {
		return []mapField{
			field(action, "name", "Name", "text", w.Name, 200, true), field(action, "outcome", "Desired outcome", "textarea", w.Outcome, 4000, true),
			field(action, "owner", "Owner", "text", w.Owner, 200, false), field(action, "sponsor", "Sponsor", "text", w.Sponsor, 200, false),
			field(action, "target_date", "Target date", "date", w.TargetDate, 0, false), field(action, "notes", "Notes", "textarea", w.Notes, 32000, false),
		}
	}
	base := func(action string, it WorkstreamItem) []mapField {
		return []mapField{
			field(action, "title", "Title", "text", it.Title, 200, true), field(action, "description", "Description", "textarea", it.Description, 32000, false),
		}
	}
	kind := func(action, k string, it WorkstreamItem) []mapField {
		switch k {
		case "task":
			return []mapField{field(action, "tracking_state", "Task status", "select", it.TrackingState, 0, false, "open", "in_progress", "blocked", "done", "cancelled"), field(action, "due_date", "Due date", "date", it.DueDate, 0, false), field(action, "blocker_reason", "Blocker reason", "textarea", it.BlockerReason, 4000, false)}
		case "ask":
			return []mapField{field(action, "counterpart", "Counterpart", "text", it.Counterpart, 200, false), field(action, "ask_status", "Ask status", "select", it.AskStatus, 0, false, "open", "waiting", "resolved", "cancelled"), field(action, "follow_up_at", "Follow up (local time)", "datetime-local", dateTimeLocal(data.Location, it.FollowUpAt), 0, false), field(action, "last_contact_at", "Last contact (local time)", "datetime-local", dateTimeLocal(data.Location, it.LastContactAt), 0, false)}
		case "signal":
			return []mapField{field(action, "value", "Value", "text", it.Value, 200, false), field(action, "unit", "Unit", "text", it.Unit, 200, false), field(action, "observed_at", "Observed at (local time)", "datetime-local", dateTimeLocal(data.Location, it.ObservedAt), 0, false), field(action, "assessment", "Manual assessment", "select", it.Assessment, 0, false, "unknown", "normal", "concerning"), field(action, "review_by", "Review by (local time)", "datetime-local", dateTimeLocal(data.Location, it.ReviewBy), 0, false)}
		}
		return nil
	}
	source := func(action string, src *Source) []mapField {
		var raw, label, sourceKind string
		if src != nil {
			raw, label, sourceKind = src.URL, src.Label, src.Kind
		}
		fields := []mapField{field(action, "source_url", "Source URL (HTTP or HTTPS)", "url", raw, 4096, false), field(action, "source_label", "Source label (defaults to item title)", "text", label, 200, false), field(action, "source_kind", "Source kind (for example RFC, ticket or meeting)", "text", sourceKind, 200, false)}
		// Source identity/provenance is shared. Edit the local title/description
		// without silently relabelling all other workstreams using this source.
		if src != nil {
			for i := range fields {
				fields[i].ReadOnly = true
			}
		}
		return fields
	}
	f := mapFormViews{CreateWorkstream: workstream("create_workstream", Workstream{Owner: "me"}), CreateKind: "task"}
	f.ItemBase = append(base("create_item", WorkstreamItem{}), source("create_item", nil)...)
	f.ItemTask = kind("create_item", "task", WorkstreamItem{TrackingState: "open"})
	f.ItemAsk = kind("create_item", "ask", WorkstreamItem{AskStatus: "open"})
	f.ItemSignal = kind("create_item", "signal", WorkstreamItem{Assessment: "unknown"})
	if data.Form.Action == "create_item" && one(data.Form.Values["kind"], "task", "reference", "ask", "signal") {
		f.CreateKind = data.Form.Values["kind"]
	}
	if data.Selected != nil {
		f.EditWorkstream = workstream("edit_workstream", data.Selected.Workstream)
	}
	if data.SelectedItem != nil {
		it := data.SelectedItem
		f.EditItem = append(base("edit_item", it.WorkstreamItem), kind("edit_item", it.Kind, it.WorkstreamItem)...)
		f.EditItem = append(f.EditItem, source("edit_item", it.Source)...)
	}
	f.Completion = []mapField{field("complete_workstream", "notes", "Outcome note", "textarea", "", 32000, true), field("complete_workstream", "override_reason", "Reason for completing with unfinished items", "textarea", "", 32000, false)}
	return f
}
