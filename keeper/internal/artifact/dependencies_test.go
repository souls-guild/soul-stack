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

// TestListDependencies_ReadsManifest — happy-path: оба блока распарсены,
// порядок сохранён, per-entry git override проброшен.
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
		t.Errorf("Modules[0].Git должен быть пуст (override запрещён для modules): %q", deps.Modules[0].Git)
	}
}

// TestListDependencies_NoBlocks — сервис без destiny/modules: оба слайса
// non-nil (пустые), не nil (JSON `[]`, не null).
func TestListDependencies_NoBlocks(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, validManifestV2)

	deps, err := ListDependencies(root, discardLogger())
	if err != nil {
		t.Fatalf("ListDependencies: %v", err)
	}
	if deps.Destiny == nil || len(deps.Destiny) != 0 {
		t.Errorf("Destiny = %+v, ожидался непустой non-nil слайс длины 0", deps.Destiny)
	}
	if deps.Modules == nil || len(deps.Modules) != 0 {
		t.Errorf("Modules = %+v, ожидался непустой non-nil слайс длины 0", deps.Modules)
	}
}

// TestListDependencies_MissingManifest — `service.yml` нет → error.
func TestListDependencies_MissingManifest(t *testing.T) {
	root := t.TempDir()
	if _, err := ListDependencies(root, discardLogger()); err == nil {
		t.Fatalf("ожидалась ошибка при отсутствии service.yml")
	}
}

// TestListDependencies_BrokenManifest — невалидный манифест → error
// (битый service.yml в репо; caller отдаёт 502).
func TestListDependencies_BrokenManifest(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, "name: 123\nstate_schema_version: oops\n")
	if _, err := ListDependencies(root, discardLogger()); err == nil {
		t.Fatalf("ожидалась ошибка при невалидном service.yml")
	}
}
