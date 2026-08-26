package config

import (
	"strings"
	"testing"
)

// The closed list, held here as a literal so a name cannot leave it unnoticed.
//
// Every other guard in this file asks the predicate, so they would all stay green if a
// word quietly dropped out — and a word dropping out is the one change that reopens the
// collision. Failing here is not automatically a bug: the list is closed and moving it
// is propose-and-wait plus a PR to docs/naming-rules.md. If it legitimately moved, move
// this with it and say so out loud.
func TestReservedVaultNamespaces_MatchTheSettledList(t *testing.T) {
	want := map[string]bool{
		// A writer puts each of these in the first segment after the mount.
		"keeper":   true, // keeper's own runtime secrets + sigil keys (ADR-014)
		"herald":   true, // keeper/internal/secretwrite.DomainHerald
		"provider": true, // keeper/internal/secretwrite.DomainProvider
		// No writer, but the read floor has refused `secret/internal/` since NIM-74: a
		// name the read side will not hand back is a name the write side must not mint
		// under, or a service mints secrets it can never reveal.
		"internal": true,
	}
	got := map[string]bool{}
	for _, n := range ReservedVaultNamespaceNames() {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("reserved list lost %q — a service can now be registered under it and derive on top of the platform's own secrets", n)
		}
	}
	for n := range got {
		if !want[n] {
			t.Errorf("reserved list gained %q; adding to a closed list is propose-and-wait plus a docs PR", n)
		}
	}
}

// The lookup normalises before comparing. A validator that refused `herald` while
// waving `Herald ` through would hand the collision back through the door it just
// closed — and it would, because the service-name grammar is applied independently and
// a caller may reach the predicate first.
func TestIsReservedVaultNamespace_FoldsCaseAndSpace(t *testing.T) {
	for _, name := range []string{"herald", "Herald", "HERALD", " herald ", "\tProvider\n"} {
		if !IsReservedVaultNamespace(name) {
			t.Errorf("IsReservedVaultNamespace(%q) = false, want true", name)
		}
	}
	// Whole word, not prefix: a neighbouring name is a different namespace.
	for _, name := range []string{"heralds", "herald-x", "my-keeper", "", "web"} {
		if IsReservedVaultNamespace(name) {
			t.Errorf("IsReservedVaultNamespace(%q) = true, want false", name)
		}
	}
}

// ★ The derivation floor (NIM-706). Registration refuses the name, so reaching this is
// either a service admitted before the rule existed or a route that skipped it. Both are
// cases for failing closed: the alternative is emitting a path that lands on
// `secret/herald/…`, where keeper/internal/secretwrite REPLACES the KV entry rather than
// merging into it and the loser of the race is destroyed with no error anywhere.
func TestVaultPath_RefusesReservedServiceNamespace(t *testing.T) {
	f := SecretField{State: "password", Path: ".properties.password"}
	for _, svc := range ReservedVaultNamespaceNames() {
		path, err := f.VaultPath("", svc, "prod", "")
		if err == nil {
			t.Errorf("VaultPath(service=%q) = %q, want an error — the derived path collides with the platform's own", svc, path)
			continue
		}
		if !strings.Contains(err.Error(), svc) {
			t.Errorf("VaultPath(service=%q) error %q does not name the offending service", svc, err)
		}
	}
	// The floor is on the service segment, not on the mount: a non-default
	// `vault.kv_mount` must not be a way around it.
	if _, err := f.VaultPath("kv-prod", "herald", "prod", ""); err == nil {
		t.Error("VaultPath on a non-default mount admitted a reserved service — the floor must not depend on the mount")
	}
	// And an ordinary service is untouched.
	got, err := f.VaultPath("", "redis", "prod", "")
	if err != nil || got != "secret/redis/prod/password" {
		t.Fatalf("VaultPath(service=redis) = %q, %v; want secret/redis/prod/password, nil", got, err)
	}
}

// The read-side half of the same list. Pinned separately from DeniedByVaultFloor because
// this predicate is what makes the floor independent of `vault.kv_mount` — the property
// its literal-prefix predecessor did not have.
func TestPathUnderReservedNamespace(t *testing.T) {
	cases := []struct {
		logical string
		want    bool
	}{
		{"secret/keeper/jwt-signing-key", true},
		{"secret/herald/h1/token", true},
		{"secret/provider/p1/credentials", true},
		{"secret/internal/x", true},
		// Any mount, including the form where vault.Client.ReadKV substitutes it.
		{"kv-prod/herald/h1/token", true},
		{"herald/h1/token", true},
		// The `vault:` marker and a `#field` selector are stripped first.
		{"vault:secret/keeper/x#value", true},
		// Self-normalising: `//` collapses downstream onto the very entry this protects.
		{"secret//keeper/x", true},
		// Whole segments only.
		{"secret/keeper-notes/x", false},
		{"secret/heralds/x", false},
		{"secret/redis/prod/password", false},
		// A reserved word deeper than segs[1] is somebody else's directory, not the
		// platform's namespace — refusing it would fence `secret/redis/keeper/…`.
		{"secret/redis/keeper/x", false},
		{"", false},
	}
	for _, c := range cases {
		if got := PathUnderReservedNamespace(c.logical); got != c.want {
			t.Errorf("PathUnderReservedNamespace(%q) = %v, want %v", c.logical, got, c.want)
		}
	}
}

// ★ The second axis (NIM-706): a collision INSIDE one service, which no service-name
// rule can reach. keeper/internal/certissue writes issued TLS material to
// `<mount>/<svc>/<inc>/tls/{cert,key}`; a collection secret on a state field named `tls`
// with an element key of `cert` derives the identical path. The element key is state
// DATA, unknown at authoring time, so the field name is the last static point where this
// can be refused.
func TestCollectSecretFields_RefusesReservedStateField(t *testing.T) {
	t.Run("collection", func(t *testing.T) {
		_, issues := CollectSecretFields(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tls": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{"type": "string"},
							"cert": map[string]any{"type": "secret", "key": "name"},
						},
					},
				},
			},
		})
		requireIssueCode(t, issues, SecretFieldReservedStateCode)
	})

	// A scalar derives `<mount>/<svc>/<inc>/tls`, which KV v2 keeps distinct from
	// `…/tls/cert` — not a data collision, but two owners on one prefix, which is the
	// confusion the namespace fence exists to remove. One predicate, both shapes.
	t.Run("scalar", func(t *testing.T) {
		_, issues := CollectSecretFields(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tls": map[string]any{"type": "secret"},
			},
		})
		requireIssueCode(t, issues, SecretFieldReservedStateCode)
	})

	// A non-reserved field of the same shape stays accepted — the guard must be the
	// name, not the shape.
	t.Run("ordinary-field-untouched", func(t *testing.T) {
		fields, issues := CollectSecretFields(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"password": map[string]any{"type": "secret"},
			},
		})
		for _, iss := range issues {
			if iss.Code == SecretFieldReservedStateCode {
				t.Fatalf("ordinary state field rejected as reserved: %+v", iss)
			}
		}
		if len(fields) != 1 || fields[0].State != "password" {
			t.Fatalf("fields = %+v, want one field on state `password`", fields)
		}
	})
}

func requireIssueCode(t *testing.T, issues []SecretFieldIssue, code string) {
	t.Helper()
	for _, iss := range issues {
		if iss.Code == code {
			return
		}
	}
	t.Fatalf("issues = %+v, want one with code %q", issues, code)
}
