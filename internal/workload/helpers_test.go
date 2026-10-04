package workload

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestJobFinished_TrueOnCompleteOrFailed(t *testing.T) {
	tests := []struct {
		name       string
		conditions []batchv1.JobCondition
		want       bool
	}{
		{"none", nil, false},
		{"complete-true", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}, true},
		{"failed-true", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}, true},
		{"complete-false", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}}, false},
		{"suspended", []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tc.conditions}}
			if got := JobFinished(job); got != tc.want {
				t.Errorf("JobFinished = %v, want %v", got, tc.want)
			}
		})
	}
}
