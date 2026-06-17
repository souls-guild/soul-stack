package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"testing"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

func genKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return priv
}

func TestExtractEd25519Key_RawForms(t *testing.T) {
	priv := genKey(t)

	cases := map[string]any{
		"raw 64 []byte":      []byte(priv),
		"raw 64 base64":      base64.StdEncoding.EncodeToString(priv),
		"raw 32 seed []byte": []byte(priv.Seed()),
		"raw 32 seed base64": base64.StdEncoding.EncodeToString(priv.Seed()),
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := extractEd25519Key(map[string]any{"signing_key": val})
			if err != nil {
				t.Fatalf("extractEd25519Key: %v", err)
			}
			if !got.Equal(priv) {
				t.Errorf("recovered key does not equal original (form %s)", name)
			}
		})
	}
}

func TestExtractEd25519Key_PKCS8_PEM_and_Base64DER(t *testing.T) {
	priv := genKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	t.Run("PEM string", func(t *testing.T) {
		got, err := extractEd25519Key(map[string]any{"signing_key": string(pemBytes)})
		if err != nil {
			t.Fatalf("extractEd25519Key PEM: %v", err)
		}
		if !got.Equal(priv) {
			t.Error("PEM-recovered key mismatch")
		}
	})

	t.Run("base64 DER string", func(t *testing.T) {
		got, err := extractEd25519Key(map[string]any{"signing_key": base64.StdEncoding.EncodeToString(der)})
		if err != nil {
			t.Fatalf("extractEd25519Key base64 DER: %v", err)
		}
		if !got.Equal(priv) {
			t.Error("base64-DER-recovered key mismatch")
		}
	})
}

func TestExtractEd25519Key_Missing(t *testing.T) {
	for _, kv := range []map[string]any{
		{},
		{"signing_key": ""},
		{"signing_key": []byte{}},
	} {
		if _, err := extractEd25519Key(kv); !errors.Is(err, ErrSigningKeyMissing) {
			t.Errorf("kv=%v: err = %v, want ErrSigningKeyMissing", kv, err)
		}
	}
}

// HS256-формат (короткий симметричный raw-secret) и прочий мусор → ошибка
// формата, не молчаливый мусорный ключ.
func TestExtractEd25519Key_InvalidFormats(t *testing.T) {
	cases := map[string]any{
		"hs256 raw 48 bytes":  make([]byte, 48), // не 32 и не 64
		"hs256 base64 string": base64.StdEncoding.EncodeToString(make([]byte, 48)),
		"plain garbage":       "not-a-key-at-all",
		"wrong type":          123,
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := extractEd25519Key(map[string]any{"signing_key": val})
			if err == nil {
				t.Fatalf("extractEd25519Key accepted invalid input %q", name)
			}
		})
	}
}

// RSA-ключ в PKCS#8 — валидный PKCS#8, но не ed25519 → ErrSigningKeyFormat.
// Покрывает ветку parsePKCS8Ed25519, где ParsePKCS8 проходит, но тип ≠ ed25519.
func TestExtractEd25519Key_RejectsNonEd25519PKCS8(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := extractEd25519Key(map[string]any{"signing_key": string(pemBytes)}); !errors.Is(err, ErrSigningKeyFormat) {
		t.Errorf("PEM RSA: err = %v, want ErrSigningKeyFormat", err)
	}

	// Сломанный PKCS#8 DER (нечитаемый) → тоже ErrSigningKeyFormat.
	broken := append([]byte{0x30, 0x03}, 0xFF, 0xFF, 0xFF)
	brokenPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: broken})
	if _, err := extractEd25519Key(map[string]any{"signing_key": string(brokenPEM)}); !errors.Is(err, ErrSigningKeyFormat) {
		t.Errorf("broken DER: err = %v, want ErrSigningKeyFormat", err)
	}
}

func TestLoadSigningKey_InputValidation(t *testing.T) {
	if _, err := LoadSigningKey(context.Background(), nil, "vault:secret/keeper/sigil"); err == nil {
		t.Error("LoadSigningKey accepted nil vault client")
	}
	vc := &keepervault.Client{}
	if _, err := LoadSigningKey(context.Background(), vc, ""); err == nil {
		t.Error("LoadSigningKey accepted empty ref")
	}
	if _, err := LoadSigningKey(context.Background(), vc, "not-a-vault-ref"); !errors.Is(err, keepervault.ErrInvalidVaultRef) {
		t.Errorf("invalid ref err = %v, want ErrInvalidVaultRef", err)
	}
}
