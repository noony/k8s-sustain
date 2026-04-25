// Package dashboard provides an HTTP server that serves a web UI for exploring
// k8s-sustain policies, workload metrics, and simulating policy changes.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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

	// Per-workload helpers used by /api/workloads/* and /api/simulate.
	QueryCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (promclient.ContainerValues, error)
	QueryMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (promclient.ContainerValues, error)
	QueryCPURangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryCPURequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (promclient.ContainerTimeSeries, error)
	QueryCPURecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow, timeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryMemoryRecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow, timeRange, step string) (promclient.ContainerTimeSeries, error)
	QueryOOMKillEvents(ctx context.Context, namespace, ownerKind, ownerName, window, step string) ([]promclient.OOMEvent, error)
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
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
	K8sClient   client.Client
	PromClient  PromQuerier
	Logger      logr.Logger
	CORSOrigins []string // Allowed CORS origins. Empty or ["*"] means allow all.

	cacheInit    sync.Once
	summaryCache *Cache
	policyCache  *Cache
}

// Handler returns an http.Handler with all dashboard routes registered.
func (s *Server) Handler() http.Handler {
	s.cacheInit.Do(func() {
		if s.summaryCache == nil {
			s.summaryCache = NewCache(8, 60*time.Second)
		}
		if s.policyCache == nil {
			s.policyCache = NewCache(32, 30*time.Second)
		}
	})

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/policies", s.handlePolicies)
	mux.HandleFunc("/api/policies/", s.handlePolicyRoutes)
	mux.HandleFunc("/api/workloads", s.handleAllWorkloads)
	mux.HandleFunc("/api/workloads/", s.handleWorkloadRoutes)
	mux.HandleFunc("/api/simulate", s.handleSimulate)
	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/summary/trend", s.handleSummaryTrend)
	mux.HandleFunc("/api/summary/activity", s.handleSummaryActivity)

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", s.handleReadyz)

	// Serve embedded UI
	mux.HandleFunc("/", s.handleUI)

	return s.withTelemetry(s.withCORS(mux))
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
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// ListenAndServe starts the dashboard server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	srv := s.NewHTTPServer(addr)
	s.Logger.Info("Starting dashboard server", "addr", addr)
	return srv.ListenAndServe()
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "*"
		if len(s.CORSOrigins) > 0 && s.CORSOrigins[0] != "*" {
			reqOrigin := r.Header.Get("Origin")
			origin = ""
			for _, o := range s.CORSOrigins {
				if o == reqOrigin {
					origin = o
					break
				}
			}
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withTelemetry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)
		requestDuration.WithLabelValues(r.URL.Path, http.StatusText(rw.statusCode)).Observe(dur.Seconds())
		s.Logger.V(1).Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration", dur.String(),
			"remote", r.RemoteAddr,
		)
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// parsePath splits a URL path by "/" and returns the segments after the prefix.
// e.g. parsePath("/api/policies/my-policy/workloads", "/api/policies/") returns ["my-policy", "workloads"]
func parsePath(path, prefix string) []string {
	trimmed := strings.TrimPrefix(path, prefix)
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
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
