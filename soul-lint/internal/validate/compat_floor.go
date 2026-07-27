package validate

// Cross-check of the declared compat window against the floor inferred from what
// the definition actually uses (ADR-0076(k), `compat_floor_too_low`). The linter
// is where this belongs: an author who declared `min: 0.1.0` and then reached for
// a feature that only exists from 0.3.0 has written a window that lies, and the
// place to say so is before the definition ships — deliberately NOT at render,
// where the run would have succeeded on the keeper actually doing the rendering.
//
// Cross-file by nature, like the vars-collision check: a scenario's features are
// weighed against the window its `service.yml` declares, a destiny's against its
// own `destiny.yml`. The diagnostic is reported on the file carrying the
// DECLARATION, since that is the file the author has to edit.

import (
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// destinyCompatFloorDiags weighs a destiny's declared window against the floor of
// its manifest grammar plus its sibling `tasks/main.yml`. A missing or
// unparseable task file is skipped — its own errors are reported by
// validate-scenario / the runtime, and a floor derived from half a definition
// would be worse than none.
func destinyCompatFloorDiags(manifestPath string, m *config.DestinyManifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	return floorDiags(manifestPath, m.Compat.KeeperWindow(), destinyKeeperFeatures(manifestPath, m))
}

// destinyKeeperFeatures collects what one destiny uses: its manifest grammar plus
// the sibling `tasks/main.yml`.
func destinyKeeperFeatures(manifestPath string, m *config.DestinyManifest) []config.KeeperFeature {
	var tasks []config.Task
	tasksPath := filepath.Join(filepath.Dir(manifestPath), "tasks", "main.yml")
	if data, err := os.ReadFile(tasksPath); err == nil {
		if parsed, _, terr := config.LoadDestinyTasksFromBytes(tasksPath, data, config.ValidateOptions{}); terr == nil {
			tasks = parsed
		}
	}
	return config.KeeperFeaturesOfDestiny(m, tasks)
}

// serviceCompatFloorDiags weighs a `service.yml` against its own manifest
// grammar. The scenarios it owns are linted one at a time and carry their own
// check (see scenarioCompatFloorDiags).
func serviceCompatFloorDiags(manifestPath string, m *config.ServiceManifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	return floorDiags(manifestPath, m.Compat.KeeperWindow(), config.KeeperFeaturesOfService(m))
}

// scenarioCompatFloorDiags weighs a scenario's task list against the window
// declared by the service that owns it — a scenario has no `compat:` block of its
// own, it renders under the service's declaration. The diagnostic is reported on
// `service.yml`, the file that has to change. Skipped when the service manifest
// is missing or unparseable (a scenario is linted standalone often enough).
func scenarioCompatFloorDiags(scenarioPath string, scn *config.ScenarioManifest) []diag.Diagnostic {
	if scn == nil {
		return nil
	}
	servicePath := filepath.Join(scenarioServiceRoot(scenarioPath), "service.yml")
	src, err := os.ReadFile(servicePath)
	if err != nil {
		return nil
	}
	svc, _, _, serr := config.LoadServiceManifestFromBytes(servicePath, src, config.ValidateOptions{})
	if serr != nil || svc == nil {
		return nil
	}
	return floorDiags(servicePath, svc.Compat.KeeperWindow(), config.KeeperFeaturesOfTasks(scn.Tasks))
}

// floorDiags — the shared tail: infer the floor over the used features, compare
// it with the declared window, and pin the diagnostic to the file that declared
// it.
func floorDiags(declaredIn string, window *config.VersionWindow, used []config.KeeperFeature) []diag.Diagnostic {
	d := config.CompatFloorDiagnostic(window, config.InferKeeperFloor(used))
	if d == nil {
		return nil
	}
	d.File = declaredIn
	return []diag.Diagnostic{*d}
}
