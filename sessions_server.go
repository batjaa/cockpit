package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var sessionsTmpl = template.Must(template.New("layout.tmpl").
	Funcs(template.FuncMap{"queryWith": queryWith}).
	ParseFS(tmplFS, "templates/layout.tmpl", "templates/sessions.tmpl"))

// queryWith rebuilds the /sessions query string from the active filter with
// one parameter overridden (empty value drops it). Used by the filter-bar
// chips so toggling agent= keeps machine=, ticket=, q=, archived= intact.
func queryWith(f SessionFilter, key, val string) template.URL {
	v := url.Values{}
	set := func(k, cur string) {
		if k == key {
			cur = val
		}
		if cur != "" {
			v.Set(k, cur)
		}
	}
	set("agent", f.Agent)
	set("machine", f.Machine)
	set("ticket", f.Ticket)
	set("repo", f.Repo)
	set("branch", f.Branch)
	set("q", f.Search)
	archived := ""
	if f.IncludeArchived {
		archived = "1"
	}
	set("archived", archived)
	all := ""
	if f.ShowAll {
		all = "1"
	}
	set("all", all)
	return template.URL(v.Encode())
}

// scanSessionsFn is the session scanner the POST /sessions/scan handler
// invokes. It is nil until main wiring (phase 4) sets it to ScanSessions —
// ScanSessions lives in sessions_scan.go, which a parallel agent owns and
// which may not exist in the tree yet. Referencing it directly here would
// break `go build`, so the handler calls through this variable instead and
// returns 503 "scanner not wired" while it is nil. Tests inject a fake via
// this variable (saving and restoring it with t.Cleanup).
var scanSessionsFn func(ctx context.Context, db *sql.DB, cfg Config) error

// sessionScanning guards the single in-flight scan: POST /sessions/scan
// flips it true and returns 409 while another scan is running. scanWG tracks
// the detached scan goroutine so shutdown can wait on it, mirroring runWG in
// server.go.
var (
	sessionScanning atomic.Bool
	scanWG          sync.WaitGroup
)

// agentChip maps an agent to its filter-bar / card badge tint. Tints are
// zinc-compatible: orange (claude), sky (codex), purple (cursor).
type agentChip struct {
	Key, Label, Classes string
}

var agentChips = map[string]agentChip{
	"claude": {Key: "claude", Label: "claude", Classes: "bg-orange-100 text-orange-700"},
	"codex":  {Key: "codex", Label: "codex", Classes: "bg-sky-100 text-sky-700"},
	"cursor": {Key: "cursor", Label: "cursor", Classes: "bg-purple-100 text-purple-700"},
}

func agentChipFor(agent string) agentChip {
	if c, ok := agentChips[agent]; ok {
		return c
	}
	return agentChip{Key: agent, Label: agent, Classes: "bg-zinc-100 text-zinc-600"}
}

// sessionCardView is one rendered session card.
type sessionCardView struct {
	SessionRow
	AgentClasses string
	ProjectShort string
	Repo         string // basename of project_dir; clickable filter chip
	Age          string
	IdleDays     int    // whole days idle, for the stale "idle Nd" badge
	IsCursor     bool   // cursor: show "open in Cursor" instead of copy-resume
	ResumeCmd    string // copyable command (empty for cursor)
	Haystack     string // lowercase concat for the client-side fuzzy filter
}

// sessionBucket groups rows under a recency heading so the list reads as
// a timeline instead of an 80-row wall.
type sessionBucket struct {
	Label string
	Open  bool // recent buckets render expanded; This month/Older collapsed
	Cards []sessionCardView
}

// bucketFor maps a last-active time to its recency bucket label.
func bucketFor(now, t time.Time) string {
	y, m, d := now.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch {
	case t.After(midnight) || t.Equal(midnight):
		return "Today"
	case t.After(midnight.AddDate(0, 0, -1)):
		return "Yesterday"
	case t.After(midnight.AddDate(0, 0, -6)):
		return "This week"
	case t.After(midnight.AddDate(0, 0, -29)):
		return "This month"
	default:
		return "Older"
	}
}

// trivialMaxMessages: sessions with fewer messages are one-shot questions
// and abandoned launches; hidden by default behind a toggle.
const trivialMaxMessages = 3

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := SessionFilter{
		Agent:           q.Get("agent"),
		Machine:         q.Get("machine"),
		Ticket:          q.Get("ticket"),
		Search:          q.Get("q"),
		Repo:            q.Get("repo"),
		Branch:          q.Get("branch"),
		IncludeArchived: q.Get("archived") == "1",
		ShowAll:         q.Get("all") == "1",
	}
	showAll := f.ShowAll

	rows, err := ListSessions(r.Context(), s.db, f)
	if err != nil {
		s.serverError(w, "list sessions", err)
		return
	}

	// Build the distinct agent and machine sets for the filter bar. To keep
	// the chips stable regardless of the active filter, derive them from the
	// full row set (same archived scope) rather than the already-filtered
	// rows.
	full := rows
	if f.Agent != "" || f.Machine != "" || f.Ticket != "" || f.Search != "" || f.Repo != "" || f.Branch != "" {
		full, err = ListSessions(r.Context(), s.db, SessionFilter{IncludeArchived: f.IncludeArchived})
		if err != nil {
			s.serverError(w, "list sessions (facets)", err)
			return
		}
	}
	agentSet := map[string]bool{}
	machineSet := map[string]bool{}
	for _, row := range full {
		agentSet[row.Agent] = true
		if row.Machine != "" && row.Machine != "local" {
			machineSet[row.Machine] = true
		}
	}
	agents := make([]agentChip, 0, len(agentSet))
	for a := range agentSet {
		agents = append(agents, agentChipFor(a))
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Key < agents[j].Key })
	machines := make([]string, 0, len(machineSet))
	for m := range machineSet {
		machines = append(machines, m)
	}
	sort.Strings(machines)

	now := time.Now()
	var stale []sessionCardView
	var buckets []sessionBucket
	trivialHidden := 0
	for _, row := range rows {
		if !showAll && row.MessageCount < trivialMaxMessages && !row.Stale {
			trivialHidden++
			continue
		}
		repo := ""
		if row.ProjectDir != "" {
			parts := strings.Split(strings.TrimRight(row.ProjectDir, "/"), "/")
			repo = parts[len(parts)-1]
		}
		card := sessionCardView{
			SessionRow:   row,
			AgentClasses: agentChipFor(row.Agent).Classes,
			ProjectShort: shortPath(row.ProjectDir),
			Repo:         repo,
			Age:          humanizeAge(now.Sub(row.LastActive)),
			IsCursor:     row.Agent == "cursor",
			Haystack: strings.ToLower(strings.Join(append([]string{
				row.Title, row.Subtitle, repo, row.Branch, row.Machine, row.Agent,
			}, row.Tickets...), " ")),
		}
		if !card.IsCursor {
			card.ResumeCmd = row.ResumeCmd
		}
		if row.Stale {
			card.IdleDays = int(now.Sub(row.LastActive).Hours() / 24)
			stale = append(stale, card)
		}
		label := bucketFor(now, row.LastActive)
		if len(buckets) == 0 || buckets[len(buckets)-1].Label != label {
			open := label == "Today" || label == "Yesterday" || label == "This week"
			buckets = append(buckets, sessionBucket{Label: label, Open: open})
		}
		buckets[len(buckets)-1].Cards = append(buckets[len(buckets)-1].Cards, card)
	}

	shown := 0
	for _, b := range buckets {
		shown += len(b.Cards)
	}

	s.render(w, sessionsTmpl, map[string]any{
		"Title":           "Sessions",
		"Count":           shown,
		"Buckets":         buckets,
		"Stale":           stale,
		"TrivialHidden":   trivialHidden,
		"ShowAll":         showAll,
		"Agents":          agents,
		"Machines":        machines,
		"Filter":          f,
		"Scanning":        sessionScanning.Load(),
		"IncludeArchived": f.IncludeArchived,
	})
}

// handleSessionScan kicks a session scan in a detached goroutine and returns
// 202. It mirrors tryStartRun in server.go: a single-flight atomic.Bool
// guards it (409 while a scan is in flight), the goroutine inherits baseCtx
// so shutdown cancels it, and scanWG lets shutdown wait for it to finish.
// Returns 503 when the scanner isn't wired (scanSessionsFn nil).
func (s *server) handleSessionScan(w http.ResponseWriter, r *http.Request) {
	if scanSessionsFn == nil {
		http.Error(w, "session scanner not wired", http.StatusServiceUnavailable)
		return
	}
	if !s.tryStartSessionScan() {
		http.Error(w, "a scan is already in progress", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// tryStartSessionScan launches a single-flight session scan goroutine.
// Shared by the POST handler and the piggyback after discovery runs, so
// the two can't overlap (the cursor scan copies a multi-GB sqlite file —
// concurrent scans are pure waste).
func (s *server) tryStartSessionScan() bool {
	fn := scanSessionsFn
	if fn == nil {
		return false
	}
	if !sessionScanning.CompareAndSwap(false, true) {
		return false
	}
	scanWG.Add(1)
	go func() {
		defer scanWG.Done()
		defer sessionScanning.Store(false)
		if err := fn(s.baseCtx, s.db, s.cfg); err != nil {
			slog.Error("scan sessions", "err", err)
		}
	}()
	return true
}

func (s *server) handleSessionScanStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Scanning bool `json:"scanning"`
	}{Scanning: sessionScanning.Load()})
}

func (s *server) handleSessionArchive(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Archived bool `json:"archived"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := SetSessionArchived(r.Context(), s.db, id, body.Archived); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		s.serverError(w, "set session archived", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
