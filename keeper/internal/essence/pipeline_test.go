package essence

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeLayers materializes layers in a tmp service-dir following the REAL
// layout (docs/service/manifest.md): key — relative path inside `essence/`
// (`_default.yaml`, `os/<family>.yaml`, `coven/<label>.yaml`), value — YAML
// content. Files are placed under `<dir>/essence/<rel>` — the resolver must
// read from the essence/ subdirectory, not the service root (BUG 2).
func writeLayers(t *testing.T, layers map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range layers {
		full := filepath.Join(dir, "essence", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// TestResolve_ReadsFromEssenceSubdir — regression for BUG 2: the resolver
// must read layers from `<ServiceDir>/essence/`, NOT from the snapshot root.
// We place _default.yaml in BOTH locations with different content; we expect
// the value from essence/ (the root file is ignored). Before the fix the
// resolver read root and this test would fail.
func TestResolve_ReadsFromEssenceSubdir(t *testing.T) {
	dir := t.TempDir()
	// Service root is NOT an essence source: this is a trap value.
	if err := os.WriteFile(filepath.Join(dir, "_default.yaml"), []byte("key: ROOT_TRAP\n"), 0o644); err != nil {
		t.Fatalf("write root trap: %v", err)
	}
	// Real layout is essence/_default.yaml.
	if err := os.MkdirAll(filepath.Join(dir, "essence"), 0o755); err != nil {
		t.Fatalf("mkdir essence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "essence", "_default.yaml"), []byte("key: from_essence\n"), 0o644); err != nil {
		t.Fatalf("write essence/_default.yaml: %v", err)
	}

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["key"] != "from_essence" {
		t.Fatalf("essence is not read from essence/: got=%#v (root file must be ignored)", got["key"])
	}
}

func TestResolve_FourLayerPrecedence(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "key: default\nonly_default: 1\n",
		"os/debian.yaml": "key: os\nonly_os: 2\n",
		"coven/web.yaml": "key: coven\nonly_coven: 3\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{
		ServiceDir:      dir,
		OSFamily:        "debian",
		Covens:          []string{"web"},
		IncarnationSpec: map[string]any{"key": "spec", "only_spec": 4},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := map[string]any{
		"key":          "spec", // the strongest layer
		"only_default": uint64(1),
		"only_os":      uint64(2),
		"only_coven":   uint64(3),
		"only_spec":    4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("precedence mismatch:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestResolve_DeepMergeMaps(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml": "redis:\n  maxmemory: 100mb\n  appendonly: true\n",
		"os/rhel.yaml":  "redis:\n  maxmemory: 200mb\n  bind: 0.0.0.0\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "rhel"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	redis, ok := got["redis"].(map[string]any)
	if !ok {
		t.Fatalf("redis is not a map: %#v", got["redis"])
	}
	want := map[string]any{
		"maxmemory":  "200mb", // override
		"appendonly": true,    // preserved from default
		"bind":       "0.0.0.0",
	}
	if !reflect.DeepEqual(redis, want) {
		t.Fatalf("deep-merge mismatch:\n got=%#v\nwant=%#v", redis, want)
	}
}

func TestResolve_ListsReplaceNotAppend(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "ports:\n  - 6379\n  - 16379\n",
		"os/debian.yaml": "ports:\n  - 7000\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "debian"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ports, ok := got["ports"].([]any)
	if !ok {
		t.Fatalf("ports is not a list: %#v", got["ports"])
	}
	want := []any{uint64(7000)}
	if !reflect.DeepEqual(ports, want) {
		t.Fatalf("list must be replaced, not appended:\n got=%#v\nwant=%#v", ports, want)
	}
}

func TestResolve_ScalarOverMapAndViceVersa(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "a:\n  nested: 1\nb: scalar\n",
		"os/debian.yaml": "a: now_scalar\nb:\n  now: map\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "debian"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["a"] != "now_scalar" {
		t.Fatalf("map must be replaced by scalar: %#v", got["a"])
	}
	bm, ok := got["b"].(map[string]any)
	if !ok || bm["now"] != "map" {
		t.Fatalf("scalar must be replaced by map: %#v", got["b"])
	}
}

func TestResolve_MultipleCovensDeterministicOrder(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "tier: base\n",
		"coven/aaa.yaml": "tier: aaa\nfrom_aaa: 1\n",
		"coven/zzz.yaml": "tier: zzz\nfrom_zzz: 1\n",
	})

	r := NewResolver(nil)
	// Pass covens in reverse order — the result must depend on name sorting,
	// not on input order.
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, Covens: []string{"zzz", "aaa"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// zzz sorts after aaa → zzz wins.
	if got["tier"] != "zzz" {
		t.Fatalf("expected zzz (last by sort order), got=%#v", got["tier"])
	}
	if got["from_aaa"] != uint64(1) || got["from_zzz"] != uint64(1) {
		t.Fatalf("both coven layers must apply: %#v", got)
	}
}

func TestResolve_MissingLayersOK(t *testing.T) {
	// No layer files at all, only spec.
	dir := writeLayers(t, map[string]string{})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{
		ServiceDir:      dir,
		OSFamily:        "debian",           // os/debian.yaml absent
		Covens:          []string{"absent"}, // coven/absent.yaml absent
		IncarnationSpec: map[string]any{"only": "spec"},
	})
	if err != nil {
		t.Fatalf("missing layers must not be an error: %v", err)
	}
	want := map[string]any{"only": "spec"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

func TestResolve_EmptyEverything(t *testing.T) {
	dir := writeLayers(t, map[string]string{})
	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty essence, got=%#v", got)
	}
}

func TestResolve_EmptyDefaultFile(t *testing.T) {
	// Empty _default.yaml (yaml.Unmarshal → nil map) must not break the merge.
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "",
		"os/debian.yaml": "key: os\n",
	})
	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "debian"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["key"] != "os" {
		t.Fatalf("got=%#v", got)
	}
}

func TestResolve_InvalidYAMLErrors(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml": "key: : : broken\n  - bad",
	})
	r := NewResolver(nil)
	if _, err := r.Resolve(ResolveInput{ServiceDir: dir}); err == nil {
		t.Fatal("expected parse error for invalid YAML")
	}
}

func TestResolve_EmptyOSFamilySkipsOSLayer(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "key: default\n",
		"os/debian.yaml": "key: os\n",
	})
	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: ""})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["key"] != "default" {
		t.Fatalf("empty OSFamily must skip os layer, got=%#v", got["key"])
	}
}

func TestResolve_SecureJoinClampsTraversal(t *testing.T) {
	// A file outside serviceDir must not be read: securejoin clamps `..` to
	// the root, so the resulting path doesn't exist → the layer is skipped
	// without error.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := writeLayers(t, map[string]string{"_default.yaml": "key: default\n"})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{
		ServiceDir: dir,
		Covens:     []string{"../../" + filepath.Base(outside) + "/secret"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, leaked := got["leaked"]; leaked {
		t.Fatal("securejoin did not clamp traversal: file outside serviceDir was read")
	}
	if got["key"] != "default" {
		t.Fatalf("got=%#v", got)
	}
}

func TestResolve_DoesNotMutateIncarnationSpecNested(t *testing.T) {
	// The operator's spec is stored separately in the DB (architecture.md):
	// Resolve must not mutate the passed-in IncarnationSpec.
	dir := writeLayers(t, map[string]string{
		"_default.yaml": "redis:\n  maxmemory: 100mb\n  bind: 127.0.0.1\n",
	})
	spec := map[string]any{"redis": map[string]any{"maxmemory": "500mb"}}

	r := NewResolver(nil)
	if _, err := r.Resolve(ResolveInput{ServiceDir: dir, IncarnationSpec: spec}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	nested := spec["redis"].(map[string]any)
	if _, polluted := nested["bind"]; polluted {
		t.Fatalf("Resolve leaked base key into IncarnationSpec: %#v", spec)
	}
	if len(nested) != 1 || nested["maxmemory"] != "500mb" {
		t.Fatalf("IncarnationSpec mutated: %#v", spec)
	}
}
