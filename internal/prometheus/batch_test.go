package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
)

// QueryShard sends the expression as given, evaluated as of the instant it is
// handed, and returns one sample per series, each attributed to its own
// identity.
func TestQueryShardReturnsSamplesPerIdentity(t *testing.T) {
	const expr = `k8s_sustain:workload_max_pod_cpu:cores{namespace="prod"}`
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var query, evaluated string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		query, evaluated = r.Form.Get("query"), r.Form.Get("time")
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
	got, err := c.QueryShard(context.Background(), expr, at)
	if err != nil {
		t.Fatalf("QueryShard: %v", err)
	}

	if query != expr {
		t.Errorf("query = %q, want %q", query, expr)
	}
	if want := strconv.FormatInt(at.Unix(), 10); evaluated != want {
		t.Errorf("time = %q, want %q", evaluated, want)
	}
	want := []ShardSample{
		{Identity: WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}, Container: "app", Value: 0.5},
		{Identity: WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}, Container: "app", Value: 1.5},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// QueryShardRange sends the expression as given over the range at the step it
// is handed, and returns one series per identity and container, its points
// at their instants; a series it cannot attribute is dropped.
func TestQueryShardRangeReturnsSeriesPerIdentity(t *testing.T) {
	const expr = `k8s_sustain:workload_max_pod_cpu:cores{namespace="prod"}`
	end := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := TimeRange{Start: end.Add(-time.Minute), End: end}
	var path string
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		path, form = req.URL.Path, req.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"api","container":"app"},"values":[[%[1]d,"0.5"],[%[2]d,"0.7"]]},
			{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"web","container":"app"},"values":[[%[2]d,"1.5"]]},
			{"metric":{"namespace":"prod","owner_kind":"Deployment","container":"app"},"values":[[%[2]d,"9"]]}
		]}}`, r.Start.Unix(), end.Unix())
	}))
	defer server.Close()
	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := c.QueryShardRange(context.Background(), expr, r, time.Minute)
	if err != nil {
		t.Fatalf("QueryShardRange: %v", err)
	}

	if path != "/api/v1/query_range" || form.Get("query") != expr || form.Get("start") != strconv.FormatInt(r.Start.Unix(), 10) ||
		form.Get("end") != strconv.FormatInt(end.Unix(), 10) || form.Get("step") != "60" {
		t.Errorf("request %s %v, want a range query of the expression over the range every 60s", path, form)
	}
	want := []ShardSeries{
		{
			Identity: WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}, Container: "app",
			Points: []TimeValue{{Timestamp: r.Start, Value: 0.5}, {Timestamp: end, Value: 0.7}},
		},
		{
			Identity: WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}, Container: "app",
			Points: []TimeValue{{Timestamp: end, Value: 1.5}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}
