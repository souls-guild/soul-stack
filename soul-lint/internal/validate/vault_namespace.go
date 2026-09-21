package validate

// The offline half of the own-namespace fence ([ADR-0083] §7).
//
// The rule itself lives in shared/config.ScanOwnNamespaceVault, one
// implementation for the linter and the keeper alike. What this file adds is the
// piece a scenario file cannot supply: the SERVICE NAME. A scenario never states
// which service owns it, and since NIM-726 neither does `service.yml` — a service
// is named once, at registration, and offline there is no registry to ask. So the
// name is stated by whoever runs the linter, via `--service-name`.
//
// Without it the fence CANNOT run, and that is reported rather than skipped. It
// used to be read out of `../../service.yml` and, when that file was missing or
// nameless, `ScanOwnNamespaceVault` opened with `if service == "" { return nil }`
// — the rule switched itself off and printed nothing, so a scenario writing into
// its own Vault namespace linted `OK` and was refused at render. The warning
// below is the whole point of moving the name to a flag: a check that did not run
// says so.
//
// It is a warning and not an error on purpose. A scenario linted standalone,
// without the operator knowing which service will own it, is a normal thing to do
// and refusing it would be worse than checking less; what is not acceptable is
// doing it silently.
//
// Includes are expanded first, so a path smuggled into a sibling file is caught
// here rather than only at render. The expansion's own diagnostics are dropped:
// stageDiagnostics already reports them, at its own carefully chosen levels, and
// a second copy would print twice.

import (
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// FenceUncheckedCode — the fence was not run because no service name was given.
const FenceUncheckedCode = "own_namespace_fence_unchecked"

func scenarioVaultNamespaceDiags(scenarioPath, serviceName string, scn *config.ScenarioManifest) []diag.Diagnostic {
	if scn == nil {
		return nil
	}
	if serviceName == "" {
		return []diag.Diagnostic{{
			Level:   diag.LevelWarning,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    FenceUncheckedCode,
			Message: "own-namespace fence not checked: no service name given",
			Hint: "pass --service-name <name> (the name the service is registered under) to check that no task " +
				"writes a Vault path under the service's own derived prefix ([ADR-0083] §7)",
		}}
	}

	root := scenarioServiceRoot(scenarioPath)
	tasks := scn.Tasks
	if serviceDir := scenarioServiceLevelDir(scenarioPath); serviceDir != "" {
		expanded, _ := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(root, filepath.Dir(scenarioPath), serviceDir))
		if expanded != nil {
			tasks = expanded
		}
	}
	return config.ScanOwnNamespaceVault(scenarioPath, serviceName, scn, tasks)
}
