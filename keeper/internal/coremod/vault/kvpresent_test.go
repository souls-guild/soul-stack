package vault_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodvault "github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// presentVault is a fake Vault for kv-present: stores per-path payload in memory.
// ReadKV returns stored value (or ErrVaultKVNotFound for missing path),
// WriteKV merges. Test sees actual written values and checks their
// length/alphabet. readErr/writeErr inject transport errors (Vault
// unavailable/no permissions) for negative cases; writes counts actual WriteKV calls.
type presentVault struct {
	store    map[string]map[string]any
	readErr  error
	writeErr error
	writes   int
}

func newPresentVault(seed map[string]map[string]any) *presentVault {
	store := make(map[string]map[string]any)
	for p, m := range seed {
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		store[p] = cp
	}
	return &presentVault{store: store}
}

func (v *presentVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if v.readErr != nil {
		return nil, v.readErr
	}
	m, ok := v.store[path]
	if !ok {
		return nil, keepervault.ErrVaultKVNotFound
	}
	cp := make(map[string]any, len(m))
	for k, val := range m {
		cp[k] = val
	}
	return cp, nil
}

func (v *presentVault) WriteKV(_ context.Context, path string, data map[string]any) error {
	v.writes++
	if v.writeErr != nil {
		return v.writeErr
	}
	cp := make(map[string]any, len(data))
	for k, val := range data {
		cp[k] = val
	}
	v.store[path] = cp
	return nil
}

func presentParams(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	return mustStruct(t, m)
}

// TestPresent_AbsentPath_Generates: absent path → value generated, changed=true.
// Vault holds value of default length (32) from ascii-printable-safe
// alphabet (default). Value doesn't appear in output/audit (separate guard).
func TestPresent_AbsentPath_Generates(t *testing.T) {
	pv := newPresentVault(nil)
	fa := &fakeAudit{}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/redis/users/admin"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("want changed=true failed=false, got %+v", ev)
	}
	got, ok := pv.store["secret/redis/users/admin"]["password"]
	if !ok {
		t.Fatal("password not written to vault")
	}
	pw := got.(string)
	if len([]rune(pw)) != 32 {
		t.Errorf("default length = %d chars, want 32", len([]rune(pw)))
	}
	assertAlphabet(t, pw, safeAlphabetRunes())
	if len(fa.events) != 1 || fa.events[0].EventType != audit.EventVaultKVPresent {
		t.Errorf("audit events = %+v", fa.events)
	}
}

// TestPresent_PathPresent_NoOp: field present → no-op, changed=false.
// Value NOT overwritten, audit-event not written (idempotent).
func TestPresent_PathPresent_NoOp(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/redis/users/admin": {"password": "EXISTING-do-not-touch"},
	})
	fa := &fakeAudit{}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/redis/users/admin"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || ev.Changed {
		t.Fatalf("want changed=false failed=false (idempotent), got %+v", ev)
	}
	if pv.store["secret/redis/users/admin"]["password"] != "EXISTING-do-not-touch" {
		t.Errorf("existing secret overwritten: %v", pv.store["secret/redis/users/admin"]["password"])
	}
	if len(fa.events) != 0 {
		t.Errorf("no-op must not write audit, got %+v", fa.events)
	}
}

// TestPresent_PartPresent_GeneratesOnlyMissing: three targets, one exists.
// Only missing two are generated; existing one untouched.
func TestPresent_PartPresent_GeneratesOnlyMissing(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/app/a": {"password": "A-existing"},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/app/a"},
				map[string]any{"path": "secret/app/b"},
				map[string]any{"path": "secret/app/c"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("want changed=true")
	}
	if pv.store["secret/app/a"]["password"] != "A-existing" {
		t.Errorf("present secret a overwritten")
	}
	for _, p := range []string{"secret/app/b", "secret/app/c"} {
		v, ok := pv.store[p]["password"].(string)
		if !ok || v == "" {
			t.Errorf("missing secret %s not generated", p)
		}
	}
	out := stream.Last().Output.AsMap()
	generated := out["generated"].(map[string]any)
	if _, leaked := generated["secret/app/a"]; leaked {
		t.Error("output.generated lists already-present path a")
	}
	if len(generated) != 2 {
		t.Errorf("output.generated paths = %d, want 2 (b, c)", len(generated))
	}
}

// TestPresent_ExplicitPolicy_Respected: explicit charset+length (step-level)
// reflected in output: hex alphabet, length 16.
func TestPresent_ExplicitPolicy_Respected(t *testing.T) {
	pv := newPresentVault(nil)
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"policy": map[string]any{
				"charset": "hex",
				"length":  float64(16),
			},
			"targets": []any{
				map[string]any{"path": "secret/x"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	pw := pv.store["secret/x"]["password"].(string)
	if len([]rune(pw)) != 16 {
		t.Errorf("length = %d, want 16", len([]rune(pw)))
	}
	assertAlphabet(t, pw, []rune("0123456789abcdef"))
}

// TestPresent_AllowedChars_Respected: explicit allowed_chars alphabet respected.
// Per-target override takes precedence over step-level policy.
func TestPresent_AllowedChars_Respected(t *testing.T) {
	pv := newPresentVault(nil)
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"policy": map[string]any{"charset": "hex", "length": float64(20)},
			"targets": []any{
				map[string]any{
					"path":  "secret/override",
					"field": "token",
					"policy": map[string]any{
						"allowed_chars": "ABCDEF",
						"length":        float64(12),
					},
				},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	pw := pv.store["secret/override"]["token"].(string)
	if len([]rune(pw)) != 12 {
		t.Errorf("override length = %d, want 12", len([]rune(pw)))
	}
	assertAlphabet(t, pw, []rune("ABCDEF"))
}

// TestPresent_MergeKeepsSiblingFields: generation to path with existing
// sibling fields doesn't lose them (read-merge-write).
func TestPresent_MergeKeepsSiblingFields(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/u": {"username": "admin"},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/u", "field": "password"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pv.store["secret/u"]["username"] != "admin" {
		t.Error("sibling field username lost on merge")
	}
	if _, ok := pv.store["secret/u"]["password"].(string); !ok {
		t.Error("password not generated")
	}
}

// TestPresent_SecurityNoLeak is a GUARD: generated VALUE doesn't leak to
// register-output or audit-payload (ADR-010).
// Recursively checks whole output and payload: secret must not appear
// as key, value, or substring.
func TestPresent_SecurityNoLeak(t *testing.T) {
	pv := newPresentVault(nil)
	fa := &fakeAudit{}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/leak/check", "field": "password"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	secret := pv.store["secret/leak/check"]["password"].(string)
	if secret == "" {
		t.Fatal("precondition: secret not generated")
	}

	// 1. register-output doesn't contain value anywhere in tree
	out := stream.Last().Output.AsMap()
	if containsSecret(out, secret) {
		t.Errorf("SECURITY: generated secret leaked into register-output: %v", out)
	}
	// Sanity: path/field name in output exists (output not empty and sensible).
	gen := out["generated"].(map[string]any)
	if _, ok := gen["secret/leak/check"]; !ok {
		t.Error("output.generated must list the path (field names ok, value not)")
	}

	// 2. audit-payload doesn't contain value
	if len(fa.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(fa.events))
	}
	if containsSecret(fa.events[0].Payload, secret) {
		t.Errorf("SECURITY: generated secret leaked into audit-payload: %v", fa.events[0].Payload)
	}
}

// TestPresent_Validate: kv-present validation. Targets required and non-empty;
// mutual exclusion charset/allowed_chars; length boundaries.
func TestPresent_Validate(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	cases := []struct {
		name   string
		params map[string]any
		wantOK bool
	}{
		{"missing targets", map[string]any{}, false},
		{"empty targets", map[string]any{"targets": []any{}}, false},
		{"ok minimal", map[string]any{"targets": []any{map[string]any{"path": "secret/x"}}}, true},
		{
			"charset and allowed_chars together",
			map[string]any{
				"policy":  map[string]any{"charset": "hex", "allowed_chars": "abc"},
				"targets": []any{map[string]any{"path": "secret/x"}},
			},
			false,
		},
		{
			"unknown charset",
			map[string]any{
				"policy":  map[string]any{"charset": "klingon"},
				"targets": []any{map[string]any{"path": "secret/x"}},
			},
			false,
		},
		{
			"length below min",
			map[string]any{
				"policy":  map[string]any{"length": float64(4)},
				"targets": []any{map[string]any{"path": "secret/x"}},
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State:  "kv-present",
				Params: mustStruct(t, tc.params),
			})
			if rep.Ok != tc.wantOK {
				t.Errorf("Ok = %v, want %v (errors=%v)", rep.Ok, tc.wantOK, rep.Errors)
			}
		})
	}
}

// TestPresent_UnknownState: unknown state on same module fails the task.
func TestPresent_UnknownState(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "kv-generate",
		Params: mustStruct(t, map[string]any{"targets": []any{map[string]any{"path": "x"}}}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("unknown state must fail")
	}
}

// TestPresent_WriteKVError_FailsTask: WriteKV-failure (Vault unavailable/no write
// permission) → task fails honestly, error not swallowed. Analog of
// kvread_test.go::TestApply_VaultError for write branch. read succeeds (path
// missing → generation), failure exactly on WriteKV.
func TestPresent_WriteKVError_FailsTask(t *testing.T) {
	pv := newPresentVault(nil)
	pv.writeErr = errors.New("permission denied")
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("WriteKV error must fail task (write-failure must not be silently swallowed)")
	}
}

// TestPresent_AuditWriteError_FailsTask: audit-write-fail → task fails
// (symmetry with kvread_test.go::TestApply_AuditWriteError_FailsTask): compliance
// write can't be silently skipped. Secret ALREADY in VAULT (WriteKV passed before
// audit) — check only terminal fail here.
func TestPresent_AuditWriteError_FailsTask(t *testing.T) {
	pv := newPresentVault(nil)
	fa := &fakeAudit{err: errors.New("pg down")}
	m := coremodvault.New(pv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("audit write error must fail task (compliance must not be silently skipped)")
	}
}

// TestPresent_EmptyStringField_Regenerates: field present, but value is
// EMPTY string → treated as absent (fieldPresent), non-empty password generated,
// changed=true. Empty password useless, so no-op on "" would be a hole.
func TestPresent_EmptyStringField_Regenerates(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/redis/users/admin": {"password": ""},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("empty string must be treated as absent → regenerate, changed=true")
	}
	pw, _ := pv.store["secret/redis/users/admin"]["password"].(string)
	if pw == "" {
		t.Error("empty password not regenerated")
	}
}

// TestPresent_DifferentFieldPresent_GeneratesTargetKeepsSibling: path exists with
// DIFFERENT field (username), target (password) missing → only password generated,
// existing username preserved. Differs from MergeKeepsSiblingFields by focus:
// explicitly check that non-target field presence doesn't mean "secret exists"
// (fieldPresent checks specific field, not path).
func TestPresent_DifferentFieldPresent_GeneratesTargetKeepsSibling(t *testing.T) {
	pv := newPresentVault(map[string]map[string]any{
		"secret/redis/users/admin": {"username": "admin"},
	})
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{map[string]any{"path": "secret/redis/users/admin", "field": "password"}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("target field password missing → should be generated (changed=true)")
	}
	if pv.store["secret/redis/users/admin"]["username"] != "admin" {
		t.Error("sibling username lost on password generation")
	}
	if pw, _ := pv.store["secret/redis/users/admin"]["password"].(string); pw == "" {
		t.Error("password not generated")
	}
}

// TestPresent_DuplicateTargetGeneratesOnce: two targets with same {path,field}
// → generated ONCE (pendingWrites-guard): one WriteKV, field in output.generated
// not duplicated. Without guard, second target would regenerate value and/or give
// extra WriteKV (new KV-version).
func TestPresent_DuplicateTargetGeneratesOnce(t *testing.T) {
	pv := newPresentVault(nil)
	m := coremodvault.New(pv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-present",
		Params: presentParams(t, map[string]any{
			"targets": []any{
				map[string]any{"path": "secret/dup", "field": "password"},
				map[string]any{"path": "secret/dup", "field": "password"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pv.writes != 1 {
		t.Errorf("WriteKV called %d times, want 1 (duplicate {path,field} must not trigger extra write)", pv.writes)
	}
	gen := stream.Last().Output.AsMap()["generated"].(map[string]any)
	fields, ok := gen["secret/dup"].([]any)
	if !ok || len(fields) != 1 {
		t.Errorf("output.generated[secret/dup] = %v, want exactly one field password (no duplicate)", gen["secret/dup"])
	}
}

// TestPresent_PolicyLengthBoundaries: length boundaries: 8 and 1024 valid,
// 7 and 1025 rejected. Checked via Validate (parsePolicy) on step-level policy.
func TestPresent_PolicyLengthBoundaries(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	cases := []struct {
		length int
		wantOK bool
	}{
		{7, false}, {8, true}, {1024, true}, {1025, false},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State: "kv-present",
				Params: mustStruct(t, map[string]any{
					"policy":  map[string]any{"length": float64(tc.length)},
					"targets": []any{map[string]any{"path": "secret/x"}},
				}),
			})
			if rep.Ok != tc.wantOK {
				t.Errorf("length=%d: Ok=%v, want %v (errors=%v)", tc.length, rep.Ok, tc.wantOK, rep.Errors)
			}
		})
	}
}

// TestPresent_EmptyPathRejected: target with empty path rejected on Validate
// (parseTargets/parseTarget): empty path is invalid target.
func TestPresent_EmptyPathRejected(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "kv-present",
		Params: mustStruct(t, map[string]any{
			"targets": []any{map[string]any{"path": ""}},
		}),
	})
	if rep.Ok {
		t.Fatal("empty path in target must be rejected")
	}
}

// TestPresent_SingleCharAlphabetRejected: allowed_chars of one character (or
// collapsed to one after deduplication) rejected: alphabet <2 distinct chars
// degenerates generation to constant (0 entropy).
func TestPresent_SingleCharAlphabetRejected(t *testing.T) {
	m := coremodvault.New(newPresentVault(nil), &fakeAudit{})
	for _, allowed := range []string{"a", "aaaa"} {
		rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "kv-present",
			Params: mustStruct(t, map[string]any{
				"policy":  map[string]any{"allowed_chars": allowed},
				"targets": []any{map[string]any{"path": "secret/x"}},
			}),
		})
		if rep.Ok {
			t.Errorf("allowed_chars=%q (collapse-to-one) must be rejected (need >=2 distinct)", allowed)
		}
	}
}

// --- helpers ---

// assertAlphabet checks that each char in s is in allowed.
func assertAlphabet(t *testing.T, s string, allowed []rune) {
	t.Helper()
	set := make(map[rune]bool, len(allowed))
	for _, r := range allowed {
		set[r] = true
	}
	for _, r := range s {
		if !set[r] {
			t.Fatalf("char %q outside allowed alphabet (value=%q)", r, s)
		}
	}
}

// safeAlphabetRunes reproduces ascii-printable-safe alphabet (0x21..0x7E minus
// excluded) for assertAlphabet default tests. Keep exclusion list in sync with
// policy.go::excludedFromSafe.
func safeAlphabetRunes() []rune {
	const excluded = " \"'#\\`$"
	ex := make(map[byte]bool)
	for i := 0; i < len(excluded); i++ {
		ex[excluded[i]] = true
	}
	var out []rune
	for c := byte(0x21); c <= 0x7E; c++ {
		if !ex[c] {
			out = append(out, rune(c))
		}
	}
	return out
}

// containsSecret recursively searches for exact secret value in arbitrary
// map/slice/string tree (keys and values). Substring match also counts as leak
// (partial disclosure not allowed).
func containsSecret(v any, secret string) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, secret)
	case map[string]any:
		for k, val := range x {
			if strings.Contains(k, secret) || containsSecret(val, secret) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if containsSecret(val, secret) {
				return true
			}
		}
	}
	return false
}
