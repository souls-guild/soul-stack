package cel

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/secretpolicy"
)

// marker extracts the secret-request marker payload from a rendered cell, failing the
// test if the cell is not exactly one marker.
func marker(t *testing.T, out any) map[string]any {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("rendered cell is %T, want map[string]any", out)
	}
	inner, ok := m[secretpolicy.MarkerKey].(map[string]any)
	if !ok {
		t.Fatalf("rendered cell %v carries no %s object", m, secretpolicy.MarkerKey)
	}
	if len(m) != 1 {
		t.Fatalf("rendered cell %v has %d keys, want only %s", m, len(m), secretpolicy.MarkerKey)
	}
	return inner
}

// TestGenerateSecret_YieldsRequestNotValue — the load-bearing property of [ADR-0083]
// §3: the call renders to a REQUEST carrying the policy, never to a secret. If it ever
// returned a string, render (re-run per passage and per retry) would mint a different
// value every time.
func TestGenerateSecret_YieldsRequestNotValue(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ generate_secret({'length': 40, 'charset': 'alphanumeric'}) }`, Vars{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := marker(t, out)

	if got["length"] != 40 {
		t.Fatalf("length = %v (%T), want 40", got["length"], got["length"])
	}
	alphabet, _ := got["allowed_chars"].(string)
	if alphabet != secretpolicy.Alphabets[secretpolicy.CharsetAlphanumeric] {
		t.Fatalf("allowed_chars = %q, want the alphanumeric alphabet", alphabet)
	}
}

// TestGenerateSecret_Defaults — `generate_secret({})` is the all-defaults spelling.
func TestGenerateSecret_Defaults(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ generate_secret({}) }`, Vars{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := marker(t, out)

	def := secretpolicy.Default()
	if got["length"] != def.Length {
		t.Fatalf("length = %v, want the default %d", got["length"], def.Length)
	}
	if got["allowed_chars"] != string(def.Alphabet) {
		t.Fatalf("allowed_chars = %q, want the default alphabet", got["allowed_chars"])
	}
}

// TestGenerateSecret_Pure — two evaluations of the same expression render identically.
// This is what makes render re-runnable ([ADR-0083] §Rejected, "returning plaintext at
// render"): a generating function would fail this test by construction, and soul-lint
// and Trial output would stop being deterministic on an unchanged tree.
func TestGenerateSecret_Pure(t *testing.T) {
	e := newEngine(t)
	const expr = `${ generate_secret({'length': 16, 'charset': 'hex'}) }`

	first, err := e.EvalInterpolation(expr, Vars{})
	if err != nil {
		t.Fatalf("eval 1: %v", err)
	}
	second, err := e.EvalInterpolation(expr, Vars{})
	if err != nil {
		t.Fatalf("eval 2: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two renders differ:\n 1: %v\n 2: %v\n(the request must be pure — a value would differ every evaluation)", first, second)
	}
}

// TestGenerateSecret_RefusesStringConcatenation — a cell mixing literal text with the
// call is an ERROR, not a rendered marker. A cell that is exactly one `${ … }` returns
// a native value, but any other cell goes through stringification — so without the
// refusal in ConvertToType, `pw-${ generate_secret({}) }` would write a marker into a
// config file and the run would look successful.
func TestGenerateSecret_RefusesStringConcatenation(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`pw-${ generate_secret({}) }`, Vars{})
	if err == nil {
		t.Fatalf("mixed cell rendered to %v, want an error", out)
	}
	var evalErr *ErrEval
	if !errors.As(err, &evalErr) {
		t.Fatalf("error is %T (%v), want *ErrEval", err, err)
	}
	if !strings.Contains(err.Error(), "SecretRequest") {
		t.Fatalf("error %q does not name the type that cannot be concatenated", err)
	}
}

// TestGenerateSecret_NoFieldAccess — a request is opaque: it has no readable fields, so
// an author cannot pull the policy back out (or believe they pulled a value out).
func TestGenerateSecret_NoFieldAccess(t *testing.T) {
	e := newEngine(t)

	if _, err := e.EvalInterpolation(`${ generate_secret({}).length }`, Vars{}); err == nil {
		t.Fatal("field access on a SecretRequest compiled, want an error")
	}
}

// TestGenerateSecret_InCollectionShape — the shape [ADR-0083] §3 documents: a request
// per element, merged into an object alongside ordinary properties. The request must
// survive merge() and .map() into native data without being flattened or stringified.
func TestGenerateSecret_InCollectionShape(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ compute.acl_inventory.map(u, merge(u, {'password': generate_secret({'length': 32, 'charset': 'alphanumeric'})})) }`,
		Vars{Compute: map[string]any{"acl_inventory": []any{
			map[string]any{"name": "default_admin", "perms": "+@all"},
			map[string]any{"name": "replica", "perms": "+@read"},
		}}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	list, ok := out.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("rendered %T (%v), want a 2-element list", out, out)
	}
	for i, el := range list {
		obj, ok := el.(map[string]any)
		if !ok {
			t.Fatalf("element %d is %T, want map[string]any", i, el)
		}
		if obj["name"] == nil || obj["perms"] == nil {
			t.Fatalf("element %d lost its ordinary properties: %v", i, obj)
		}
		inner, ok := obj["password"].(map[string]any)[secretpolicy.MarkerKey].(map[string]any)
		if !ok {
			t.Fatalf("element %d password is %v, want a secret-request marker", i, obj["password"])
		}
		if inner["length"] != 32 {
			t.Fatalf("element %d policy length = %v, want 32", i, inner["length"])
		}
	}
}

// TestGenerateSecret_PolicyErrorAtCallSite — the policy is parsed eagerly, so a typo is
// a render error naming the expression the author wrote. A silently-ignored key would
// hand out secrets from the DEFAULT alphabet while the YAML says otherwise.
func TestGenerateSecret_PolicyErrorAtCallSite(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want string
	}{
		{"unknown key", `${ generate_secret({'charsett': 'hex'}) }`, "unknown key"},
		{"unknown charset", `${ generate_secret({'charset': 'hexx'}) }`, "unknown"},
		{"length below floor", `${ generate_secret({'length': 4}) }`, "8..1024"},
		{"length above ceiling", `${ generate_secret({'length': 2000}) }`, "8..1024"},
		{"charset and allowed_chars", `${ generate_secret({'charset': 'hex', 'allowed_chars': 'ab'}) }`, "mutually exclusive"},
		{"single-character alphabet", `${ generate_secret({'allowed_chars': 'aaa'}) }`, ">= 2 distinct"},
	}
	e := newEngine(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.EvalInterpolation(tc.expr, Vars{})
			if err == nil {
				t.Fatalf("rendered to %v, want a policy error", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestGenerateSecret_OnlyInRenderPass — the function exists only where its request can
// be resolved. In the other three passes it is rejected with a stated reason: a marker
// nobody resolves is inert data that looks like it did something.
func TestGenerateSecret_OnlyInRenderPass(t *testing.T) {
	build := []struct {
		name string
		ctor func(...Option) (*Engine, error)
	}{
		{"migration", NewMigration},
		{"flow-control", NewFlowControl},
		{"service-vars", NewServiceVars},
	}
	for _, tc := range build {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tc.ctor()
			if err != nil {
				t.Fatalf("build engine: %v", err)
			}
			_, err = e.EvalInterpolation(`${ generate_secret({}) }`, Vars{State: map[string]any{}})
			if err == nil {
				t.Fatal("generate_secret() evaluated, want it unavailable in this pass")
			}
			var unsup *ErrUnsupported
			if !errors.As(err, &unsup) {
				t.Fatalf("error is %T (%v), want *ErrUnsupported", err, err)
			}
		})
	}
}
