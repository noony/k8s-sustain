package workload

import (
	"testing"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPodTemplateOf(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}

	cases := []struct {
		name   string
		obj    client.Object
		wantOK bool
	}{
		{"deployment", &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: tmpl}}, true},
		{"rollout", &rolloutsv1alpha1.Rollout{Spec: rolloutsv1alpha1.RolloutSpec{Template: tmpl}}, true},
		{"cronjob", &batchv1.CronJob{Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: tmpl}}}}, true},
		{"job", &batchv1.Job{Spec: batchv1.JobSpec{Template: tmpl}}, true},
		{"replicaset", &appsv1.ReplicaSet{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := PodTemplateOf(c.obj)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if len(got.Spec.Containers) != 1 || got.Spec.Containers[0].Name != "app" {
				t.Errorf("template containers = %v, want [app]", got.Spec.Containers)
			}
		})
	}
}
