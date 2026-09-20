package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const logSweepInterval = 6 * time.Hour

// sweepLogs deletes the old card logs at the start and then periodically.
func (d *Daemon) sweepLogs(ctx context.Context) {
	if d.cfg.LogRetention.Duration == 0 {
		return
	}
	for {
		removed, err := removeOldLogs(d.logDir(), d.removeOldLog)
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

// removeOldLog deletes the log of a card when no turn wrote to it for the
// retention. The check and the deletion are one step under the lock that
// openLog takes, so a turn cannot open the log in between.
func (d *Daemon) removeOldLog(path string) (bool, error) {
	d.logFiles.Lock()
	defer d.logFiles.Unlock()
	return removeIfOlder(path, d.cfg.LogRetention.Duration, time.Now())
}

func removeIfOlder(path string, retention time.Duration, now time.Time) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if now.Sub(info.ModTime()) < retention {
		return false, nil
	}
	return true, os.Remove(path)
}

// removeOldLogs gives each card log to remove, and returns how many were
// deleted.
func removeOldLogs(dir string, remove func(path string) (bool, error)) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		var card int
		if _, err := fmt.Sscanf(entry.Name(), "card-%d.log", &card); err != nil || entry.Name() != fmt.Sprintf("card-%d.log", card) {
			continue
		}
		deleted, err := remove(filepath.Join(dir, entry.Name()))
		if err != nil {
			return removed, err
		}
		if deleted {
			removed++
		}
	}
	return removed, nil
}
