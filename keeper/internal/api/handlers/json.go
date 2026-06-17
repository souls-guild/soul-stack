package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON пишет JSON-ответ (Content-Type + статус + тело). Пакетный helper
// (w,r)-обёрток handlers-домена. Извлечён из operator.go при handler-native-
// развороте operator (T5d): operator больше не несёт (w,r), helper остаётся
// общим для остальных доменов.
func writeJSON(w http.ResponseWriter, status int, body any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Warn("writeJSON: encode failed", slog.Any("error", err))
	}
}
