package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/config"
)

// makeJWT builds a JWT with arbitrary claims (header/signature are stubs).
// The client doesn't verify the signature, so a valid base64 blob is enough.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestDecodeJWTClaims(t *testing.T) {
	jwt := makeJWT(t, map[string]any{
		"iss":   "keeper-eu-west-01",
		"sub":   "archon-alice",
		"iat":   int64(1716206200),
		"exp":   int64(1716292600),
		"roles": []string{"cluster-admin"},
	})
	claims, err := client.DecodeJWTClaims(jwt)
	if err != nil {
		t.Fatalf("DecodeJWTClaims: %v", err)
	}
	if claims.Sub != "archon-alice" {
		t.Errorf("sub: %q", claims.Sub)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "cluster-admin" {
		t.Errorf("roles: %v", claims.Roles)
	}
}

func TestDecodeJWTClaimsBadFormat(t *testing.T) {
	_, err := client.DecodeJWTClaims("not-a-jwt")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "three segments") {
		t.Errorf("unexpected message: %v", err)
	}
}

func TestArchonPing(t *testing.T) {
	hit := 0
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations": func(w http.ResponseWriter, r *http.Request) {
			hit++
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("limit: %q", r.URL.Query().Get("limit"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{}, "offset": 0, "limit": 1, "total": 0,
			})
		},
	})
	if err := cl.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if hit != 1 {
		t.Errorf("handler called %d times", hit)
	}
}

func TestArchonPingUnauthorized(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":  "https://soul-stack.io/errors/unauthenticated",
				"title": "unauthenticated", "status": 401,
				"detail": "JWT expired",
			})
		},
	})
	err := cl.Ping(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	rendered := renderAPIError(err)
	if !strings.Contains(rendered.Error(), "not authenticated") {
		t.Errorf("renderAPIError for 401: %q", rendered.Error())
	}
}

func TestArchonLoginSavesCredentials(t *testing.T) {
	jwt := makeJWT(t, map[string]any{"sub": "archon-alice"})
	srv, _ := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+jwt {
				t.Errorf("Authorization: %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{}, "offset": 0, "limit": 1, "total": 0,
			})
		},
	})

	tmpDir := t.TempDir()
	jwtFile := filepath.Join(tmpDir, "jwt")
	if err := os.WriteFile(jwtFile, []byte(jwt+"\n"), 0o600); err != nil {
		t.Fatalf("write jwt: %v", err)
	}
	credsPath := filepath.Join(tmpDir, "credentials.yaml")

	root := NewRoot("test")
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{
		"--config", credsPath,
		"archon", "login",
		"--keeper-url", srv.URL,
		"--jwt-file", jwtFile,
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout.String(), "logged in as archon-alice") {
		t.Errorf("output does not contain aid: %q", stdout.String())
	}
	loaded, err := config.Load(credsPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ArchonJWT != jwt {
		t.Errorf("stored JWT does not match")
	}
	if loaded.KeeperURL != srv.URL {
		t.Errorf("keeper_url: got %q, want %q", loaded.KeeperURL, srv.URL)
	}
	info, err := os.Stat(credsPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode credentials: got %o, want 0600", mode)
	}
}

func TestArchonLogoutRemovesCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	credsPath := filepath.Join(tmpDir, "credentials.yaml")
	if err := os.WriteFile(credsPath, []byte("keeper_url: x\narchon_jwt: y\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	root := NewRoot("test")
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"--config", credsPath, "archon", "logout"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(credsPath); !os.IsNotExist(err) {
		t.Errorf("credentials still exist: %v", err)
	}
	if !strings.Contains(stdout.String(), "logged out") {
		t.Errorf("output: %q", stdout.String())
	}
}
