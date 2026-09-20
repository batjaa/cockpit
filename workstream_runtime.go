package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// configureWorkstreams only prepares local services. In particular, it must not
// open or write the vault before Serve has acquired its listening socket.
func (s *server) configureWorkstreams() error {
	location := time.Local
	if name := s.cfg.Workstreams.Timezone; name != "" {
		var err error
		location, err = time.LoadLocation(name)
		if err != nil {
			return fmt.Errorf("workstreams.timezone must be a valid IANA timezone: %w", err)
		}
	}
	s.workstreams = NewWorkstreamStore(s.db, time.Now, location)
	mirrorConfig := s.cfg.Workstreams.Mirror
	enabled := mirrorConfig.Enabled == nil || *mirrorConfig.Enabled
	s.workstreams.SetMirrorEnabled(enabled)
	if !enabled {
		return nil
	}
	root := mirrorConfig.Root
	if root == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			// A missing home directory must not disable local workstream saves.
			// The worker treats an unavailable root as an export error.
			slog.Warn("workstream mirror root unavailable", "error_type", "home_directory")
		} else {
			root = filepath.Join(homeDir, "cockpit-vault")
		}
	}
	s.mirror = NewMirrorWorker(s.workstreams, MirrorWorkerConfig{Root: root, Now: time.Now})
	s.workstreams.SetAfterCommit(s.mirror.Wake)
	return nil
}

func (s *server) runWorkstreamMirror(ctx context.Context) {
	if s.mirror == nil {
		return
	}
	s.mirror.Run(ctx)
}
