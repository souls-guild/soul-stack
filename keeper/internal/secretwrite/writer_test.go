package secretwrite

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeVault — VaultWriter-заглушка, запоминает последнюю запись.
type fakeVault struct {
	path string
	data map[string]any
	err  error
}

func (f *fakeVault) WriteKV(_ context.Context, path string, data map[string]any) error {
	if f.err != nil {
		return f.err
	}
	f.path = path
	f.data = data
	return nil
}

func TestNewWriter(t *testing.T) {
	if _, err := NewWriter(nil, "secret"); err == nil {
		t.Fatal("NewWriter(nil) must error")
	}
	w, err := NewWriter(&fakeVault{}, "")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if w.mount != defaultMount {
		t.Fatalf("empty mount → %q, want %q", w.mount, defaultMount)
	}
}

func TestWriteString(t *testing.T) {
	fv := &fakeVault{}
	w, _ := NewWriter(fv, "secret")
	ref, err := w.WriteString(context.Background(), DomainHerald, "my-hook", "secret", "hunter2")
	if err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if want := "vault:secret/herald/my-hook/secret#secret"; ref != want {
		t.Fatalf("ref=%q want %q", ref, want)
	}
	if fv.path != "secret/herald/my-hook/secret" {
		t.Fatalf("vault path=%q", fv.path)
	}
	if fv.data["secret"] != "hunter2" {
		t.Fatalf("written data=%v", fv.data)
	}
}

func TestWriteMap(t *testing.T) {
	fv := &fakeVault{}
	w, _ := NewWriter(fv, "secret")
	creds := map[string]any{"access_key": "AKIA", "secret_key": "s3cr3t"}
	ref, err := w.WriteMap(context.Background(), DomainProvider, "aws-prod", "credentials", creds)
	if err != nil {
		t.Fatalf("WriteMap: %v", err)
	}
	if want := "vault:secret/provider/aws-prod/credentials"; ref != want {
		t.Fatalf("ref=%q want %q", ref, want)
	}
	if fv.data["access_key"] != "AKIA" || fv.data["secret_key"] != "s3cr3t" {
		t.Fatalf("written data=%v", fv.data)
	}
}

func TestCustomMountInRef(t *testing.T) {
	fv := &fakeVault{}
	w, _ := NewWriter(fv, "kv")
	ref, _ := w.WriteString(context.Background(), DomainHerald, "h", "secret", "v")
	if !strings.HasPrefix(ref, "vault:kv/herald/") {
		t.Fatalf("custom mount not in ref: %q", ref)
	}
}

// TestPathRejectsUnsafeSegments — fail-closed на обход scope (`..`, слеши, пусто).
func TestPathRejectsUnsafeSegments(t *testing.T) {
	fv := &fakeVault{}
	w, _ := NewWriter(fv, "secret")
	for _, bad := range []string{"..", "a/b", "", "a.b", "a b"} {
		if _, err := w.WriteString(context.Background(), DomainHerald, bad, "secret", "v"); err == nil {
			t.Fatalf("entity %q must be rejected", bad)
		}
		if _, err := w.WriteString(context.Background(), DomainHerald, "ok", bad, "v"); err == nil {
			t.Fatalf("field %q must be rejected", bad)
		}
	}
}

// TestSecretValueNotInError — plaintext НЕ утекает в текст ошибки Vault-записи
// (главный митигейшн ADR-064(b)).
func TestSecretValueNotInError(t *testing.T) {
	const plaintext = "SUPER-SECRET-TOKEN-42"
	fv := &fakeVault{err: errors.New("vault down")}
	w, _ := NewWriter(fv, "secret")

	_, err := w.WriteString(context.Background(), DomainHerald, "h", "secret", plaintext)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("plaintext leaked into error: %v", err)
	}

	_, err = w.WriteMap(context.Background(), DomainProvider, "p", "credentials",
		map[string]any{"k": plaintext})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("plaintext leaked into error: %v", err)
	}
}

func TestEmptyValueRejected(t *testing.T) {
	fv := &fakeVault{}
	w, _ := NewWriter(fv, "secret")
	if _, err := w.WriteString(context.Background(), DomainHerald, "h", "secret", ""); err == nil {
		t.Fatal("empty value must error")
	}
	if _, err := w.WriteMap(context.Background(), DomainProvider, "p", "credentials", nil); err == nil {
		t.Fatal("empty map must error")
	}
}
