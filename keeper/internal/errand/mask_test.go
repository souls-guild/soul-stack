package errand

import (
	"strings"
	"testing"
)

func TestMaskAndCapBytes_NoMask(t *testing.T) {
	s, trunc := MaskAndCapBytes("plain stdout")
	if trunc {
		t.Fatalf("trunc=true, want false")
	}
	if s != "plain stdout" {
		t.Fatalf("masked = %q, want passthrough", s)
	}
}

func TestMaskAndCapBytes_VaultRefMasked(t *testing.T) {
	// vault-ref inside a string is masked whole (shared/audit.MaskSecrets,
	// vaultRefRe matches `vault:<mount>/`).
	in := "error: bad creds at vault:secret/db-prod/conn"
	s, _ := MaskAndCapBytes(in)
	if strings.Contains(s, "vault:secret/") {
		t.Fatalf("masked = %q, vault-ref must be hidden", s)
	}
	if !strings.Contains(s, "MASKED") {
		t.Fatalf("masked = %q, want ***MASKED***", s)
	}
}

func TestMaskAndCapBytes_TruncCap(t *testing.T) {
	in := strings.Repeat("a", OutputCapBytes+1024)
	s, trunc := MaskAndCapBytes(in)
	if !trunc {
		t.Fatalf("trunc=false, want true (input > cap)")
	}
	if len(s) != OutputCapBytes {
		t.Fatalf("len(masked) = %d, want %d", len(s), OutputCapBytes)
	}
}

func TestMaskAndCapBytes_Empty(t *testing.T) {
	s, trunc := MaskAndCapBytes("")
	if trunc {
		t.Fatalf("trunc=true, want false (empty input)")
	}
	if s != "" {
		t.Fatalf("masked = %q, want empty", s)
	}
}

func TestMaskOutputMap_Nil(t *testing.T) {
	if out := MaskOutputMap(nil); out != nil {
		t.Fatalf("MaskOutputMap(nil) = %v, want nil", out)
	}
}

func TestMaskOutputMap_SecretKey(t *testing.T) {
	in := map[string]any{
		"status_code": 200,
		"token":       "secret-jwt-value",
	}
	out := MaskOutputMap(in)
	if out["token"] == "secret-jwt-value" {
		t.Fatalf("token is not masked: %v", out["token"])
	}
}
