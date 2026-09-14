package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// assets holds the web UI. It is embedded so the daemon is a single binary with
// nothing to install alongside it — which is what makes the Windows/macOS
// service and the minimal container image possible.
//
//go:embed assets
var assets embed.FS

// webUI serves the single-page interface. The page itself contains nothing
// secret and asks for a pairing code before it can read any data, so it is
// served without authentication.
func (s *Server) webUI() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// The assets are compiled in, so this cannot fail at runtime.
		panic(err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := "index.html"
		if r.URL.Path != "/" {
			name = r.URL.Path[1:]
			if _, err := fs.Stat(sub, name); err != nil {
				http.NotFound(w, r)
				return
			}
		}
		if name == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeFileFS(w, r, sub, name)
	})
}
