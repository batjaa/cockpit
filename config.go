package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	Search   string         `json:"search"`
	Schedule ScheduleConfig `json:"schedule"`
	Claude   ClaudeConfig   `json:"claude"`
	Review   ReviewConfig   `json:"review"`
	HTTP     HTTPConfig     `json:"http"`
	Sessions SessionsConfig `json:"sessions"`
}

// ReviewConfig controls what gets reviewed.
type ReviewConfig struct {
	// ExcludePaths drops findings on matching paths before a review is
	// persisted — generated code, lockfiles, vendored deps. The review
	// skill is instructed to skip generated files too; this filter is the
	// deterministic backstop. See pathExcluded for matching rules.
	// nil (field absent from the config file) gets defaultExcludePaths;
	// an explicit empty list disables exclusion entirely.
	ExcludePaths []string `json:"exclude_paths"`
}

type ScheduleConfig struct {
	StartHour     int  `json:"start_hour"`
	EndHour       int  `json:"end_hour"`
	IntervalHours int  `json:"interval_hours"`
	RunOnLaunch   bool `json:"run_on_launch"`
}

type ClaudeConfig struct {
	Binary         string `json:"binary"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Concurrency    int    `json:"concurrency"`
	// Skill is the slug cockpit invokes: `claude -p "/{skill} <pr-url>"`.
	// Point this at any skill under ~/.claude/skills that emits the JSON
	// structure documented in skills/pr-review-structured/SKILL.md. Install
	// the template under a custom name with `cockpit install-skill --as X`.
	Skill string `json:"skill"`
}

type HTTPConfig struct {
	Addr string `json:"addr"`
}

// RemoteConfig is a manually configured remote machine to scan in addition
// to any devbox-discovered hosts.
type RemoteConfig struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

// SessionsConfig controls the agent-session tracking feature.
type SessionsConfig struct {
	Enabled         bool           `json:"enabled"`
	DevboxDiscovery bool           `json:"devbox_discovery"`
	Remotes         []RemoteConfig `json:"remotes"`
	ScanClaude      bool           `json:"scan_claude"`
	ScanCodex       bool           `json:"scan_codex"`
	ScanCursor      bool           `json:"scan_cursor"`
	// ScanIntervalMinutes is the cadence of the standalone sessions
	// ticker, independent of the review scheduler. <= 0 disables it
	// (scans then run only on launch/post-run/manual triggers).
	ScanIntervalMinutes int `json:"scan_interval_minutes"`
}

func DefaultConfig() Config {
	return Config{
		Search: "",
		Schedule: ScheduleConfig{
			StartHour:     6,
			EndHour:       18,
			IntervalHours: 4,
			RunOnLaunch:   true,
		},
		Claude: ClaudeConfig{
			Binary:         "claude",
			Model:          "sonnet",
			TimeoutSeconds: 600,
			Concurrency:    3,
			Skill:          defaultSkillName,
		},
		Review: ReviewConfig{
			ExcludePaths: defaultExcludePaths(),
		},
		HTTP: HTTPConfig{
			Addr: "127.0.0.1:8765",
		},
		Sessions: SessionsConfig{
			Enabled: true,
			// Off by default: devbox discovery shells out to an internal
			// remote-host CLI named `devbox` (not a public tool). Set true (and
			// have `devbox` on PATH) to auto-scan remote hosts; otherwise
			// configure `remotes` explicitly.
			DevboxDiscovery:     false,
			Remotes:             nil,
			ScanClaude:          true,
			ScanCodex:           true,
			ScanCursor:          true,
			ScanIntervalMinutes: 20,
		},
	}
}

// LoadConfig reads config from path. If the file doesn't exist, it writes
// defaults to path and returns them.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := DefaultConfig()
		if err := SaveConfig(path, cfg); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults backfills zero values so configs written by older
// versions (or hand-edited ones) keep working when new fields appear.
func (c *Config) applyDefaults() {
	if c.Claude.Binary == "" {
		c.Claude.Binary = "claude"
	}
	if c.Claude.Model == "" {
		c.Claude.Model = "sonnet"
	}
	if c.Claude.TimeoutSeconds <= 0 {
		c.Claude.TimeoutSeconds = 600
	}
	if c.Claude.Concurrency <= 0 {
		c.Claude.Concurrency = 3
	}
	if c.Claude.Skill == "" {
		c.Claude.Skill = defaultSkillName
	}
	// nil means the field was absent from the config file; an explicit
	// empty list means the user turned exclusion off.
	if c.Review.ExcludePaths == nil {
		c.Review.ExcludePaths = defaultExcludePaths()
	}
	if c.HTTP.Addr == "" {
		c.HTTP.Addr = "127.0.0.1:8765"
	}
	// Backfill the sessions block for older config files written before the
	// feature existed. A fully zero-value SessionsConfig (all bools false,
	// nil remotes) is treated as "absent" and gets the on-by-default
	// settings. Limitation: to intentionally disable everything, a config
	// must set enabled=false while keeping at least one other field set
	// (e.g. devbox_discovery or a scan flag true, or a non-nil remotes
	// list) so the block isn't mistaken for absent.
	if c.Sessions.isZero() {
		c.Sessions = DefaultConfig().Sessions
	}
	if c.Sessions.Enabled && c.Sessions.ScanIntervalMinutes == 0 {
		// Backfill for configs written before the standalone ticker existed.
		c.Sessions.ScanIntervalMinutes = 20
	}
}

// isZero reports whether the sessions block is entirely unset, which we
// treat as "absent from the config file" so applyDefaults can backfill the
// on-by-default values. SessionsConfig has a slice field, so it isn't
// comparable with ==; check each field explicitly.
func (s SessionsConfig) isZero() bool {
	return !s.Enabled &&
		!s.DevboxDiscovery &&
		len(s.Remotes) == 0 &&
		!s.ScanClaude &&
		!s.ScanCodex &&
		!s.ScanCursor
}

func SaveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
