package httpapi

import (
	"io/fs"
	"net/http"
)

// staticHandler serves the embedded SPA. Unknown paths fall back to
// index.html so client-side routes survive a full-page load; the /api/ and
// /healthz namespaces are routed before this handler and never reach it.
func staticHandler(dist fs.FS) http.Handler {
	fileServer := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		// Vite emits external JS/CSS assets; everything is same-origin.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")

		path := r.URL.Path
		if path != "/" {
			if _, err := fs.Stat(dist, path[1:]); err != nil {
				// SPA fallback: unknown non-API path → index.html.
				http.ServeFileFS(w, r, dist, "index.html")
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}
