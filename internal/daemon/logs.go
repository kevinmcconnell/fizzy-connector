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
		removed, err := removeOldLogs(d.logDir(), d.removeUnusedLog)
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

// removeUnusedLog deletes the log of a card when no turn wrote to it for
// the retention, unless a turn of the card is in progress: the turn may have
// opened the log before its first write. The checks and the deletion are one
// step under the mutex, so a turn cannot start or end in between.
func (d *Daemon) removeUnusedLog(card int, path string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[card] {
		return false, nil
	}
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
func removeOldLogs(dir string, remove func(card int, path string) (bool, error)) (int, error) {
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
		deleted, err := remove(card, filepath.Join(dir, entry.Name()))
		if err != nil {
			return removed, err
		}
		if deleted {
			removed++
		}
	}
	return removed, nil
}
