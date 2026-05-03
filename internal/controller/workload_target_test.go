package controller

import (
	"testing"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestDeploymentToTarget(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "my-policy"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app"}},
				},
			},
		},
	}

	target := deploymentToTarget(d)
	if target.Kind != "Deployment" {
		t.Errorf("expected kind Deployment, got %s", target.Kind)
	}
	if target.Name != "web" {
		t.Errorf("expected name web, got %s", target.Name)
	}
	if target.Namespace != "prod" {
		t.Errorf("expected namespace prod, got %s", target.Namespace)
	}
	if len(target.Containers) != 1 || target.Containers[0].Name != "app" {
		t.Errorf("unexpected containers: %v", target.Containers)
	}
}

func TestStatefulSetToTarget(t *testing.T) {
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "postgres"}},
				},
			},
		},
	}

	target := statefulSetToTarget(s)
	if target.Kind != "StatefulSet" || target.Name != "db" {
		t.Errorf("unexpected target: %+v", target)
	}
}

func TestDaemonSetToTarget(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "infra"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "collector"}},
				},
			},
		},
	}

	target := daemonSetToTarget(ds)
	if target.Kind != "DaemonSet" || target.Name != "agent" {
		t.Errorf("unexpected target: %+v", target)
	}
}

func TestRolloutToTarget(t *testing.T) {
	r := &rolloutsv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: "canary-app", Namespace: "prod"},
		Spec: rolloutsv1alpha1.RolloutSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "my-policy"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app"}},
				},
			},
		},
	}

	target := rolloutToTarget(r)
	if target.Kind != "Rollout" {
		t.Errorf("expected kind Rollout, got %s", target.Kind)
	}
	if target.Name != "canary-app" {
		t.Errorf("expected name canary-app, got %s", target.Name)
	}
}

func TestDeploymentToTargetCapturesInitContainers(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "app"}},
					InitContainers: []corev1.Container{{Name: "migrate"}, {Name: "warm-cache"}},
				},
			},
		},
	}

	target := deploymentToTarget(d)
	if got, want := len(target.InitContainers), 2; got != want {
		t.Fatalf("init containers: got %d, want %d", got, want)
	}
	if target.InitContainers[0].Name != "migrate" || target.InitContainers[1].Name != "warm-cache" {
		t.Errorf("unexpected init container names: %+v", target.InitContainers)
	}
}

func TestRecommendableContainers(t *testing.T) {
	t.Run("no init containers returns regular slice unchanged", func(t *testing.T) {
		w := &workloadTarget{
			Containers: []corev1.Container{{Name: "app"}},
		}
		got, initNames := w.recommendableContainers(false)
		if len(got) != 1 || got[0].Name != "app" {
			t.Fatalf("unexpected containers: %+v", got)
		}
		if initNames != nil {
			t.Fatalf("expected nil initNames, got %v", initNames)
		}
	})

	t.Run("merges regular + init when not excluded", func(t *testing.T) {
		w := &workloadTarget{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "migrate"}},
		}
		got, initNames := w.recommendableContainers(false)
		if len(got) != 2 {
			t.Fatalf("expected 2 containers, got %d", len(got))
		}
		if _, ok := initNames["migrate"]; !ok {
			t.Errorf("expected migrate in init names, got %v", initNames)
		}
		if _, ok := initNames["app"]; ok {
			t.Errorf("regular container should not be in init names, got %v", initNames)
		}
	})

	t.Run("excludes init when ExcludeInitContainers=true", func(t *testing.T) {
		w := &workloadTarget{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "migrate"}},
		}
		got, initNames := w.recommendableContainers(true)
		if len(got) != 1 || got[0].Name != "app" {
			t.Fatalf("expected only regular container, got %+v", got)
		}
		if initNames != nil {
			t.Errorf("expected nil initNames when excluded, got %v", initNames)
		}
	})
}

func TestFilterTargets(t *testing.T) {
	targets := []workloadTarget{
		{Kind: "Deployment", Name: "web", Namespace: "prod", PolicyName: "my-policy"},
		{Kind: "Deployment", Name: "api", Namespace: "prod", PolicyName: "other-policy"},
		{Kind: "Deployment", Name: "admin", Namespace: "kube-system", PolicyName: "my-policy"},
	}

	filtered := filterTargets(targets, "my-policy", []string{"kube-system"})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 target, got %d", len(filtered))
	}
	if filtered[0].Name != "web" {
		t.Errorf("expected web, got %s", filtered[0].Name)
	}
}
