package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Path is where the engine serves the browser dashboard.
const Path = "/dashboard"

// dist is the Vite production build of ../../web.
//
// Rebuild with: cd web && npm install && npm run build
//
//go:embed all:dist
var dist embed.FS

// Handler serves the dashboard. Every path under /dashboard that is not a
// built asset gets the same HTML, so a deep link survives a refresh.
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("web: " + err.Error())
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		panic("web: " + err.Error())
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		name := strings.TrimPrefix(r.URL.Path, Path)
		name = strings.TrimPrefix(path.Clean("/"+name), "/")
		if name == "" || name == "index.html" {
			serveIndex(w, index)
			return
		}

		if _, err := fs.Stat(root, name); err != nil {
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, index)
			return
		}

		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + name
		r2.URL.RawPath = ""
		files.ServeHTTP(w, r2)
	})
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}
