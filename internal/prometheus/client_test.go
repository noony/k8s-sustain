package prometheus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestQueryInstantReturnsScalarFromVector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		q := r.Form.Get("query")
		if !strings.Contains(q, "k8s_sustain:cluster_cpu_savings_cores") {
			t.Fatalf("unexpected query: %q (raw=%s)", q, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"3.2"]}]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.QueryInstant(context.Background(), "k8s_sustain:cluster_cpu_savings_cores")
	if err != nil {
		t.Fatal(err)
	}
	if v != 3.2 {
		t.Fatalf("got %v want 3.2", v)
	}
}

func TestQueryInstantEmptyVectorReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	v, err := c.QueryInstant(context.Background(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("expected 0, got %v", v)
	}
}

func TestQueryByLabelMapsLabelValueToSample(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"policy":"prod"},"value":[1700000000,"1.5"]},
			{"metric":{"policy":"dev"},"value":[1700000000,"0.25"]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	out, err := c.QueryByLabel(context.Background(), "anything", "policy")
	if err != nil {
		t.Fatal(err)
	}
	if got := out["prod"]; got != 1.5 {
		t.Fatalf("prod: got %v want 1.5", got)
	}
	if got := out["dev"]; got != 0.25 {
		t.Fatalf("dev: got %v want 0.25", got)
	}
}

func TestQueryRangeReturnsTimeValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{},"values":[[1700000000,"1"],[1700000060,"2"]]}
		]}}`))
	}))
	defer server.Close()

	now := time.Now()
	tr := TimeRange{Start: now.Add(-5 * time.Minute), End: now}
	c, _ := New(server.URL)
	out, err := c.QueryRange(context.Background(), "anything", tr, "1m")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d points, want 2", len(out))
	}
	if out[0].Value != 1 || out[1].Value != 2 {
		t.Fatalf("unexpected values: %+v", out)
	}
}

func TestQueryWorkloadCPUByContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		q := r.Form.Get("query")
		if !strings.Contains(q, "workload_max_pod_cpu") {
			t.Errorf("expected workload_max_pod_cpu in query, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"success",
			"data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"0.5"]},
				{"metric":{"container":"sidecar"},"value":[0,"0.1"]}
			]}
		}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.QueryWorkloadCPUByContainer(context.Background(), "ns", "Deployment", "web", 0.95, "168h")
	if err != nil {
		t.Fatalf("QueryWorkloadCPUByContainer: %v", err)
	}
	if got["app"] != 0.5 || got["sidecar"] != 0.1 {
		t.Errorf("unexpected values: %v", got)
	}
}

func TestQueryWorkloadMemoryByContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		q := r.Form.Get("query")
		if !strings.Contains(q, "workload_max_pod_memory") {
			t.Errorf("expected workload_max_pod_memory in query, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"success",
			"data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"104857600"]}
			]}
		}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.QueryWorkloadMemoryByContainer(context.Background(), "ns", "Deployment", "web", 0.95, "168h")
	if err != nil {
		t.Fatalf("QueryWorkloadMemoryByContainer: %v", err)
	}
	if got["app"] != 104857600 {
		t.Errorf("expected 104857600 got %v", got["app"])
	}
}

func TestQueryByLabels_JoinsMultipleLabelsWithPipe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
	}))
	defer server.Close()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"namespace":"ns1","owner_name":"a"},"value":[0,"1"]},
			{"metric":{"namespace":"ns2","owner_name":"b"},"value":[0,"2"]},
			{"metric":{"namespace":"ns3"},"value":[0,"3"]}
		]}}`))
	})

	c, _ := New(server.URL)
	out, err := c.QueryByLabels(context.Background(), "anything", "namespace", "owner_name")
	if err != nil {
		t.Fatalf("QueryByLabels: %v", err)
	}
	if got := out["ns1|a"]; got != 1 {
		t.Errorf("ns1|a = %v, want 1", got)
	}
	if got := out["ns2|b"]; got != 2 {
		t.Errorf("ns2|b = %v, want 2", got)
	}
	if _, ok := out["ns3|"]; ok {
		t.Errorf("incomplete label series should be dropped, got entry for %q", "ns3|")
	}
	if len(out) != 2 {
		t.Errorf("expected 2 entries, got %d (%v)", len(out), out)
	}
}

func TestQueryCPUByContainer_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		if !strings.Contains(q, "container_cpu_usage_by_workload") {
			t.Errorf("expected per-pod CPU rule in query, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"container":"app"},"value":[0,"0.42"]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	got, err := c.QueryCPUByContainer(context.Background(), "ns", "Deployment", "web", 0.95, "168h")
	if err != nil {
		t.Fatalf("QueryCPUByContainer: %v", err)
	}
	if got["app"] != 0.42 {
		t.Errorf("got %v want 0.42", got["app"])
	}
}

func TestQueryMemoryByContainer_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		if !strings.Contains(q, "container_memory_by_workload") {
			t.Errorf("expected per-pod memory rule in query, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"container":"app"},"value":[0,"67108864"]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	got, err := c.QueryMemoryByContainer(context.Background(), "ns", "Deployment", "web", 0.95, "168h")
	if err != nil {
		t.Fatalf("QueryMemoryByContainer: %v", err)
	}
	if got["app"] != 67108864 {
		t.Errorf("got %v want 67108864", got["app"])
	}
}

func TestQueryCPURangeByContainer_ReturnsTimeSeries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"container":"app"},"values":[[1700000000,"0.1"],[1700000060,"0.2"]]},
			{"metric":{"container":""},"values":[[1700000000,"99"]]}
		]}}`))
	}))
	defer server.Close()

	now := time.Now()
	tr := TimeRange{Start: now.Add(-5 * time.Minute), End: now}
	c, _ := New(server.URL)
	got, err := c.QueryCPURangeByContainer(context.Background(), "ns", "Deployment", "web", tr, "1m")
	if err != nil {
		t.Fatalf("QueryCPURangeByContainer: %v", err)
	}
	if _, ok := got[""]; ok {
		t.Error("series with empty container label must be dropped")
	}
	if len(got["app"]) != 2 {
		t.Fatalf("expected 2 points for 'app', got %d", len(got["app"]))
	}
	if got["app"][0].Value != 0.1 || got["app"][1].Value != 0.2 {
		t.Errorf("unexpected values: %+v", got["app"])
	}
}

func TestQueryCPURangeByContainer_BadStep(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer server.Close()

	now := time.Now()
	tr := TimeRange{Start: now.Add(-5 * time.Minute), End: now}
	c, _ := New(server.URL)
	if _, err := c.QueryCPURangeByContainer(context.Background(), "ns", "Deployment", "web", tr, "nope"); err == nil {
		t.Fatal("expected error for malformed step")
	}
}

func TestQueryMemoryRangeByContainer_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		if !strings.Contains(q, "container_memory_by_workload") {
			t.Errorf("expected memory rule in query, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"container":"app"},"values":[[1700000000,"1024"]]}
		]}}`))
	}))
	defer server.Close()
	now := time.Now()
	tr := TimeRange{Start: now.Add(-5 * time.Minute), End: now}
	c, _ := New(server.URL)
	got, err := c.QueryMemoryRangeByContainer(context.Background(), "ns", "Deployment", "web", tr, "1m")
	if err != nil {
		t.Fatalf("QueryMemoryRangeByContainer: %v", err)
	}
	if got["app"][0].Value != 1024 {
		t.Errorf("got %v want 1024", got["app"][0].Value)
	}
}

func TestQueryRequestRange_UsesMaxByContainer(t *testing.T) {
	now := time.Now()
	tr := TimeRange{Start: now.Add(-5 * time.Minute), End: now}
	for _, fn := range []func(server string) error{
		func(addr string) error {
			c, _ := New(addr)
			_, err := c.QueryCPURequestRangeByContainer(context.Background(), "ns", "Deployment", "web", tr, "1m")
			return err
		},
		func(addr string) error {
			c, _ := New(addr)
			_, err := c.QueryMemoryRequestRangeByContainer(context.Background(), "ns", "Deployment", "web", tr, "1m")
			return err
		},
	} {
		var query string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			query = r.Form.Get("query")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		}))
		if err := fn(server.URL); err != nil {
			t.Errorf("query: %v", err)
		}
		if !strings.Contains(query, "max by (container)") {
			t.Errorf("expected 'max by (container)' aggregator in query, got %q", query)
		}
		server.Close()
	}
}

func TestQueryRecommendationRange_AppliesQuantileOverWindow(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		query = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer server.Close()

	now := time.Now()
	tr := TimeRange{Start: now.Add(-24 * time.Hour), End: now}
	c, _ := New(server.URL)
	if _, err := c.QueryCPURecommendationRangeByContainer(context.Background(), "ns", "Deployment", "web", 0.95, "168h", tr, "1h"); err != nil {
		t.Fatalf("QueryCPURecommendationRangeByContainer: %v", err)
	}
	if !strings.Contains(query, "quantile_over_time(0.95") {
		t.Errorf("expected quantile_over_time(0.95) in query, got %q", query)
	}
	if !strings.Contains(query, "[168h]") {
		t.Errorf("expected window [168h] in query, got %q", query)
	}
	if strings.Contains(query, "[168h:1m]") {
		t.Errorf("expected range vector, got subquery form: %q", query)
	}
}

func TestQueryOOMKillEvents_FiltersZeroSamplesAndEmptyContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"container":"app","pod":"pod-1"},"values":[[1700000000,"1"],[1700000060,"0"]]},
			{"metric":{"container":"","pod":"pod-x"},"values":[[1700000000,"5"]]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}, "1m")
	if err != nil {
		t.Fatalf("QueryOOMKillEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (only the >0 sample with non-empty container), got %d: %+v", len(events), events)
	}
	if events[0].Container != "app" || events[0].Pod != "pod-1" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestQueryOOMKillEvents_Dedup(t *testing.T) {
	cases := []struct {
		name   string
		values string
		want   int
	}{
		{
			name: "flat smear collapses to one event",
			values: `[
				[1700000000,"1"],[1700000060,"1"],[1700000120,"1"],
				[1700000180,"1"],[1700000240,"1"],[1700000300,"1"],[1700000360,"1"]
			]`,
			want: 1,
		},
		{
			name: "value bump within same lookback emits a second event",
			values: `[
				[1700000000,"1"],[1700000060,"2"],[1700000120,"2"],
				[1700000180,"1"],[1700000240,"0"]
			]`,
			want: 2,
		},
		{
			name: "distinct bursts separated by zero each emit",
			values: `[
				[1700000000,"1"],[1700000060,"1"],[1700000120,"0"],
				[1700000180,"1"],[1700000240,"1"]
			]`,
			want: 2,
		},
		{
			name: "extrapolation noise stays under the threshold",
			values: `[
				[1700000000,"0.96"],[1700000060,"1.04"],
				[1700000120,"0.98"],[1700000180,"1.02"]
			]`,
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"container":"app","pod":"pod-1"},"values":%s}
			]}}`, tc.values)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			c, _ := New(server.URL)
			events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}, "1m")
			if err != nil {
				t.Fatalf("QueryOOMKillEvents: %v", err)
			}
			if len(events) != tc.want {
				t.Fatalf("got %d events, want %d: %+v", len(events), tc.want, events)
			}
		})
	}
}

func TestQueryOOMKillEvents_DistinctPodsNotDeduped(t *testing.T) {
	// Two different pods OOM'ing at the same instant must both surface.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"container":"app","pod":"pod-1"},"values":[[1700000000,"1"]]},
			{"metric":{"container":"app","pod":"pod-2"},"values":[[1700000000,"1"]]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}, "1m")
	if err != nil {
		t.Fatalf("QueryOOMKillEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (one per pod), got %d: %+v", len(events), events)
	}
}

// Regression: a kube-state-metrics rollout exposes overlapping series for the
// same pod/container with different `instance` labels for a brief window. The
// OOM query joins restarts_total with last_terminated_reason on
// (namespace, pod, container); if the right-hand side carries `instance`,
// Prometheus rejects the join with "many-to-many matching not allowed" and
// the whole range query fails. The handler swallows that error and the chart
// shows zero OOMs. The query must aggregate both sides with `max by` to drop
// scrape-target labels.
func TestQueryOOMKillEvents_QueryDropsScrapeTargetLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		q := r.Form.Get("query")
		if !strings.Contains(q, "max by (namespace, pod, container)") {
			t.Fatalf("OOM query must aggregate KSM scrape-target labels with `max by (namespace, pod, container)`, got: %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	if _, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}, "1m"); err != nil {
		t.Fatalf("QueryOOMKillEvents: %v", err)
	}
}

func TestQueryOOMKillEvents_ServerErrorIsNonFatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	c, _ := New(server.URL)
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}, "1m")
	// Documented contract: this method returns (nil, nil) on error so a missing
	// kube-state-metrics doesn't break dashboard rendering.
	if err != nil {
		t.Errorf("expected nil error on backend failure, got %v", err)
	}
	if events != nil {
		t.Errorf("expected nil events, got %v", events)
	}
}

// Regression: when a CrashLoopBackOff pod's last_terminated_reason stays
// OOMKilled across the whole window, the underlying signal must still
// resolve to one event per restart bump, not collapse to a single event
// for the entire stuck-OOM tail. This is the failure mode that motivated
// dropping the max_over_time(reason==OOMKilled) fallback arm.
func TestQueryOOMKillEvents_CrashLoopCountsEachRestart(t *testing.T) {
	// 3 OOMs within the lookback push `increase()` to 1, 2, 3, then ages
	// back down. A union with max_over_time would have left the signal at
	// a flat 1 throughout and produced just one event.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"container":"app","pod":"pod-1"},"values":[
				[1700000000,"1"],[1700000060,"2"],[1700000120,"3"],
				[1700000180,"3"],[1700000240,"2"],[1700000300,"1"],[1700000360,"0"]
			]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}, "1m")
	if err != nil {
		t.Fatalf("QueryOOMKillEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (one per restart bump), got %d: %+v", len(events), events)
	}
}

// A single positive sample followed by a zero must still surface one event,
// even though the latest sample is no longer positive.
func TestQueryOOMKillEvents_HistoricalOOMEmits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"container":"app","pod":"pod-1"},"values":[[1700000000,"1"],[1700000600,"0"]]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}, "1m")
	if err != nil {
		t.Fatalf("QueryOOMKillEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected the historical OOM marker to survive, got %d events: %+v", len(events), events)
	}
	if events[0].Container != "app" || events[0].Pod != "pod-1" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

// TestQueryWorkloadOOMSignal_ReturnsCountAndPeak covers the single-series
// (no kube-state-metrics duplication) case: one query, one sample per metric
// family, values pass through unchanged. The duplicate-series aggregation
// path (sum for counts, max for peak/limit) is covered separately by
// TestQueryWorkloadOOMSignalSingleQueryAggregatesDuplicates.
func TestQueryWorkloadOOMSignal_ReturnsCountAndPeak(t *testing.T) {
	var (
		queriesMu sync.Mutex
		queries   []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		queriesMu.Lock()
		queries = append(queries, q)
		queriesMu.Unlock()
		if !strings.Contains(q, `__name__=~"`) {
			t.Errorf("expected a combined __name__ regex selector, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"__name__":"k8s_sustain:workload_oom_24h","container":"app"},"value":[0,"3"]},
			{"metric":{"__name__":"k8s_sustain:workload_oom_24h","container":"sidecar"},"value":[0,"1"]},
			{"metric":{"__name__":"k8s_sustain:container_peak_memory_24h:bytes","container":"app"},"value":[0,"209715200"]},
			{"metric":{"__name__":"k8s_sustain:container_oom_limit_24h:bytes","container":"app"},"value":[0,"104857600"]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	sig, err := c.QueryWorkloadOOMSignal(context.Background(), "ns", "Deployment", "web")
	if err != nil {
		t.Fatalf("QueryWorkloadOOMSignal: %v", err)
	}
	if sig.OOMCounts["app"] != 3 || sig.OOMCounts["sidecar"] != 1 {
		t.Errorf("OOMCounts: got %v want app=3 sidecar=1", sig.OOMCounts)
	}
	if sig.TotalOOMs() != 4 {
		t.Errorf("TotalOOMs: got %v want 4", sig.TotalOOMs())
	}
	if sig.PeakMemoryBytes["app"] != 209715200 {
		t.Errorf("peak[app]: got %v want 209715200", sig.PeakMemoryBytes["app"])
	}
	if sig.OOMLimitBytes["app"] != 104857600 {
		t.Errorf("oom-limit[app]: got %v want 104857600", sig.OOMLimitBytes["app"])
	}
	queriesMu.Lock()
	n := len(queries)
	queriesMu.Unlock()
	if n != 1 {
		t.Errorf("expected exactly 1 query (all three metric families in one), got %d", n)
	}
}

func TestQueryWorkloadOOMSignal_NoOOMReturnsZeroCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	sig, err := c.QueryWorkloadOOMSignal(context.Background(), "ns", "Deployment", "web")
	if err != nil {
		t.Fatalf("QueryWorkloadOOMSignal: %v", err)
	}
	if sig.TotalOOMs() != 0 {
		t.Errorf("expected 0 total OOM count, got %v", sig.TotalOOMs())
	}
	if len(sig.OOMCounts) != 0 {
		t.Errorf("expected empty OOM count map, got %v", sig.OOMCounts)
	}
	if len(sig.PeakMemoryBytes) != 0 {
		t.Errorf("expected empty peak map, got %v", sig.PeakMemoryBytes)
	}
}

// breakerFailures reads the breaker's failure counter under its mutex.
func breakerFailures(c *Client) int {
	c.breaker.mu.Lock()
	defer c.breaker.mu.Unlock()
	return c.breaker.failures
}

// hangingOOMServer returns an httptest server whose handler blocks until its
// request context is cancelled or the returned release channel is closed. The
// caller MUST close release at test end so blocked handler goroutines unwind
// and goleak stays happy.
func hangingOOMServer(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	return server, release
}

// TestExecInstant_GenuineErrorCountsOne asserts that a genuine Prometheus
// error (server 500, per-call context still live) on the execInstant path
// records exactly one breaker failure — unchanged from the old behaviour.
func TestExecInstant_GenuineErrorCountsOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"boom"}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryWorkloadCPUByContainer(context.Background(), "ns", "Deployment", "web", 0.9, "7d")
	if err == nil {
		t.Fatal("expected an error from the failing query")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a genuine query error, got %d", got)
	}
}

// TestExecInstant_PerCallTimeoutCountsOne asserts that the per-call
// queryTimeout deadline firing (a genuinely stalled Prometheus) on the
// execInstant path still records one breaker failure — the cause is
// context.DeadlineExceeded, a real failure attributable to Prometheus.
func TestExecInstant_PerCallTimeoutCountsOne(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL, WithQueryTimeout(50*time.Millisecond))
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryWorkloadCPUByContainer(context.Background(), "ns", "Deployment", "web", 0.9, "7d")
	if err == nil {
		t.Fatal("expected an error from the per-call timeout")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a per-call timeout, got %d", got)
	}
}

// TestExecInstant_ParentCancelCountsOne asserts that cancelling the parent
// context (a plain context.WithCancel, cause context.Canceled) while a query
// is in flight on the execInstant path still records exactly one breaker
// failure. This is a distinct branch from TestExecInstant_PerCallTimeoutCountsOne
// (cause context.DeadlineExceeded) and from
// TestExecInstant_OuterSiblingCancelCountsZero (cause a non-context sibling
// error) — recordQueryFailure treats DeadlineExceeded and Canceled as the same
// real-failure case, but that is exactly the assumption this test pins down.
//
// The handler signals `received` once the request lands, and the cancelling
// goroutine waits on that channel instead of sleeping — the cancel is
// guaranteed to land only once the request is actually in flight, so the test
// is deterministic under -race -shuffle=on.
func TestExecInstant_ParentCancelCountsOne(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(received)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-received
		cancel()
	}()

	_, err := c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "web", 0.9, "7d")
	if err == nil {
		t.Fatal("expected an error from the parent-context cancellation")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a parent cancellation, got %d", got)
	}
}

// TestExecInstant_OuterSiblingCancelCountsZero asserts that when an errgroup
// sibling fails (these queries run inside the FetchWorkloadInputs errgroup in
// recommender/build.go), the collateral cancellation — whose cause is a
// NON-context error propagated through the WithTimeout child — records ZERO
// breaker failures on the execInstant path. The outage belongs to the sibling,
// which already counted its own failure. Before the fix every error counted,
// so one Prometheus outage recorded 2-3 failures per reconcile.
func TestExecInstant_OuterSiblingCancelCountsZero(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	siblingErr := errors.New("errgroup sibling failed")
	ctx, cancelCause := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancelCause(siblingErr)
	}()
	defer cancelCause(nil)

	_, err := c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "web", 0.9, "7d")
	if err == nil {
		t.Fatal("expected an error from the sibling cancellation")
	}
	if got := breakerFailures(c); got != 0 {
		t.Fatalf("expected 0 breaker failures for sibling collateral cancellation, got %d", got)
	}
}

// TestRunRange_GenuineErrorCountsOne is the runRange-path twin of
// TestExecInstant_GenuineErrorCountsOne.
func TestRunRange_GenuineErrorCountsOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"boom"}`))
	}))
	defer server.Close()

	now := time.Now()
	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryCPURangeByContainer(context.Background(), "ns", "Deployment", "web", tr, "1m")
	if err == nil {
		t.Fatal("expected an error from the failing range query")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a genuine range-query error, got %d", got)
	}
}

// TestRunRange_PerCallTimeoutCountsOne is the runRange-path twin of
// TestExecInstant_PerCallTimeoutCountsOne.
func TestRunRange_PerCallTimeoutCountsOne(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	now := time.Now()
	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	c, _ := New(server.URL, WithQueryTimeout(50*time.Millisecond))
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryCPURangeByContainer(context.Background(), "ns", "Deployment", "web", tr, "1m")
	if err == nil {
		t.Fatal("expected an error from the per-call timeout")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a range-query timeout, got %d", got)
	}
}

// TestRunRange_OuterSiblingCancelCountsZero is the runRange-path twin of
// TestExecInstant_OuterSiblingCancelCountsZero.
func TestRunRange_OuterSiblingCancelCountsZero(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	siblingErr := errors.New("errgroup sibling failed")
	ctx, cancelCause := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancelCause(siblingErr)
	}()
	defer cancelCause(nil)

	now := time.Now()
	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	_, err := c.QueryCPURangeByContainer(ctx, "ns", "Deployment", "web", tr, "1m")
	if err == nil {
		t.Fatal("expected an error from the sibling cancellation")
	}
	if got := breakerFailures(c); got != 0 {
		t.Fatalf("expected 0 breaker failures for sibling collateral cancellation, got %d", got)
	}
}

// TestErrgroupSiblings_SingleOutageCountsOnce reproduces the FetchWorkloadInputs
// shape (recommender/build.go): two queries share an errgroup, one genuinely
// fails (500) and the sibling hangs until the group cancels it. The genuine
// failure counts one breaker failure; the cancelled sibling counts zero —
// before the fix the sibling's context.Canceled also counted, so one outage
// recorded a failure per in-flight query.
func TestErrgroupSiblings_SingleOutageCountsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.Contains(r.Form.Get("query"), "workload_max_pod_cpu"):
			// The genuine, independent failure.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"boom"}`))
		default:
			// Sibling query: block until the errgroup cancels gctx.
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	g, gctx := errgroup.WithContext(context.Background())
	g.Go(func() error {
		_, err := c.QueryWorkloadCPUByContainer(gctx, "ns", "Deployment", "web", 0.9, "7d")
		return err
	})
	g.Go(func() error {
		_, err := c.QueryWorkloadMemoryByContainer(gctx, "ns", "Deployment", "web", 0.9, "7d")
		return err
	})
	if err := g.Wait(); err == nil {
		t.Fatal("expected an error from the failing cpu query")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a single outage across errgroup siblings, got %d", got)
	}
}

func TestPing_OK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestPing_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	c, _ := New(server.URL)
	if err := c.Ping(context.Background()); err == nil {
		t.Error("expected error on 500")
	}
}

func TestQuantileOverTimeExprUsesRangeVectorNotSubquery(t *testing.T) {
	got := quantileOverTimeExpr(0.95, "k8s_sustain:workload_max_pod_cpu:cores",
		`{namespace="prod",owner_kind="Deployment",owner_name="api"}`, "7d")

	want := `quantile_over_time(0.95, k8s_sustain:workload_max_pod_cpu:cores{namespace="prod",owner_kind="Deployment",owner_name="api"}[7d])`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if strings.Contains(got, ":1m]") {
		t.Fatalf("expression still contains a 1m subquery step: %s", got)
	}
}

// Three metric families arrive in one vector, with DUPLICATE series per
// container simulating two kube-state-metrics replicas. Counts must sum,
// peaks and limits must take the max — matching the server-side aggregation
// the three separate probes used to perform.
func TestQueryWorkloadOOMSignalSingleQueryAggregatesDuplicates(t *testing.T) {
	var queries []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		mu.Lock()
		queries = append(queries, r.Form.Get("query"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"__name__":"k8s_sustain:workload_oom_24h","container":"app","instance":"ksm-a"},"value":[1700000000,"2"]},
			{"metric":{"__name__":"k8s_sustain:workload_oom_24h","container":"app","instance":"ksm-b"},"value":[1700000000,"3"]},
			{"metric":{"__name__":"k8s_sustain:container_peak_memory_24h:bytes","container":"app","instance":"ksm-a"},"value":[1700000000,"1000"]},
			{"metric":{"__name__":"k8s_sustain:container_peak_memory_24h:bytes","container":"app","instance":"ksm-b"},"value":[1700000000,"2000"]},
			{"metric":{"__name__":"k8s_sustain:container_oom_limit_24h:bytes","container":"app","instance":"ksm-a"},"value":[1700000000,"4096"]},
			{"metric":{"__name__":"k8s_sustain:container_peak_memory_24h:bytes","container":"sidecar"},"value":[1700000000,"0"]}
		]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := c.QueryWorkloadOOMSignal(context.Background(), "prod", "Deployment", "api")
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	n := len(queries)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expected exactly 1 query, got %d: %v", n, queries)
	}

	if got := sig.OOMCounts["app"]; got != 5 {
		t.Fatalf("OOMCounts[app]: got %v want 5 (sum of 2+3)", got)
	}
	if got := sig.PeakMemoryBytes["app"]; got != 2000 {
		t.Fatalf("PeakMemoryBytes[app]: got %v want 2000 (max of 1000,2000)", got)
	}
	if got := sig.OOMLimitBytes["app"]; got != 4096 {
		t.Fatalf("OOMLimitBytes[app]: got %v want 4096", got)
	}
	// A legitimately-zero peak must still be PRESENT — ComputeContainerRec
	// gates memory emission on key presence, not on the value.
	if _, ok := sig.PeakMemoryBytes["sidecar"]; !ok {
		t.Fatal("PeakMemoryBytes[sidecar] missing: zero-valued samples must retain key presence")
	}
}

// A query that never reached Prometheus because it was cancelled while queued
// behind the in-flight semaphore must be distinguishable from one that ran and
// failed — they call for opposite operator responses — and must NOT count as a
// breaker failure, since a self-imposed wait says nothing about Prometheus.
func TestAcquireAbortWrapsSentinelAndSkipsBreaker(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	// Single slot, occupied by a query parked in the handler.
	c, err := New(server.URL, WithMaxInflight(1))
	if err != nil {
		t.Fatal(err)
	}
	var g errgroup.Group
	g.Go(func() error {
		_, qErr := c.QueryInstant(context.Background(), "up")
		return qErr
	})
	<-arrived // the slot is now genuinely held

	// This caller can never get a slot; its context is already done, so it
	// aborts in acquire rather than reaching Prometheus.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.QueryInstant(ctx, "up")

	if !errors.Is(err, ErrInflightQueueAborted) {
		t.Fatalf("expected ErrInflightQueueAborted, got %v", err)
	}
	// The underlying context cause must survive the wrapping too.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped context.Canceled, got %v", err)
	}
	if got := breakerFailures(c); got != 0 {
		t.Fatalf("breaker recorded %d failures; a self-imposed queue abort must record none", got)
	}

	// Unpark the held query so the goroutine and server shut down cleanly
	// (goleak runs in this package).
	close(release)
	if err := g.Wait(); err != nil {
		t.Fatalf("parked query: %v", err)
	}
}

// oomSignalSelector interpolates metric names into an RE2 alternation without
// escaping them. That is only safe while every name is composed of RE2
// literals. Renaming a recording rule to include a metacharacter would widen
// what the selector matches — silently, since nothing else would fail. This
// pins the invariant so such a rename breaks here instead of in production.
func TestOOMSignalSelectorUsesLiteralAlternation(t *testing.T) {
	for _, name := range oomMetricNames {
		if got := regexp.QuoteMeta(name); got != name {
			t.Errorf("metric name %q contains RE2 metacharacters (quoted form %q); "+
				"oomSignalSelector interpolates it unescaped and would match more than intended",
				name, got)
		}
	}

	// The rendered selector must be an exact literal alternation of the three
	// names, anchored inside the __name__ matcher.
	got := oomSignalSelector("prod", "Deployment", "api")
	want := `{__name__=~"` + strings.Join(oomMetricNames, "|") + `",namespace="prod",owner_kind="Deployment",owner_name="api"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// TestWithMaxInflightBoundsConcurrentQueries confirms WithMaxInflight caps the
// number of requests the client has open against Prometheus at once, even
// when far more callers are trying to query concurrently.
//
// Deterministic by construction rather than by sleep: the handler reports its
// arrival on a buffered "arrived" channel before blocking on "release". The
// test drains exactly `limit` arrivals — proving `limit` requests are
// in-flight simultaneously — before closing "release" to let them all
// complete. Any (limit+1)-th request that reached the handler before the
// first `limit` finished would show up as an extra send on "arrived", which
// the fixed-size receive loop below would not drain, deadlocking the test
// instead of silently passing. A sleep-based version of this test would be a
// flake risk under -race/-shuffle and CI load; this version cannot flake on
// timing because it never depends on how long anything takes, only on how
// many requests reach the handler before "release" closes.
func TestWithMaxInflightBoundsConcurrentQueries(t *testing.T) {
	const limit = 2
	const callers = 8

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	arrived := make(chan struct{}, callers)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, WithMaxInflight(limit))
	if err != nil {
		t.Fatal(err)
	}

	var g errgroup.Group
	for i := 0; i < callers; i++ {
		g.Go(func() error {
			_, qErr := c.QueryInstant(context.Background(), "up")
			return qErr
		})
	}

	// Wait for exactly `limit` requests to reach the handler. Since the
	// semaphore admits at most `limit` at a time and none can finish until
	// "release" closes, this proves peak in-flight has reached `limit`
	// without racing on inFlight/peak directly.
	for i := 0; i < limit; i++ {
		<-arrived
	}
	close(release)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > limit {
		t.Fatalf("peak concurrent requests %d exceeded limit %d", peak, limit)
	}
	if peak < limit {
		t.Fatalf("peak concurrent requests %d never reached limit %d — semaphore may be over-restrictive", peak, limit)
	}
}

// The queue wait must be bounded by the CLIENT, not only by the caller's
// context. The controller's reconcile context carries no deadline, so before
// this bound existed a saturated semaphore parked the goroutine forever and
// ErrInflightQueueAborted was unreachable for exactly the caller that needed it
// most. The pre-existing abort test passes a context that is already cancelled,
// so it never exercised this case.
func TestAcquire_BoundsWaitWhenCallerHasNoDeadline(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()
	defer close(release)

	c, err := New(server.URL, WithMaxInflight(1), WithQueueTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	// Occupy the only slot with a query parked in the handler.
	go func() { _, _ = c.QueryInstant(context.Background(), "up") }()
	<-arrived

	// A caller with NO deadline of its own — the controller's shape.
	done := make(chan error, 1)
	go func() {
		_, qErr := c.QueryInstant(context.Background(), "up")
		done <- qErr
	}()

	select {
	case qErr := <-done:
		if !errors.Is(qErr, ErrInflightQueueAborted) {
			t.Fatalf("expected ErrInflightQueueAborted, got %v", qErr)
		}
		if !errors.Is(qErr, context.DeadlineExceeded) {
			t.Errorf("expected the wrapped cause to be DeadlineExceeded, got %v", qErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a deadline-less caller queued behind a saturated semaphore never returned: " +
			"the queue wait must be bounded by the client, not only by the caller's context")
	}

	if got := breakerFailures(c); got != 0 {
		t.Errorf("breaker recorded %d failures; a self-imposed queue abort must record none", got)
	}
}

// The breaker is checked before queuing, and the queue can be long. A query
// that waited while Prometheus went down should not spend a slot on a call
// already known to fail. The re-check must also not consume the half-open
// probe, and must not leak the slot it just took.
func TestAcquire_RejectsWhenBreakerIsOpenAfterQueuing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, WithMaxInflight(1))
	if err != nil {
		t.Fatal(err)
	}
	for range defaultBreakerMaxFailures {
		c.breaker.failure()
	}

	// holdsProbe=false: a caller that passed allow() while the circuit was
	// closed and then queued while it tripped. This is the caller the re-check
	// was written for, and it must still bail.
	if _, err := c.acquire(context.Background(), false); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("acquire with an open breaker: got %v, want ErrCircuitOpen", err)
	}
	if len(c.sem) != 0 {
		t.Error("acquire leaked its in-flight slot when rejecting on an open breaker")
	}
	// The rejection must be an observation, not a claim: the probe a cooldown
	// expiry grants has to still be available to a real caller.
	if !c.breaker.isOpen() {
		t.Error("the re-check consumed the half-open probe; it must observe, not claim")
	}
}

// The counterpart to the test above, and the half that keeps the circuit able
// to close: the caller holding the half-open probe must NOT be rejected by the
// deadline its own allow() just advanced. Rejecting it means the probe never
// reaches Prometheus, success() is never called, and the circuit is open
// forever.
func TestAcquire_AdmitsTheHalfOpenProbeHolder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, WithMaxInflight(1))
	if err != nil {
		t.Fatal(err)
	}
	for range defaultBreakerMaxFailures {
		c.breaker.failure()
	}
	// The circuit is open and, after allow() hands out the probe, openUntil is
	// advanced — so isOpen() is true for the probe holder by construction.
	if !c.breaker.isOpen() {
		t.Fatal("precondition: the circuit must be open")
	}

	release, err := c.acquire(context.Background(), true)
	if err != nil {
		t.Fatalf("acquire must admit the half-open probe holder, got %v", err)
	}
	release()
	if len(c.sem) != 0 {
		t.Error("release did not return the in-flight slot")
	}
}
