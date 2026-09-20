package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const logSweepInterval = 6 * time.Hour

// sweepLogs deletes the old card logs at the start and then periodically.
func (d *Daemon) sweepLogs(ctx context.Context) {
	if d.cfg.LogRetention.Duration == 0 {
		return
	}
	for {
		removed, err := removeOldLogs(d.logDir(), d.cfg.LogRetention.Duration, time.Now())
		if err != nil {
			d.logger.Warn("log sweep failed", "error", err)
		} else if removed > 0 {
			d.logger.Info("old card logs deleted", "logs", removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(logSweepInterval):
		}
	}
}

// removeOldLogs deletes each card log that no turn wrote to for the
// retention, and returns how many it deleted.
func removeOldLogs(dir string, retention time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "card-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < retention {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
