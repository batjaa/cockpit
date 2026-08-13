package main

import (
	"encoding/json"
	"testing"
)

func TestPathExcluded(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{"basename glob", []string{"*.pb.go"}, "svc/api/thing.pb.go", true},
		{"basename glob no match", []string{"*.pb.go"}, "svc/api/thing.go", false},
		{"exact basename anywhere", []string{"feature_flags.go"}, "co/svc/internal/feature_flags.go", true},
		{"exact basename at root", []string{"feature_flags.go"}, "feature_flags.go", true},
		{"basename must be full segment", []string{"feature_flags.go"}, "svc/notfeature_flags.go", false},
		{"multi-segment suffix", []string{"internal/feature_flags.go"}, "co/svc/internal/feature_flags.go", true},
		{"multi-segment suffix no match", []string{"internal/feature_flags.go"}, "co/svc/other/feature_flags.go", false},
		{"dir pattern mid-path", []string{"vendor/"}, "svc/vendor/dep/x.go", true},
		{"dir pattern at root", []string{"vendor/"}, "vendor/dep/x.go", true},
		{"dir pattern skips basename", []string{"vendor/"}, "svc/vendor", false},
		{"dir pattern glob", []string{"__snapshots__/"}, "web/src/__snapshots__/a.snap", true},
		{"lockfile", []string{"go.sum"}, "go.sum", true},
		{"generated prefix glob", []string{"zz_generated*.go"}, "api/v1/zz_generated.deepcopy.go", true},
		{"empty pattern ignored", []string{""}, "anything.go", false},
		{"no patterns", nil, "anything.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathExcluded(tc.patterns, tc.path); got != tc.want {
				t.Errorf("pathExcluded(%v, %q) = %v, want %v", tc.patterns, tc.path, got, tc.want)
			}
		})
	}
}

func TestDropExcludedFindings(t *testing.T) {
	findings := []StructuredReviewFinding{
		{ID: "B1", Path: "svc/handler.go"},
		{ID: "M1", Path: "svc/internal/feature_flags.go"},
		{ID: "m1", Path: "svc/api/thing.pb.go"},
		{ID: "n1", Path: "svc/handler_test.go"},
	}
	got := dropExcludedFindings(findings, []string{"feature_flags.go", "*.pb.go"}, "https://github.com/o/r/pull/1")
	if len(got) != 2 || got[0].ID != "B1" || got[1].ID != "n1" {
		t.Fatalf("dropExcludedFindings kept %+v, want B1 and n1", got)
	}

	// Empty pattern list is a no-op passthrough.
	got = dropExcludedFindings(findings[:1], nil, "")
	if len(got) != 1 {
		t.Fatalf("nil patterns dropped findings: %+v", got)
	}
}

func TestExcludePathsDefaultBackfill(t *testing.T) {
	// Absent field → defaults.
	var absent Config
	if err := json.Unmarshal([]byte(`{"claude":{}}`), &absent); err != nil {
		t.Fatal(err)
	}
	absent.applyDefaults()
	if len(absent.Review.ExcludePaths) == 0 {
		t.Error("absent review block should backfill default exclude paths")
	}

	// Explicit empty list → exclusion disabled, not backfilled.
	var off Config
	if err := json.Unmarshal([]byte(`{"review":{"exclude_paths":[]}}`), &off); err != nil {
		t.Fatal(err)
	}
	off.applyDefaults()
	if off.Review.ExcludePaths == nil || len(off.Review.ExcludePaths) != 0 {
		t.Errorf("explicit empty exclude_paths should stay empty, got %v", off.Review.ExcludePaths)
	}
}
