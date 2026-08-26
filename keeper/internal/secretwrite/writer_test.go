package secretwrite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeVault is a VaultWriter stub that records the latest write.
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

// TestPathRejectsUnsafeSegments verifies fail-closed rejection of scope bypass (`..`, slashes, empty).
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

// TestSecretValueNotInError verifies plaintext never leaks into Vault write error text
// (primary mitigation for ADR-064(b)).
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

// Every write domain must be refused as a service name (NIM-706).
//
// This package writes `<mount>/<domain>/<entity>/<field>` while a declared secret
// derives `<mount>/<service>/<incarnation>/<field>[/<key>]` ([ADR-0083] §1). The two
// families share the mount and both fix their first segment, so a service named after
// a domain lands its incarnation on this package's `<entity>` slot and its state field
// on this package's `<field>` slot — the same KV entry, two writers.
//
// The collision is not symmetric with the one under `keeper`. [Writer.WriteString]
// REPLACES the entry (Vault KV v2 has no merge), where the state mint reads first and
// merges; so on this path the second write silently deletes the first one's fields.
//
// The subject is [WriteDomains], not a literal pair. Naming today's two members here
// would leave this green for a THIRD domain added later — the one change that reopens
// the collision without touching anything this file mentions. Iterating the closed set
// makes the guard grow with the package; [Writer.path] refusing an unlisted domain is
// what stops a new constant from quietly bypassing the set.
func TestWriteDomains_AreReservedServiceNames(t *testing.T) {
	for _, domain := range WriteDomains() {
		if !config.IsReservedVaultNamespace(domain) {
			t.Errorf("secretwrite domain %q is not a reserved service name: a service by that "+
				"name derives secrets onto this package's paths, and WriteString replaces "+
				"rather than merges", domain)
		}
	}
}

// The floor under the derivation refuses the same words ([config.SecretField.VaultPath]),
// so a domain that somehow reached the mint is denied a path instead of being handed
// one that overwrites this package's.
//
// Every segment other than the service name is a valid one, and the identical field is
// derived once against a service that is NOT a domain. Both halves are load-bearing: a
// zero-value SecretField is refused for its empty state field long before the namespace
// is consulted, so without the control this would stay green with the floor deleted and
// assert nothing at all.
func TestWriteDomains_AreDeniedByTheDerivationFloor(t *testing.T) {
	f := config.SecretField{State: "password"}
	if _, err := f.VaultPath(defaultMount, "postgres", "prod", ""); err != nil {
		t.Fatalf("the control derivation failed for a reason unrelated to the name, so a "+
			"refusal below would prove nothing: %v", err)
	}
	for _, domain := range WriteDomains() {
		if _, err := f.VaultPath(defaultMount, domain, "prod", ""); err == nil {
			t.Errorf("VaultPath derived a path for service %q instead of refusing it", domain)
		}
	}
}

// The set is enforced, not documentary: a segment-safe word that is not a domain writes
// nowhere. Without this the closed set would be a naming convention, and the guard above
// would prove only that two constants happen to be reserved.
func TestPath_RefusesUnknownDomain(t *testing.T) {
	w, err := NewWriter(&fakeVault{}, "")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	// `sigil` is segment-safe and a real keeper subsystem — exactly the shape of a
	// domain someone would add as a constant and forget to reserve.
	if _, err := w.WriteString(context.Background(), "sigil", "prod", "token", "v"); err == nil {
		t.Fatal("WriteString accepted an unlisted domain: the closed set is not enforced")
	}
}
