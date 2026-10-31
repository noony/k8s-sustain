package dashboard

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/noony/k8s-sustain/internal/workload"
)

func TestKindsServesEverySupportedKind(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/kinds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got []kindItem
	decodeEnvelopeData(t, rec.Body, &got)

	names := make([]string, 0, len(got))
	byName := map[string]kindItem{}
	for _, k := range got {
		names = append(names, k.Name)
		byName[k.Name] = k
	}
	if !slices.Equal(names, workload.SupportedKinds) {
		t.Errorf("kinds = %q, want workload.SupportedKinds %q in its order", names, workload.SupportedKinds)
	}
	for name, want := range map[string]kindItem{
		"Deployment": {Name: "Deployment", UpdateField: "deployment"},
		"Rollout":    {Name: "Rollout", UpdateField: "argoRollout"},
		"CronJob":    {Name: "CronJob", UpdateField: "cronJob", InPlaceOnly: true},
		"Pod":        {Name: "Pod", UpdateField: "pod", InPlaceOnly: true},
	} {
		if byName[name] != want {
			t.Errorf("%s = %+v, want %+v", name, byName[name], want)
		}
	}
}
