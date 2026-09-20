package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMap_EmptyStateRendersServerSide(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "map.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db, workstreams: NewWorkstreamStore(db, nil, nil)}
	mux := http.NewServeMux()
	s.mapRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{"Start with an outcome", "Not available in this module", "Map shortcuts"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(w.Body.String(), `name="operation_id" value="create-workstream"`) {
		t.Fatal("Map must render a fresh operation identity, not a shared literal")
	}
}
