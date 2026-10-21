package validate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/servicevars"
)

// varsDirName — the service-vars directory, relative to the service root.
const varsDirName = "vars"

// retiredVarsDirName — the pre-ADR-0082 directory. A service still carrying it
// was migrated halfway, and the failure that follows is the worst-shaped one
// available: every `default(vars.X, y)` in the repo silently takes its fallback
// and the service deploys with a plausible set of wrong values.
const retiredVarsDirName = "essence"

// serviceVarsDiags checks the `vars/` directory next to a `service.yml`, offline.
//
// Four classes, all of which otherwise surface as a service that resolved to
// something other than what its author wrote — and resolved without complaint:
//
//   - `stack_step_invalid` — `_stack.yaml` cannot be read as written. Runs the
//     SAME parser the keeper runs (servicevars.ParseStack), so the offline
//     answer and the runtime answer cannot disagree.
//   - `vars_dir_nested` — a layer file in a subdirectory of `vars/` that no
//     `_stack.yaml` step can name. The file is simply never read.
//   - `vars_retired_layout` — the retired directory is still present.
//   - `incarnation_name_legacy_root` — the retired CEL root in a `_stack.yaml`
//     selector or in a definition of any layer file under `vars/`.
//
// Plus the var checks of the service view (var_views.go), its stub evaluation
// against serviceName.
//
// Any I/O error other than "not there" is left alone: the linter reports on what
// it can read, and an unreadable tree is the caller's problem to notice.
func serviceVarsDiags(manifestPath, serviceName string) []diag.Diagnostic {
	root := filepath.Dir(manifestPath)
	varsDir := filepath.Join(root, varsDirName)

	var out []diag.Diagnostic

	if info, err := os.Stat(filepath.Join(root, retiredVarsDirName)); err == nil && info.IsDir() {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    filepath.Join(root, retiredVarsDirName),
			Code:    "vars_retired_layout",
			Message: fmt.Sprintf("%s/ is the retired layer directory; a service's parameters live in %s/ (ADR-0082)", retiredVarsDirName, varsDirName),
			Hint:    fmt.Sprintf("git mv %s/_default.yaml %s/00-base.yaml, then rewrite `essence.X` as `vars.X`", retiredVarsDirName, varsDirName),
		})
	}

	entries, err := os.ReadDir(varsDir)
	if err != nil {
		// No vars/ at all is legitimate — a service may declare no defaults.
		return out
	}

	out = append(out, nestedVarsDiags(varsDir, entries)...)
	stackDiags := stackFileDiags(varsDir, entries)
	out = append(out, stackDiags...)
	out = append(out, layerLegacyRootDiags(varsDir)...)
	out = append(out, serviceVarViewDiags(root, serviceName, !diag.HasErrors(stackDiags))...)
	return out
}

// nestedVarsDiags reports layer files parked where the resolver will never look.
//
// The lexical scan reads `vars/*.yaml` and does not descend; only a `_stack.yaml`
// `file:` step reaches a nested file, at any depth. A `vars/coven/prod.yaml` no step
// can name is therefore not a layer that failed to apply — it is a file nothing
// reads, and nothing says so. This is the exact shape the retired `essence/coven/`
// layout had, so it is the mistake an author migrating a service is most likely to
// make.
func nestedVarsDiags(varsDir string, entries []os.DirEntry) []diag.Diagnostic {
	reachable := stackReach(varsDir)
	var out []diag.Diagnostic
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var dead string
		_ = filepath.WalkDir(filepath.Join(varsDir, e.Name()), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isYAMLName(d.Name()) {
				return nil
			}
			rel, rerr := filepath.Rel(varsDir, p)
			if rerr != nil || reachable(filepath.ToSlash(rel)) {
				return nil
			}
			dead = p
			return fs.SkipAll
		})
		if dead == "" {
			continue
		}
		rel, _ := filepath.Rel(varsDir, dead)
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelWarning,
			Phase:   diag.PhaseSemanticValidate,
			File:    dead,
			Code:    "vars_dir_nested",
			Message: fmt.Sprintf("%s/%s is never read: the lexical scan reads only files directly inside %s/, and no %s step names it", varsDirName, filepath.ToSlash(rel), varsDirName, config.StackFileName),
			Hint:    fmt.Sprintf("move it up into %s/ with a name that sorts where you want it, or name it from a %s step", varsDirName, config.StackFileName),
		})
		// One diagnostic per subdirectory: an author who parked ten files under
		// coven/ has one mistake, not ten.
	}
	return out
}

// stackReach reports whether some `_stack.yaml` step can include a file, by its
// path relative to `vars/` ([servicevars.StepFileMatcher]). Without a stack that
// parses, nothing is reachable but the top level.
func stackReach(varsDir string) func(rel string) bool {
	data, err := os.ReadFile(filepath.Join(varsDir, config.StackFileName))
	if err != nil {
		return func(string) bool { return false }
	}
	stack, err := servicevars.ParseStack(data)
	if err != nil {
		return func(string) bool { return false }
	}
	matchers := make([]func(string) bool, 0, len(stack.Stack))
	for _, step := range stack.Stack {
		matchers = append(matchers, servicevars.StepFileMatcher(step.File))
	}
	return func(rel string) bool {
		return slices.ContainsFunc(matchers, func(match func(string) bool) bool { return match(rel) })
	}
}

// stackFileDiags validates `_stack.yaml` through the parser keeper runs, and
// catches the `.yml` spelling that would otherwise sit unread.
func stackFileDiags(varsDir string, entries []os.DirEntry) []diag.Diagnostic {
	var out []diag.Diagnostic

	for _, e := range entries {
		if e.IsDir() || e.Name() != config.StackFileMisspelt {
			continue
		}
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    filepath.Join(varsDir, e.Name()),
			Code:    "stack_step_invalid",
			Message: fmt.Sprintf("%s is not read; the pipeline file is %s", config.StackFileMisspelt, config.StackFileName),
			Hint:    "rename it — a mis-spelled pipeline file leaves the directory resolving lexically, with no error",
		})
	}

	stackPath := filepath.Join(varsDir, config.StackFileName)
	data, err := os.ReadFile(stackPath)
	if err != nil {
		return out
	}
	stack, err := servicevars.ParseStack(data)
	if err != nil {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    stackPath,
			Code:    "stack_step_invalid",
			Message: strings.TrimPrefix(err.Error(), "service vars: "),
			Hint:    "docs/service/manifest.md → Service vars; the decode is strict, so an unknown key is a typo, not an extension",
		})
		return out
	}
	return append(out, stackLegacyRootDiags(stackPath, data, stack)...)
}

// stackLegacyRootDiags reports the retired CEL root `incarnation.name` inside
// `_stack.yaml` ([ADR-0085], NIM-730).
//
// This file is the THIRD CEL environment carrying that root, and the one easiest
// to forget: it is not a scenario, so the scenario walk never sees it, and its
// env is the resolver's own (`cel.NewServiceVars`, which declares `incarnation`
// and `vars` and nothing else). A step written against the old spelling resolves
// today through the window's alias and becomes a `no such key` when the alias
// goes — before the render, so the whole run fails to start rather than one task
// failing, and with nothing having warned.
//
// Only the three selectors of a step are walked here (`inline:` is refused at
// parse). The layer files' definitions are the same environment and are walked by
// [layerLegacyRootDiags].
func stackLegacyRootDiags(stackPath string, data []byte, stack *config.ServiceVarsStack) []diag.Diagnostic {
	eng := computeScopeEngine()
	if eng == nil || stack == nil {
		return nil
	}
	c := &legacyRootChecker{
		eng:  eng,
		path: stackPath,
		position: func(yamlPath string) (int, int, bool) {
			return config.PositionIn(data, yamlPath)
		},
	}
	for i, step := range stack.Stack {
		where := fmt.Sprintf("$.stack[%d]", i)
		// `when:` is an expression key — the whole string is CEL, no `${ }`.
		c.expression(where+".when", step.When)
		c.interpolation(where+".foreach", step.Foreach)
		c.interpolation(where+".file", step.File)
	}
	return c.out
}

// layerLegacyRootDiags reports the retired CEL root `incarnation.name` in the
// definitions of the layer files ([ADR-0085]). A definition is evaluated since the
// ADR-0082 amendment of 2026-10-15, in the same environment as the selectors, so
// `owner: "${ incarnation.name }"` resolves through the window's alias today and
// fails the run when the alias goes.
//
// Every `*.yaml`/`*.yml` under `vars/` is walked, subdirectories included: the
// lexical scan reads only the top level, but a `_stack.yaml` `file:` can name a
// nested file, and which ones it names is per-incarnation. `_stack.yaml` itself is
// not a layer and has its own walk; `_stack.yml` is refused by the parser and is
// never read.
func layerLegacyRootDiags(varsDir string) []diag.Diagnostic {
	eng := computeScopeEngine()
	if eng == nil {
		return nil
	}
	var out []diag.Diagnostic
	_ = filepath.WalkDir(varsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isYAMLName(d.Name()) {
			return nil
		}
		if filepath.Dir(p) == varsDir && (d.Name() == config.StackFileName || d.Name() == config.StackFileMisspelt) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		defs, err := config.LoadDestinyVarsFromBytes(p, data)
		if err != nil || len(defs) == 0 {
			return nil
		}
		c := &legacyRootChecker{
			eng:  eng,
			path: p,
			position: func(yamlPath string) (int, int, bool) {
				return config.PositionIn(data, yamlPath)
			},
		}
		c.value(defs, "$")
		out = append(out, c.out...)
		return nil
	})
	return out
}

// isYAMLName — the extensions the resolver treats as a layer, matched the way it
// matches them (case-insensitively).
func isYAMLName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// serviceVarNames returns the top-level names a service's `vars/` declares,
// read from the service root.
//
// Deliberately a SUPERSET: it unions the top-level keys of every `*.yaml`
// directly inside `vars/` without evaluating `_stack.yaml`, so a name a
// conditional step might contribute still counts. For the shadowing warning
// that is the right side to err on — a `when:`-gated layer that shadows on
// Tuesdays is exactly the case an author will not find by reading.
func serviceVarNames(serviceRoot string) map[string]struct{} {
	entries, err := os.ReadDir(filepath.Join(serviceRoot, varsDirName))
	if err != nil {
		return nil
	}
	names := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() || e.Name() == config.StackFileName || !isYAMLName(e.Name()) {
			continue
		}
		layer, err := config.LoadDestinyVars(filepath.Join(serviceRoot, varsDirName, e.Name()))
		if err != nil {
			continue
		}
		for k := range layer {
			names[k] = struct{}{}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// serviceVarShadowDiags warns when a scenario-level or task-level `vars:` takes
// over the name of one of the service's own vars.
//
// This is the one cost of merging the two namespaces (ADR-0082). Before, a
// service's parameters were `essence.X` and a local was `vars.X`: two spellings,
// no way to collide. Now they are one ladder, and a local silently wins. The
// shadow is deterministic and sometimes deliberate — deriving a value from the
// layer below by writing `conf_dir: "${ vars.conf_dir }/conf.d"` is a supported
// idiom — so this is a WARNING and not an error, modelled on `vars_collision`.
//
// What it catches is the accident: an author names a local `version`, a service
// var of the same name exists three directories away, and every later reference
// to `vars.version` in that scenario silently means the local. Nothing else in
// the system will say so.
func serviceVarShadowDiags(scenarioPath string, scn *config.ScenarioManifest) []diag.Diagnostic {
	if scn == nil {
		return nil
	}
	svcVars := serviceVarNames(scenarioServiceRoot(scenarioPath))
	if len(svcVars) == 0 {
		return nil
	}

	// Deduplicate by name: the same shadow repeated across ten tasks is one
	// decision the author made, and ten copies of it would bury the rest.
	seen := make(map[string]struct{})
	var out []diag.Diagnostic
	report := func(name, where string) {
		if _, dup := seen[name]; dup {
			return
		}
		if _, isSvc := svcVars[name]; !isSvc {
			return
		}
		seen[name] = struct{}{}
		out = append(out, diag.Diagnostic{
			Level:    diag.LevelWarning,
			Phase:    diag.PhaseSemanticValidate,
			File:     scenarioPath,
			Code:     "vars_shadows_service_var",
			Message:  fmt.Sprintf("vars.%s (%s) shadows a service var of the same name; every later vars.%s here means the local one", name, where, name),
			Hint:     fmt.Sprintf("rename the local, or keep it if the shadow is intentional — `${ vars.%s }` inside the local's own value still reads the service layer", name),
			YAMLPath: "$." + name,
		})
	}

	for name := range scn.Vars {
		report(name, "scenario-level")
	}
	for i := range scn.Tasks {
		for name := range scn.Tasks[i].Vars {
			report(name, "task-level")
		}
	}
	return out
}
