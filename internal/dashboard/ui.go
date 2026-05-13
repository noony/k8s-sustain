package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed ui/dist
var uiFS embed.FS

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// SPA routing: rewrite unknown non-asset paths to /index.html so
	// client-side routes (e.g. /policies/foo) render the shell instead of 404.
	// Asset requests (anything with a file extension) keep their original path
	// so a missing .js/.css still surfaces as 404 — caching index.html as a
	// script would break the app on reload.
	reqPath := strings.TrimPrefix(r.URL.Path, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}
	if _, err := fs.Stat(sub, reqPath); err != nil {
		if path.Ext(reqPath) != "" {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/index.html"
		http.FileServer(http.FS(sub)).ServeHTTP(w, r2)
		return
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}
