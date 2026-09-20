package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const mapRequestLimit = 128 << 10

// workstreamStore is initialized while registering routes, before the mux is
// published. Tests can supply their controlled clock and temporary database.
func (s *server) workstreamStore() *WorkstreamStore {
	if s.workstreams == nil {
		s.workstreams = NewWorkstreamStore(s.db, time.Now, time.Local)
	}
	return s.workstreams
}

func (s *server) workstreamRoutes(mux *http.ServeMux) {
	s.workstreamStore()
	mux.HandleFunc("GET /map/data", s.handleMapData)
	mux.HandleFunc("GET /map/prs", s.handleMapPRs)
	protection := http.NewCrossOriginProtection()
	mux.Handle("POST /map/commands", protection.Handler(http.HandlerFunc(s.handleMapCommand)))
	mux.Handle("POST /map/mirror/retry", protection.Handler(http.HandlerFunc(s.handleMapMirrorRetry)))
}

func (s *server) handleMapMirrorRetry(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, mapRequestLimit)
	input := struct {
		EntityType string `json:"entity_type"`
		EntityID   string `json:"entity_id"`
		Workstream string `json:"workstream"`
	}{}
	wantsJSON := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	if wantsJSON {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			mapDecodeError(w, err)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			mapDecodeError(w, err)
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			mapDecodeError(w, err)
			return
		}
		input.EntityType, input.EntityID, input.Workstream = r.Form.Get("entity_type"), r.Form.Get("entity_id"), r.Form.Get("workstream")
	}
	store := s.workstreamStore()
	if !store.mirrorEnabled {
		mapJSON(w, http.StatusConflict, map[string]string{"error": "The vault mirror is disabled in configuration."})
		return
	}
	key := MirrorKey{EntityType: input.EntityType, EntityID: input.EntityID}
	if err := store.RetryMirror(r.Context(), key, store.now()); err != nil {
		mapHTTPError(w, r, err)
		return
	}
	if s.mirror != nil {
		s.mirror.Wake()
	}
	if wantsJSON {
		mapJSON(w, http.StatusOK, map[string]string{"status": "pending", "message": "Retry scheduled. A conflict still requires manual reconciliation."})
		return
	}
	q := url.Values{"workstream": {input.Workstream}, "tab": {"obsidian"}}
	http.Redirect(w, r, "/map?"+q.Encode(), http.StatusSeeOther)
}

func (s *server) handleMapData(w http.ResponseWriter, r *http.Request) {
	q := mapQueryFromRequest(r)
	if q.WorkstreamID == "" {
		q.WorkstreamID = r.URL.Query().Get("workstream_id")
	}
	view, err := s.workstreamStore().ListMapPage(r.Context(), q)
	if err != nil {
		mapHTTPError(w, r, err)
		return
	}
	mapJSON(w, http.StatusOK, view)
}

func (s *server) handleMapPRs(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	limit, _ := strconv.Atoi(values.Get("limit"))
	view, err := s.workstreamStore().SearchCachedPRs(r.Context(), PRSearchQuery{
		Search: values.Get("q"), Cursor: values.Get("cursor"), Limit: limit,
	})
	if err != nil {
		mapHTTPError(w, r, err)
		return
	}
	mapJSON(w, http.StatusOK, view)
}

func (s *server) handleMapCommand(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, mapRequestLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var cmd WorkstreamCommand
	if err := decoder.Decode(&cmd); err != nil {
		mapDecodeError(w, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		mapDecodeError(w, err)
		return
	}
	started := time.Now()
	result, err := s.workstreamStore().Execute(r.Context(), cmd)
	if err != nil {
		mapHTTPError(w, r, err)
		return
	}
	slog.Debug("map command saved", "action", cmd.Action, "entity_id", result.WorkstreamID, "duration_ms", time.Since(started).Milliseconds())
	mapJSON(w, http.StatusOK, result)
}

func mapDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		mapJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "This submission is too large. Shorten the fields and try again."})
		return
	}
	mapJSON(w, http.StatusBadRequest, map[string]string{"error": "Submit one valid JSON command with supported fields."})
}

func mapHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	var validation *ValidationError
	var conflict *ConflictError
	var missing *NotFoundError
	switch {
	case errors.As(err, &validation):
		mapJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "Review the highlighted fields.", "fields": validation.Fields})
	case errors.As(err, &conflict):
		mapJSON(w, http.StatusConflict, map[string]any{"error": "This record changed. Reload it and review your changes before saving.", "entity_id": conflict.EntityID, "actual_revision": conflict.ActualRevision})
	case errors.As(err, &missing):
		mapJSON(w, http.StatusNotFound, map[string]string{"error": "This record could not be found."})
	default:
		// The route pattern excludes free-text query/form data. Record the
		// unexpected failure once, without echoing submitted content.
		slog.Error("map request failed", "route", r.Pattern, "error_type", "local_storage")
		mapJSON(w, http.StatusInternalServerError, map[string]string{"error": "Cockpit could not complete this request. Your saved data is unchanged; try again."})
	}
}

func mapJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
