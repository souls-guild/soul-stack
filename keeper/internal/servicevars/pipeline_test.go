package servicevars

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeLayers materializes layer files in a tmp service-dir following the REAL
// layout (docs/service/manifest.md): key — path relative to `vars/`, value —
// YAML content. Files land under `<dir>/vars/<rel>` — the resolver must read
// from the vars/ subdirectory, not from the service root.
func writeLayers(t *testing.T, layers map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vars"), 0o755); err != nil {
		t.Fatalf("mkdir vars: %v", err)
	}
	for rel, body := range layers {
		full := filepath.Join(dir, "vars", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

func resolve(t *testing.T, dir string) map[string]any {
	t.Helper()
	got, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: dir})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return got
}

// TestResolve_ReadsFromVarsSubdir — the resolver must read layers from
// `<ServiceDir>/vars/`, NOT from the snapshot root. The same file name sits in
// both places with different content; the root copy is a trap.
func TestResolve_ReadsFromVarsSubdir(t *testing.T) {
	dir := writeLayers(t, map[string]string{"00-base.yaml": "key: from_vars\n"})
	if err := os.WriteFile(filepath.Join(dir, "00-base.yaml"), []byte("key: ROOT_TRAP\n"), 0o644); err != nil {
		t.Fatalf("write root trap: %v", err)
	}

	if got := resolve(t, dir)["key"]; got != "from_vars" {
		t.Fatalf("vars are not read from vars/: got=%#v (the root file must be ignored)", got)
	}
}

// TestResolve_LexicalOrderAcrossCases — the load-bearing guard of ADR-0082 §5.
// `_` is 0x5F, between 'Z' (0x5A) and 'a' (0x61), so a base file named
// `_default.yaml` sorts ahead of its siblings ONLY while every one of them is
// lower-case. `00-base.yaml` is first regardless of case. Renaming the base file
// proves nothing without neighbours in both cases, which is why they are here.
func TestResolve_LexicalOrderAcrossCases(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":  "key: base\nfrom_base: 1\n",
		"Base.yaml":     "key: upper\nfrom_upper: 1\n",
		"_default.yaml": "key: underscore\nfrom_underscore: 1\n",
		"base.yaml":     "key: lower\nfrom_lower: 1\n",
	})

	got := resolve(t, dir)

	// Sort order: 00-base.yaml < Base.yaml < _default.yaml < base.yaml.
	// The underscore file is NOT last, and NOT first — that is the whole point.
	if got["key"] != "lower" {
		t.Fatalf("last by lexical order must win: got=%#v want=%q", got["key"], "lower")
	}
	for _, k := range []string{"from_base", "from_upper", "from_underscore", "from_lower"} {
		if got[k] != uint64(1) {
			t.Errorf("every file in vars/ must be applied, %s missing: %#v", k, got)
		}
	}
}

// TestResolve_BaseFileIsFirstAmongUpperCaseNeighbours — the direct statement of
// what the rename buys: with an upper-case neighbour present, `00-base.yaml`
// still loses to it (it is the BASE, the weakest layer), whereas `_default.yaml`
// would have won over it and silently stopped being a base.
func TestResolve_BaseFileIsFirstAmongUpperCaseNeighbours(t *testing.T) {
	numbered := resolve(t, writeLayers(t, map[string]string{
		"00-base.yaml": "key: base\n",
		"Zebra.yaml":   "key: neighbour\n",
	}))
	if numbered["key"] != "neighbour" {
		t.Errorf("00-base.yaml must be the WEAKEST layer even against an upper-case neighbour: %#v", numbered["key"])
	}

	underscored := resolve(t, writeLayers(t, map[string]string{
		"_default.yaml": "key: base\n",
		"Zebra.yaml":    "key: neighbour\n",
	}))
	if underscored["key"] != "base" {
		t.Errorf("precondition changed: `_` no longer sorts after an upper-case name (%#v)", underscored["key"])
	}
}

func TestResolve_DeepMergeMaps(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":  "redis:\n  maxmemory: 100mb\n  appendonly: true\n",
		"10-tuned.yaml": "redis:\n  maxmemory: 200mb\n  bind: 0.0.0.0\n",
	})

	redis, ok := resolve(t, dir)["redis"].(map[string]any)
	if !ok {
		t.Fatalf("redis is not a map")
	}
	want := map[string]any{
		"maxmemory":  "200mb", // override
		"appendonly": true,    // preserved from the base
		"bind":       "0.0.0.0",
	}
	if !reflect.DeepEqual(redis, want) {
		t.Fatalf("deep-merge mismatch:\n got=%#v\nwant=%#v", redis, want)
	}
}

func TestResolve_ListsReplaceNotAppend(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":  "ports:\n  - 6379\n  - 16379\n",
		"10-tuned.yaml": "ports:\n  - 7000\n",
	})

	ports, ok := resolve(t, dir)["ports"].([]any)
	if !ok {
		t.Fatalf("ports is not a list")
	}
	if want := []any{uint64(7000)}; !reflect.DeepEqual(ports, want) {
		t.Fatalf("a list must be replaced, not appended:\n got=%#v\nwant=%#v", ports, want)
	}
}

func TestResolve_ScalarOverMapAndViceVersa(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":  "a:\n  nested: 1\nb: scalar\n",
		"10-tuned.yaml": "a: now_scalar\nb:\n  now: map\n",
	})

	got := resolve(t, dir)
	if got["a"] != "now_scalar" {
		t.Fatalf("a map must be replaced by a scalar: %#v", got["a"])
	}
	bm, ok := got["b"].(map[string]any)
	if !ok || bm["now"] != "map" {
		t.Fatalf("a scalar must be replaced by a map: %#v", got["b"])
	}
}

// TestResolve_YmlExtensionIsALayer — both YAML spellings load, so a service is
// not tripped up by the one it happens to use.
func TestResolve_YmlExtensionIsALayer(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "key: base\n",
		"10-extra.yml": "key: yml\nonly_yml: 1\n",
	})

	got := resolve(t, dir)
	if got["key"] != "yml" || got["only_yml"] != uint64(1) {
		t.Fatalf(".yml must be a layer too: %#v", got)
	}
}

// TestResolve_StackFileIsNotALayer — `_stack.yaml` is a program, not data: it is
// executed as the order and must never appear in the result as a `stack:` key.
// Folding it into the order it declares would be circular.
func TestResolve_StackFileIsNotALayer(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "key: base\n",
		"_stack.yaml":  "stack:\n  - file: 00-base.yaml\n",
	})

	got := resolve(t, dir)
	if _, leaked := got["stack"]; leaked {
		t.Fatalf("_stack.yaml was merged as a layer: %#v", got)
	}
	if got["key"] != "base" {
		t.Fatalf("got=%#v", got)
	}
}

// TestResolve_NonYAMLFilesIgnored — a neighbour without a YAML extension is
// selected out by extension, not by luck.
//
// The fixture is deliberately content YAML CANNOT parse. A README whose body
// happens to be a valid YAML comment would pass this test with the extension
// check deleted, which is the difference between a guard and a decoration.
func TestResolve_NonYAMLFilesIgnored(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "key: base\n",
		"README.md":    "key: : : broken\n  - and not a layer\n",
		"notes.txt":    "key: : : also broken\n  - nope\n",
	})

	if got := resolve(t, dir)["key"]; got != "base" {
		t.Fatalf("got=%#v", got)
	}
}

// TestResolve_StackFileMisspeltRefuses — `_stack.yml` is refused rather than
// treated as a layer. Left alone it fails twice and says nothing either time:
// the pipeline does not run, and its own manifest is merged in as data.
func TestResolve_StackFileMisspeltRefuses(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "key: base\n",
		"_stack.yml":   "stack:\n  - file: 00-base.yaml\n",
	})

	_, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: dir})
	if err == nil {
		t.Fatal("_stack.yml must be refused, not merged as a layer")
	}
	if !errors.Is(err, ErrStackStepInvalid) {
		t.Fatalf("want ErrStackStepInvalid, got %v", err)
	}
}

// TestResolve_SubdirectoriesNotWalked — a nested directory is reachable only
// through an explicit `_stack.yaml` step (ADR-0082 §3); the flat scan must not
// descend into it. An unreferenced one is silent until soul-lint gains
// `vars_dir_nested` (NIM-416).
func TestResolve_SubdirectoriesNotWalked(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":   "key: base\n",
		"os/debian.yaml": "key: nested\nfrom_nested: 1\n",
	})

	got := resolve(t, dir)
	if got["key"] != "base" {
		t.Fatalf("a subdirectory must not be walked: got=%#v", got["key"])
	}
	if _, leaked := got["from_nested"]; leaked {
		t.Fatalf("a nested file leaked into the result: %#v", got)
	}
}

// TestResolve_MissingVarsDirOK — a service with no vars of its own resolves to
// an empty map, not an error.
func TestResolve_MissingVarsDirOK(t *testing.T) {
	got, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: t.TempDir()})
	if err != nil {
		t.Fatalf("a missing vars/ must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty vars, got=%#v", got)
	}
}

// TestResolve_RetiredLayoutRefuses — the dangerous window of the migration. A
// repo whose scenarios were rewritten to `vars.` while the DIRECTORY rename was
// missed resolves zero vars, and every `default(vars.X, y)` / `has(vars.X)` then
// takes its fallback: runs go green on default conf_dirs and default mirror URLs,
// with no error anywhere. `essence/` present and `vars/` absent is that state
// exactly, and it is refused.
func TestResolve_RetiredLayoutRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "essence"), 0o755); err != nil {
		t.Fatalf("mkdir essence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "essence", "_default.yaml"), []byte("key: old\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: dir})
	if err == nil {
		t.Fatal("a snapshot on the retired layout must be refused, not resolved to nothing")
	}
	if !strings.Contains(err.Error(), "retired layout") {
		t.Fatalf("the error must name the cause, got %v", err)
	}

	// Once vars/ exists the retired directory is simply ignored — a repo may keep
	// it around during a migration without the resolver caring.
	if err := os.MkdirAll(filepath.Join(dir, "vars"), 0o755); err != nil {
		t.Fatalf("mkdir vars: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vars", "00-base.yaml"), []byte("key: new\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := resolve(t, dir)["key"]; got != "new" {
		t.Fatalf("got=%#v, want the vars/ value", got)
	}
}

// TestResolve_ExtensionMatchIsCaseInsensitive — a `.YAML` layer that silently did
// not load is the same failure as one that loaded and was wrong.
func TestResolve_ExtensionMatchIsCaseInsensitive(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":  "key: base\n",
		"10-extra.YAML": "key: upper\nfrom_upper: 1\n",
	})

	got := resolve(t, dir)
	if got["key"] != "upper" || got["from_upper"] != uint64(1) {
		t.Fatalf("an uppercase extension must load: %#v", got)
	}
}

func TestResolve_EmptyVarsDir(t *testing.T) {
	if got := resolve(t, writeLayers(t, map[string]string{})); len(got) != 0 {
		t.Fatalf("expected empty vars, got=%#v", got)
	}
}

// TestResolve_EmptyLayerFile — an empty file (yaml.Unmarshal → nil map) must not
// break the merge.
func TestResolve_EmptyLayerFile(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":  "",
		"10-tuned.yaml": "key: tuned\n",
	})

	if got := resolve(t, dir)["key"]; got != "tuned" {
		t.Fatalf("got=%#v", got)
	}
}

func TestResolve_InvalidYAMLErrors(t *testing.T) {
	dir := writeLayers(t, map[string]string{"00-base.yaml": "key: : : broken\n  - bad"})
	if _, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: dir}); err == nil {
		t.Fatal("expected a parse error for invalid YAML")
	}
}

// TestResolve_SymlinkOutOfTreeIsNotRead — securejoin resolves a layer path
// inside the service dir, so a symlink pointing outside is clamped to the root
// and the file behind it is not read.
func TestResolve_SymlinkOutOfTreeIsNotRead(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.yaml")
	if err := os.WriteFile(secret, []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := writeLayers(t, map[string]string{"00-base.yaml": "key: base\n"})
	if err := os.Symlink(secret, filepath.Join(dir, "vars", "10-link.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := resolve(t, dir)
	if _, leaked := got["leaked"]; leaked {
		t.Fatal("securejoin did not clamp the symlink: a file outside serviceDir was read")
	}
	if got["key"] != "base" {
		t.Fatalf("got=%#v", got)
	}
}
