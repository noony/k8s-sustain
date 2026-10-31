package dashboard

import (
	"net/http"

	"github.com/noony/k8s-sustain/internal/workload"
)

// kindItem is one workload kind a Policy can manage, as the SPA needs it.
type kindItem struct {
	Name        string `json:"name"`
	UpdateField string `json:"updateField"`
	InPlaceOnly bool   `json:"inPlaceOnly"`
}

func (s *Server) handleKinds(w http.ResponseWriter, _ *http.Request) {
	kinds := workload.Kinds()
	items := make([]kindItem, 0, len(kinds))
	for _, k := range kinds {
		items = append(items, kindItem{Name: k.Name, UpdateField: k.UpdateField, InPlaceOnly: k.InPlaceOnly})
	}
	writeJSON(w, http.StatusOK, items)
}
