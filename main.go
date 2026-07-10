package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	// Subcommands that need neither config nor db.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version)
			return
		case "install-skill":
			path, err := installSkill()
			if err != nil {
				fail("install-skill", err)
			}
			fmt.Println("installed pr-review-structured skill →", path)
			return
		}
	}

	configPath := flag.String("config", "", "path to config file (default: ~/.cockpit/config.json)")
	runOnce := flag.Bool("run-once", false, "discover via gh search and review once, then exit")
	reviewPR := flag.String("pr", "", "review a single PR by URL and exit")
	scanSessions := flag.Bool("scan-sessions", false, "scan agent sessions once, then exit")
	flag.Parse()

	home, err := os.UserHomeDir()
	if err != nil {
		fail("user home dir", err)
	}
	cockpitDir := filepath.Join(home, ".cockpit")
	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = filepath.Join(cockpitDir, "config.json")
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fail("load config", err)
	}

	// `doctor` checks external deps (gh/claude/skill) and exits; no db needed.
	if flag.Arg(0) == "doctor" {
		if !doctor(context.Background(), cfg) {
			os.Exit(1)
		}
		return
	}

	dbPath := filepath.Join(cockpitDir, "cockpit.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		fail("open db", err)
	}
	defer db.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	now := time.Now()

	switch {
	case *reviewPR != "":
		if err := ReviewOne(ctx, db, cfg, *reviewPR, now, nil); err != nil {
			fail("review-one", err)
		}
	case *scanSessions:
		if err := ScanSessions(ctx, db, cfg); err != nil {
			fail("scan-sessions", err)
		}
	case *runOnce:
		if err := Discover(ctx, db, cfg, "manual", now, nil); err != nil {
			fail("discover", err)
		}
	default:
		preflight(ctx, cfg)
		if err := Serve(ctx, db, cfg); err != nil {
			fail("serve", err)
		}
	}
}

func fail(stage string, err error) {
	slog.Error(stage, "err", err)
	os.Exit(1)
}
