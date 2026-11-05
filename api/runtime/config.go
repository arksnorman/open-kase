package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/settings"
)

// settingsPollInterval bounds how long a changed setting takes to reach
// this instance.
const settingsPollInterval = 5 * time.Second

// watchSettings loads the settings snapshot and refreshes it until ctx ends.
// A failed refresh keeps the last validated snapshot.
func (c *Container) watchSettings(ctx context.Context) {
	refresh := func() {
		before := c.Settings.Current()
		snap, err := c.Settings.Refresh(ctx)
		switch {
		case errors.Is(err, settings.ErrNotInitialized):
			c.Log.Debug("settings not available yet; installation not initialized")
		case err != nil && ctx.Err() == nil:
			c.Log.Warn("settings refresh failed; keeping last valid snapshot", "error", err.Error())
		case snap != nil && (before == nil || before.Version() != snap.Version()):
			c.Log.Info("settings snapshot loaded", "configVersion", snap.Version())
		}
	}
	refresh()
	t := time.NewTicker(settingsPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

// uploadCleanupInterval is how often expired staged uploads are removed.
const uploadCleanupInterval = 15 * time.Minute

// cleanupUploads periodically removes expired, never-attached uploads. The
// state lives in MongoDB (expiresAt plus a conditional "deleting" claim),
// so a restart loses nothing and several processes may run it at once.
func (c *Container) cleanupUploads(ctx context.Context) {
	run := func() {
		n, err := c.Tickets.CleanupExpiredUploads(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			c.Log.Warn("staged upload cleanup failed", "error", err.Error())
		case n > 0:
			c.Log.Info("removed expired staged uploads", "count", n)
		}
		if n, err := c.Tickets.SweepExpiredHolds(ctx, 100); err != nil && ctx.Err() == nil {
			c.Log.Warn("hold expiry sweep failed", "error", err.Error())
		} else if n > 0 {
			c.Log.Info("ended expired holds", "count", n)
		}
		if n, err := c.Automation.EnsureScheduled(ctx); err != nil && ctx.Err() == nil {
			c.Log.Warn("automation scheduling failed", "error", err.Error())
		} else if n > 0 {
			c.Log.Debug("automation scans scheduled", "count", n)
		}
	}
	t := time.NewTicker(uploadCleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
