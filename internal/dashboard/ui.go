package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui/dist
var uiFS embed.FS

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Serve static files; fall back to index.html for SPA routing
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}

	// Try to open the requested file
	f, err := sub.Open(path[1:]) // strip leading /
	if err != nil {
		// SPA fallback: serve index.html for any unknown path
		path = "index.html"
		f, err = sub.Open(path)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	f.Close()

	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}
