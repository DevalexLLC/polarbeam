package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("httpapi: encode response", "err", err)
	}
}

// writeError emits the uniform API error shape {"error": "..."}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs the real cause and returns an opaque 500 (internals
// belong in the server log, not in a browser).
func internalError(w http.ResponseWriter, what string, err error) {
	slog.Error("httpapi: "+what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
