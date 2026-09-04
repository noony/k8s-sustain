// Package k8s holds small helpers shared by every binary that needs a
// controller-runtime client without taking on the full manager machinery.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// New builds an uncached controller-runtime client against the in-cluster
// or kubeconfig REST config. Every Get/List is a direct apiserver round trip.
func New(scheme *runtime.Scheme) (client.Client, error) {
	restCfg := ctrl.GetConfigOrDie()
	return client.New(restCfg, client.Options{Scheme: scheme})
}

// NewCached builds a client whose Policy, WorkloadRecommendation and
// Namespace reads are served from pre-warmed informers; the owner-chain kinds
// stay uncached. runCtx bounds the cache lifetime and is the caller's to
// cancel; startCtx bounds the blocking CRD wait and cache sync. On error the
// cache is stopped before returning. Expect roughly 20-50 MB resident for the
// WorkloadRecommendation informer at ~10k workloads.
func NewCached(runCtx, startCtx context.Context, scheme *runtime.Scheme) (_ client.Client, err error) {
	restCfg := ctrl.GetConfigOrDie()

	c, err := cache.New(restCfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building informer cache: %w", err)
	}

	// Register informers before Start: cache.New creates none, so
	// WaitForCacheSync on a fresh cache returns true instantly, and the lazy
	// first Get would then LIST inline on the admission path. One deadline
	// for all kinds so the chart's startup probe budget has a single number.
	crdDeadline := time.Now().Add(crdWaitTimeout)
	for _, obj := range cachedKinds() {
		if err := registerInformerWithRetry(startCtx, c, obj, crdDeadline, crdWaitInterval); err != nil {
			return nil, fmt.Errorf("waiting up to %s for custom resource definitions to be served: %w",
				crdWaitTimeout, err)
		}
	}

	// runCtx outlives this call, so a failed construction must stop the
	// cache itself or the informers would leak.
	cacheCtx, stopCache := context.WithCancel(runCtx)
	defer func() {
		if err != nil {
			stopCache()
		}
	}()

	go func() {
		_ = c.Start(cacheCtx)
	}()

	if !c.WaitForCacheSync(startCtx) {
		// WaitForCacheSync also fails without a context error, and %w on a
		// nil error renders as "%!w(<nil>)".
		if err := startCtx.Err(); err != nil {
			return nil, fmt.Errorf("informer cache failed to sync: %w", err)
		}
		return nil, errors.New("informer cache failed to sync")
	}

	cached, err := client.New(restCfg, client.Options{
		Scheme: scheme,
		Cache: &client.CacheOptions{
			Reader:     c,
			DisableFor: OwnerChainDisableFor(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("building cache-backed client: %w", err)
	}
	return cached, nil
}

// OwnerChainDisableFor lists the owner-chain kinds NewCached reads straight
// from the apiserver: each is read at most once per pod CREATE, so an
// informer over every object of the kind would cost more than it saves.
// Exported so the webhook can assert it covers every kind it Gets.
func OwnerChainDisableFor() []client.Object {
	return []client.Object{
		&appsv1.ReplicaSet{},
		&batchv1.Job{},
		&appsv1.Deployment{},
		&appsv1.StatefulSet{},
		&appsv1.DaemonSet{},
		&batchv1.CronJob{},
		&rolloutsv1alpha1.Rollout{},
	}
}

// crdWaitTimeout bounds how long NewCached waits for the CRDs to be served
// on a fresh install, where establishment lags creation. Nothing answers
// /healthz during the wait, so the chart's webhook startupProbe budget must
// exceed this constant; a test pins the relationship.
const crdWaitTimeout = 2 * time.Minute

// crdWaitInterval is the poll gap while waiting for CRD establishment.
const crdWaitInterval = 2 * time.Second

// registerInformerWithRetry registers obj's informer until deadline,
// retrying only "no matches for kind" errors; anything else returns at once.
func registerInformerWithRetry(
	ctx context.Context, c cache.Cache, obj client.Object, deadline time.Time, interval time.Duration,
) error {
	logger := log.FromContext(ctx)
	for attempt := 1; ; attempt++ {
		_, err := c.GetInformer(ctx, obj)
		if err == nil {
			return nil
		}
		if !meta.IsNoMatchError(err) {
			return fmt.Errorf("registering informer for %T: %w", obj, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registering informer for %T: custom resource definition still not "+
				"served (is the CRD installed?): %w", obj, err)
		}
		logger.Info("custom resource definition not served yet; waiting for it to be established",
			"type", fmt.Sprintf("%T", obj), "attempt", attempt, "retryIn", interval)
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return fmt.Errorf("registering informer for %T: %w", obj, ctx.Err())
		}
	}
}

// cachedKinds lists the types NewCached keeps informers for.
func cachedKinds() []client.Object {
	return []client.Object{
		&sustainv1alpha1.Policy{},
		&sustainv1alpha1.WorkloadRecommendation{},
		&corev1.Namespace{},
	}
}
