package validate

import (
	"os"
	"path/filepath"
	"testing"
)

// The repo's own examples/ carry both halves — plugin manifests and the
// definitions that address them — so they are the natural fixture for the flag.
const examplesModuleRoot = "../../../examples/module"

func TestLoadModuleManifests_IndexesByDeclaredAddress(t *testing.T) {
	r, err := LoadModuleManifests(examplesModuleRoot)
	if err != nil {
		t.Fatalf("LoadModuleManifests(%s): %v", examplesModuleRoot, err)
	}

	// community.redis lives in a directory named after its BINARY
	// (soul-mod-community-redis), not its address. Indexing by the directory
	// would miss it, and a task addresses the module, not the folder.
	m, ok := r.ResolveModule("community", "redis")
	if !ok {
		t.Fatal("community.redis did not resolve — indexed by directory name rather than by what the manifest declares?")
	}
	if _, hasConfig := m.Spec.States["config"]; !hasConfig {
		t.Errorf("community.redis resolved without its config state: %v", m.Spec.States)
	}
}

// A cloud_driver or ssh_provider manifest sits in the same tree and is not a
// module a task can address; picking one up would let a definition "resolve"
// against the wrong contract.
func TestLoadModuleManifests_SkipsNonSoulModules(t *testing.T) {
	r, err := LoadModuleManifests(examplesModuleRoot)
	if err != nil {
		t.Fatalf("LoadModuleManifests: %v", err)
	}
	for _, addr := range [][2]string{{"example", "example"}, {"static", "static"}} {
		if m, ok := r.ResolveModule(addr[0], addr[1]); ok && m.Kind != "soul_module" {
			t.Errorf("%s.%s resolved as kind %q", addr[0], addr[1], m.Kind)
		}
	}
}

// A directory that is not there is fatal, not "no manifests". The author asked
// for these checks by passing the flag; running without them and reporting
// success is the exact failure this feature removes.
func TestLoadModuleManifests_MissingDirIsFatal(t *testing.T) {
	if _, err := LoadModuleManifests(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing --modules dir was accepted, so the flag would silently check nothing")
	}
}

func TestLoadModuleManifests_FileInsteadOfDirIsFatal(t *testing.T) {
	f := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(f, []byte("kind: soul_module\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadModuleManifests(f); err == nil {
		t.Error("a file was accepted as --modules")
	}
}

// One unparseable manifest in the tree must not cost the author every other
// check — the module it would have described simply reports as unchecked.
func TestLoadModuleManifests_BadManifestIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	bad := filepath.Join(dir, "bad")
	for _, d := range []string{good, bad} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	goodSrc := "kind: soul_module\nprotocol_version: 1\nnamespace: acme\nname: widget\nspec:\n  states:\n    present:\n      input:\n        path:\n          type: string\n          required: true\n"
	if err := os.WriteFile(filepath.Join(good, "manifest.yaml"), []byte(goodSrc), 0o600); err != nil {
		t.Fatalf("write good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bad, "manifest.yaml"), []byte("kind: soul_module\nthis is: not: valid: yaml:\n"), 0o600); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	r, err := LoadModuleManifests(dir)
	if err != nil {
		t.Fatalf("one broken manifest made the whole tree fatal: %v", err)
	}
	if _, ok := r.ResolveModule("acme", "widget"); !ok {
		t.Error("the valid manifest beside a broken one was lost")
	}
}
