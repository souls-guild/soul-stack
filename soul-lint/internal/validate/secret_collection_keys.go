package validate

// The offline half of the collection-key uniqueness check ([ADR-0083] §1, NIM-704).
//
// The rule itself lives in shared/config.ScanDuplicateSecretKeys, one
// implementation for the linter and the keeper alike. What this file adds is the
// piece a scenario file cannot supply: the SERVICE's `state_schema`, which says
// which property is a declared secret and which sibling addresses it. It is read
// out of `../../service.yml`, the same route scenarioVaultNamespaceDiags and
// scenarioCompatFloorDiags take, and it fails the same way — silently, when the
// file is absent. A scenario is linted standalone often enough that refusing it
// would be worse than checking less; the apply-time half has the schema on the run
// context and does not skip.
//
// Includes are expanded first, the same route scenarioVaultNamespaceDiags takes, so
// the rule reads the same task list the run will. It buys this rule less than it
// buys the fence: a capture whose `value:` is a literal list also fails the module
// param check, and the expander drops a branch whose file has errors, so such a
// task never survives expansion. Kept because the alternative is a rule that reads a
// different task list than the run does, for no gain. The expansion's own
// diagnostics are dropped: stageDiagnostics already reports them, at its own
// carefully chosen levels, and a second copy would print twice.

import (
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

func scenarioSecretKeyDiags(scenarioPath string, scn *config.ScenarioManifest) []diag.Diagnostic {
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
	// Resolve the state_schema's `$type` references before reading it: a declared
	// secret may live in a named type ([NIM-740]), and an unresolved reference node
	// carries no properties — the rule would find no collection secrets and go quiet.
	// The catalog's own diagnostics are dropped for the same reason the include
	// expansion's are: the service.yml lint reports them, at its own file, and a
	// second copy here would name a scenario that did nothing wrong.
	stateSchemaTypeRefDiags(servicePath, svc)

	tasks := scn.Tasks
	if serviceDir := scenarioServiceLevelDir(scenarioPath); serviceDir != "" {
		expanded, _ := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(root, filepath.Dir(scenarioPath), serviceDir))
		if expanded != nil {
			tasks = expanded
		}
	}
	return config.ScanDuplicateSecretKeys(scenarioPath, svc.StateSchema, tasks)
}
