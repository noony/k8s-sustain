package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

func podSeriesPresent(t *testing.T, ns, kind, name string) (pods, stale bool) {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	labels := map[string]string{"namespace": ns, "owner_kind": kind, "owner_name": name}
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			if !matchesLabels(m, labels) {
				continue
			}
			switch mf.GetName() {
			case "k8s_sustain_workload_pods":
				pods = true
			case "k8s_sustain_workload_stale_pods":
				stale = true
			}
		}
	}
	return pods, stale
}

func podGaugePolicy(t *testing.T, name string) *sustainv1alpha1.Policy {
	t.Helper()
	ongoing := sustainv1alpha1.UpdateModeOngoing
	p := policyForReconcileWorkload(t, name)
	p.Finalizers = []string{"k8s.sustain.io/cleanup"}
	p.Spec.RightSizing.Update.Types = sustainv1alpha1.UpdateTypes{Deployment: &ongoing}
	return p
}

func reconcilePolicy(t *testing.T, r *PolicyReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestReconcile_DepartedTargetLosesPodGauges(t *testing.T) {
	const ns, name, policyName = "podgauge-departed", "web", "podgauge-departed"
	dep := annotatedDeployment(ns, name, policyName)
	dep.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	server := promServerFor(ns, "Deployment", name)
	defer server.Close()
	r := reconcilerWithProm(t, server, false, podGaugePolicy(t, policyName), dep)

	reconcilePolicy(t, r, policyName)
	if pods, stale := podSeriesPresent(t, ns, "Deployment", name); !pods || !stale {
		t.Fatalf("cycle 1 must emit both pod gauges, got pods=%v stale=%v", pods, stale)
	}

	if err := r.Delete(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	reconcilePolicy(t, r, policyName)
	if pods, stale := podSeriesPresent(t, ns, "Deployment", name); pods || stale {
		t.Errorf("departed target must lose its pod gauges, got pods=%v stale=%v", pods, stale)
	}
}

func TestReconcile_PolicyDeletionRemovesPodGauges(t *testing.T) {
	const ns, name, policyName = "podgauge-deleted", "web", "podgauge-deleted"
	dep := annotatedDeployment(ns, name, policyName)
	dep.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	server := promServerFor(ns, "Deployment", name)
	defer server.Close()
	policy := podGaugePolicy(t, policyName)
	r := reconcilerWithProm(t, server, false, policy, dep)

	reconcilePolicy(t, r, policyName)
	if pods, stale := podSeriesPresent(t, ns, "Deployment", name); !pods || !stale {
		t.Fatalf("cycle 1 must emit both pod gauges, got pods=%v stale=%v", pods, stale)
	}

	var live sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: policyName}, &live); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	reconcilePolicy(t, r, policyName)
	if pods, stale := podSeriesPresent(t, ns, "Deployment", name); pods || stale {
		t.Errorf("deleted policy must remove its pod gauges, got pods=%v stale=%v", pods, stale)
	}
}

func TestPodGaugeTracker_SkipsKeysAnotherPolicyStillTargets(t *testing.T) {
	const ns = "podgauge-moved"
	var tr podGaugeTracker
	moved := podGaugeKey{Namespace: ns, Kind: "Deployment", Name: "moved"}
	gone := podGaugeKey{Namespace: ns, Kind: "Deployment", Name: "gone"}

	tr.observe("a", []podGaugeKey{moved, gone})
	tr.observe("b", []podGaugeKey{moved})
	EmitWorkloadPods(ns, "Deployment", "moved", workload.PodCounts{Total: 1})
	EmitWorkloadPods(ns, "Deployment", "gone", workload.PodCounts{Total: 1})

	tr.observe("a", nil)
	if pods, stale := podSeriesPresent(t, ns, "Deployment", "moved"); !pods || !stale {
		t.Errorf("series another policy still targets must survive, got pods=%v stale=%v", pods, stale)
	}
	if pods, stale := podSeriesPresent(t, ns, "Deployment", "gone"); pods || stale {
		t.Errorf("departed series must be removed, got pods=%v stale=%v", pods, stale)
	}

	tr.forget("b")
	if pods, stale := podSeriesPresent(t, ns, "Deployment", "moved"); pods || stale {
		t.Errorf("forgetting the last policy must remove the series, got pods=%v stale=%v", pods, stale)
	}
}
