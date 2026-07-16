package artifact

import "testing"

const manifestWithDeps = `name: redis-cluster
state_schema_version: 2
state_schema:
  type: object
  properties:
    master_host:
      type: string
destiny:
  - { name: redis, ref: v2.0.0 }
  - { name: redis-replication-config, ref: v1.0.0, git: "git@github.com:custom/destiny-repl.git" }
modules:
  - { name: wb.redis-failover, ref: v1.2.0 }
`

// TestListDependencies_ReadsManifest — happy-path: both blocks parsed,
// order preserved, per-entry git override passed through.
func TestListDependencies_ReadsManifest(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, manifestWithDeps)

	deps, err := ListDependencies(root, discardLogger())
	if err != nil {
		t.Fatalf("ListDependencies: %v", err)
	}
	if len(deps.Destiny) != 2 {
		t.Fatalf("Destiny len = %d, want 2; %+v", len(deps.Destiny), deps.Destiny)
	}
	if deps.Destiny[0].Name != "redis" || deps.Destiny[0].Ref != "v2.0.0" {
		t.Errorf("Destiny[0] = %+v", deps.Destiny[0])
	}
	if deps.Destiny[1].Name != "redis-replication-config" ||
		deps.Destiny[1].Ref != "v1.0.0" ||
		deps.Destiny[1].Git != "git@github.com:custom/destiny-repl.git" {
		t.Errorf("Destiny[1] = %+v", deps.Destiny[1])
	}
	if len(deps.Modules) != 1 || deps.Modules[0].Name != "wb.redis-failover" || deps.Modules[0].Ref != "v1.2.0" {
		t.Errorf("Modules = %+v", deps.Modules)
	}
	if deps.Modules[0].Git != "" {
		t.Errorf("Modules[0].Git should be empty (override is forbidden for modules): %q", deps.Modules[0].Git)
	}
}

// TestListDependencies_NoBlocks — a service without destiny/modules: both
// slices are non-nil (empty), not nil (JSON `[]`, not null).
func TestListDependencies_NoBlocks(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, validManifestV2)

	deps, err := ListDependencies(root, discardLogger())
	if err != nil {
		t.Fatalf("ListDependencies: %v", err)
	}
	if deps.Destiny == nil || len(deps.Destiny) != 0 {
		t.Errorf("Destiny = %+v, want non-nil empty slice", deps.Destiny)
	}
	if deps.Modules == nil || len(deps.Modules) != 0 {
		t.Errorf("Modules = %+v, want non-nil empty slice", deps.Modules)
	}
}

// TestListDependencies_MissingManifest — no `service.yml` → error.
func TestListDependencies_MissingManifest(t *testing.T) {
	root := t.TempDir()
	if _, err := ListDependencies(root, discardLogger()); err == nil {
		t.Fatalf("want error when service.yml is missing")
	}
}

// TestListDependencies_BrokenManifest — invalid manifest → error
// (broken service.yml in the repo; caller returns 502).
func TestListDependencies_BrokenManifest(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, "name: 123\nstate_schema_version: oops\n")
	if _, err := ListDependencies(root, discardLogger()); err == nil {
		t.Fatalf("want error for invalid service.yml")
	}
}
