// Package dashboard provides an HTTP server that serves a web UI for exploring
// k8s-sustain policies, workload metrics, and simulating policy changes.
package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/httpx"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// PromQuerier is the subset of the Prometheus client used by the dashboard.
// Defining it as an interface lets tests inject fakes.
type PromQuerier interface {
	Ping(ctx context.Context) error

	// Generic helpers used by /api/summary.
	QueryInstant(ctx context.Context, expr string) (float64, error)
	QueryRange(ctx context.Context, expr, window, step string) ([]promclient.TimeValue, error)
	QueryByLabel(ctx context.Context, expr, label string) (map[string]float64, error)
	QueryByLabels(ctx context.Context, query string, labels ...string) (map[string]float64, error)

	// Per-workload helpers used by /api/workloads/* and /api/simulate.
	QueryCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (promclient.ContainerValues, error)
	QueryMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (promclient.ContainerValues, error)
	QueryCPURangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryCPURequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryCPULimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryLimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryCPURecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow, timeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow, timeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryOOMKillEvents(ctx context.Context, namespace, ownerKind, ownerName, window, step string) ([]promclient.OOMEvent, error)
	QueryWorkloadOOMSignal(ctx context.Context, namespace, ownerKind, ownerName string) (promclient.OOMSignal, error)
}

var dashboardScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(dashboardScheme))
	utilruntime.Must(sustainv1alpha1.AddToScheme(dashboardScheme))
}

// Scheme returns the runtime scheme with all needed types registered.
func Scheme() *runtime.Scheme { return dashboardScheme }

// Server is the dashboard HTTP server.
type Server struct {
	K8sClient  client.Client
	PromClient PromQuerier
	Logger     logr.Logger
	// CORSOrigins is the allowed CORS origin allowlist.
	//
	//   - nil / empty: no Access-Control-Allow-Origin header is set
	//     (same-origin requests only — the safe default).
	//   - ["*"]: explicit wildcard, every origin allowed.
	//   - other: the request's Origin must match one of the listed values.
	CORSOrigins []string

	cacheInit    sync.Once
	summaryCache *Cache
}

// Handler returns an http.Handler with all dashboard routes registered.
// Routes use Go 1.22 method-specific patterns so the stdlib mux returns a
// proper 405 with an `Allow` header when callers use the wrong verb, and
// path variables are read with r.PathValue rather than hand-parsed.
func (s *Server) Handler() http.Handler {
	s.cacheInit.Do(func() {
		if s.summaryCache == nil {
			s.summaryCache = NewCache(8, 60*time.Second)
		}
	})

	mux := http.NewServeMux()

	// Policy routes.
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

	// Workload routes.
	mux.HandleFunc("GET /api/workloads", s.handleAllWorkloads)
	mux.HandleFunc("GET /api/workloads/{namespace}/{kind}/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleWorkloadDetail(w, r, r.PathValue("namespace"), r.PathValue("kind"), r.PathValue("name"))
	})
	mux.HandleFunc("GET /api/workloads/{namespace}/{kind}/{name}/metrics", func(w http.ResponseWriter, r *http.Request) {
		s.handleWorkloadMetrics(w, r, r.PathValue("namespace"), r.PathValue("kind"), r.PathValue("name"))
	})
	mux.HandleFunc("GET /api/workloads/{namespace}/{kind}/{name}/recommendations", func(w http.ResponseWriter, r *http.Request) {
		s.handleWorkloadRecommendations(w, r, r.PathValue("namespace"), r.PathValue("kind"), r.PathValue("name"))
	})

	// Simulation.
	mux.HandleFunc("POST /api/simulate", s.handleSimulate)

	// Summary routes.
	mux.HandleFunc("GET /api/summary", s.handleSummary)
	mux.HandleFunc("GET /api/summary/trend", s.handleSummaryTrend)
	mux.HandleFunc("GET /api/summary/activity", s.handleSummaryActivity)

	// Health.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Embedded UI catch-all.
	mux.HandleFunc("/", s.handleUI)

	// Request-ID assignment must sit outermost so the telemetry and recovery
	// middlewares (which log the request ID) see a populated context.
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
// The caller is responsible for calling ListenAndServe and Shutdown.
func (s *Server) NewHTTPServer(addr string) *http.Server {
	// Hardened timeouts come from httpx.NewServer's shared defaults
	// (ReadHeaderTimeout 5s, Read/WriteTimeout 15s, IdleTimeout 60s) — the
	// same values this used to set inline.
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
	// CPU
	if milliValue >= 1000 {
		return fmt.Sprintf("%.2f", float64(milliValue)/1000.0)
	}
	return fmt.Sprintf("%dm", milliValue)
}
