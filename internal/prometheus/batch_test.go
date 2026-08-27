package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestQueryShardCPUReturnsPerIdentityValues is the batched counterpart of
// TestQueryWorkloadCPUByContainer: one round trip carries two distinct
// workloads (same namespace/owner_kind, different owner_name — the only
// dimension a Shard varies on), and the two must decode into separate
// IdentityValues entries without bleeding into each other.
func TestQueryShardCPUReturnsPerIdentityValues(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		query = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[0,"0.5"]},
			{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"web","container":"app"},"value":[0,"1.5"]}
		]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shard := Shard{Namespace: "prod", OwnerKind: "Deployment", Names: []string{"api", "web"}}
	got, err := c.QueryShardCPU(context.Background(), shard, 0.95, "168h")
	if err != nil {
		t.Fatalf("QueryShardCPU: %v", err)
	}

	apiID := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}
	webID := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}
	if got[apiID]["app"] != 0.5 {
		t.Errorf("api/app: got %v want 0.5", got[apiID]["app"])
	}
	if got[webID]["app"] != 1.5 {
		t.Errorf("web/app: got %v want 1.5 (must not bleed from api)", got[webID]["app"])
	}

	if !strings.Contains(query, "workload_max_pod_cpu") {
		t.Errorf("expected workload_max_pod_cpu rule in query, got %q", query)
	}
	if !strings.Contains(query, "api|web") {
		t.Errorf("expected name alternation \"api|web\" in query, got %q", query)
	}
	if strings.Contains(query, ":1m]") {
		t.Errorf("query regressed to a :1m subquery step: %q", query)
	}
}

// TestQueryShardMemoryReturnsPerIdentityValues mirrors the CPU test for the
// memory recording rule, confirming it also goes through vectorToIdentityValues
// and does not regress to a :1m subquery.
func TestQueryShardMemoryReturnsPerIdentityValues(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		query = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[0,"104857600"]},
			{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"web","container":"app"},"value":[0,"209715200"]}
		]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shard := Shard{Namespace: "prod", OwnerKind: "Deployment", Names: []string{"api", "web"}}
	got, err := c.QueryShardMemory(context.Background(), shard, 0.95, "168h")
	if err != nil {
		t.Fatalf("QueryShardMemory: %v", err)
	}

	apiID := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}
	webID := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}
	if got[apiID]["app"] != 104857600 {
		t.Errorf("api/app: got %v want 104857600", got[apiID]["app"])
	}
	if got[webID]["app"] != 209715200 {
		t.Errorf("web/app: got %v want 209715200 (must not bleed from api)", got[webID]["app"])
	}

	if !strings.Contains(query, "workload_max_pod_memory") {
		t.Errorf("expected workload_max_pod_memory rule in query, got %q", query)
	}
	if strings.Contains(query, ":1m]") {
		t.Errorf("query regressed to a :1m subquery step: %q", query)
	}
}

// TestQueryShardOOMSignalFoldsPerIdentityIndependently is the batched
// counterpart of TestQueryWorkloadOOMSignalSingleQueryAggregatesDuplicates:
// workload A gets duplicate OOM-count series (two kube-state-metrics replicas)
// that must SUM into one value, workload B gets only a peak-memory series, and
// neither may inherit the other's data — proving the partition-then-fold in
// QueryShardOOMSignal keys strictly on identity before calling foldOOMVector.
func TestQueryShardOOMSignalFoldsPerIdentityIndependently(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"__name__":"k8s_sustain:workload_oom_24h","namespace":"prod","owner_kind":"Deployment","owner_name":"a","container":"app","instance":"ksm-1"},"value":[0,"2"]},
			{"metric":{"__name__":"k8s_sustain:workload_oom_24h","namespace":"prod","owner_kind":"Deployment","owner_name":"a","container":"app","instance":"ksm-2"},"value":[0,"3"]},
			{"metric":{"__name__":"k8s_sustain:container_peak_memory_24h:bytes","namespace":"prod","owner_kind":"Deployment","owner_name":"b","container":"app"},"value":[0,"2048"]}
		]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shard := Shard{Namespace: "prod", OwnerKind: "Deployment", Names: []string{"a", "b"}}
	got, err := c.QueryShardOOMSignal(context.Background(), shard)
	if err != nil {
		t.Fatalf("QueryShardOOMSignal: %v", err)
	}

	aID := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "a"}
	bID := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "b"}

	sigA, ok := got[aID]
	if !ok {
		t.Fatalf("missing signal for identity a: %+v", got)
	}
	if sigA.OOMCounts["app"] != 5 {
		t.Errorf("a/app OOMCounts: got %v want 5 (sum of 2+3)", sigA.OOMCounts["app"])
	}
	if len(sigA.PeakMemoryBytes) != 0 {
		t.Errorf("a must not inherit b's peak memory: %+v", sigA.PeakMemoryBytes)
	}

	sigB, ok := got[bID]
	if !ok {
		t.Fatalf("missing signal for identity b: %+v", got)
	}
	if sigB.PeakMemoryBytes["app"] != 2048 {
		t.Errorf("b/app PeakMemoryBytes: got %v want 2048", sigB.PeakMemoryBytes["app"])
	}
	if len(sigB.OOMCounts) != 0 {
		t.Errorf("b must not inherit a's OOM counts: %+v", sigB.OOMCounts)
	}
}

// TestQueryShardEscapesDottedNamesInBothQueries is the regression guard for
// the escaping trap: a shard containing a dotted owner name must produce a
// query with the doubled-backslash RE2 form for BOTH the percentile queries
// and the OOM query. If a second, independent selector builder ever
// reappears for the OOM path, it will join shard.Names raw and this test
// will catch the un-escaped single-backslash (or bare dot) form.
func TestQueryShardEscapesDottedNamesInBothQueries(t *testing.T) {
	const wantAlternation = `owner_name=~"payments\\.worker"`

	var cpuQuery, oomQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		q := r.Form.Get("query")
		if strings.Contains(q, "__name__") {
			oomQuery = q
		} else {
			cpuQuery = q
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shard := Shard{Namespace: "prod", OwnerKind: "Deployment", Names: []string{"payments.worker"}}

	if _, err := c.QueryShardCPU(context.Background(), shard, 0.95, "168h"); err != nil {
		t.Fatalf("QueryShardCPU: %v", err)
	}
	if !strings.Contains(cpuQuery, wantAlternation) {
		t.Errorf("CPU query missing escaped alternation %q, got %q", wantAlternation, cpuQuery)
	}

	if _, err := c.QueryShardOOMSignal(context.Background(), shard); err != nil {
		t.Fatalf("QueryShardOOMSignal: %v", err)
	}
	if !strings.Contains(oomQuery, wantAlternation) {
		t.Errorf("OOM query missing escaped alternation %q, got %q", wantAlternation, oomQuery)
	}
}

// QueryShardOOMSignal returns a SPARSE map: only identities with actual OOM
// samples appear. Most workloads never OOM, so for a realistic shard the map
// is nearly empty — and a consumer that treats a missing key as a failed
// lookup rather than as "no OOM signal" would break recommendations for the
// overwhelming majority of workloads. Pin the contract so the next consumer
// cannot get it wrong on the basis of an assumption.
func TestQueryShardOOMSignalReturnsSparseMap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Three workloads in the shard; only "api" has any OOM history.
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"__name__":"k8s_sustain:workload_oom_24h","namespace":"prod","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[1700000000,"1"]}
		]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	shard := Shard{
		Namespace: "prod",
		OwnerKind: "Deployment",
		Names:     []string{"api", "web", "worker"},
	}
	out, err := c.QueryShardOOMSignal(context.Background(), shard)
	if err != nil {
		t.Fatal(err)
	}

	if len(out) != 1 {
		t.Fatalf("got %d entries want 1: the map must be sparse, not padded to shard size (%+v)", len(out), out)
	}
	// The absent identities must read as a usable zero value, not panic and
	// not require the caller to check presence first.
	quiet := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}
	if got := out[quiet].TotalOOMs(); got != 0 {
		t.Fatalf("absent identity TotalOOMs: got %v want 0", got)
	}
	if out[quiet].PeakMemoryBytes != nil {
		t.Fatalf("absent identity should yield the zero OOMSignal, got %+v", out[quiet])
	}
}
