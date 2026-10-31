// Package dashboard serves the read-only web UI and JSON API for exploring
// policies, workload metrics and simulated policy changes.
package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/noony/k8s-sustain/internal/cache"
	"github.com/noony/k8s-sustain/internal/governance"
	"github.com/noony/k8s-sustain/internal/httpx"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

// PromQuerier is the subset of the Prometheus client the dashboard reads
// series through; tests inject fakes. What a recommendation is computed from,
// a Simulation's history and the detail page's signal line included, is
// read through Inputs instead.
type PromQuerier interface {
	Ping(ctx context.Context) error

	QueryInstant(ctx context.Context, expr string) (float64, error)
	QueryRange(ctx context.Context, expr string, r promclient.TimeRange, step time.Duration) ([]promclient.TimeValue, error)
	QueryByLabel(ctx context.Context, expr, label string) (map[string]float64, error)
	// QueryRangeByLabel is QueryRange for an expression with one series per
	// value of label, keyed by it: an identity's running requests and limits,
	// and the replica-averaged usage its detail page charts, per container.
	QueryRangeByLabel(ctx context.Context, expr, label string, r promclient.TimeRange, step time.Duration) (map[string][]promclient.TimeValue, error)
}

// Server is the dashboard HTTP server.
type Server struct {
	K8sClient  client.Client
	PromClient PromQuerier
	// Health is where every view reads identity health from; production
	// wires NewPrometheusHealthSignals.
	Health HealthSignals
	// Inputs fetches recommendation inputs through the same port the
	// controller uses, so a Simulation computes from what the controller sees
	// and the detail page charts what its signals read, the OOM kills
	// included.
	Inputs recommender.InputsFetcher
	// Clock is what a Simulation computes as of, what dates every inventory
	// snapshot and what the /api/summary cache ages by. Required.
	Clock  clock.PassiveClock
	Logger logr.Logger
	// CORSOrigins is the allowed origin allowlist: empty means same-origin
	// only, ["*"] allows every origin.
	CORSOrigins []string

	// Policies is shared by every snapshot the views take, so a Policy's
	// selector is compiled once per version of it. It holds the namespaces no
	// Policy governs, a hard deny applied ahead of a Policy's own selector.
	Policies *governance.PolicyLister

	summaryOnce     sync.Once
	summarySnapshot *cache.LRU[string, summaryResponseV2]

	// summary collapses concurrent /api/summary recomputes onto one shared
	// computation, so a slow recompute cannot overwrite a newer snapshot.
	summary *cache.ReadThrough[string, summaryResponseV2]
}

// Handler returns an http.Handler with all dashboard routes registered.
func (s *Server) Handler() http.Handler {
	s.summaryOnce.Do(func() {
		s.summarySnapshot = cache.NewLRU[string, summaryResponseV2](8, 60*time.Second, s.Clock)
		s.summary = cache.NewReadThrough(s.summarySnapshot, summaryComputeTimeout, s.fetchSummary, nil)
	})

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/policies", s.handlePolicies)
	mux.HandleFunc("GET /api/policies/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handlePolicyDetail(w, r, r.PathValue("name"))
	})
	mux.HandleFunc("GET /api/policies/{name}/workloads", func(w http.ResponseWriter, r *http.Request) {
		s.handlePolicyWorkloads(w, r, r.PathValue("name"))
	})
	mux.HandleFunc("GET /api/policies/{name}/batch-simulate", func(w http.ResponseWriter, r *http.Request) {
		s.handlePolicyBatchSimulate(w, r, r.PathValue("name"))
	})

	mux.HandleFunc("GET /api/workloads", s.handleAllWorkloads)
	mux.HandleFunc("GET /api/workloads/{namespace}/{kind}/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleWorkloadDetail(w, r, r.PathValue("namespace"), r.PathValue("kind"), r.PathValue("name"))
	})
	mux.HandleFunc("GET /api/workloads/{namespace}/{kind}/{name}/metrics", func(w http.ResponseWriter, r *http.Request) {
		s.handleWorkloadMetrics(w, r, r.PathValue("namespace"), r.PathValue("kind"), r.PathValue("name"))
	})

	mux.HandleFunc("POST /api/simulate", s.handleSimulate)

	mux.HandleFunc("GET /api/kinds", s.handleKinds)

	mux.HandleFunc("GET /api/summary", s.handleSummary)
	mux.HandleFunc("GET /api/summary/trend", s.handleSummaryTrend)
	mux.HandleFunc("GET /api/summary/activity", s.handleSummaryActivity)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	mux.HandleFunc("/", s.handleUI)

	// Request-ID must sit outermost so telemetry and recovery see it.
	return s.withRequestID(s.withTelemetry(s.withRecovery(s.withCORS(s.withGzip(mux)))))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.PromClient.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":     "not ready",
			"prometheus": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// NewHTTPServer creates an http.Server for the dashboard bound to addr.
func (s *Server) NewHTTPServer(addr string) *http.Server {
	return httpx.NewServer(addr, s.Handler())
}

func formatQuantity(milliValue int64, format string) string {
	if format == "memory" {
		mib := float64(milliValue) / 1000.0 / (1024 * 1024)
		if mib >= 1024 {
			return fmt.Sprintf("%.1f Gi", mib/1024)
		}
		return fmt.Sprintf("%.0f Mi", mib)
	}
	if milliValue >= 1000 {
		return fmt.Sprintf("%.2f", float64(milliValue)/1000.0)
	}
	return fmt.Sprintf("%dm", milliValue)
}
