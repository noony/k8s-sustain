package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	c, _ := New(server.URL)
	out, err := c.QueryRange(context.Background(), "anything", "5m", "1m")
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

	c, _ := New(server.URL)
	got, err := c.QueryCPURangeByContainer(context.Background(), "ns", "Deployment", "web", "5m", "1m")
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

func TestQueryCPURangeByContainer_BadWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	if _, err := c.QueryCPURangeByContainer(context.Background(), "ns", "Deployment", "web", "not-a-duration", "1m"); err == nil {
		t.Fatal("expected error for malformed window")
	}
	if _, err := c.QueryCPURangeByContainer(context.Background(), "ns", "Deployment", "web", "5m", "nope"); err == nil {
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
	c, _ := New(server.URL)
	got, err := c.QueryMemoryRangeByContainer(context.Background(), "ns", "Deployment", "web", "5m", "1m")
	if err != nil {
		t.Fatalf("QueryMemoryRangeByContainer: %v", err)
	}
	if got["app"][0].Value != 1024 {
		t.Errorf("got %v want 1024", got["app"][0].Value)
	}
}

func TestQueryRequestRange_UsesMaxByContainer(t *testing.T) {
	for _, fn := range []func(server string) error{
		func(addr string) error {
			c, _ := New(addr)
			_, err := c.QueryCPURequestRangeByContainer(context.Background(), "ns", "Deployment", "web", "5m", "1m")
			return err
		},
		func(addr string) error {
			c, _ := New(addr)
			_, err := c.QueryMemoryRequestRangeByContainer(context.Background(), "ns", "Deployment", "web", "5m", "1m")
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

	c, _ := New(server.URL)
	if _, err := c.QueryCPURecommendationRangeByContainer(context.Background(), "ns", "Deployment", "web", 0.95, "168h", "24h", "1h"); err != nil {
		t.Fatalf("QueryCPURecommendationRangeByContainer: %v", err)
	}
	if !strings.Contains(query, "quantile_over_time(0.95") {
		t.Errorf("expected quantile_over_time(0.95) in query, got %q", query)
	}
	if !strings.Contains(query, "[168h:1m]") {
		t.Errorf("expected window [168h:1m] in query, got %q", query)
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
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", "1h", "1m")
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
			events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", "1h", "1m")
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
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", "1h", "1m")
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
	if _, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", "1h", "1m"); err != nil {
		t.Fatalf("QueryOOMKillEvents: %v", err)
	}
}

func TestQueryOOMKillEvents_ServerErrorIsNonFatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	c, _ := New(server.URL)
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", "1h", "1m")
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
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", "1h", "1m")
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
	events, err := c.QueryOOMKillEvents(context.Background(), "ns", "Deployment", "web", "1h", "1m")
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

func TestQueryWorkloadOOMSignal_ReturnsCountAndPeak(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		queries = append(queries, q)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_oom_24h"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"3"]}]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"209715200"]}]}}`))
		case strings.Contains(q, "container_oom_limit_24h:bytes"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"104857600"]}]}}`))
		default:
			t.Errorf("unexpected query: %q", q)
		}
	}))
	defer server.Close()

	c, _ := New(server.URL)
	sig, err := c.QueryWorkloadOOMSignal(context.Background(), "ns", "Deployment", "web")
	if err != nil {
		t.Fatalf("QueryWorkloadOOMSignal: %v", err)
	}
	if sig.OOMCount != 3 {
		t.Errorf("OOMCount: got %v want 3", sig.OOMCount)
	}
	if sig.PeakMemoryBytes["app"] != 209715200 {
		t.Errorf("peak[app]: got %v want 209715200", sig.PeakMemoryBytes["app"])
	}
	if sig.OOMLimitBytes["app"] != 104857600 {
		t.Errorf("oom-limit[app]: got %v want 104857600", sig.OOMLimitBytes["app"])
	}
	if len(queries) != 3 {
		t.Errorf("expected 3 queries (oom + peak + oom-limit), got %d", len(queries))
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
	if sig.OOMCount != 0 {
		t.Errorf("expected 0 OOM count, got %v", sig.OOMCount)
	}
	if len(sig.PeakMemoryBytes) != 0 {
		t.Errorf("expected empty peak map, got %v", sig.PeakMemoryBytes)
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
