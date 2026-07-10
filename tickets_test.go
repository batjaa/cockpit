package main

import (
	"reflect"
	"testing"
)

func TestExtractTickets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "no match",
			in:   "just some prose with no references at all",
			want: nil,
		},
		{
			name: "single jira key verbatim",
			in:   "Fixes PLAT-422 in the scheduler.",
			want: []string{"PLAT-422"},
		},
		{
			name: "multiple jira keys dedup",
			in:   "PLAT-422 and ENG-7 then PLAT-422 again",
			want: []string{"PLAT-422", "ENG-7"},
		},
		{
			name: "jira with digits in prefix",
			in:   "see S3-100 for the bucket fix",
			want: []string{"S3-100"},
		},
		{
			name: "github pull url with scheme",
			in:   "PR https://github.com/batjaa/cockpit/pull/12 is up",
			want: []string{"batjaa/cockpit#12"},
		},
		{
			name: "github issues url without scheme",
			in:   "tracked in github.com/acme/widget/issues/345",
			want: []string{"acme/widget#345"},
		},
		{
			name: "short cross-repo ref",
			in:   "depends on octo/repo#99 landing first",
			want: []string{"octo/repo#99"},
		},
		{
			name: "dedup across url and short ref forms",
			in:   "see https://github.com/batjaa/cockpit/pull/12 aka batjaa/cockpit#12",
			want: []string{"batjaa/cockpit#12"},
		},
		{
			name: "dedup short ref then url form (order by first appearance)",
			in:   "batjaa/cockpit#12 — full link github.com/batjaa/cockpit/pull/12",
			want: []string{"batjaa/cockpit#12"},
		},
		{
			name: "interleaved mixed ordering preserved",
			in:   "First PLAT-1, then github.com/o/r/pull/2, then ENG-3, then a/b#4, last PLAT-1",
			want: []string{"PLAT-1", "o/r#2", "ENG-3", "a/b#4"},
		},
		{
			name: "exclude UTF-8",
			in:   "encoded as UTF-8 throughout",
			want: nil,
		},
		{
			name: "exclude SHA-256",
			in:   "hashed with SHA-256",
			want: nil,
		},
		{
			name: "exclude RFC-7231",
			in:   "per RFC-7231 semantics",
			want: nil,
		},
		{
			name: "exclude ISO and GPT and HTTP",
			in:   "ISO-8601 dates, GPT-4 model, HTTP-2 transport",
			want: nil,
		},
		{
			name: "CVE is kept",
			in:   "patched CVE-2024-1234 today",
			want: []string{"CVE-2024-1234"},
		},
		{
			name: "excluded prefix alongside real ticket",
			in:   "UTF-8 file referenced in PLAT-9",
			want: []string{"PLAT-9"},
		},
		{
			name: "lowercase jira does not match",
			in:   "the plat-422 thing",
			want: nil,
		},
		{
			name: "leading-char key XPLAT-422 is a valid-looking key",
			in:   "XPLAT-422 shipped",
			want: []string{"XPLAT-422"},
		},
		{
			name: "everything interleaved with dedup and exclusion",
			in: "Start ENG-10, link https://github.com/batjaa/cockpit/pull/12, " +
				"hash SHA-1, ref batjaa/cockpit#12, CVE-2024-1234, " +
				"and ENG-10 once more, plus o/r#7.",
			want: []string{
				"ENG-10",
				"batjaa/cockpit#12",
				"CVE-2024-1234",
				"o/r#7",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractTickets(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractTickets(%q)\n got = %#v\nwant = %#v", tt.in, got, tt.want)
			}
		})
	}
}
