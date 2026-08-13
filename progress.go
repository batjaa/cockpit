package main

import (
	"sync"
	"time"
)

type prProgressState string

const (
	prQueued    prProgressState = "queued"
	prReviewing prProgressState = "reviewing"
	prDone      prProgressState = "done"
	prSkipped   prProgressState = "skipped"
	prFailed    prProgressState = "failed"
)

// PRProgress is one line item in the live run view.
type PRProgress struct {
	Owner     string          `json:"owner"`
	Repo      string          `json:"repo"`
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	URL       string          `json:"url"`
	State     prProgressState `json:"state"`
	Findings  int             `json:"findings"`
	Error     string          `json:"error,omitempty"`
	StartedAt *time.Time      `json:"started_at,omitempty"`
}

// RunProgress tracks per-PR state for the currently executing run. The
// reviewer goroutine writes; HTTP handlers read snapshots. All methods
// are safe for concurrent use.
type RunProgress struct {
	mu    sync.Mutex
	items []PRProgress
	index map[string]int // url -> items index
}

func NewRunProgress() *RunProgress {
	return &RunProgress{index: map[string]int{}}
}

// SetQueued initializes the item list from the discovered PRs. Called
// once per run, right after gh returns.
func (rp *RunProgress) SetQueued(prs []GHPR) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.items = rp.items[:0]
	clear(rp.index)
	for _, p := range prs {
		owner, repo, number, err := ParseRepo(p.URL)
		if err != nil {
			continue
		}
		rp.index[p.URL] = len(rp.items)
		rp.items = append(rp.items, PRProgress{
			Owner: owner, Repo: repo, Number: number,
			Title: p.Title, URL: p.URL, State: prQueued,
		})
	}
}

func (rp *RunProgress) MarkReviewing(url string) {
	now := time.Now()
	rp.set(url, func(it *PRProgress) {
		it.State = prReviewing
		it.StartedAt = &now
	})
}

func (rp *RunProgress) MarkDone(url string, findings int) {
	rp.set(url, func(it *PRProgress) {
		it.State = prDone
		it.Findings = findings
	})
}

func (rp *RunProgress) MarkSkipped(url string) {
	rp.set(url, func(it *PRProgress) { it.State = prSkipped })
}

func (rp *RunProgress) MarkFailed(url, message string) {
	rp.set(url, func(it *PRProgress) {
		it.State = prFailed
		it.Error = message
	})
}

func (rp *RunProgress) set(url string, fn func(*PRProgress)) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if i, ok := rp.index[url]; ok {
		fn(&rp.items[i])
	}
}

// Snapshot returns a copy of the current items, safe to serialize while
// the run keeps mutating state.
func (rp *RunProgress) Snapshot() []PRProgress {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	out := make([]PRProgress, len(rp.items))
	copy(out, rp.items)
	return out
}
