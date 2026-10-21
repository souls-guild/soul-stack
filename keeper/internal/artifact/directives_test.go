package artifact

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
)

// TestLoadDirectiveCatalog_ReadsTheWholeVarsDirectory — the catalog is wherever
// its author put it. Reading one hard-coded file answered a narrower question
// than the service declares: a repo that keeps `redis_directives` in a later
// `NN-*.yaml`, or names its base file something else, would have been seen as
// having no catalog at all — an empty editor in the UI while the run validates
// fine against the same data.
func TestLoadDirectiveCatalog_ReadsTheWholeVarsDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "vars", "00-base.yaml"), "conf_dir: /etc/redis\n")
	writeFile(t, filepath.Join(root, "vars", "50-catalog.yaml"),
		"redis_directives:\n  \"7.0\":\n    - maxmemory\n    - appendonly\n")

	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}
	if got := cat["7.0"]; len(got) != 2 {
		t.Fatalf("a catalog in a non-base file must be found: %#v", cat)
	}
}

// TestLoadDirectiveCatalog_LaterFileWins — the assembly is lexical, so a later
// file overrides the series an earlier one declared, exactly as it would for any
// other var.
func TestLoadDirectiveCatalog_LaterFileWins(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "vars", "00-base.yaml"),
		"redis_directives:\n  \"7.0\":\n    - old\n")
	writeFile(t, filepath.Join(root, "vars", "90-override.yaml"),
		"redis_directives:\n  \"7.0\":\n    - new\n")

	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}
	if got := cat["7.0"]; len(got) != 1 || got[0] != "new" {
		t.Fatalf("the later file must win: %#v", cat)
	}
}

// TestLoadDirectiveCatalog_EvaluatesOnlyItsKey — point 7 of the ADR-0082
// amendment of 2026-10-15: the catalog reads the service view host-free and
// evaluates `redis_directives` alone. A broken var elsewhere in the service does
// not empty the editor, and the evaluation counter proves it was never touched;
// a catalog that is itself a definition is evaluated like any other.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: read the catalog from
// `view.Materialize()` in directiveCatalogOf. The broken var then fails it.
func TestLoadDirectiveCatalog_EvaluatesOnlyItsKey(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "vars", "00-base.yaml"),
		"series: \"7.0\"\nbroken: \"${ vars.nope }\"\n")
	writeFile(t, filepath.Join(root, "vars", "50-catalog.yaml"),
		"redis_directives:\n  \"7.0\":\n    - maxmemory\n    - \"${ 'appendonly' }\"\n")

	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}
	if want := []string{"appendonly", "maxmemory"}; !equalStr(cat["7.0"], want) {
		t.Fatalf("7.0 = %v, want %v", cat["7.0"], want)
	}

	view, err := servicevars.NewResolver(nil).LexicalView(root)
	if err != nil {
		t.Fatalf("LexicalView: %v", err)
	}
	if _, err := directiveCatalogOf(view); err != nil {
		t.Fatalf("directiveCatalogOf: %v", err)
	}
	if n := view.Evaluations(); n != 1 {
		t.Fatalf("Evaluations = %d, want 1: the catalog evaluated a var it does not read", n)
	}
}

// TestLoadDirectiveCatalog_NoCatalog — guard #3 (the loader half): a service
// without redis_directives (and without a vars file) → an empty non-nil
// map + nil error.
func TestLoadDirectiveCatalog_NoCatalog(t *testing.T) {
	// (a) vars/00-base.yaml exists, but without redis_directives.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "vars", "00-base.yaml"), "conf_dir: /etc/redis\nmemory_reserve_percent: 75\n")
	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}
	if cat == nil {
		t.Fatalf("catalog is nil, want non-nil empty map")
	}
	if len(cat) != 0 {
		t.Errorf("catalog %v, want empty", keysOf(cat))
	}

	// (b) the vars file is absent entirely → also an empty catalog, no error.
	empty := t.TempDir()
	cat2, err := LoadDirectiveCatalog(empty, "8.2.2")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(no file): %v", err)
	}
	if cat2 == nil || len(cat2) != 0 {
		t.Errorf("catalog without a vars file = %v, want empty non-nil", cat2)
	}
}

// TestLoadDirectiveCatalog_SortsNames — names within a series are sorted
// (defensive: the generator might have returned an unsorted list).
func TestLoadDirectiveCatalog_SortsNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "vars", "00-base.yaml"),
		"redis_directives:\n  \"8.2\":\n    - zebra\n    - alpha\n    - maxmemory\n")
	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}
	want := []string{"alpha", "maxmemory", "zebra"}
	if !equalStr(cat["8.2"], want) {
		t.Errorf("8.2 = %v, want sorted %v", cat["8.2"], want)
	}
}

// TestFilterDirectivesByVersion — a unit test of the narrowing rule (emulates
// the assert regex).
func TestFilterDirectivesByVersion(t *testing.T) {
	cat := map[string][]string{
		"6.2": {"a"},
		"7.4": {"b"},
		"8.2": {"c"},
	}
	// version="" → the whole catalog (the same map).
	if got := FilterDirectivesByVersion(cat, ""); len(got) != 3 {
		t.Errorf("version='' -> %d series, want 3", len(got))
	}
	// A distro pin with an epoch prefix matches the series.
	if got := FilterDirectivesByVersion(cat, "5:7.4.1-1~deb12u7"); len(got) != 1 || got["7.4"] == nil {
		t.Errorf("epoch pin 5:7.4.1 -> %v, want {7.4}", keysOf(got))
	}
	// 7.4 does not catch a 7.04-like series (the series boundary is the
	// trailing dot).
	if got := FilterDirectivesByVersion(cat, "7.42.0"); len(got) != 0 {
		t.Errorf("7.42.0 -> %v, want empty (7.4 does not match 7.42)", keysOf(got))
	}
	// An unknown version → an empty non-nil map (assert-skip semantics).
	got := FilterDirectivesByVersion(cat, "9.9.9")
	if got == nil || len(got) != 0 {
		t.Errorf("9.9.9 -> %v, want empty non-nil", got)
	}
}

// --- helpers ---

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
