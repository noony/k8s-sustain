package dashboard

import (
	"net/http"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// kindItem is one workload kind a Policy can manage, as the SPA needs it.
type kindItem struct {
	Name string `json:"name"`
	// UpdateField is the Policy's spec.rightSizing.update.types field that
	// manages the kind.
	UpdateField string `json:"updateField"`
	InPlaceOnly bool   `json:"inPlaceOnly"`
}

func (s *Server) handleKinds(w http.ResponseWriter, _ *http.Request) {
	items := make([]kindItem, 0, len(workload.SupportedKinds))
	for _, name := range workload.SupportedKinds {
		items = append(items, kindItem{
			Name:        name,
			UpdateField: sustainv1alpha1.UpdateTypesField(name),
			InPlaceOnly: workload.InPlaceOnly(name),
		})
	}
	writeJSON(w, http.StatusOK, items)
}
