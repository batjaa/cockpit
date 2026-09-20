package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var mapTmpl = template.Must(template.New("map.tmpl").Funcs(template.FuncMap{
	"formValue": formValue, "mapStatus": mapStatus, "operationID": mapOperationID, "dateTimeLocal": dateTimeLocal,
	"expectedRevision": mapExpectedRevision, "mapPostURL": mapPostURL,
	"slice": func(values ...string) []string { return values },
	"add":   func(a, b int) int { return a + b }, "mul": func(a, b int) int { return a * b },
	"lower": strings.ToLower,
}).Funcs(mapViewFuncs()).ParseFS(tmplFS, "templates/map.tmpl", "templates/map_forms.tmpl", "templates/map_views.tmpl"))

type mapTemplateData struct {
	Title string
	MapPageView
	MapExtras
	Form     mapForm
	Location *time.Location
	Forms    mapFormViews
}

type mapForm struct {
	Action, Message string
	Values, Errors  map[string]string
	Conflict        bool
}

// mapRoutes is registered by the application router. Commands use normal
// HTML forms so Map remains useful without JavaScript.
func (s *server) mapRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /map", s.handleMap)
	protection := http.NewCrossOriginProtection()
	mux.Handle("POST /map/action", protection.Handler(http.HandlerFunc(s.handleMapAction)))
}

func mapQueryFromRequest(r *http.Request) MapQuery {
	q := r.URL.Query()
	limit := func(key string) int {
		n, _ := strconv.Atoi(q.Get(key))
		if n < 1 || n > 50 {
			return 50
		}
		return n
	}
	return MapQuery{
		UnfinishedCursor: q.Get("unfinished_cursor"),
		WorkstreamID:     q.Get("workstream"), ItemID: q.Get("item"), Tab: q.Get("tab"),
		Filter: q.Get("filter"), Search: q.Get("q"), WorkstreamsCursor: q.Get("workstreams_cursor"),
		ItemsCategory: q.Get("category"), ItemsCursor: q.Get("items_cursor"), TimelineCursor: q.Get("timeline_cursor"),
		DetachedCursor: q.Get("detached_cursor"), DecisionsCursor: q.Get("decisions_cursor"), ReasonsCursor: q.Get("reasons_cursor"),
		WorkstreamsLimit: limit("workstreams_limit"), ItemsLimit: limit("items_limit"), TimelineLimit: limit("timeline_limit"),
	}
}

func (s *server) handleMap(w http.ResponseWriter, r *http.Request) {
	view, err := s.workstreamStore().ListMapPage(r.Context(), mapQueryFromRequest(r))
	if err != nil {
		var invalid *ValidationError
		if errors.As(err, &invalid) {
			clean, readErr := s.workstreamStore().ListMapPage(r.Context(), MapQuery{})
			if readErr == nil {
				s.renderMapStatus(w, mapTemplateData{Title: "Map", MapPageView: clean, Form: mapForm{Message: "This view URL is invalid. Choose a supported filter or return to the first page.", Errors: invalid.Fields}}, http.StatusBadRequest)
				return
			}
		}
		s.serverError(w, "read map", err)
		return
	}
	if r.URL.Query().Get("form") == "new" {
		view.Selected = nil
		view.SelectedItem = nil
	}
	s.renderMapStatus(w, mapTemplateData{Title: "Map", MapPageView: view, MapExtras: s.prepareMapExtras(r.Context(), view)}, http.StatusOK)
}

func (s *server) handleMapAction(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, mapRequestLimit)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	cmd := mapCommandFromForm(r.Form)
	if err := normalizeMapTimes(&cmd, s.workstreamStore().location); err != nil {
		mapFormError(w, r, s, cmd, err)
		return
	}
	result, err := s.workstreamStore().Execute(r.Context(), cmd)
	if err == nil {
		workstreamID := result.WorkstreamID
		if workstreamID == "" {
			workstreamID = cmd.WorkstreamID
		}
		params := mapActionQuery(r.Form, mapQueryFromRequest(r))
		params.Set("workstream", workstreamID)
		if result.ItemID != "" {
			params.Set("item", result.ItemID)
		}
		http.Redirect(w, r, "/map?"+params.Encode(), http.StatusSeeOther)
		return
	}
	mapFormError(w, r, s, cmd, err)
}

func mapFormError(w http.ResponseWriter, r *http.Request, s *server, cmd WorkstreamCommand, err error) {
	formQuery := mapQueryFromRequest(r)
	formQuery = mapQueryFromValues(r.Form, formQuery)
	if formQuery.WorkstreamID == "" {
		formQuery.WorkstreamID = cmd.WorkstreamID
	}
	if formQuery.ItemID == "" {
		formQuery.ItemID = cmd.ItemID
	}
	view, readErr := s.workstreamStore().ListMapPage(r.Context(), formQuery)
	if readErr != nil {
		s.serverError(w, "reload map form", readErr)
		return
	}
	form := mapForm{Action: cmd.Action, Values: copyForm(r.Form), Errors: map[string]string{}}
	var validation *ValidationError
	var conflict *ConflictError
	var missing *NotFoundError
	status := http.StatusInternalServerError
	switch {
	case errors.As(err, &validation):
		status = http.StatusUnprocessableEntity
		form.Errors = validation.Fields
		form.Message = "Review the marked fields and save again."
	case errors.As(err, &conflict):
		status = http.StatusConflict
		form.Conflict, form.Message = true, "This record changed elsewhere. Your draft is preserved; reload before saving again."
	case errors.As(err, &missing):
		status = http.StatusNotFound
		form.Message = "This record is no longer available. Your draft is preserved; return to the workstream list."
	default:
		form.Message = "Cockpit could not save this change. Try again."
	}
	s.renderMapStatus(w, mapTemplateData{Title: "Map", MapPageView: view, MapExtras: s.prepareMapExtras(r.Context(), view), Form: form}, status)
}

func normalizeMapTimes(cmd *WorkstreamCommand, location *time.Location) error {
	fields := map[string]*string{"follow_up_at": &cmd.Item.FollowUpAt, "last_contact_at": &cmd.Item.LastContactAt, "observed_at": &cmd.Item.ObservedAt, "review_by": &cmd.Item.ReviewBy}
	problems := map[string]string{}
	for name, value := range fields {
		if *value == "" {
			continue
		}
		if instant, err := time.Parse(time.RFC3339, *value); err == nil {
			*value = instant.UTC().Format(time.RFC3339)
			continue
		}
		parsed, err := time.ParseInLocation("2006-01-02T15:04", *value, location)
		if err != nil || parsed.In(location).Format("2006-01-02T15:04") != *value {
			problems[name] = "Enter a local date and time."
			continue
		}
		*value = parsed.UTC().Format(time.RFC3339)
	}
	if len(problems) > 0 {
		return &ValidationError{Fields: problems}
	}
	return nil
}

func (s *server) renderMap(w http.ResponseWriter, data mapTemplateData) {
	s.renderMapStatus(w, data, http.StatusOK)
}

func (s *server) renderMapStatus(w http.ResponseWriter, data mapTemplateData, status int) {
	data.Location = s.workstreamStore().location
	data.Forms = prepareMapForms(data)
	var body bytes.Buffer
	if err := mapTmpl.ExecuteTemplate(&body, "layout", data); err != nil {
		s.serverError(w, "render map", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = body.WriteTo(w)
}

func newMapOperationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("random operation id: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func copyForm(v url.Values) map[string]string {
	out := make(map[string]string, len(v))
	for key, val := range v {
		if len(val) > 0 {
			out[key] = val[0]
		}
	}
	return out
}

func formBool(v url.Values, key string) bool {
	return v.Get(key) == "on" || v.Get(key) == "true" || v.Get(key) == "1"
}
func formInt(v url.Values, key string) int64 { n, _ := strconv.ParseInt(v.Get(key), 10, 64); return n }

func mapCommandFromForm(v url.Values) WorkstreamCommand {
	return WorkstreamCommand{
		OperationID: v.Get("operation_id"), Action: v.Get("action"), WorkstreamID: v.Get("workstream_id"), ItemID: v.Get("item_id"), DestinationWorkstreamID: v.Get("destination_workstream_id"), ExpectedRevision: formInt(v, "expected_revision"), Confirm: formBool(v, "confirm"), OverrideReason: strings.TrimSpace(v.Get("override_reason")),
		Workstream: WorkstreamInput{Name: v.Get("name"), Outcome: v.Get("outcome"), Owner: v.Get("owner"), Sponsor: v.Get("sponsor"), TargetDate: v.Get("target_date"), Notes: v.Get("notes")},
		Item:       WorkstreamItemInput{Kind: v.Get("kind"), Title: v.Get("title"), Description: v.Get("description"), SourceURL: v.Get("source_url"), SourceLabel: v.Get("source_label"), SourceKind: v.Get("source_kind"), PRID: formInt(v, "pr_id"), DueDate: v.Get("due_date"), TrackingState: v.Get("tracking_state"), BlockerReason: v.Get("blocker_reason"), Counterpart: v.Get("counterpart"), AskStatus: v.Get("ask_status"), FollowUpAt: v.Get("follow_up_at"), LastContactAt: v.Get("last_contact_at"), Value: v.Get("value"), Unit: v.Get("unit"), ObservedAt: v.Get("observed_at"), Assessment: v.Get("assessment"), ReviewBy: v.Get("review_by")},
		Decision:   DecisionInput{Note: v.Get("decision_note"), ItemID: v.Get("decision_item_id"), SourceID: v.Get("decision_source_id")},
	}
}

func formValue(values map[string]string, key, fallback string) string {
	if v, ok := values[key]; ok {
		return v
	}
	return fallback
}

func mapOperationID(form mapForm, action string, itemIDs ...string) string {
	if form.Action == action && form.Values["operation_id"] != "" && (len(itemIDs) == 0 || form.Values["item_id"] == itemIDs[0]) {
		return form.Values["operation_id"]
	}
	return newMapOperationID()
}

func mapExpectedRevision(form mapForm, action string, revision int64, itemIDs ...string) int64 {
	if form.Action == action && (len(itemIDs) == 0 || form.Values["item_id"] == itemIDs[0]) {
		if original, err := strconv.ParseInt(form.Values["expected_revision"], 10, 64); err == nil {
			return original
		}
	}
	return revision
}

func mapPostURL(q MapQuery) string {
	return strings.Replace(mapNav(q), "/map?", "/map/action?", 1)
}

func mapQueryFromValues(values url.Values, q MapQuery) MapQuery {
	if values.Has("tab") {
		q.Tab = values.Get("tab")
	}
	if values.Has("q") {
		q.Search = values.Get("q")
	}
	if values.Has("filter") {
		q.Filter = values.Get("filter")
	}
	return q
}

func mapActionQuery(values url.Values, q MapQuery) url.Values {
	q = mapQueryFromValues(values, q)
	parsed, _ := url.Parse(mapNav(q))
	return parsed.Query()
}

func dateTimeLocal(location *time.Location, value *time.Time) string {
	if value == nil {
		return ""
	}
	if location == nil {
		location = time.Local
	}
	return value.In(location).Format("2006-01-02T15:04")
}
func mapStatus(attention string) string {
	switch attention {
	case "", "no-recorded-attention":
		return "[·] No recorded attention items"
	case "waiting":
		return "[·] Waiting"
	case "needs-me":
		return "[·] Needs me"
	default:
		return fmt.Sprintf("[!] %s", attention)
	}
}
