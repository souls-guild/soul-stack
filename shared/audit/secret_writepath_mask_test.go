package audit

// Defense-in-depth guard (ADR-064 mitigation b, NIM-11): even if a plaintext
// secret accidentally reached an audit/OTel payload under a secret/credentials/
// *token key, MaskSecrets masks it by key name. The plaintext_ingested marker
// (no sensitive fragment) is NOT masked — the audit signal is preserved.

import "testing"

func TestSecretWritepathKeysMasked(t *testing.T) {
	const plaintext = "LEAK-ME-PLAINTEXT-42"
	payload := map[string]any{
		"secret":        plaintext, // webhook signing (top-level)
		"credentials":   map[string]any{"secret_key": plaintext},
		"bot_token":     plaintext, // telegram channel-token
		"header_secret": plaintext, // custom Authorization
	}
	masked := MaskSecrets(payload)

	for _, k := range []string{"secret", "credentials", "bot_token", "header_secret"} {
		if masked[k] == plaintext {
			t.Fatalf("ключ %q не замаскирован", k)
		}
	}
	// credentials — a nested map; MaskSecrets masked the whole key.
	if masked["credentials"] != maskedValue {
		t.Fatalf("credentials должен быть замаскирован целиком, got %v", masked["credentials"])
	}
}

func TestPlaintextIngestedMarkerSurvivesMasking(t *testing.T) {
	masked := MaskSecrets(map[string]any{"plaintext_ingested": true, "name": "ops-hook"})
	if masked["plaintext_ingested"] != true {
		t.Fatalf("маркер plaintext_ingested замаскирован (не должен): %v", masked["plaintext_ingested"])
	}
	if masked["name"] != "ops-hook" {
		t.Fatalf("name не должен маскироваться: %v", masked["name"])
	}
}
