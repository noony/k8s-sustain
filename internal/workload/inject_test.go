package workload

import (
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func container(name string, r corev1.ResourceRequirements) corev1.Container {
	return corev1.Container{Name: name, Resources: r}
}

// written is what one ContainerWrite sets, in the strings a table can compare.
type written struct {
	name   string
	init   bool
	index  int
	cpuReq string
	cpuLim string
	memReq string
	memLim string
	raised []corev1.ResourceName
}

func writtenOf(w ContainerWrite) written {
	str := func(l corev1.ResourceList, name corev1.ResourceName) string {
		q, ok := l[name]
		if !ok {
			return ""
		}
		return q.String()
	}
	return written{
		name: w.Name, init: w.Init, index: w.Index,
		cpuReq: str(w.Resources.Requests, corev1.ResourceCPU), cpuLim: str(w.Resources.Limits, corev1.ResourceCPU),
		memReq: str(w.Resources.Requests, corev1.ResourceMemory), memLim: str(w.Resources.Limits, corev1.ResourceMemory),
		raised: w.RaisedLimits,
	}
}

func TestInject(t *testing.T) {
	cases := []struct {
		name           string
		containers     []corev1.Container
		initContainers []corev1.Container
		excludeInit    bool
		recs           map[string]ContainerRecommendation
		wantRecs       []string
		want           []written
	}{
		{
			name:       "sets requests on a container without any",
			containers: []corev1.Container{{Name: "app"}},
			recs:       map[string]ContainerRecommendation{"app": {CPURequest: qtyp("100m"), MemoryRequest: qtyp("64Mi")}},
			wantRecs:   []string{"app"},
			want:       []written{{name: "app", cpuReq: "100m", memReq: "64Mi"}},
		},
		{
			name:       "a recommendation for a container not declared is ignored",
			containers: []corev1.Container{{Name: "app"}, {Name: "other"}},
			recs: map[string]ContainerRecommendation{
				"app":     {CPURequest: qtyp("100m")},
				"sidecar": {CPURequest: qtyp("50m")},
			},
			wantRecs: []string{"app"},
			want:     []written{{name: "app", cpuReq: "100m"}},
		},
		{
			name:       "a container already at its recommendation is not written",
			containers: []corev1.Container{container("app", reqsRR("100m", "64Mi", "", ""))},
			recs:       map[string]ContainerRecommendation{"app": {CPURequest: qtyp("100m"), MemoryRequest: qtyp("64Mi")}},
			wantRecs:   []string{"app"},
		},
		{
			name:       "removes a limit",
			containers: []corev1.Container{container("app", reqsRR("", "", "500m", ""))},
			recs:       map[string]ContainerRecommendation{"app": {CPURequest: qtyp("100m"), RemoveCPULimit: true}},
			wantRecs:   []string{"app"},
			want:       []written{{name: "app", cpuReq: "100m"}},
		},
		{
			name:           "init containers are written after the containers",
			containers:     []corev1.Container{{Name: "app"}},
			initContainers: []corev1.Container{{Name: "migrate"}, {Name: "warm-cache"}},
			recs: map[string]ContainerRecommendation{
				"app":        {CPURequest: qtyp("100m")},
				"warm-cache": {CPURequest: qtyp("50m")},
			},
			wantRecs: []string{"app", "warm-cache"},
			want:     []written{{name: "app", cpuReq: "100m"}, {name: "warm-cache", init: true, index: 1, cpuReq: "50m"}},
		},
		{
			name:           "excludeInit leaves init containers out",
			containers:     []corev1.Container{{Name: "app"}},
			initContainers: []corev1.Container{{Name: "migrate"}},
			excludeInit:    true,
			recs: map[string]ContainerRecommendation{
				"app":     {CPURequest: qtyp("100m")},
				"migrate": {CPURequest: qtyp("250m")},
			},
			wantRecs: []string{"app"},
			want:     []written{{name: "app", cpuReq: "100m"}},
		},
		{
			// Kubernetes allows one name as a container and an init container,
			// and recommendations are keyed by name.
			name:           "a name declared twice is written in both places",
			containers:     []corev1.Container{{Name: "shared"}},
			initContainers: []corev1.Container{{Name: "shared"}},
			recs:           map[string]ContainerRecommendation{"shared": {CPURequest: qtyp("123m")}},
			wantRecs:       []string{"shared"},
			want:           []written{{name: "shared", cpuReq: "123m"}, {name: "shared", init: true, cpuReq: "123m"}},
		},
		{
			// The pod's limit differs from what the recommendation was
			// computed against, and the recommendation keeps it.
			name:       "a recommended request above the running limit raises the limit",
			containers: []corev1.Container{container("app", reqsRR("100m", "128Mi", "200m", "256Mi"))},
			recs:       map[string]ContainerRecommendation{"app": {CPURequest: qtyp("300m"), MemoryRequest: qtyp("512Mi")}},
			wantRecs:   []string{"app"},
			want: []written{{
				name: "app", cpuReq: "300m", cpuLim: "300m", memReq: "512Mi", memLim: "512Mi",
				raised: []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory},
			}},
		},
		{
			name:       "a recommended limit below the recommended request is raised",
			containers: []corev1.Container{{Name: "app"}},
			recs:       map[string]ContainerRecommendation{"app": {CPURequest: qtyp("300m"), CPULimit: qtyp("200m")}},
			wantRecs:   []string{"app"},
			want:       []written{{name: "app", cpuReq: "300m", cpuLim: "300m", raised: []corev1.ResourceName{corev1.ResourceCPU}}},
		},
		{
			name:       "a request at or below its limit leaves the limit alone",
			containers: []corev1.Container{container("app", reqsRR("100m", "", "200m", ""))},
			recs:       map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}},
			wantRecs:   []string{"app"},
			want:       []written{{name: "app", cpuReq: "200m", cpuLim: "200m"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inj := Inject(tc.containers, tc.initContainers, tc.excludeInit, tc.recs)
			var gotRecs []string
			for name := range inj.Recs {
				gotRecs = append(gotRecs, name)
			}
			slices.Sort(gotRecs)
			if !slices.Equal(gotRecs, tc.wantRecs) {
				t.Errorf("recs for %v, want %v", gotRecs, tc.wantRecs)
			}
			var got []written
			for _, w := range inj.Writes {
				got = append(got, writtenOf(w))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("writes:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// Inject never aliases the caller's containers: the webhook builds its patch
// and the patcher its resize from the writes, while the pod they came from
// stays what the apiserver holds.
func TestInjectDoesNotMutateTheContainers(t *testing.T) {
	containers := []corev1.Container{container("app", reqsRR("100m", "", "200m", ""))}
	inj := Inject(containers, nil, false, map[string]ContainerRecommendation{"app": {CPURequest: qtyp("300m")}})
	if len(inj.Writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(inj.Writes))
	}
	if got := containers[0].Resources.Requests.Cpu().String(); got != "100m" {
		t.Errorf("caller's request = %s, want 100m untouched", got)
	}
	if got := containers[0].Resources.Limits.Cpu().String(); got != "200m" {
		t.Errorf("caller's limit = %s, want 200m untouched", got)
	}
}
