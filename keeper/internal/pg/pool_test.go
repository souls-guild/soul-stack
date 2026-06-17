package pg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

func TestResolveDSN_PlainPassthrough(t *testing.T) {
	const dsn = "postgres://keeper:keeper@localhost:5432/keeper?sslmode=disable"
	got, err := ResolveDSN(context.Background(), nil, dsn)
	if err != nil {
		t.Fatalf("ResolveDSN: %v", err)
	}
	if got != dsn {
		t.Errorf("got = %q, want %q", got, dsn)
	}
}

func TestResolveDSN_VaultRefRequiresClient(t *testing.T) {
	_, err := ResolveDSN(context.Background(), nil, "vault:secret/keeper/postgres")
	if !errors.Is(err, ErrVaultClientRequired) {
		t.Errorf("err = %v, want errors.Is ErrVaultClientRequired", err)
	}
}

func TestResolveDSN_EmptyRef(t *testing.T) {
	_, err := ResolveDSN(context.Background(), nil, "")
	if !errors.Is(err, ErrEmptyDSN) {
		t.Errorf("err = %v, want errors.Is ErrEmptyDSN", err)
	}
}

func TestExtractDSN_HappyPath(t *testing.T) {
	const dsn = "postgres://u:p@h:5432/db?sslmode=disable"
	got, err := extractDSN(map[string]any{"dsn": dsn})
	if err != nil {
		t.Fatalf("extractDSN: %v", err)
	}
	if got != dsn {
		t.Errorf("got = %q, want %q", got, dsn)
	}
}

func TestExtractDSN_Missing(t *testing.T) {
	if _, err := extractDSN(map[string]any{"other": "x"}); !errors.Is(err, ErrDSNFieldMissing) {
		t.Errorf("err = %v, want ErrDSNFieldMissing", err)
	}
}

func TestExtractDSN_Empty(t *testing.T) {
	if _, err := extractDSN(map[string]any{"dsn": ""}); !errors.Is(err, ErrDSNFieldMissing) {
		t.Errorf("err = %v, want ErrDSNFieldMissing", err)
	}
}

func TestExtractDSN_UnsupportedType(t *testing.T) {
	if _, err := extractDSN(map[string]any{"dsn": 42}); err == nil {
		t.Errorf("unsupported type: expected error, got nil")
	}
}

func TestNewPool_RejectsVaultRefWithoutClient(t *testing.T) {
	_, err := NewPool(context.Background(), config.KeeperPostgres{
		DSNRef: "vault:secret/keeper/postgres",
	}, nil)
	if !errors.Is(err, ErrVaultClientRequired) {
		t.Fatalf("err = %v, want ErrVaultClientRequired", err)
	}
}

func TestNewPool_RejectsEmptyDSN(t *testing.T) {
	_, err := NewPool(context.Background(), config.KeeperPostgres{}, nil)
	if err == nil {
		t.Fatal("NewPool with empty dsn_ref returned nil err")
	}
	if !errors.Is(err, ErrEmptyDSN) {
		t.Errorf("err = %v, want errors.Is ErrEmptyDSN", err)
	}
}

func TestNewPool_RejectsMalformedDSN(t *testing.T) {
	// `pgxpool.ParseConfig` фейлит на не-URL / не-keyvalue строке.
	_, err := NewPool(context.Background(), config.KeeperPostgres{
		DSNRef: "not-a-dsn",
	}, nil)
	if err == nil {
		t.Fatal("NewPool with bogus DSN returned nil err")
	}
	if !strings.Contains(err.Error(), "parse DSN") {
		t.Errorf("err = %v, want substring \"parse DSN\"", err)
	}
}
