// Package dashboard serves the read-only web UI and JSON API for exploring
// policies, workload metrics and simulated policy changes.
package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/go-logr/logr"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/httpx"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// PromQuerier is the subset of the Prometheus client the dashboard uses;
// tests inject fakes.
type PromQuerier interface {
	Ping(ctx context.Context) error

	QueryInstant(ctx context.Context, expr string) (float64, error)
	QueryRange(ctx context.Context, expr string, r promclient.TimeRange, step string) ([]promclient.TimeValue, error)
	QueryByLabel(ctx context.Context, expr, label string) (map[string]float64, error)
	QueryByLabels(ctx context.Context, query string, labels ...string) (map[string]float64, error)

	QueryCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (promclient.ContainerValues, error)
	QueryMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (promclient.ContainerValues, error)
	QueryCPURangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryCPURequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryCPULimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryLimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryCPURecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow string, r promclient.TimeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryOOMKillEvents(ctx context.Context, namespace, ownerKind, ownerName string, r promclient.TimeRange, step string) ([]promclient.OOMEvent, error)
	QueryWorkloadOOMSignal(ctx context.Context, namespace, ownerKind, ownerName string) (promclient.OOMSignal, error)
}

var dashboardScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(dashboardScheme))
	utilruntime.Must(sustainv1alpha1.AddToScheme(dashboardScheme))
	utilruntime.Must(rolloutsv1alpha1.AddToScheme(dashboardScheme))
}

// Scheme returns the runtime scheme with all needed types registered.
func Scheme() *runtime.Scheme { return dashboardScheme }

// Server is the dashboard HTTP server.
type Server struct {
	K8sClient  client.Client
	PromClient PromQuerier
	Logger     logr.Logger
	// CORSOrigins is the allowed origin allowlist: empty means same-origin
	// only, ["*"] allows every origin.
	CORSOrigins []string

	// ExcludedNamespaces mirrors the controller's --excluded-namespaces flag,
	// a hard deny applied ahead of a Policy's own selector.
	ExcludedNamespaces []string

	cacheInit    sync.Once
	summaryCache *Cache

	// summarySF collapses concurrent /api/summary recomputes onto one shared
	// computation, so a slow recompute cannot overwrite a newer snapshot.
	summarySF singleflight.Group

	// sfJoinHook, if non-nil, is invoked with the key by every request just
	// after it joins summarySF. Tests use it as a barrier; a field rather than
	// a global so stragglers from one test cannot race the next.
	sfJoinHook func(key string)
}

// Handler returns an http.Handler with all dashboard routes registered.
func (s *Server) Handler() http.Handler {
	s.cacheInit.Do(func() {
		if s.summaryCache == nil {
			s.summaryCache = NewCache(8, 60*time.Second)
		}
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
	mux.HandleFunc("GET /api/workloads/{namespace}/{kind}/{name}/recommendations", func(w http.ResponseWriter, r *http.Request) {
		s.handleWorkloadRecommendations(w, r, r.PathValue("namespace"), r.PathValue("kind"), r.PathValue("name"))
	})

	mux.HandleFunc("POST /api/simulate", s.handleSimulate)

	mux.HandleFunc("GET /api/summary", s.handleSummary)
	mux.HandleFunc("GET /api/summary/trend", s.handleSummaryTrend)
	mux.HandleFunc("GET /api/summary/activity", s.handleSummaryActivity)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", s.handleReadyz)

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
