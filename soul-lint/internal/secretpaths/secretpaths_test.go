package secretpaths

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// run writes src to a temp service.yml and runs the subcommand over it.
func run(t *testing.T, src, serviceName string) (stdout, stderr string, code int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.yml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	var out, errOut bytes.Buffer
	code = Run(Options{Path: path, ServiceName: serviceName}, &out, &errOut)
	return out.String(), errOut.String(), code
}

// wbRedisShape — the two collections wb-service-redis actually declares. Kept in the
// test rather than read from that repository: the acceptance run against the real file
// is a separate, manual step, and a unit test that reads a neighbouring checkout would
// pass or fail on whether the checkout is there.
const wbRedisShape = `
state_schema_version: 2
state_schema:
  namespace: { type: string }
  redis_users:
    type: array
    items:
      type: object
      properties:
        name:  { type: string }
        perms: { type: string }
        password:
          type: secret
          key: name
          label: "Redis user password"
  system_acl_users:
    type: array
    items:
      type: object
      properties:
        name: { type: string }
        password:
          type: secret
          key: name
`

// TestRun_CollectionShape — the acceptance case. Both collections appear, in sorted
// declaration order, each ending in the sibling property that addresses one element.
func TestRun_CollectionShape(t *testing.T) {
	stdout, stderr, code := run(t, wbRedisShape, "redis")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; stderr = %q", code, stderr)
	}
	want := "secret/redis/<incarnation>/redis_users/<name>#password        ← state_schema.redis_users[].password\n" +
		"secret/redis/<incarnation>/system_acl_users/<name>#password   ← state_schema.system_acl_users[].password\n"
	if stdout != want {
		t.Errorf("stdout mismatch:\n got: %q\nwant: %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestRun_ScalarShape — a scalar has no key segment, and its value sits under the
// constant field name rather than under a property name it does not have.
func TestRun_ScalarShape(t *testing.T) {
	src := `
state_schema_version: 1
state_schema:
  admin_password: { type: secret }
`
	stdout, stderr, code := run(t, src, "redis")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; stderr = %q", code, stderr)
	}
	want := "secret/redis/<incarnation>/admin_password#" + config.ScalarSecretVaultField +
		"   ← state_schema.admin_password\n"
	if stdout != want {
		t.Errorf("stdout mismatch:\n got: %q\nwant: %q", stdout, want)
	}
}

// TestRun_BothShapesAlign — scalar and collection in one service: both are printed,
// and the arrow column is aligned on the longest path.
func TestRun_BothShapesAlign(t *testing.T) {
	src := `
state_schema_version: 1
state_schema:
  admin_password: { type: secret }
  redis_users:
    type: array
    items:
      type: object
      properties:
        name: { type: string }
        password: { type: secret, key: name }
`
	stdout, _, code := run(t, src, "redis")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK", code)
	}
	want := "secret/redis/<incarnation>/admin_password#value          ← state_schema.admin_password\n" +
		"secret/redis/<incarnation>/redis_users/<name>#password   ← state_schema.redis_users[].password\n"
	if stdout != want {
		t.Errorf("stdout mismatch:\n got: %q\nwant: %q", stdout, want)
	}
}

// TestRun_KeyPlaceholderIsTheKeyName — `key:` names the sibling property, and the
// placeholder is that NAME. A run that printed a value there would be inventing state
// data the linter cannot have.
func TestRun_KeyPlaceholderIsTheKeyName(t *testing.T) {
	src := `
state_schema_version: 1
state_schema:
  accounts:
    type: array
    items:
      type: object
      properties:
        login:    { type: string }
        password: { type: secret, key: login }
`
	stdout, _, code := run(t, src, "redis")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK", code)
	}
	if !strings.Contains(stdout, "/accounts/<login>#password") {
		t.Errorf("key placeholder is not the key's name: %q", stdout)
	}
}

// TestRun_NoDeclaredSecrets — the guard. A service that declares none prints an empty
// list and exits zero; it does not fail, and it does not print a header that would read
// as a finding.
func TestRun_NoDeclaredSecrets(t *testing.T) {
	for name, src := range map[string]string{
		"schema without secrets": `
state_schema_version: 1
state_schema:
  namespace: { type: string }
`,
		"empty properties": `
state_schema_version: 1
state_schema: {}
`,
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := run(t, src, "redis")
			if code != ExitOK {
				t.Errorf("code = %d, want ExitOK; stderr = %q", code, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

// TestRun_RefusedDeclarationIsNotSilent — a `type: secret` in a position the collector
// refuses derives no path. Printing the rest and exiting zero would tell the author
// their secret is fine; the command has to say the list is short.
func TestRun_RefusedDeclarationIsNotSilent(t *testing.T) {
	src := `
state_schema_version: 1
state_schema:
  nested:
    type: object
    properties:
      deeper:
        type: object
        properties:
          password: { type: secret }
`
	stdout, stderr, code := run(t, src, "redis")
	if code != ExitHasErrors {
		t.Fatalf("code = %d, want ExitHasErrors", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty — nothing derived", stdout)
	}
	if !strings.Contains(stderr, "secret_field_unsupported_location") {
		t.Errorf("stderr does not name the refusal code: %q", stderr)
	}
	if !strings.Contains(stderr, "INCOMPLETE") {
		t.Errorf("stderr does not say the list is short: %q", stderr)
	}
}

// TestRun_ReservedServiceNamespaceFailsClosed — the fence inside VaultPath is reached
// through this command too. A path under a reserved namespace is never printed, not
// even as a shape: an author who saw it would build on an address registration refuses.
func TestRun_ReservedServiceNamespaceFailsClosed(t *testing.T) {
	stdout, stderr, code := run(t, wbRedisShape, "keeper")
	if code != ExitHasErrors {
		t.Fatalf("code = %d, want ExitHasErrors", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "reserved Vault namespace") {
		t.Errorf("stderr does not explain the refusal: %q", stderr)
	}
}

// TestRun_UnsafeServiceNameFailsClosed — the name arrives on a flag and becomes a path
// segment, so it is checked by the same grammar every other segment is.
func TestRun_UnsafeServiceNameFailsClosed(t *testing.T) {
	stdout, stderr, code := run(t, wbRedisShape, "../keeper")
	if code != ExitHasErrors {
		t.Fatalf("code = %d, want ExitHasErrors", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "unsafe service segment") {
		t.Errorf("stderr does not name the unsafe segment: %q", stderr)
	}
}

// TestRun_MissingServiceName — refused before anything is read. A default would put a
// wrong word in the one segment the reader came here to check.
func TestRun_MissingServiceName(t *testing.T) {
	stdout, stderr, code := run(t, wbRedisShape, "")
	if code != ExitIOFatal {
		t.Fatalf("code = %d, want ExitIOFatal", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--service-name is required") {
		t.Errorf("stderr does not say what is missing: %q", stderr)
	}
}

// TestRun_NoStateSchemaIsRefusedNotAnsweredEmpty — the same wrong answer as an
// unparseable file, reached by a route that parses.
//
// Every YAML in a service repository that is not the manifest — types.yml,
// covenant.yml, a scenario's main.yml — parses cleanly and carries no `state_schema`.
// Pointed at one of those, an empty list would not mean "unreadable", it would mean
// "this service declares no secrets" — about a file whose declarations were never read.
// `state_schema` is a required field of the manifest, so its absence is never the
// no-secrets case.
func TestRun_NoStateSchemaIsRefusedNotAnsweredEmpty(t *testing.T) {
	for name, src := range map[string]string{
		"no state_schema key": `
state_schema_version: 1
description: a service that states no schema
`,
		"not the manifest at all": `
AclUser:
  properties:
    name: { type: string, required: true }
`,
		"state_schema is a scalar": `
state_schema_version: 1
state_schema: not-a-map
`,
		"state_schema is a list": `
state_schema_version: 1
state_schema: [a, b]
`,
		"state_schema is null": `
state_schema_version: 1
state_schema:
`,
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := run(t, src, "redis")
			if code != ExitHasErrors {
				t.Errorf("code = %d, want ExitHasErrors", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "no `state_schema:` map") {
				t.Errorf("stderr does not say why it refused: %q", stderr)
			}
		})
	}
}

// TestRun_UnparseableManifest — a file that is not YAML must not read as "declares no
// secrets". That is the one wrong answer this command can give.
func TestRun_UnparseableManifest(t *testing.T) {
	stdout, stderr, code := run(t, "state_schema: [unclosed\n", "redis")
	if code != ExitHasErrors {
		t.Fatalf("code = %d, want ExitHasErrors", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "does not parse") {
		t.Errorf("stderr does not say the file is unreadable: %q", stderr)
	}
}

// TestRun_MissingFile — an absent path is a usage/IO fatal, distinct from a file with
// errors in it.
func TestRun_MissingFile(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(Options{Path: filepath.Join(t.TempDir(), "absent.yml"), ServiceName: "redis"}, &out, &errOut)
	if code != ExitIOFatal {
		t.Fatalf("code = %d, want ExitIOFatal", code)
	}
}

// TestRun_Deterministic — the acceptance criterion: one input, a byte-identical output
// every time.
//
// The mutation this guards against is real code, not a comment: dropping the sorted
// traversal in CollectSecretFields (or adding an unsorted one here) makes the order
// follow Go's randomised map iteration. Eight declarations over thirty-two runs make
// that all but certain to show, where a single run would pass by luck.
func TestRun_Deterministic(t *testing.T) {
	var b strings.Builder
	b.WriteString("state_schema_version: 1\nstate_schema:\n")
	// Written in an order that is NOT the sorted one, so a traversal that echoed the
	// document order would also be caught.
	for _, n := range []string{"users_h", "users_c", "users_a", "users_g", "users_d", "users_b", "users_f", "users_e"} {
		fmt.Fprintf(&b, "  %s:\n    type: array\n    items:\n      type: object\n      properties:\n        name: { type: string }\n        password: { type: secret, key: name }\n", n)
	}
	src := b.String()

	first, stderr, code := run(t, src, "redis")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; stderr = %q", code, stderr)
	}
	if got := strings.Count(first, "\n"); got != 8 {
		t.Fatalf("printed %d lines, want 8:\n%s", got, first)
	}
	var wantSorted strings.Builder
	for _, n := range []string{"users_a", "users_b", "users_c", "users_d", "users_e", "users_f", "users_g", "users_h"} {
		fmt.Fprintf(&wantSorted, "secret/redis/<incarnation>/%s/<name>#password   ← state_schema.%s[].password\n", n, n)
	}
	if first != wantSorted.String() {
		t.Errorf("not in sorted declaration order:\n got:\n%s\nwant:\n%s", first, wantSorted.String())
	}
	for i := 0; i < 32; i++ {
		again, _, _ := run(t, src, "redis")
		if again != first {
			t.Fatalf("run %d differs from run 0:\n got:\n%s\nwant:\n%s", i+1, again, first)
		}
	}
}

// TestPathForm_AgreesWithTheDerivation — the guard that keeps the printed shape tied to
// [config.SecretField.VaultPath] instead of drifting into a second copy of the formula.
// Substituting the two placeholders for real values must reproduce, byte for byte, what
// the derivation returns for those values.
func TestPathForm_AgreesWithTheDerivation(t *testing.T) {
	for _, f := range []config.SecretField{
		{State: "redis_users", Property: "password", Key: "name"},
		{State: "admin_password"},
	} {
		t.Run(f.ID(), func(t *testing.T) {
			form, err := pathForm(f, "redis")
			if err != nil {
				t.Fatalf("pathForm: %v", err)
			}
			real, err := f.VaultPath("", "redis", "prod-1", "alice")
			if err != nil {
				t.Fatalf("VaultPath: %v", err)
			}
			want := real + "#" + f.VaultField()
			got := strings.NewReplacer("<incarnation>", "prod-1", "<"+f.Key+">", "alice").Replace(form)
			if got != want {
				t.Errorf("shape disagrees with the derivation:\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestRun_UnsafeKeyNameFailsClosed — the key's NAME becomes the last segment of the
// printed shape, and nothing upstream checks it: CollectSecretFields validates the key's
// runtime VALUE, which is a different string. A property named `a/b` would render
// `<a/b>` — punctuation that reads as a segment boundary the derivation does not make.
//
// The declaration is legal: `validate-service` passes it, because at runtime that name
// never reaches a path. Only this command puts it in one, so only this command refuses.
func TestRun_UnsafeKeyNameFailsClosed(t *testing.T) {
	for _, key := range []string{"a/b", "a#b", "a b"} {
		t.Run(key, func(t *testing.T) {
			src := fmt.Sprintf(`
state_schema_version: 1
state_schema:
  users:
    type: array
    items:
      type: object
      properties:
        %q: { type: string }
        password: { type: secret, key: %q }
`, key, key)
			stdout, stderr, code := run(t, src, "redis")
			if code != ExitHasErrors {
				t.Fatalf("code = %d, want ExitHasErrors; stdout = %q", code, stdout)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty — the shape must not be printed", stdout)
			}
			if !strings.Contains(stderr, "not a safe Vault path segment") {
				t.Errorf("stderr does not name the reason: %q", stderr)
			}
		})
	}
}

// TestRun_NonASCIIRefusedOnEveryAxis — the alignment pads by rune count, which is only
// display width while every printed segment stays ASCII. That is not an assumption
// about authors: it is closed on all four axes that can carry a name into a line, and
// this drives a wide glyph down each of them.
//
// A single happy-path run asserting the output is ASCII would prove nothing — it passes
// with every one of these checks removed. Each case here fails the moment its own axis
// stops refusing.
func TestRun_NonASCIIRefusedOnEveryAxis(t *testing.T) {
	const wide = "名前" // two full-width runes: 2 runes, 4 display columns
	collection := func(state, prop, key string) string {
		return fmt.Sprintf(`
state_schema_version: 1
state_schema:
  %q:
    type: array
    items:
      type: object
      properties:
        %q: { type: string }
        %q: { type: secret, key: %q }
`, state, key, prop, key)
	}
	for name, c := range map[string]struct{ src, service string }{
		// The mount and the `<incarnation>` segment take no input, so they are not axes.
		"service name": {collection("users", "password", "name"), wide},
		"state field":  {collection(wide, "password", "name"), "redis"},
		"property":     {collection("users", wide, "name"), "redis"},
		"key name":     {collection("users", "password", wide), "redis"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := run(t, c.src, c.service)
			if code == ExitOK {
				t.Fatalf("a wide glyph reached the output through %s: %q", name, stdout)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if stderr == "" {
				t.Error("refused without saying why")
			}
		})
	}
}

// TestPathForm_SentinelCollisionFailsClosed — the substitution finds its placeholders by
// segment value, so a service named exactly like a sentinel would have its own segment
// rewritten. Refusing beats printing a path with the service name replaced by
// `<incarnation>`.
func TestPathForm_SentinelCollisionFailsClosed(t *testing.T) {
	f := config.SecretField{State: "redis_users", Property: "password", Key: "name"}
	if _, err := pathForm(f, incarnationSentinel); err == nil {
		t.Fatal("a service name colliding with the incarnation sentinel was accepted")
	}
	if _, err := pathForm(config.SecretField{State: keySentinel}, "redis"); err == nil {
		t.Fatal("a state field colliding with the key sentinel was accepted")
	}
}

// runIn is `run` with a sibling types.yml written next to the manifest.
func runIn(t *testing.T, src, catalog, serviceName string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "service.yml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if catalog != "" {
		if err := os.WriteFile(filepath.Join(dir, "types.yml"), []byte(catalog), 0o600); err != nil {
			t.Fatalf("write catalog: %v", err)
		}
	}
	var out, errOut bytes.Buffer
	code = Run(Options{Path: path, ServiceName: serviceName}, &out, &errOut)
	return out.String(), errOut.String(), code
}

const typedUsersManifest = `state_schema_version: 1
state_schema:
  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
`

const aclUserCatalog = `types:
  AclUser:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string, required: true }
`

// A declaration whose element shape lives in types.yml is still printed. Since
// NIM-742 a collection may reach its shape through `$type`, and an unresolved
// reference carries no properties -- so without the resolve this command would print
// NOTHING and exit 0, which is the answer it refuses to give without having read the
// declarations.
func TestRun_DeclarationThroughTypeRefIsPrinted(t *testing.T) {
	stdout, stderr, code := runIn(t, typedUsersManifest, aclUserCatalog, "redis")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; stderr = %q", code, stderr)
	}
	want := "secret/redis/<incarnation>/redis_users/<name>#password   ← state_schema.redis_users[].password\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// A catalog that does not resolve REFUSES rather than printing a shorter list: an
// under-report here reads exactly like a service that declares fewer secrets.
func TestRun_UnresolvableTypeRefRefuses(t *testing.T) {
	for name, catalog := range map[string]string{
		"absent":  "",
		"unknown": "types: {}\n",
		"duplicate": "types:\n  AclUser: { type: object, properties: { a: { type: string } } }\n" +
			"  AclUser: { type: object, properties: { b: { type: string } } }\n",
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := runIn(t, typedUsersManifest, catalog, "redis")
			if code == ExitOK {
				t.Fatalf("code = ExitOK on a catalog that does not resolve; stdout = %q", stdout)
			}
			if stdout != "" {
				t.Errorf("printed a list anyway: %q", stdout)
			}
			if !strings.Contains(stderr, "$type") {
				t.Errorf("stderr does not name the unresolved reference: %q", stderr)
			}
		})
	}
}
