package dashboard

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed ui/dist
var uiFS embed.FS

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	// An unmatched /api/* path is a missing endpoint, not a SPA route: return
	// the JSON 404 envelope instead of index.html with a 200.
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

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
		// Serve index.html bytes directly. Using http.FileServer here would
		// canonicalize /index.html to ./, which redirects the browser back
		// to the original deep-link path's parent and hides the SPA route.
		s.serveIndex(w, r, sub)
		return
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	f, err := sub.Open("index.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", stat.ModTime(), rs)
}
