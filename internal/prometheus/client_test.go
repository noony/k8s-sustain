package prometheus

import (
	"context"
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
		if !strings.Contains(q, "workload_cpu_usage") {
			t.Errorf("expected workload_cpu_usage in query, got %q", q)
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
		if !strings.Contains(q, "workload_memory_usage") {
			t.Errorf("expected workload_memory_usage in query, got %q", q)
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

func TestQueryReplicaCountMedian(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		q := r.Form.Get("query")
		if !strings.Contains(q, "workload_replicas") {
			t.Errorf("expected workload_replicas in query, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"success",
			"data":{"resultType":"vector","result":[{"metric":{},"value":[0,"4"]}]}
		}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.QueryReplicaCountMedian(context.Background(), "ns", "Deployment", "web", "168h")
	if err != nil {
		t.Fatalf("QueryReplicaCountMedian: %v", err)
	}
	if got != 4 {
		t.Errorf("expected 4 got %v", got)
	}
}

func TestQueryReplicaCountMedian_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.QueryReplicaCountMedian(context.Background(), "ns", "Deployment", "web", "168h")
	if err != nil {
		t.Fatalf("QueryReplicaCountMedian empty: %v", err)
	}
	if got != 0 {
		t.Errorf("expected 0 for empty result, got %v", got)
	}
}
