package server

import (
	"encoding/json"
	"net/http"
	"time"
)

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// handleEcho returns the request path and rate limit headers so you can
// observe limiting behavior directly with curl.
func handleEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"path":   r.URL.Path,
		"method": r.Method,
	})
}
