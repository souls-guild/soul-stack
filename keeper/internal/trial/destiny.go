package trial

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/definition"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// destinyNamePlaceholder is a marker in default_destiny_source, replaced by
// destiny name during URL resolution. Mirrors the prod constant
// scenario.destinyNamePlaceholder (keeper/internal/scenario/destiny.go).
const destinyNamePlaceholder = "{name}"

// fileScheme is the only URL scheme supported in L0: hermetic test execution
// requires that cases read destiny only from local filesystem, no git/network.
const fileScheme = "file://"

// fixtureDestinyResolver is a hermetic implementation of [render.DestinyResolver] for
// L0, MIRRORING the prod resolve logic for apply:destiny (slice A, ADR-023):
//
//  1. name → lookup in service.yml::destiny[] → {name, ref, git?}; undeclared
//     dependency is rejected (apply:destiny only references declared
//     dependencies, ADR-007) — same error as in prod scenario.destinyResolver;
//  2. URL: per-entry git override (if set) wins, otherwise name
//     is substituted into default_destiny_source template (case.yml::fixtures).
//
// In L0, URL must be `file://` (hermetic): non-file-scheme is rejected
// with explicit error. `file://` is stripped from URL, path is resolved relative to
// case's service-root; securejoin clamps the path to stay within destiny-root, so
// {name} cannot escape via `../` outside the declared destiny directory.
//
// Heuristic directory traversal ([serviceRoot, parent, grandparent] +
// destiny-<name>/ convention) was removed: it did not mirror prod and gave wrong
// results on cross-location layouts (service in one subtree, standalone destiny in another).
type fixtureDestinyResolver struct {
	// serviceRoot is the absolute path to the case's service (or _trial-wrapper) directory;
	// the base against which relative file:// paths are resolved.
	serviceRoot string
	// template is the URL template for default_destiny_source from case.yml (empty is allowed:
	// then only dependencies with per-entry git override are resolved).
	template string
	// deps maps destiny[] dependencies from case's service.yml, keyed by name.
	deps map[string]config.DependencyRef
	// modules resolves plugin manifests for the destiny's own task file and every
	// body it includes (`--modules`, NIM-790). nil means no catalog was bound,
	// which is reported rather than passed over.
	modules config.ModuleManifestResolver
	// notes collects the non-error verdict of every destiny this resolver loads.
	//
	// A field rather than a return value because [render.DestinyResolver] returns a
	// destiny or an error and has no third channel — and widening that interface
	// for L0's benefit would change a contract the keeper implements too. The
	// caller reads it after render; a destiny resolved twice by two
	// `apply:destiny` steps is deduplicated by [notices].
	//
	// The constraint that buys: this resolver is no longer safe to share across
	// goroutines. Render is single-goroutine today ([render.Pipeline] resolves
	// destinies inline), so it holds; a resolver handed to a concurrent render
	// would need a mutex here or a sink per call.
	notes notices
}

// newFixtureDestinyResolver constructs a resolver from case's service-root,
// default_destiny_source template, and parsed service.yml::destiny[]. serviceRoot
// is converted to absolute path: securejoin on a relative base (`../...`)
// normalizes `..` and loses leading upward traversal, breaking os.ReadFile.
func newFixtureDestinyResolver(serviceRoot, template string, deps []config.DependencyRef, modules config.ModuleManifestResolver) *fixtureDestinyResolver {
	if abs, err := filepath.Abs(serviceRoot); err == nil {
		serviceRoot = abs
	}
	byName := make(map[string]config.DependencyRef, len(deps))
	for _, dep := range deps {
		byName[dep.Name] = dep
	}
	return &fixtureDestinyResolver{serviceRoot: serviceRoot, template: template, deps: byName, modules: modules}
}

// notices is the non-error verdict of every destiny loaded so far.
func (r *fixtureDestinyResolver) notices() []diag.Diagnostic { return r.notes.list() }

// Resolve loads destiny by name, mirroring prod resolve logic: name → destiny[] entry,
// URL via hybrid rule, read from local filesystem, parse
// destiny.yml + tasks/main.yml.
func (r *fixtureDestinyResolver) Resolve(_ context.Context, name string) (*render.ResolvedDestiny, error) {
	dir, err := r.locate(name)
	if err != nil {
		return nil, err
	}

	manPath := filepath.Join(dir, "destiny.yml")
	manData, err := os.ReadFile(manPath)
	if err != nil {
		return nil, fmt.Errorf("trial: read destiny.yml fixture %q: %w", name, err)
	}
	// The full path, not the bare file name. Its diagnostics are reported now
	// (NIM-790), so the name has to be one a reader can open — and it is also the
	// dedupe key: two destinies both called `destiny.yml` would collapse each
	// other's findings into one.
	manifest, _, mDiags, err := config.LoadDestinyManifestFromBytes(manPath, manData, config.ValidateOptions{ModuleManifests: r.modules})
	if err != nil {
		return nil, fmt.Errorf("trial: parse destiny.yml fixture %q: %w", name, err)
	}
	r.notes.keep(mDiags)
	if diag.HasErrors(mDiags) {
		return nil, fmt.Errorf("trial: destiny.yml fixture %q invalid: %s", name, formatDiags(mDiags))
	}

	tasksPath, err := securejoin.SecureJoin(dir, filepath.Join("tasks", "main.yml"))
	if err != nil {
		return nil, fmt.Errorf("trial: unsafe path tasks/main.yml fixture %q: %w", name, err)
	}
	tasksData, err := os.ReadFile(tasksPath)
	if err != nil {
		return nil, fmt.Errorf("trial: read tasks/main.yml fixture %q: %w", name, err)
	}
	// The parse and the within-destiny include expansion (tasks/<sub>.yml, same as
	// prod DestinyLoader.parseTasks, destiny/tasks.md §4) are one call through
	// [definition.LoadDestinyTasks] since NIM-790 — the same call `soul-lint
	// validate-destiny` makes, so the two tools cannot judge one task file
	// differently.
	//
	// DestinyTasks (NIM-749) is set inside it and matters most HERE: the L0 fold
	// keys on the module address, so a keeper-side step written into a destiny
	// predicts its result exactly as a routed one does and the case goes green on a
	// plan the run cannot execute.
	expanded, tDiags := definition.LoadDestinyTasks(tasksPath, tasksData, fixtureDestinyIncludeResolver(dir), r.modules)
	r.notes.keep(tDiags)
	if diag.HasErrors(tDiags) {
		return nil, fmt.Errorf("trial: tasks/main.yml fixture %q invalid: %s", name, formatDiags(tDiags))
	}

	// destiny-local vars.yml (docs/destiny/vars.md) mirrors prod implementation
	// (artifact.DestinyLoader.parseVars): same config.LoadDestinyVars, optional
	// (no file → nil). securejoin clamps path to stay within dir.
	varsPath, err := securejoin.SecureJoin(dir, "vars.yml")
	if err != nil {
		return nil, fmt.Errorf("trial: unsafe path vars.yml fixture %q: %w", name, err)
	}
	vars, err := config.LoadDestinyVars(varsPath)
	if err != nil {
		return nil, fmt.Errorf("trial: vars.yml fixture %q: %w", name, err)
	}

	// .tmpl destiny files are read from its fixture directory dir (single-level
	// resolution — destiny has no scenario-local layer). securejoin clamps path to stay
	// within dir, symmetric to prod snapshot.
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return readWithin(dir, rel) },
		"",
	)
	return &render.ResolvedDestiny{
		Name:      manifest.Name,
		Tasks:     expanded,
		Input:     manifest.Input,
		Validate:  manifest.Validate,
		Vars:      vars,
		Templates: templates,
	}, nil
}

// locate resolves destiny directory by name: name → destiny[] entry → file:// URL
// → absolute path under service-root. Mirrors prod resolveURL + Load.
func (r *fixtureDestinyResolver) locate(name string) (string, error) {
	dep, ok := r.deps[name]
	if !ok {
		return "", fmt.Errorf("trial: destiny %q not declared in service.yml::destiny[] — apply:destiny only references declared dependencies (ADR-007)", name)
	}

	root, leaf, err := r.resolvePath(name, dep.Git)
	if err != nil {
		return "", err
	}

	// Security boundary is destiny-root (trusted part of URL up to {name} segment),
	// not service-root: securejoin clamps the substituted name so {name}
	// cannot escape via `../` outside the declared destiny directory. destiny-root
	// itself may lie outside service-root (cross-location layout, template like
	// `file://../../destiny/...`) — this is trusted operator path, not destiny name.
	full, err := securejoin.SecureJoin(root, leaf)
	if err != nil {
		return "", fmt.Errorf("trial: unsafe path destiny %q (name %q under %q): %w", name, leaf, root, err)
	}
	info, serr := os.Stat(full)
	if serr != nil {
		return "", fmt.Errorf("trial: destiny %q not found at %q (resolved from default_destiny_source): %w", name, full, serr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("trial: destiny %q resolves to %q — not a directory", name, full)
	}
	return full, nil
}

// resolvePath extracts destiny-root (trusted absolute directory) and leaf
// (untrusted segment with substituted {name}) from the hybrid rule:
// per-entry git override wins, otherwise default_destiny_source template is used.
// Mirrors prod scenario.DestinySource.resolveURL, but additionally splits URL
// into trusted base and substitutable name — so securejoin boundary applies to
// destiny-root, not service-root.
//
// {name} must live in the LAST path segment (e.g., `.../destiny-{name}` or
// `.../{name}`): base (everything before last `/`) — trusted operator path and
// resolved via normal Join (allows `../` for cross-location), last segment
// with substituted name — untrusted and clamped by securejoin.
func (r *fixtureDestinyResolver) resolvePath(name, gitOverride string) (root, leaf string, err error) {
	tmpl := gitOverride
	if tmpl == "" {
		if r.template == "" {
			return "", "", fmt.Errorf("trial: default_destiny_source not set in case.yml::fixtures, and destiny %q has no per-entry git — apply:destiny resolve impossible", name)
		}
		tmpl = r.template
	}

	rel, ok := strings.CutPrefix(tmpl, fileScheme)
	if !ok {
		return "", "", fmt.Errorf("trial: destiny %q resolves to %q, but L0 is hermetic — only %s scheme is supported", name, tmpl, fileScheme)
	}

	baseTmpl, leafTmpl := filepath.Dir(rel), filepath.Base(rel)
	if !strings.Contains(leafTmpl, destinyNamePlaceholder) {
		return "", "", fmt.Errorf("trial: default_destiny_source %q must contain %s in last path segment (e.g., %sdestiny-%s) — no safe place to substitute destiny name", tmpl, destinyNamePlaceholder, fileScheme, destinyNamePlaceholder)
	}

	// Base is trusted operator path: resolved relative to service-root
	// via normal Join (Clean inside Join handles leading `../`).
	root = filepath.Join(r.serviceRoot, baseTmpl)
	leaf = strings.ReplaceAll(leafTmpl, destinyNamePlaceholder, name)
	return root, leaf, nil
}

// fixtureDestinyIncludeResolver is a within-destiny [config.IncludeResolver] for L0:
// include targets strictly within fixture's `tasks/` directory (securejoin clamps path).
func fixtureDestinyIncludeResolver(destinyDir string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		rel := filepath.Join("tasks", name)
		full, err := securejoin.SecureJoin(destinyDir, rel)
		if err != nil {
			return nil, "", fmt.Errorf("unsafe path %q: %w", rel, err)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, "", err
		}
		return data, rel, nil
	}
}
