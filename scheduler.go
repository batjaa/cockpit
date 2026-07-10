package main

import (
	"log/slog"
	"time"
)

// NextRunTime returns the next scheduled tick strictly after `from`.
// Slots are local-time hours h where start <= h < end and
// (h - start) % interval == 0 — e.g. start=6 end=18 interval=6 fires at
// 06:00 and 12:00.
func NextRunTime(from time.Time, sc ScheduleConfig) time.Time {
	for dayOffset := 0; ; dayOffset++ {
		for hour := sc.StartHour; hour < sc.EndHour; hour += sc.IntervalHours {
			candidate := time.Date(from.Year(), from.Month(), from.Day()+dayOffset,
				hour, 0, 0, 0, from.Location())
			if candidate.After(from) {
				return candidate
			}
		}
	}
}

// schedulerEnabled reports whether the config describes a usable window.
func schedulerEnabled(sc ScheduleConfig) bool {
	return sc.IntervalHours > 0 && sc.StartHour < sc.EndHour
}

// runScheduler fires discovery runs on the configured slots until the
// server's base context is cancelled. Runs share the run worker with manual
// triggers. Discover jobs coalesce, so a slot that fires while a discovery is
// already active or pending is skipped (the next slot picks up whatever it
// missed, since reviews are cached by SHA); a slot that fires during a manual
// review instead queues behind it.
func (s *server) runScheduler() {
	sc := s.cfg.Schedule
	if !schedulerEnabled(sc) {
		slog.Info("scheduler disabled", "start", sc.StartHour, "end", sc.EndHour, "interval", sc.IntervalHours)
		return
	}
	if sc.RunOnLaunch {
		if s.tryStartRun("launch") {
			slog.Info("run-on-launch triggered")
		} else {
			slog.Warn("run-on-launch skipped", "reason", "search empty or run in progress")
		}
	}
	for {
		next := NextRunTime(time.Now(), sc)
		slog.Info("scheduler armed", "next", next.Format("Mon 15:04"))
		select {
		case <-s.baseCtx.Done():
			return
		case <-time.After(time.Until(next)):
			if s.tryStartRun("schedule") {
				slog.Info("scheduled run triggered", "slot", next.Format("15:04"))
			} else {
				slog.Warn("scheduled run skipped", "slot", next.Format("15:04"),
					"reason", "search empty or run in progress")
			}
		}
	}
}

// runSessionTicker scans agent sessions on a fixed cadence, independent of
// the review scheduler (whose slots are designed around LLM cost, not the
// near-free incremental session scan). Single-flight via the same guard as
// the Scan button, so overlapping triggers collapse.
func (s *server) runSessionTicker() {
	sc := s.cfg.Sessions
	if !sc.Enabled || sc.ScanIntervalMinutes <= 0 {
		slog.Info("session ticker disabled", "enabled", sc.Enabled, "interval_min", sc.ScanIntervalMinutes)
		return
	}
	interval := time.Duration(sc.ScanIntervalMinutes) * time.Minute
	slog.Info("session ticker armed", "interval", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case <-t.C:
			if !s.tryStartSessionScan() {
				slog.Debug("session ticker skipped; scan already running")
			}
		}
	}
}
