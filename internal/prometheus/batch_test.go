package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// QueryShard sends the expression as given and returns one sample per series,
// each attributed to its own identity.
func TestQueryShardReturnsSamplesPerIdentity(t *testing.T) {
	const expr = `k8s_sustain:workload_max_pod_cpu:cores{namespace="prod"}`
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
	got, err := c.QueryShard(context.Background(), expr)
	if err != nil {
		t.Fatalf("QueryShard: %v", err)
	}

	if query != expr {
		t.Errorf("query = %q, want %q", query, expr)
	}
	want := []ShardSample{
		{Identity: WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}, Container: "app", Value: 0.5},
		{Identity: WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}, Container: "app", Value: 1.5},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}
