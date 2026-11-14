// SPDX-License-Identifier: PostgreSQL

// Package webui serves the management panel: the files of the dist directory,
// embedded into the binary, with a single-page-app fallback. The web build
// replaces the placeholder content of dist.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

//go:embed all:dist
var embedded embed.FS

// Handler serves the embedded panel.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	return NewHandler(sub), nil
}

// NewHandler serves files from fsys at "/". A request for a path that is not
// a file and has no file extension gets index.html (client-side routing); a
// missing file with an extension is a 404 problem, so a lost script is not
// answered with HTML. Paths under /api are never served from here: the router
// answers them first, and the handler refuses them as a second line of defense.
func NewHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			problem.Write(w, r.URL.Path, w.Header().Get("X-Request-Id"),
				problem.New(problem.MethodNotAllowed, "Method "+r.Method+" is not allowed here.").
					WithHeader("Allow", "GET, HEAD"))
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if p == "/api" || strings.HasPrefix(p, "/api/") {
			problem.Write(w, r.URL.Path, w.Header().Get("X-Request-Id"),
				problem.New(problem.NotFound, "No such API route: "+r.URL.Path))
			return
		}
		name := strings.TrimPrefix(p, "/")
		if name == "" {
			name = "."
		}
		if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
			setCache(w, p)
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(p) != "" {
			problem.Write(w, r.URL.Path, w.Header().Get("X-Request-Id"),
				problem.New(problem.NotFound, "No such file: "+r.URL.Path))
			return
		}
		// SPA fallback.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/index.html"
		w.Header().Set("Cache-Control", "no-cache")
		serveIndex(w, r2, fsys)
	})
}

// serveIndex writes index.html directly; http.FileServer would redirect
// requests for /index.html to "./".
func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		problem.Write(w, r.URL.Path, w.Header().Get("X-Request-Id"),
			problem.New(problem.NotFound, "The panel is not installed in this build."))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

func setCache(w http.ResponseWriter, p string) {
	if strings.HasPrefix(p, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
