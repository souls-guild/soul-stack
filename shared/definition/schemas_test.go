package definition_test

// The `--modules <alias>=<path>` contract, shared by every offline tool that has
// the flag. Moved here from soul-lint (NIM-790) together with the loader: two
// tools with the flag and one loader between them is the whole point, and a test
// living in one of the two would have gone on describing that tool's copy.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/definition"
	"github.com/souls-guild/soul-stack/shared/definition/deftest"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

const sshSchemaJSON = `{"kind":"ssh_provider","protocol_version":1,"provider_kind":"static_key"}`

// A document that parses but does not validate: a secret with no vault pattern.
const invalidSchemaJSON = `{"kind":"soul_module","protocol_version":1,` +
	`"modules":[{"name":"acl","states":{"present":{"input":{"pw":{"type":"string","secret":true}}}}}]}`

// The indexing decision, pinned.
//
// The artifact carries no self-name, so the address a task writes has to be STATED. It
// is stated on the binding, not read off the path: the directory here is named after the
// binary (`community-redis`), and the very same bytes bound under two aliases
// have to produce two independent address spaces.
func TestLoadSchemas_AddressComesFromTheBindingNotThePath(t *testing.T) {
	dir := t.TempDir()
	p := deftest.WriteSchemaFile(t, dir, "community-redis", deftest.SchemaJSON)

	r, err := definition.LoadSchemas([]string{"redis=" + p, "redis-community=" + p})
	if err != nil {
		t.Fatalf("LoadSchemas: %v", err)
	}
	for _, alias := range []string{"redis", "redis-community"} {
		m, ok := r.ResolveModule(alias, "acl")
		if !ok {
			t.Fatalf("%s.acl did not resolve", alias)
		}
		if _, hasState := m.States["present"]; !hasState {
			t.Errorf("%s.acl resolved without its present state: %v", alias, m.States)
		}
	}
	// Nothing may be reachable under the directory's name, or the path would be
	// deciding an address again.
	if _, ok := r.ResolveModule("community-redis", "acl"); ok {
		t.Error("the directory name resolved as an alias — the address is following the file again")
	}
	if _, ok := r.ResolveModule("community", "redis"); ok {
		t.Error("the old declared-namespace address resolved; the artifact declares no name")
	}
}

func TestLoadSchemas_AcceptsStampedArtifact(t *testing.T) {
	art := deftest.WriteStampedArtifact(t, filepath.Join(t.TempDir(), "redis"), deftest.SchemaJSON)

	r, err := definition.LoadSchemas([]string{"redis=" + art})
	if err != nil {
		t.Fatalf("LoadSchemas: %v", err)
	}
	if _, ok := r.ResolveModule("redis", "acl"); !ok {
		t.Error("a stamped artifact did not resolve; the loader must read the trailer as well as the file")
	}
}

// `dist/` is what an author has in front of them after a build.
func TestLoadSchemas_AcceptsDistDirectory(t *testing.T) {
	dir := t.TempDir()
	deftest.WriteSchemaFile(t, dir, "dist", deftest.SchemaJSON)

	r, err := definition.LoadSchemas([]string{"redis=" + filepath.Join(dir, "dist")})
	if err != nil {
		t.Fatalf("LoadSchemas: %v", err)
	}
	if _, ok := r.ResolveModule("redis", "acl"); !ok {
		t.Error("a dist/ directory holding schema.json did not resolve")
	}
}

// Fail closed. Every one of these used to have a "skip it and carry on" answer under the
// tree walk, which was right when the flag pointed at a tree that could hold unrelated
// files. It is wrong now: the author named this binding, so a document that cannot be
// read is a check they asked for and did not get.
func TestLoadSchemas_UnreadableBindingsAreFatal(t *testing.T) {
	dir := t.TempDir()
	noTrailer := filepath.Join(dir, "unstamped")
	if err := os.WriteFile(noTrailer, []byte("\x7fELF never stamped"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	truncated := deftest.WriteSchemaFile(t, dir, "truncated", `{"kind":"soul_module","protocol`)
	invalid := deftest.WriteSchemaFile(t, dir, "invalid", invalidSchemaJSON)
	ssh := deftest.WriteSchemaFile(t, dir, "ssh", sshSchemaJSON)

	for _, tc := range []struct{ name, binding string }{
		{"missing path", "redis=" + filepath.Join(dir, "nope", "schema.json")},
		{"directory without schema.json", "redis=" + dir},
		{"artifact with no trailer", "redis=" + noTrailer},
		{"truncated document", "redis=" + truncated},
		{"document that fails validation", "redis=" + invalid},
		{"wrong kind", "redis=" + ssh},
	} {
		if _, err := definition.LoadSchemas([]string{tc.binding}); err == nil {
			t.Errorf("%s was accepted; an unreadable schema must not look like a clean pass", tc.name)
		}
	}
}

// A reserved alias cannot be registered in a cluster, so binding one here would let a
// document of the author's choosing describe what `core.*` accepts.
func TestLoadSchemas_ReservedAliasIsRefused(t *testing.T) {
	p := deftest.WriteSchemaFile(t, t.TempDir(), "redis", deftest.SchemaJSON)
	for _, name := range plugin.ReservedNames() {
		_, err := definition.LoadSchemas([]string{name + "=" + p})
		if err == nil {
			t.Errorf("--modules %s=<doc> was accepted; the alias is reserved", name)
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("--modules %s=<doc> failed with %v, which does not say the alias is reserved", name, err)
		}
	}
}

func TestLoadSchemas_MalformedBindingIsRefused(t *testing.T) {
	dir := t.TempDir()
	p := deftest.WriteSchemaFile(t, dir, "redis", deftest.SchemaJSON)

	for _, tc := range []struct{ name, binding string }{
		// The old form. It cannot work any more and must say so rather than index
		// nothing and report success.
		{"bare directory", dir},
		{"bare file", p},
		{"empty value", ""},
		{"empty alias", "=" + p},
		{"empty path", "redis="},
		{"dotted alias", "redis.acl=" + p},
		{"uppercase alias", "Redis=" + p},
		{"path traversal in alias", "../redis=" + p},
	} {
		if _, err := definition.LoadSchemas([]string{tc.binding}); err == nil {
			t.Errorf("--modules %q (%s) was accepted", tc.binding, tc.name)
		}
	}
}

// Two documents under one alias is one address space with two answers; resolving it by
// flag order would make the verdict depend on argument order.
func TestLoadSchemas_DuplicateAliasIsRefused(t *testing.T) {
	dir := t.TempDir()
	a := deftest.WriteSchemaFile(t, dir, "one", deftest.SchemaJSON)
	b := deftest.WriteSchemaFile(t, dir, "two", deftest.SchemaJSON)
	if _, err := definition.LoadSchemas([]string{"redis=" + a, "redis=" + b}); err == nil {
		t.Error("the same alias was bound twice without complaint")
	}
}

// No binding at all is legal and means "no catalog here" — the case
// plugin_params_unchecked describes. The nil must be a literal nil: a typed
// non-nil interface holding an empty map answers `ok=false` to everything, which
// looks the same from inside the check and different to every caller that tests
// the resolver for presence.
func TestLoadSchemas_NoBindingsMeansNoResolver(t *testing.T) {
	r, err := definition.LoadSchemas(nil)
	if err != nil {
		t.Fatalf("no bindings should not be an error: %v", err)
	}
	if r != nil {
		t.Errorf("resolver = %v, want nil so the caller reports plugin_params_unchecked", r)
	}
}
