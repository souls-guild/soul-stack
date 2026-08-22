package validate

// The offline half of the own-namespace fence ([ADR-0083] §7).
//
// The rule itself lives in shared/config.ScanOwnNamespaceVault, one
// implementation for the linter and the keeper alike. What this file adds is the
// piece a scenario file cannot supply: the SERVICE NAME. A scenario never states
// which service owns it, so the fence is read out of `../../service.yml` — the
// same route scenarioCompatFloorDiags takes for the compat window, and it fails
// the same way, silently, when the file is absent. A scenario linted standalone
// is linted often enough that refusing it would be worse than checking less; the
// keeper's own copy of the fence has the manifest in hand and does not skip.
//
// Includes are expanded first, so a path smuggled into a sibling file is caught
// here rather than only at render. The expansion's own diagnostics are dropped:
// stageDiagnostics already reports them, at its own carefully chosen levels, and
// a second copy would print twice.

import (
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

func scenarioVaultNamespaceDiags(scenarioPath string, scn *config.ScenarioManifest) []diag.Diagnostic {
	if scn == nil {
		return nil
	}
	root := scenarioServiceRoot(scenarioPath)
	servicePath := filepath.Join(root, "service.yml")
	src, err := os.ReadFile(servicePath)
	if err != nil {
		return nil
	}
	svc, _, _, serr := config.LoadServiceManifestFromBytes(servicePath, src, config.ValidateOptions{})
	if serr != nil || svc == nil {
		return nil
	}

	tasks := scn.Tasks
	if serviceDir := scenarioServiceLevelDir(scenarioPath); serviceDir != "" {
		expanded, _ := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(root, filepath.Dir(scenarioPath), serviceDir))
		if expanded != nil {
			tasks = expanded
		}
	}
	return config.ScanOwnNamespaceVault(scenarioPath, svc.Name, scn, tasks)
}
