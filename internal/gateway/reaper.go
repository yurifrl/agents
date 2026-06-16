package gateway

import (
	"context"
	"time"
)

// ReaperConfig tunes the TTL/stale reaper.
type ReaperConfig struct {
	// Interval between sweeps.
	Interval time.Duration
	// MaxRunning cancels running tasks older than this (a backstop independent of
	// the pod's own activeDeadlineSeconds).
	MaxRunning time.Duration
	// Retention prunes terminal, acknowledged tasks older than this.
	Retention time.Duration
}

// DefaultReaperConfig is used when fields are zero.
func DefaultReaperConfig() ReaperConfig {
	return ReaperConfig{
		Interval:   5 * time.Minute,
		MaxRunning: 30 * time.Minute,
		Retention:  24 * time.Hour,
	}
}

// StartReaper runs the TTL/stale reaper until ctx is canceled: it cancels
// runaway running tasks and prunes old acknowledged terminal tasks.
func (g *Gateway) StartReaper(ctx context.Context, cfg ReaperConfig) {
	if cfg.Interval == 0 {
		cfg = DefaultReaperConfig()
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweep(ctx, cfg)
		}
	}
}

func (g *Gateway) sweep(ctx context.Context, cfg ReaperConfig) {
	now := time.Now()
	for _, t := range g.store.List("", false) {
		switch t.State {
		case StatePending, StateRunning:
			if now.Sub(t.CreatedAt) > cfg.MaxRunning {
				_ = g.Cancel(ctx, t.ID)
			}
		case StateSucceeded, StateFailed, StateCanceled:
			if t.Acknowledged && now.Sub(t.UpdatedAt) > cfg.Retention {
				_ = g.store.Delete(t.ID)
			}
		}
	}
}
