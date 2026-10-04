package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

var identityHealthMetrics = []string{
	"k8s_sustain_workload_pods",
	"k8s_sustain_workload_stale_pods",
	"k8s_sustain_workload_retry_state",
	"k8s_sustain_workload_retry_attempts",
	"k8s_sustain_autoscaler_present",
	"k8s_sustain_autoscaler_target_configured",
	"k8s_sustain_recycle_suppressed_total",
}

// promServerForIdentities serves CPU and memory samples for each identity, so
// one sharded batch resolves all of them.
func promServerForIdentities(ids ...promclient.WorkloadIdentity) *httptest.Server {
	series := func(value string) string {
		parts := make([]string, 0, len(ids))
		for _, id := range ids {
			parts = append(parts, fmt.Sprintf(`{"metric":{"namespace":%q,"owner_kind":%q,"owner_name":%q,"container":"app"},"value":[0,%q]}`,
				id.Namespace, id.OwnerKind, id.OwnerName, value))
		}
		return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(parts, ",") + `]}}`
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		q := req.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(series("0.1")))
		case strings.Contains(q, "workload_max_pod_memory"):
			_, _ = w.Write([]byte(series("67108864")))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
}

func groupedDeployment(ns, name, ownerName, policy string) *appsv1.Deployment {
	d := annotatedDeployment(ns, name, policy)
	d.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = ownerName
	return d
}

func runningPod(ns, name string, labels, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: labels, Annotations: annotations,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-24 * time.Hour)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func gaugeOf(t *testing.T, metric, ns, kind, name string) float64 {
	t.Helper()
	series := seriesForWorkload(t, metric, ns, kind, name)
	if len(series) != 1 {
		t.Fatalf("%s{%s/%s/%s}: want exactly one series, got %d", metric, ns, kind, name, len(series))
	}
	return series[0].GetGauge().GetValue()
}

func labelOf(m *dto.Metric, name string) string {
	for _, l := range m.Label {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func policyGauge(t *testing.T, metric, policy string) float64 {
	t.Helper()
	return gaugeValue(t, metric, map[string]string{"policy": policy})
}

func reconcilePolicy(t *testing.T, r *PolicyReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// Two owner-name members of one Deployment identity and a bare-pod group must
// each surface as ONE series per health metric under the identity's labels,
// aggregated over the members, never under a member's own name; once the
// identities are gone so are their series.
func TestReconcile_IdentityHealthSeriesAggregateMembers(t *testing.T) {
	const ns, policyName = "health-agg", "health-agg"
	api := promclient.WorkloadIdentity{Namespace: ns, OwnerKind: "Deployment", OwnerName: "api"}
	etl := promclient.WorkloadIdentity{Namespace: ns, OwnerKind: "Pod", OwnerName: "etl"}

	onCreate := sustainv1alpha1.UpdateModeOnCreate
	policy := policyForReconcileWorkload(t, policyName)
	policy.Finalizers = []string{"k8s.sustain.io/cleanup"}
	policy.Spec.RightSizing.Update.Types = sustainv1alpha1.UpdateTypes{Deployment: &onCreate, Pod: &onCreate}

	blue := groupedDeployment(ns, "api-blue", "api", policyName)
	green := groupedDeployment(ns, "api-green", "api", policyName)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "api-green"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api-green"},
			MaxReplicas:    4,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](70)},
				},
			}},
		},
	}
	etlAnnotations := map[string]string{
		sustainv1alpha1.PolicyAnnotation:    policyName,
		sustainv1alpha1.OwnerNameAnnotation: "etl",
	}
	objs := []runtime.Object{
		policy, blue, green, hpa,
		runningPod(ns, "api-blue-1", map[string]string{"app": "api-blue"}, nil),
		runningPod(ns, "api-blue-2", map[string]string{"app": "api-blue"}, nil),
		runningPod(ns, "api-green-1", map[string]string{"app": "api-green"}, nil),
		runningPod(ns, "etl-run-1", nil, etlAnnotations),
		runningPod(ns, "etl-run-2", nil, etlAnnotations),
	}

	server := promServerForIdentities(api, etl)
	defer server.Close()
	r := reconcilerWithProm(t, server, false, objs...)

	reconcilePolicy(t, r, policyName)

	if got := gaugeOf(t, "k8s_sustain_workload_pods", ns, "Deployment", "api"); got != 3 {
		t.Errorf("identity pods = %v, want 3 (2 blue + 1 green)", got)
	}
	if got := gaugeOf(t, "k8s_sustain_workload_stale_pods", ns, "Deployment", "api"); got != 3 {
		t.Errorf("identity stale pods = %v, want 3", got)
	}
	if got := gaugeOf(t, "k8s_sustain_workload_pods", ns, "Pod", "etl"); got != 2 {
		t.Errorf("bare-pod identity pods = %v, want 2", got)
	}
	if got := gaugeOf(t, "k8s_sustain_workload_stale_pods", ns, "Pod", "etl"); got != 2 {
		t.Errorf("bare-pod identity stale pods = %v, want 2", got)
	}
	present := seriesForWorkload(t, "k8s_sustain_autoscaler_present", ns, "Deployment", "api")
	if len(present) != 1 || labelOf(present[0], "kind") != "HPA" {
		t.Errorf("autoscaler_present: want one HPA series for the identity, got %v", present)
	}
	targets := seriesForWorkload(t, "k8s_sustain_autoscaler_target_configured", ns, "Deployment", "api")
	if len(targets) != 1 || targets[0].GetGauge().GetValue() != 70 {
		t.Errorf("autoscaler_target_configured: want one series at 70, got %v", targets)
	}
	if got := seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, "Deployment", "api"); len(got) != 0 {
		t.Errorf("a healthy identity must have no retry_state series, got %d", len(got))
	}
	for _, member := range []string{"api-blue", "api-green"} {
		for _, metric := range identityHealthMetrics {
			if got := seriesForWorkload(t, metric, ns, "Deployment", member); len(got) != 0 {
				t.Errorf("%s carries member %s: health series must be keyed by the identity", metric, member)
			}
		}
	}
	if got := policyGauge(t, "k8s_sustain_policy_workload_count", policyName); got != 2 {
		t.Errorf("policy_workload_count = %v, want 2 identities", got)
	}
	if got := policyGauge(t, "k8s_sustain_policy_blocked_count", policyName); got != 0 {
		t.Errorf("policy_blocked_count = %v, want 0", got)
	}

	ctx := context.Background()
	blueTarget, greenTarget := targetFromObject(blue, "Deployment"), targetFromObject(green, "Deployment")
	transient := apierrors.NewServiceUnavailable("apiserver unavailable")
	_ = r.handleStepError(ctx, &greenTarget, "prometheus", "Prometheus query failed", transient)
	_ = r.handleStepError(ctx, &blueTarget, "patch", "Pod recycle failed", transient)

	reconcilePolicy(t, r, policyName)

	state := seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, "Deployment", "api")
	if len(state) != 1 {
		t.Fatalf("retry_state: want one series for the identity with two blocked members, got %d", len(state))
	}
	if got := labelOf(state[0], "reason"); got != "patch" {
		t.Errorf("retry_state reason = %q, want the first blocked member's phase (api-blue: patch)", got)
	}
	if got := len(seriesForWorkload(t, "k8s_sustain_workload_retry_attempts", ns, "Deployment", "api")); got != 1 {
		t.Errorf("retry_attempts: want one series for the identity, got %d", got)
	}
	if got := gaugeOf(t, "k8s_sustain_workload_pods", ns, "Deployment", "api"); got != 3 {
		t.Errorf("backed-off members must keep their last counts: pods = %v, want 3", got)
	}
	if got := policyGauge(t, "k8s_sustain_policy_blocked_count", policyName); got != 1 {
		t.Errorf("policy_blocked_count = %v, want 1 identity (not 2 members)", got)
	}

	for _, o := range []client.Object{blue, green} {
		if err := r.Delete(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"etl-run-1", "etl-run-2"} {
		if err := r.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	reconcilePolicy(t, r, policyName)

	for _, id := range []promclient.WorkloadIdentity{api, etl} {
		for _, metric := range identityHealthMetrics {
			if got := seriesForWorkload(t, metric, id.Namespace, id.OwnerKind, id.OwnerName); len(got) != 0 {
				t.Errorf("%s still has %d series for departed identity %s/%s", metric, len(got), id.OwnerKind, id.OwnerName)
			}
		}
	}
	if got := policyGauge(t, "k8s_sustain_policy_workload_count", policyName); got != 0 {
		t.Errorf("policy_workload_count = %v, want 0 once every identity departed", got)
	}
}

func TestReconcile_PolicyDeletionRemovesIdentityHealth(t *testing.T) {
	const ns, name, policyName = "health-deleted", "web", "health-deleted"
	dep := annotatedDeployment(ns, name, policyName)
	dep.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	server := promServerFor(ns, "Deployment", name)
	defer server.Close()
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := policyForReconcileWorkload(t, policyName)
	policy.Finalizers = []string{"k8s.sustain.io/cleanup"}
	policy.Spec.RightSizing.Update.Types = sustainv1alpha1.UpdateTypes{Deployment: &ongoing}
	r := reconcilerWithProm(t, server, false, policy, dep)

	reconcilePolicy(t, r, policyName)
	if got := len(seriesForWorkload(t, "k8s_sustain_workload_pods", ns, "Deployment", name)); got != 1 {
		t.Fatalf("cycle 1 must emit the pods series, got %d", got)
	}

	var live sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: policyName}, &live); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	reconcilePolicy(t, r, policyName)
	for _, metric := range identityHealthMetrics {
		if got := seriesForWorkload(t, metric, ns, "Deployment", name); len(got) != 0 {
			t.Errorf("deleted policy must remove %s, got %d series", metric, len(got))
		}
	}
}

func TestReconcileWorkload_RecycleSuppressedCountsUnderIdentity(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()
	r := reconcilerWithProm(t, server, true, runningPod("default", "web-blue-pod", map[string]string{"app": "web-blue"}, nil))
	tgt := deploymentTarget("default", "web-blue")
	tgt.IdentityName = "web"
	policy := policyForReconcileWorkload(t, "p")
	policy.Spec.RightSizing.ResourcesConfigs.CPU.DownsizeThreshold = &sustainv1alpha1.DownsizeThreshold{Percent: ptr.To[int32](100)}

	counter := func(name string) float64 {
		var v float64
		for _, m := range seriesForWorkload(t, "k8s_sustain_recycle_suppressed_total", "default", "Deployment", name) {
			if labelOf(m, "resource") == "cpu" {
				v += m.GetCounter().GetValue()
			}
		}
		return v
	}
	before := counter("web")
	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatal(err)
	}
	if got := counter("web") - before; got != 1 {
		t.Errorf("suppressed cpu decreases under the identity = %v, want 1", got)
	}
	if got := len(seriesForWorkload(t, "k8s_sustain_recycle_suppressed_total", "default", "Deployment", "web-blue")); got != 0 {
		t.Errorf("recycle_suppressed_total must not carry the member name, got %d series", got)
	}
}
