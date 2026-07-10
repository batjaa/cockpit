package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// runKind distinguishes the two things the run worker executes.
type runKind int

const (
	runDiscover runKind = iota // Discover() — batch search + review
	runReview                  // ReviewOne() — one PR by URL
)

// runJob is one unit of work for the run worker. Discover jobs coalesce
// (at most one active-or-pending); review jobs dedupe by URL.
type runJob struct {
	kind    runKind
	trigger string // discover: "launch"/"schedule"/"manual" (log label)
	url     string // review only; normalized PR URL
}

// progressItem renders a queued job as a placeholder row for /run-status so
// the dashboard shows pending work, not just the active run.
func (j runJob) progressItem() PRProgress {
	if j.kind == runReview {
		owner, repo, number, _ := ParseRepo(j.url) // url is pre-normalized
		return PRProgress{Owner: owner, Repo: repo, Number: number, URL: j.url, State: prQueued}
	}
	return PRProgress{Title: "Discovery run", State: prQueued}
}

// enqueue outcomes, surfaced to the UI so it can toast the right message.
const (
	enqStarted   = "started"   // worker picked it up immediately
	enqQueued    = "queued"    // waiting behind other jobs
	enqDuplicate = "duplicate" // coalesced with an active/pending equivalent
	enqShutdown  = "shutdown"  // server is shutting down; refused
)

// enqueue adds a job and ensures the single worker goroutine is draining the
// queue. Discover jobs coalesce and review jobs dedupe by URL, so a scheduler
// slot firing mid-run or a double-click can't stack redundant work. Returns
// the outcome and how many jobs run before this one (0 == starts now).
func (s *server) enqueue(job runJob) (status string, ahead int) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if s.baseCtx != nil && s.baseCtx.Err() != nil {
		return enqShutdown, 0
	}
	if s.equivalentPending(job) {
		return enqDuplicate, 0
	}
	ahead = len(s.queue)
	if s.current != nil {
		ahead++
	}
	s.queue = append(s.queue, job)
	if !s.active {
		s.active = true
		s.runWG.Add(1)
		go s.runWorker()
	}
	if ahead == 0 {
		return enqStarted, 0
	}
	return enqQueued, ahead
}

// equivalentPending reports whether an equivalent job is already running or
// waiting: same URL for reviews, any other discover for discovers. Caller
// holds runMu.
func (s *server) equivalentPending(job runJob) bool {
	match := func(a runJob) bool {
		if a.kind != job.kind {
			return false
		}
		if job.kind == runReview {
			return a.url == job.url
		}
		return true // any discover subsumes another discover
	}
	if s.current != nil && match(*s.current) {
		return true
	}
	for i := range s.queue {
		if match(s.queue[i]) {
			return true
		}
	}
	return false
}

// runWorker drains the queue one job at a time until it empties (or the
// server shuts down), then exits. enqueue starts a fresh worker when the next
// job arrives. Only ever one worker runs at once (guarded by s.active).
func (s *server) runWorker() {
	defer s.runWG.Done()
	for {
		s.runMu.Lock()
		if (s.baseCtx != nil && s.baseCtx.Err() != nil) || len(s.queue) == 0 {
			s.queue = nil
			s.current = nil
			s.active = false
			s.runMu.Unlock()
			return
		}
		job := s.queue[0]
		s.queue = s.queue[1:]
		s.current = &job
		s.runMu.Unlock()

		s.execJob(job)
	}
}

// execJob runs one job with its own progress tracker, published to
// s.progress so the dashboard shows it as the active run.
func (s *server) execJob(job runJob) {
	prog := NewRunProgress()
	s.progress.Store(prog)
	switch job.kind {
	case runReview:
		if err := ReviewOne(s.baseCtx, s.db, s.cfg, job.url, time.Now(), prog); err != nil {
			slog.Error("manual review", "url", job.url, "err", err)
		}
	default: // runDiscover
		if err := Discover(s.baseCtx, s.db, s.cfg, job.trigger, time.Now(), prog); err != nil {
			slog.Error("discover run", "trigger", job.trigger, "err", err)
		}
		// Piggyback a session scan on every discovery run (scanner rides the
		// scheduler). Single-flight guarded; skipped if a scan is running.
		if s.cfg.Sessions.Enabled && (s.baseCtx == nil || s.baseCtx.Err() == nil) {
			s.tryStartSessionScan()
		}
	}
}

// runSnapshot reports whether the worker is active and returns placeholder
// rows for any queued jobs, for the dashboard / run-status.
func (s *server) runSnapshot() (active bool, queued []PRProgress) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	active = s.active
	for i := range s.queue {
		queued = append(queued, s.queue[i].progressItem())
	}
	return
}

// isRunning reports whether the run worker is currently draining the queue.
func (s *server) isRunning() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.active
}

// writeRunAccepted responds to a run/review enqueue with the queue outcome so
// the UI can toast "started" vs "queued (N ahead)" vs "already queued".
func (s *server) writeRunAccepted(w http.ResponseWriter, status string, ahead int, url string) {
	if status == enqShutdown {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
		Ahead  int    `json:"ahead"`
		URL    string `json:"url,omitempty"`
	}{Status: status, Ahead: ahead, URL: url})
}
