package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// writeDestinyFixture writes a minimal valid destiny `destiny-<name>/`
// (destiny.yml + tasks/main.yml) under base and returns its directory.
func writeDestinyFixture(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, "destiny-"+name)
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	man := "name: " + name + "\ndescription: fixture\n"
	if err := os.WriteFile(filepath.Join(dir, "destiny.yml"), []byte(man), 0o644); err != nil {
		t.Fatalf("write destiny.yml: %v", err)
	}
	tasks := "- name: noop\n  module: core.file.present\n  params:\n    path: /tmp/x\n    content: ok\n"
	if err := os.WriteFile(filepath.Join(dir, "tasks", "main.yml"), []byte(tasks), 0o644); err != nil {
		t.Fatalf("write tasks/main.yml: %v", err)
	}
	return dir
}

// TestFixtureDestinyResolver_MirrorsProd is the happy path: name is declared in
// destiny[], file:// URL is resolved relative to service-root, destiny is loaded.
func TestFixtureDestinyResolver_MirrorsProd(t *testing.T) {
	root := t.TempDir()
	serviceRoot := filepath.Join(root, "svc")
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatalf("mkdir svc: %v", err)
	}
	// destiny in neighbor subtree (cross-location): root/dst/destiny-pilot.
	writeDestinyFixture(t, filepath.Join(root, "dst"), "pilot")

	r := newFixtureDestinyResolver(serviceRoot, "file://../dst/destiny-{name}",
		[]config.DependencyRef{{Name: "pilot", Ref: "v1.0.0"}})

	got, err := r.Resolve(context.Background(), "pilot")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "pilot" {
		t.Fatalf("Name = %q, want pilot", got.Name)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(got.Tasks))
	}
}

// TestFixtureDestinyResolver_RejectsUndeclared rejects undeclared dependency
// in destiny[] (mirror of prod-error, ADR-007).
func TestFixtureDestinyResolver_RejectsUndeclared(t *testing.T) {
	r := newFixtureDestinyResolver(t.TempDir(), "file://destiny-{name}", nil)
	_, err := r.Resolve(context.Background(), "ghost")
	if err == nil {
		t.Fatal("want error on undeclared destiny")
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("want error about undeclared dependency, got: %v", err)
	}
}

// TestFixtureDestinyResolver_RejectsNonFileScheme rejects non-file:// scheme in L0
// (hermeticity: no git, no network).
func TestFixtureDestinyResolver_RejectsNonFileScheme(t *testing.T) {
	r := newFixtureDestinyResolver(t.TempDir(), "",
		[]config.DependencyRef{{Name: "x", Ref: "v1", Git: "git@github.com:acme/destiny-x.git"}})
	_, err := r.Resolve(context.Background(), "x")
	if err == nil {
		t.Fatal("want error on non-file:// scheme")
	}
	if !strings.Contains(err.Error(), "hermetic") {
		t.Fatalf("want error about L0 hermeticity, got: %v", err)
	}
}

// TestFixtureDestinyResolver_NameCannotEscapeRoot is CRITICAL for security:
// {name} with `../` escape does not break out of destiny-root (securejoin clamps it).
// destiny-name comes from service.yml::destiny[]; even if it contains `../`,
// result stays inside declared destiny-root, not go to secret/.
func TestFixtureDestinyResolver_NameCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	serviceRoot := filepath.Join(root, "svc", "dst")
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// "Secret" outside destiny-root: root/svc/destiny-secret. If {name} could
	// escape via ../, leaf `destiny-../destiny-secret` would reach it.
	writeDestinyFixture(t, filepath.Join(root, "svc"), "secret")

	// destiny-root = serviceRoot (template without `../`); name with escape.
	r := newFixtureDestinyResolver(serviceRoot, "file://destiny-{name}",
		[]config.DependencyRef{{Name: "../destiny-secret", Ref: "v1"}})
	_, err := r.Resolve(context.Background(), "../destiny-secret")
	if err == nil {
		t.Fatal("want error: {name} with ../ must not escape destiny-root")
	}
	// Clamp gives path INSIDE destiny-root (securejoin collapses ../), which
	// doesn't exist → not-found, NOT successful escape to ../destiny-secret.
	if strings.Contains(err.Error(), filepath.Join(root, "svc", "destiny-secret")) {
		t.Fatalf("leak out of destiny-root: resolve reached external dir: %v", err)
	}
}

// TestFixtureDestinyResolver_RejectsPlaceholderNotInLeaf requires {name} to live in
// the last path segment (otherwise no safe clamp-boundary for name).
func TestFixtureDestinyResolver_RejectsPlaceholderNotInLeaf(t *testing.T) {
	r := newFixtureDestinyResolver(t.TempDir(), "file://{name}/destiny",
		[]config.DependencyRef{{Name: "x", Ref: "v1"}})
	_, err := r.Resolve(context.Background(), "x")
	if err == nil {
		t.Fatal("want error: {name} not in last segment")
	}
	if !strings.Contains(err.Error(), "last segment") {
		t.Fatalf("want error about last segment, got: %v", err)
	}
}

// TestFixtureDestinyResolver_LoadsVarsYml is a trial mirror of prod: neighbor vars.yml
// (destiny-locals) is loaded via config.LoadDestinyVars into ResolvedDestiny.Vars,
// like DestinyLoader.parseVars. Missing file → nil (optional).
func TestFixtureDestinyResolver_LoadsVarsYml(t *testing.T) {
	root := t.TempDir()
	dir := writeDestinyFixture(t, root, "withvars")
	varsYml := "redis_unit_name: redis-server\nacl_path: \"/acl/${ input.user }.acl\"\n"
	if err := os.WriteFile(filepath.Join(dir, "vars.yml"), []byte(varsYml), 0o644); err != nil {
		t.Fatalf("write vars.yml: %v", err)
	}

	r := newFixtureDestinyResolver(root, "file://destiny-{name}",
		[]config.DependencyRef{{Name: "withvars", Ref: "v1.0.0"}})
	got, err := r.Resolve(context.Background(), "withvars")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Vars["redis_unit_name"] != "redis-server" {
		t.Errorf("Vars[redis_unit_name] = %v, want redis-server", got.Vars["redis_unit_name"])
	}
	if got.Vars["acl_path"] != "/acl/${ input.user }.acl" {
		t.Errorf("Vars[acl_path] = %v — RAW, without CEL resolution (resolution in render)", got.Vars["acl_path"])
	}
}

// TestFixtureDestinyResolver_NoVarsYml: missing vars.yml is not error: Vars=nil.
func TestFixtureDestinyResolver_NoVarsYml(t *testing.T) {
	root := t.TempDir()
	writeDestinyFixture(t, root, "novars")
	r := newFixtureDestinyResolver(root, "file://destiny-{name}",
		[]config.DependencyRef{{Name: "novars", Ref: "v1.0.0"}})
	got, err := r.Resolve(context.Background(), "novars")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Vars != nil {
		t.Errorf("Vars = %v, want nil for destiny without vars.yml", got.Vars)
	}
}
