package validate

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/plugin"
)

// A minimal, valid soul_module document: one module, one state, two params. Written as
// text rather than built through `sdk/schema` — a fixture the linter can only read the
// way an author's `dist/schema.json` reaches it.
const aclSchemaJSON = `{"kind":"soul_module","protocol_version":1,` +
	`"modules":[{"name":"acl","description":"Redis ACL users","states":{"present":{` +
	`"description":"the user exists","input":{` +
	`"host":{"type":"string","required":true},"port":{"type":"int"}}}}}]}`

const sshSchemaJSON = `{"kind":"ssh_provider","protocol_version":1,"provider_kind":"static_key"}`

// A document that parses but does not validate: a secret with no vault pattern.
const invalidSchemaJSON = `{"kind":"soul_module","protocol_version":1,` +
	`"modules":[{"name":"acl","states":{"present":{"input":{"pw":{"type":"string","secret":true}}}}}]}`

// writeSchemaFile drops a document at <dir>/<name>/schema.json and returns the file.
// The directory is named after a BINARY on purpose — the alias must come from the flag,
// never from the path.
func writeSchemaFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	slot := filepath.Join(dir, name)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(slot, plugin.SchemaFileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// writeStampedArtifact fakes what `soul-mod stamp` produces: arbitrary executable bytes
// followed by <payload><uint64 length><magic>.
//
// The magic is spelled out rather than imported: `soul-lint` reads artifacts through
// `shared/plugin` and has no business depending on the SDK. If the trailer format ever
// changes, this fixture stops being recognized and the test fails loudly — which is the
// correct signal, not a silent pass.
func writeStampedArtifact(t *testing.T, path, body string) string {
	t.Helper()
	const magic = "SOULSTACKSCHEMA1"
	out := []byte("\x7fELF not really an executable, but loaders ignore trailing bytes")
	out = append(out, body...)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(body)))
	out = append(out, n[:]...)
	out = append(out, magic...)
	if err := os.WriteFile(path, out, 0o700); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return path
}

// The indexing decision, pinned.
//
// The artifact carries no self-name, so the address a task writes has to be STATED. It
// is stated on the flag, not read off the path: the directory here is named after the
// binary (`soul-mod-community-redis`), and the very same bytes bound under two aliases
// have to produce two independent address spaces.
func TestLoadModuleSchemas_AddressComesFromTheFlagNotThePath(t *testing.T) {
	dir := t.TempDir()
	p := writeSchemaFile(t, dir, "soul-mod-community-redis", aclSchemaJSON)

	r, err := LoadModuleSchemas([]string{"redis=" + p, "redis-community=" + p})
	if err != nil {
		t.Fatalf("LoadModuleSchemas: %v", err)
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
	if _, ok := r.ResolveModule("soul-mod-community-redis", "acl"); ok {
		t.Error("the directory name resolved as an alias — the address is following the file again")
	}
	if _, ok := r.ResolveModule("community", "redis"); ok {
		t.Error("the old declared-namespace address resolved; the artifact declares no name")
	}
}

func TestLoadModuleSchemas_AcceptsStampedArtifact(t *testing.T) {
	art := writeStampedArtifact(t, filepath.Join(t.TempDir(), "soul-mod-redis"), aclSchemaJSON)

	r, err := LoadModuleSchemas([]string{"redis=" + art})
	if err != nil {
		t.Fatalf("LoadModuleSchemas: %v", err)
	}
	if _, ok := r.ResolveModule("redis", "acl"); !ok {
		t.Error("a stamped artifact did not resolve; the linter must read the trailer as well as the file")
	}
}

// `dist/` is what an author has in front of them after a build.
func TestLoadModuleSchemas_AcceptsDistDirectory(t *testing.T) {
	dir := t.TempDir()
	writeSchemaFile(t, dir, "dist", aclSchemaJSON)

	r, err := LoadModuleSchemas([]string{"redis=" + filepath.Join(dir, "dist")})
	if err != nil {
		t.Fatalf("LoadModuleSchemas: %v", err)
	}
	if _, ok := r.ResolveModule("redis", "acl"); !ok {
		t.Error("a dist/ directory holding schema.json did not resolve")
	}
}

// Fail closed. Every one of these used to have a "skip it and carry on" answer under the
// tree walk, which was right when the flag pointed at a tree that could hold unrelated
// files. It is wrong now: the author named this binding, so a document that cannot be
// read is a check they asked for and did not get.
func TestLoadModuleSchemas_UnreadableBindingsAreFatal(t *testing.T) {
	dir := t.TempDir()
	noTrailer := filepath.Join(dir, "soul-mod-unstamped")
	if err := os.WriteFile(noTrailer, []byte("\x7fELF never stamped"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	truncated := writeSchemaFile(t, dir, "truncated", `{"kind":"soul_module","protocol`)
	invalid := writeSchemaFile(t, dir, "invalid", invalidSchemaJSON)
	ssh := writeSchemaFile(t, dir, "ssh", sshSchemaJSON)

	for _, tc := range []struct{ name, binding string }{
		{"missing path", "redis=" + filepath.Join(dir, "nope", "schema.json")},
		{"directory without schema.json", "redis=" + dir},
		{"artifact with no trailer", "redis=" + noTrailer},
		{"truncated document", "redis=" + truncated},
		{"document that fails validation", "redis=" + invalid},
		{"wrong kind", "redis=" + ssh},
	} {
		if _, err := LoadModuleSchemas([]string{tc.binding}); err == nil {
			t.Errorf("%s was accepted; an unreadable schema must not look like a clean pass", tc.name)
		}
	}
}

// A reserved alias cannot be registered in a cluster, so binding one here would let a
// document of the author's choosing describe what `core.*` accepts.
func TestLoadModuleSchemas_ReservedAliasIsRefused(t *testing.T) {
	p := writeSchemaFile(t, t.TempDir(), "soul-mod-redis", aclSchemaJSON)
	for _, name := range plugin.ReservedNames() {
		_, err := LoadModuleSchemas([]string{name + "=" + p})
		if err == nil {
			t.Errorf("--modules %s=<doc> was accepted; the alias is reserved", name)
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("--modules %s=<doc> failed with %v, which does not say the alias is reserved", name, err)
		}
	}
}

func TestLoadModuleSchemas_MalformedBindingIsRefused(t *testing.T) {
	dir := t.TempDir()
	p := writeSchemaFile(t, dir, "soul-mod-redis", aclSchemaJSON)

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
		if _, err := LoadModuleSchemas([]string{tc.binding}); err == nil {
			t.Errorf("--modules %q (%s) was accepted", tc.binding, tc.name)
		}
	}
}

// Two documents under one alias is one address space with two answers; resolving it by
// flag order would make the lint depend on argument order.
func TestLoadModuleSchemas_DuplicateAliasIsRefused(t *testing.T) {
	dir := t.TempDir()
	a := writeSchemaFile(t, dir, "one", aclSchemaJSON)
	b := writeSchemaFile(t, dir, "two", aclSchemaJSON)
	if _, err := LoadModuleSchemas([]string{"redis=" + a, "redis=" + b}); err == nil {
		t.Error("the same alias was bound twice without complaint")
	}
}

// No flag at all is legal and means "no catalog here" — the case
// plugin_params_unchecked describes.
func TestLoadModuleSchemas_NoBindingsMeansNoResolver(t *testing.T) {
	r, err := LoadModuleSchemas(nil)
	if err != nil {
		t.Fatalf("no bindings should not be an error: %v", err)
	}
	if r != nil {
		t.Errorf("resolver = %v, want nil so the caller reports plugin_params_unchecked", r)
	}
}

// writeScenario lays out `<root>/svc/scenario/add-user/main.yml` with one task
// addressing `redis.acl.present` and the given params block.
func writeScenario(t *testing.T, root, params string) string {
	t.Helper()
	dir := filepath.Join(root, "svc", "scenario", "add-user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "name: add-user\ntasks:\n  - name: t\n    module: redis.acl.present\n    params:\n" + params
	p := filepath.Join(dir, "main.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return p
}

// End to end through the subcommand: the point of the flag is that an undeclared param
// on a plugin module fails HERE, with a line, instead of on the host as
// module.unknown_param (ADR-0076(t)).
func TestRun_BoundSchemaCatchesUndeclaredParam(t *testing.T) {
	dir := t.TempDir()
	doc := writeSchemaFile(t, dir, "soul-mod-redis", aclSchemaJSON)
	scenario := writeScenario(t, dir, "      host: h1\n      prot: 6379\n")

	var out, errOut bytes.Buffer
	code := Run(Options{Path: scenario, Kind: KindScenario, Modules: []string{"redis=" + doc}}, &out, &errOut)
	if code != ExitHasErrors {
		t.Fatalf("exit %d, want %d; stdout:\n%s", code, ExitHasErrors, out.String())
	}
	if !strings.Contains(out.String(), "unknown_param") {
		t.Errorf("a param the schema does not declare was accepted:\n%s", out.String())
	}
}

// The same run without the binding must not print a clean bill of health.
func TestRun_WithoutBindingReportsUnchecked(t *testing.T) {
	dir := t.TempDir()
	scenario := writeScenario(t, dir, "      host: h1\n")

	var out, errOut bytes.Buffer
	code := Run(Options{Path: scenario, Kind: KindScenario}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit %d, want %d — an unbound plugin is not the author's error; stdout:\n%s", code, ExitOK, out.String())
	}
	if !strings.Contains(out.String(), "plugin_params_unchecked") {
		t.Errorf("no notice that nothing was checked:\n%s", out.String())
	}
}

// A binding that cannot be read stops the run at exit 2. It must never degrade into the
// unchecked hint and an `OK:` line — that is "checked and clean" written over "could not
// look", the failure this whole flag exists to remove.
func TestRun_UnreadableBindingIsFatalNotUnchecked(t *testing.T) {
	dir := t.TempDir()
	scenario := writeScenario(t, dir, "      host: h1\n")

	var out, errOut bytes.Buffer
	code := Run(Options{
		Path: scenario, Kind: KindScenario,
		Modules: []string{"redis=" + filepath.Join(dir, "gone", "schema.json")},
	}, &out, &errOut)
	if code != ExitIOFatal {
		t.Fatalf("exit %d, want %d (I/O fatal)", code, ExitIOFatal)
	}
	if strings.Contains(out.String(), "OK:") {
		t.Errorf("printed a clean result for a run whose schema never loaded:\n%s", out.String())
	}
}

// validate-manifest reads a schema document now — both carriers, told apart by content.
func TestRun_ValidateManifestReadsBothCarriers(t *testing.T) {
	dir := t.TempDir()
	file := writeSchemaFile(t, dir, "published", aclSchemaJSON)
	art := writeStampedArtifact(t, filepath.Join(dir, "soul-mod-redis"), aclSchemaJSON)

	for _, p := range []string{file, art} {
		var out, errOut bytes.Buffer
		if code := Run(Options{Path: p, Kind: KindManifest}, &out, &errOut); code != ExitOK {
			t.Errorf("validate-manifest %s: exit %d\nstdout:\n%s\nstderr:\n%s", p, code, out.String(), errOut.String())
		}
	}
}

// An artifact that was never stamped has no disclosure to approve, and the finding says
// so rather than "this file is not JSON".
func TestRun_ValidateManifestFailsClosedOnUnstampedArtifact(t *testing.T) {
	p := filepath.Join(t.TempDir(), "soul-mod-redis")
	if err := os.WriteFile(p, []byte("\x7fELF never stamped"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := Run(Options{Path: p, Kind: KindManifest}, &out, &errOut); code != ExitHasErrors {
		t.Fatalf("exit %d, want %d\nstdout:\n%s", code, ExitHasErrors, out.String())
	}
	if !strings.Contains(out.String(), "schema_trailer_missing") {
		t.Errorf("an unstamped artifact was not reported as unstamped:\n%s", out.String())
	}
}
