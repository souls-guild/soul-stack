package errand

import "github.com/souls-guild/soul-stack/shared/audit"

// OutputCapBytes is the stdout/stderr size ceiling per Errand channel
// (ADR-033 §6 "Invariants", 64 KiB / channel). Applied keeper-side when
// receiving ErrandResult from the Soul and when writing to DB/audit event.
// The Soul-side errand-runner (slice E3) applies the same cap defense-in-depth.
const OutputCapBytes = 64 * 1024

// truncate cuts s to n bytes, returning (cut, true) if it was over the limit.
// Cap is byte-based (not runes) to match SQL semantics (PG TEXT stores a
// byte-stream, not codepoints); upstream and downstream caps must agree.
// Cutting mid multi-byte UTF-8 sequence can leave an invalid trailing byte —
// fine for captured output ([]byte stream), still handed to the client as a
// string (JSON allows invalid UTF-8 escaped as �, past the truncation point).
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return s[:n], true
}

// MaskAndCapBytes runs stdout/stderr through secret-masking then the cap.
// Masking first (vault-ref and sensitive-key lookup via shared/audit),
// then cap — otherwise truncation could cut a sensitive substring and
// leave a half-leak. Returns (masked, truncated).
//
// Masking operates on a map payload (shared/audit.MaskSecrets); for a
// single string we wrap it under key "v" and unwrap (same pattern as
// keeper/internal/grpc/events_taskevent.go::maskString).
func MaskAndCapBytes(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	m := audit.MaskSecrets(map[string]any{"v": s})
	masked, _ := m["v"].(string)
	if masked == "" {
		masked = s
	}
	cut, trunc := truncate(masked, OutputCapBytes)
	return cut, trunc
}

// MaskOutputMap runs structured output from read-safe modules through
// secret-masking. Always nil for shell/exec output — the function isn't
// called then. nil in → nil out (the handler decides whether to write NULL).
func MaskOutputMap(out map[string]any) map[string]any {
	if out == nil {
		return nil
	}
	return audit.MaskSecrets(out)
}
