package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// stubVault is an in-memory passwordResolver for password-resolution unit
// tests without a live Vault. Returns a pre-set KV payload by path.
type stubVault struct {
	byPath map[string]map[string]any
	err    error
}

func (s stubVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if s.err != nil {
		return nil, s.err
	}
	kv, ok := s.byPath[path]
	if !ok {
		return nil, errors.New("stubVault: path not found: " + path)
	}
	return kv, nil
}

// TestNewClient_EmptyAddr guards the sentinel error for an empty addr in
// standalone mode (the config parser doesn't catch it — this is a runtime
// invariant).
func TestNewClient_EmptyAddr(t *testing.T) {
	_, err := NewClient(context.Background(), Config{}, nil)
	if err == nil {
		t.Fatal("NewClient with empty addr should return error")
	}
}

// TestNewClient_Miniredis — happy path: connect to miniredis (standalone),
// Ping, Close. A repeat Close returns no error (idempotency contract).
func TestNewClient_Miniredis(t *testing.T) {
	mr := miniredis.RunT(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close should be idempotent, got %v", err)
	}
}

// TestNewClient_PingFails — an addr:port nobody is listening on, Ping
// fails, NewClient returns a wrapped error.
func TestNewClient_PingFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := NewClient(ctx, Config{Addr: "127.0.0.1:1"}, nil)
	if err == nil {
		t.Fatal("NewClient should fail on unreachable addr")
	}
}

// TestResolvedMode — empty Mode == standalone (forward-compat), everything
// else passes through as-is.
func TestResolvedMode(t *testing.T) {
	cases := map[string]string{
		"":           ModeStandalone,
		ModeSentinel: ModeSentinel,
		ModeCluster:  ModeCluster,
	}
	for in, want := range cases {
		if got := resolvedMode(in); got != want {
			t.Errorf("resolvedMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuild_ModeSelection — the constructor branches on Mode and validates
// the fields required for each mode (build without Ping, so no live Redis is
// needed).
func TestBuild_ModeSelection(t *testing.T) {
	t.Run("standalone requires addr", func(t *testing.T) {
		if _, err := build(Config{Mode: ModeStandalone}, "", ""); err == nil {
			t.Fatal("standalone without addr should error")
		}
		if _, err := build(Config{Mode: ModeStandalone, Addr: "x:6379"}, "", ""); err != nil {
			t.Fatalf("standalone with addr: %v", err)
		}
	})
	t.Run("sentinel requires master_name+sentinels", func(t *testing.T) {
		if _, err := build(Config{Mode: ModeSentinel, Sentinels: []string{"s:26379"}}, "", ""); err == nil {
			t.Fatal("sentinel without master_name should error")
		}
		if _, err := build(Config{Mode: ModeSentinel, MasterName: "mymaster"}, "", ""); err == nil {
			t.Fatal("sentinel without sentinels should error")
		}
		if _, err := build(Config{Mode: ModeSentinel, MasterName: "mymaster", Sentinels: []string{"s:26379"}}, "", ""); err != nil {
			t.Fatalf("sentinel ok: %v", err)
		}
	})
	t.Run("cluster requires nodes", func(t *testing.T) {
		if _, err := build(Config{Mode: ModeCluster}, "", ""); err == nil {
			t.Fatal("cluster without nodes should error")
		}
		if _, err := build(Config{Mode: ModeCluster, Nodes: []string{"n:6379"}}, "", ""); err != nil {
			t.Fatalf("cluster ok: %v", err)
		}
	})
	t.Run("unknown mode", func(t *testing.T) {
		if _, err := build(Config{Mode: "weird"}, "", ""); err == nil {
			t.Fatal("unknown mode should error")
		}
	})
}

// TestResolvePassword — every branch of vault password resolution.
func TestResolvePassword(t *testing.T) {
	ctx := context.Background()

	t.Run("empty ref -> empty", func(t *testing.T) {
		got, err := resolvePassword(ctx, nil, "")
		if err != nil || got != "" {
			t.Fatalf("got (%q,%v), want (\"\",nil)", got, err)
		}
	})

	t.Run("plaintext ref -> as-is", func(t *testing.T) {
		got, err := resolvePassword(ctx, nil, "s3cr3t")
		if err != nil || got != "s3cr3t" {
			t.Fatalf("got (%q,%v), want (\"s3cr3t\",nil)", got, err)
		}
	})

	t.Run("vault ref with nil vc -> ErrVaultClientRequired", func(t *testing.T) {
		_, err := resolvePassword(ctx, nil, "vault:secret/keeper/redis")
		if !errors.Is(err, ErrVaultClientRequired) {
			t.Fatalf("err = %v, want ErrVaultClientRequired", err)
		}
	})

	t.Run("vault ref default field=password", func(t *testing.T) {
		vc := stubVault{byPath: map[string]map[string]any{
			"secret/keeper/redis": {"password": "from-vault"},
		}}
		got, err := resolvePassword(ctx, vc, "vault:secret/keeper/redis")
		if err != nil || got != "from-vault" {
			t.Fatalf("got (%q,%v), want (\"from-vault\",nil)", got, err)
		}
	})

	t.Run("vault ref #field override", func(t *testing.T) {
		vc := stubVault{byPath: map[string]map[string]any{
			"secret/keeper/redis": {"password": "wrong", "rotated": "right"},
		}}
		got, err := resolvePassword(ctx, vc, "vault:secret/keeper/redis#rotated")
		if err != nil || got != "right" {
			t.Fatalf("got (%q,%v), want (\"right\",nil)", got, err)
		}
	})

	t.Run("vault ref missing field -> ErrPasswordFieldMissing", func(t *testing.T) {
		vc := stubVault{byPath: map[string]map[string]any{
			"secret/keeper/redis": {"other": "x"},
		}}
		_, err := resolvePassword(ctx, vc, "vault:secret/keeper/redis")
		if !errors.Is(err, ErrPasswordFieldMissing) {
			t.Fatalf("err = %v, want ErrPasswordFieldMissing", err)
		}
	})

	t.Run("vault ref non-string field -> error", func(t *testing.T) {
		vc := stubVault{byPath: map[string]map[string]any{
			"secret/keeper/redis": {"password": 12345},
		}}
		_, err := resolvePassword(ctx, vc, "vault:secret/keeper/redis")
		if err == nil {
			t.Fatal("non-string field should error")
		}
		if errors.Is(err, ErrPasswordFieldMissing) {
			t.Fatalf("non-string should be type-error, not ErrPasswordFieldMissing: %v", err)
		}
	})

	t.Run("malformed vault ref -> error", func(t *testing.T) {
		vc := stubVault{byPath: map[string]map[string]any{}}
		if _, err := resolvePassword(ctx, vc, "vault:noslash"); err == nil {
			t.Fatal("malformed vault-ref should error")
		}
	})
}

// TestNewClient_VaultRef_NilVC — password_ref: vault:... without a vc fails
// NewClient with ErrVaultClientRequired (this used to be
// ErrPasswordResolveNotImplemented).
func TestNewClient_VaultRef_NilVC(t *testing.T) {
	_, err := NewClient(context.Background(), Config{
		Addr:        "127.0.0.1:0",
		PasswordRef: "vault:secret/keeper/redis",
	}, nil)
	if !errors.Is(err, ErrVaultClientRequired) {
		t.Fatalf("err = %v, want ErrVaultClientRequired", err)
	}
}

// TestNewClient_VaultRef_Resolved — password_ref: vault:... resolves via a
// stub-vc, the client connects to miniredis (no password — miniredis ignores
// AUTH when requirepass isn't set, but the resolve path is exercised).
func TestNewClient_VaultRef_Resolved(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("from-vault")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	vc := stubVault{byPath: map[string]map[string]any{
		"secret/keeper/redis": {"password": "from-vault"},
	}}
	c, err := NewClient(ctx, Config{Addr: mr.Addr(), PasswordRef: "vault:secret/keeper/redis"}, vc)
	if err != nil {
		t.Fatalf("NewClient with resolved vault password: %v", err)
	}
	defer c.Close()
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping with auth: %v", err)
	}
}
