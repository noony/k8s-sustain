package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// orphanReaper is a manager Runnable that periodically deletes
// WorkloadRecommendation objects whose owning Policy no longer exists
// (wlrcache.Reap). It covers force-deleted policies, controller crashes
// mid-delete, and any other path that bypasses the per-policy finalizer.
type orphanReaper struct {
	client   client.Client
	interval time.Duration
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
	o.reap(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			o.reap(ctx)
		}
	}
}

func (o *orphanReaper) reap(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("orphan-reaper")
	deleted, err := wlrcache.Reap(ctx, o.client)
	if err != nil {
		logger.V(1).Info("failed to reap orphan WorkloadRecommendations", "err", err)
	}
	if deleted > 0 {
		logger.Info("reaped orphan WorkloadRecommendations", "deleted", deleted)
	}
}

// NeedLeaderElection ensures only the leader runs the orphan reaper, so
// multi-replica controllers don't double-delete or race.
func (o *orphanReaper) NeedLeaderElection() bool { return true }
