package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON writes a JSON response (Content-Type + status + body). A package helper
// for the (w,r) wrappers of the handlers domain. Extracted from operator.go during the
// handler-native turn of operator (T5d): operator no longer carries (w,r), the helper
// stays shared for the remaining domains.
func writeJSON(w http.ResponseWriter, status int, body any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Warn("writeJSON: encode failed", slog.Any("error", err))
	}
}
