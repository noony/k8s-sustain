package recommender

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// recordedInputs serves canned inputs by identity, fails those in failed,
// gives every other identity empty inputs, and records each call. A fetch
// takes a minute of clock, when it is set.
type recordedInputs struct {
	inputs map[promclient.WorkloadIdentity]*WorkloadInputs
	failed map[promclient.WorkloadIdentity]error
	cfgs   []sustainv1alpha1.ResourcesConfigs
	calls  [][]InputsRequest
	clock  *clocktesting.FakePassiveClock
}

func (f *recordedInputs) FetchInputs(_ context.Context, cfg sustainv1alpha1.ResourcesConfigs, reqs []InputsRequest) map[promclient.WorkloadIdentity]InputsResult {
	f.cfgs = append(f.cfgs, cfg)
	f.calls = append(f.calls, slices.Clone(reqs))
	out := make(map[promclient.WorkloadIdentity]InputsResult, len(reqs))
	for _, r := range reqs {
		if err := f.failed[r.Identity]; err != nil {
			out[r.Identity] = InputsResult{Err: err}
			continue
		}
		in := f.inputs[r.Identity]
		if in == nil {
			in = &WorkloadInputs{}
		}
		out[r.Identity] = InputsResult{Inputs: in}
	}
	if f.clock != nil {
		f.clock.SetTime(f.clock.Now().Add(time.Minute))
	}
	return out
}

// memberAutoscalers answers every identity of a namespace with its group,
// fails every read of the namespaces in failed, and records the members each
// read named and how many reads each namespace took.
type memberAutoscalers struct {
	groups map[string]autoscaler.Group
	failed map[string]error
	asked  map[string][]string
	reads  map[string]int
}

func (f *memberAutoscalers) ForIdentity(_ context.Context, namespace, _ string, members []string) (autoscaler.Group, error) {
	if f.asked == nil {
		f.asked, f.reads = map[string][]string{}, map[string]int{}
	}
	f.asked[namespace] = members
	f.reads[namespace]++
	if err := f.failed[namespace]; err != nil {
		return autoscaler.Group{Info: autoscaler.Info{Kind: autoscaler.KindNone}}, err
	}
	if g, ok := f.groups[namespace]; ok {
		return g, nil
	}
	return autoscaler.Group{Info: autoscaler.Info{Kind: autoscaler.KindNone}}, nil
}

func member(name, policy string) inventory.Member {
	return inventory.Member{Object: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name}}, Policy: policy}
}

// governed is a live identity of Deployment ns/name governed by p, with
// container app and init container migrate.
func governed(ns, name string, members ...inventory.Member) *inventory.Identity {
	if len(members) == 0 {
		members = []inventory.Member{member(name, "p")}
	}
	return &inventory.Identity{
		Key:            promclient.WorkloadIdentity{Namespace: ns, OwnerKind: "Deployment", OwnerName: name},
		Members:        members,
		Policy:         "p",
		Containers:     containers("app"),
		InitContainers: containers("migrate"),
		Since:          old,
	}
}

func names(cs []corev1.Container) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

var hpa = autoscaler.Info{Kind: autoscaler.KindHPA, Name: "api-green", ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 50}}

// What the assembly makes of one identity: the containers it is recommended
// for and the fetch is sized by, the autoscaler of the members its Policy
// governs, its age, the instant it computes as of, read from the clock once
// the fetch returned, and why it has no Request.
func TestAssemble(t *testing.T) {
	cases := []struct {
		name        string
		spec        sustainv1alpha1.RightSizingSpec
		id          *inventory.Identity
		inputs      *recordedInputs
		autoscalers *memberAutoscalers
		// wantContainers are the Request's containers and the fetch's size
		// hint their count.
		wantContainers []string
		wantAsked      []string
		wantAuto       autoscaler.Kind
		wantErr        bool
	}{
		{
			name:           "init containers are recommended",
			id:             governed("prod", "api"),
			wantContainers: []string{"app", "migrate"},
			wantAsked:      []string{"api"},
			wantAuto:       autoscaler.KindNone,
		},
		{
			name:           "init containers are left out when the Policy excludes them",
			spec:           sustainv1alpha1.RightSizingSpec{ExcludeInitContainers: true},
			id:             governed("prod", "api"),
			wantContainers: []string{"app"},
			wantAsked:      []string{"api"},
			wantAuto:       autoscaler.KindNone,
		},
		{
			name: "the autoscaler is read for every member the Policy governs, not under the identity's name",
			id: governed("prod", "api",
				member("api-blue", "p"), member("api-green", "p"), member("api-legacy", "")),
			autoscalers:    &memberAutoscalers{groups: map[string]autoscaler.Group{"prod": {Info: hpa}}},
			wantContainers: []string{"app", "migrate"},
			wantAsked:      []string{"api-blue", "api-green"},
			wantAuto:       autoscaler.KindHPA,
		},
		{
			name: "a departed identity has no member to read an autoscaler for",
			id: &inventory.Identity{
				Key:        promclient.WorkloadIdentity{Namespace: "batch", OwnerKind: "Job", OwnerName: "nightly"},
				Policy:     "p",
				Containers: containers("main"),
			},
			autoscalers:    &memberAutoscalers{groups: map[string]autoscaler.Group{"batch": {Info: hpa}}},
			wantContainers: []string{"main"},
			wantAsked:      nil,
			wantAuto:       autoscaler.KindNone,
		},
		{
			name: "a departed identity's snapshot init containers are left out when the Policy excludes them",
			spec: sustainv1alpha1.RightSizingSpec{ExcludeInitContainers: true},
			id: &inventory.Identity{
				Key:            promclient.WorkloadIdentity{Namespace: "batch", OwnerKind: "Job", OwnerName: "nightly"},
				Policy:         "p",
				Containers:     containers("main"),
				InitContainers: containers("prep"),
			},
			wantContainers: []string{"main"},
			wantAuto:       autoscaler.KindNone,
		},
		{
			name:           "a failed autoscaler read leaves the identity uncoordinated",
			id:             governed("prod", "api"),
			autoscalers:    &memberAutoscalers{failed: map[string]error{"prod": errors.New("forbidden")}},
			wantContainers: []string{"app", "migrate"},
			wantAsked:      []string{"api"},
			wantAuto:       autoscaler.KindNone,
		},
		{
			name: "an identity whose inputs cannot be read has no Request but keeps its autoscaler",
			id:   governed("prod", "api"),
			inputs: &recordedInputs{failed: map[promclient.WorkloadIdentity]error{
				{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}: errors.New("prometheus down"),
			}},
			autoscalers:    &memberAutoscalers{groups: map[string]autoscaler.Group{"prod": {Info: hpa}}},
			wantContainers: []string{"app", "migrate"},
			wantAsked:      []string{"api"},
			wantAuto:       autoscaler.KindHPA,
			wantErr:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.inputs == nil {
				tc.inputs = &recordedInputs{}
			}
			if tc.autoscalers == nil {
				tc.autoscalers = &memberAutoscalers{}
			}
			tc.inputs.clock = clocktesting.NewFakePassiveClock(now)

			got := Assemble(context.Background(), tc.inputs, tc.autoscalers, tc.inputs.clock.Now, tc.spec, []*inventory.Identity{tc.id})[0]

			if call := tc.inputs.calls; len(call) != 1 || len(call[0]) != 1 ||
				call[0][0] != (InputsRequest{Identity: tc.id.Key, Containers: len(tc.wantContainers)}) {
				t.Errorf("fetch = %+v, want one request for %v sized %d", call, tc.id.Key, len(tc.wantContainers))
			}
			if asked := tc.autoscalers.asked[tc.id.Key.Namespace]; !slices.Equal(asked, tc.wantAsked) {
				t.Errorf("autoscaler read for %v, want %v", asked, tc.wantAsked)
			}
			if got.Autoscaler.Info.Kind != tc.wantAuto {
				t.Errorf("autoscaler = %s, want %s", got.Autoscaler.Info.Kind, tc.wantAuto)
			}
			if tc.wantErr {
				if got.Err == nil || !reflect.DeepEqual(got.Request, Request{}) {
					t.Errorf("Err = %v with Request %+v, want the fetch error and no Request", got.Err, got.Request)
				}
				return
			}
			if got.Err != nil {
				t.Fatalf("Err = %v", got.Err)
			}
			req := got.Request
			if !slices.Equal(names(req.containers), tc.wantContainers) {
				t.Errorf("containers = %v, want %v", names(req.containers), tc.wantContainers)
			}
			if req.autoInfo.Kind != tc.wantAuto || !req.since.Equal(tc.id.Since) || req.inputs == nil {
				t.Errorf("request = %+v, want the %s autoscaler, the identity's age and its inputs", req, tc.wantAuto)
			}
			if fetched := now.Add(time.Minute); !req.now.Equal(fetched) {
				t.Errorf("Now = %v, want %v: the clock once the fetch returned", req.now, fetched)
			}
		})
	}
}

// A namespace whose autoscalers cannot be read is read once per assembly:
// every identity of it is recommended uncoordinated without another List,
// and another namespace is still read and coordinated.
func TestAssemble_AnUnreadableNamespaceIsReadOnce(t *testing.T) {
	autoscalers := &memberAutoscalers{
		groups: map[string]autoscaler.Group{"staging": {Info: hpa}},
		failed: map[string]error{"prod": errors.New("forbidden")},
	}

	got := Assemble(context.Background(), &recordedInputs{}, autoscalers, time.Now, sustainv1alpha1.RightSizingSpec{},
		[]*inventory.Identity{governed("prod", "api"), governed("prod", "web"), governed("staging", "db"), governed("prod", "worker")})

	if want := map[string]int{"prod": 1, "staging": 1}; !maps.Equal(autoscalers.reads, want) {
		t.Errorf("reads = %v, want %v", autoscalers.reads, want)
	}
	for i, want := range []autoscaler.Kind{autoscaler.KindNone, autoscaler.KindNone, autoscaler.KindHPA, autoscaler.KindNone} {
		if a := got[i]; a.Err != nil || a.Autoscaler.Info.Kind != want || a.Request.autoInfo.Kind != want {
			t.Errorf("identity %d = %+v, want a Request shaped against %s", i, a, want)
		}
	}
}

// Every identity is fetched in one batch, under the Policy's resources with
// the recommender's defaults made explicit, which is also what each Request
// computes with, as of one instant.
func TestAssemble_OneBatchUnderThePolicysResources(t *testing.T) {
	inputs := &recordedInputs{}
	spec := sustainv1alpha1.RightSizingSpec{
		ResourcesConfigs:       sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{Window: "24h"}},
		AutoscalerCoordination: sustainv1alpha1.AutoscalerCoordination{Enabled: true},
	}

	clock := now
	ticking := func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}

	got := Assemble(context.Background(), inputs, &memberAutoscalers{}, ticking, spec,
		[]*inventory.Identity{governed("prod", "api"), governed("prod", "web"), governed("staging", "db")})

	if len(inputs.calls) != 1 || len(inputs.calls[0]) != 3 {
		t.Fatalf("fetch calls = %+v, want one batch of three", inputs.calls)
	}
	want := WithDefaults(spec.ResourcesConfigs)
	if !reflect.DeepEqual(inputs.cfgs[0], want) {
		t.Errorf("fetched under %+v, want %+v", inputs.cfgs[0], want)
	}
	for i, a := range got {
		if !reflect.DeepEqual(a.Request.resources, want) || !a.Request.coordination.Enabled {
			t.Errorf("identity %d computes with %+v, want the fetch's resources and the Policy's coordination", i, a.Request)
		}
	}
	if !got[0].Request.now.Equal(got[1].Request.now) || !got[0].Request.now.Equal(got[2].Request.now) {
		t.Error("one batch must be computed as of one instant")
	}
}

// An identity the inventory has not seen has no containers: it is
// recommended for every container its inputs report, an OOM-killed one
// without usage included. One whose containers are all excluded has nothing
// to recommend.
func TestAssemble_ContainersOfAnUnseenIdentityComeFromItsInputs(t *testing.T) {
	key := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "gone"}
	inputs := &recordedInputs{inputs: map[promclient.WorkloadIdentity]*WorkloadInputs{key: {
		CPUPerPod: promclient.ContainerValues{"app": 0.5},
		MemPerPod: promclient.ContainerValues{"app": 100 * mib},
		OOM:       map[string]OOM{"crashy": {Kills: 1, PeakBytes: 300 * mib, HasPeak: true}},
	}}}

	unseen := Assemble(context.Background(), inputs, &memberAutoscalers{}, time.Now, sustainv1alpha1.RightSizingSpec{},
		[]*inventory.Identity{{Key: key}})[0]
	excluded := Assemble(context.Background(), inputs, &memberAutoscalers{}, time.Now,
		sustainv1alpha1.RightSizingSpec{ExcludeInitContainers: true},
		[]*inventory.Identity{{Key: key, InitContainers: containers("app")}})[0]

	if got := names(unseen.Request.containers); !slices.Equal(got, []string{"app", "crashy"}) {
		t.Errorf("containers = %v, want every container the inputs report", got)
	}
	if size := inputs.calls[0][0].Containers; size != 0 {
		t.Errorf("fetch size hint = %d, want 0: the size is unknown", size)
	}
	res := Compute(unseen.Request)
	if got := res.Recs["app"].CPURequest; got == nil || got.String() != "500m" {
		t.Errorf("app cpu = %v, want 500m", got)
	}
	if got := res.Recs["crashy"].MemoryRequest; got == nil || got.String() != "300Mi" {
		t.Errorf("crashy memory = %v, want 300Mi from the OOM peak with no usage samples", got)
	}
	if res := Compute(excluded.Request); res.Outcome != NoData {
		t.Errorf("outcome with every container excluded = %v, want NoData", res.Outcome)
	}
}

// A kept request stays the container's own: the Recommendation has no request
// for that resource.
func TestAssemble_KeptRequestIsNotRecommended(t *testing.T) {
	id := governed("prod", "api")
	inputs := &recordedInputs{inputs: map[promclient.WorkloadIdentity]*WorkloadInputs{id.Key: {
		CPUPerPod: promclient.ContainerValues{"app": 0.2},
		MemPerPod: promclient.ContainerValues{"app": 64 * mib},
	}}}
	spec := sustainv1alpha1.RightSizingSpec{ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
		Memory: sustainv1alpha1.ResourceConfig{Requests: sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true}},
	}}

	res := Compute(Assemble(context.Background(), inputs, &memberAutoscalers{}, time.Now, spec, []*inventory.Identity{id})[0].Request)

	if rec := res.Recs["app"]; rec.CPURequest == nil || rec.MemoryRequest != nil {
		t.Errorf("app = %+v, want a CPU request and the memory request kept", rec)
	}
}

func TestWithDefaults(t *testing.T) {
	ratio := 1.5
	cases := []struct {
		name string
		in   sustainv1alpha1.ResourceConfig
		want sustainv1alpha1.ResourceConfig
	}{
		{
			name: "unset",
			want: sustainv1alpha1.ResourceConfig{
				Window:   "168h",
				Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To[int32](95), Headroom: ptr.To[int32](0)},
				Limits:   sustainv1alpha1.ResourceLimitsConfig{KeepLimit: true},
			},
		},
		{
			name: "set values and a limit strategy are kept",
			in: sustainv1alpha1.ResourceConfig{
				Window:   "24h",
				Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To[int32](99), Headroom: ptr.To[int32](10)},
				Limits:   sustainv1alpha1.ResourceLimitsConfig{RequestsLimitsRatio: &ratio},
			},
			want: sustainv1alpha1.ResourceConfig{
				Window:   "24h",
				Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To[int32](99), Headroom: ptr.To[int32](10)},
				Limits:   sustainv1alpha1.ResourceLimitsConfig{RequestsLimitsRatio: &ratio},
			},
		},
		{
			name: "a kept request has no percentile or headroom",
			in:   sustainv1alpha1.ResourceConfig{Requests: sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true}},
			want: sustainv1alpha1.ResourceConfig{
				Window:   "168h",
				Requests: sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true},
				Limits:   sustainv1alpha1.ResourceLimitsConfig{KeepLimit: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sustainv1alpha1.ResourcesConfigs{CPU: tc.in, Memory: *tc.in.DeepCopy()}

			got := WithDefaults(in)

			if !reflect.DeepEqual(got.CPU, tc.want) || !reflect.DeepEqual(got.Memory, tc.want) {
				t.Errorf("WithDefaults = %+v, want %+v for both resources", got, tc.want)
			}
			if !reflect.DeepEqual(in.CPU, tc.in) {
				t.Errorf("the input was changed to %+v", in.CPU)
			}
		})
	}
}
