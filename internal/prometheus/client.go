package prometheus

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// ContainerValues maps container name → metric value (cores for CPU, bytes for memory).
type ContainerValues map[string]float64

// Client wraps the Prometheus HTTP API for k8s-sustain queries.
type Client struct {
	api prometheusv1.API
}

// New creates a Prometheus client targeting addr (e.g. "http://prometheus:9090").
func New(addr string) (*Client, error) {
	c, err := api.NewClient(api.Config{Address: addr})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	return &Client{api: prometheusv1.NewAPI(c)}, nil
}

// QueryCPUByContainer returns per-container CPU usage (cores) at the given quantile,
// averaged across pods of the workload, over the specified window.
// Relies on the k8s_sustain:container_cpu_usage_by_workload:rate5m recording rule.
func (c *Client) QueryCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := fmt.Sprintf(
		`avg by (container) (quantile_over_time(%.2f, k8s_sustain:container_cpu_usage_by_workload:rate5m{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m]))`,
		quantile, namespace, ownerKind, ownerName, window,
	)
	return c.queryByContainer(ctx, expr)
}

// QueryMemoryByContainer returns per-container memory working set (bytes) at the given quantile,
// averaged across pods of the workload, over the specified window.
// Relies on the k8s_sustain:container_memory_by_workload:bytes recording rule.
func (c *Client) QueryMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := fmt.Sprintf(
		`avg by (container) (quantile_over_time(%.2f, k8s_sustain:container_memory_by_workload:bytes{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m]))`,
		quantile, namespace, ownerKind, ownerName, window,
	)
	return c.queryByContainer(ctx, expr)
}

func (c *Client) queryByContainer(ctx context.Context, expr string) (ContainerValues, error) {
	result, _, err := c.api.Query(ctx, expr, time.Now())
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", expr, err)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T", result)
	}

	values := make(ContainerValues, len(vector))
	for _, sample := range vector {
		name := string(sample.Metric["container"])
		if name != "" {
			values[name] = float64(sample.Value)
		}
	}
	return values, nil
}
