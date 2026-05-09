package controller

import (
	"context"
	"time"
)

// orphanReaper is a manager Runnable that periodically deletes
// WorkloadRecommendation objects whose owning Policy no longer exists.
// Strategy 2 cleanup — covers force-deleted policies, controller crashes
// mid-delete, and any other path that bypasses the per-policy finalizer.
type orphanReaper struct {
	reconciler *PolicyReconciler
	interval   time.Duration
}

func (o *orphanReaper) Start(ctx context.Context) error {
	interval := o.interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Run once at startup so a controller restart catches anything left over
	// while it was down.
	_ = o.reconciler.reapOrphanedRecommendations(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_ = o.reconciler.reapOrphanedRecommendations(ctx)
		}
	}
}

// NeedLeaderElection ensures only the leader runs the orphan reaper, so
// multi-replica controllers don't double-delete or race.
func (o *orphanReaper) NeedLeaderElection() bool { return true }
