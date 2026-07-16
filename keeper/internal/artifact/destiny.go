package artifact

import (
	"context"
	"fmt"
	"log/slog"
	"path"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// destinyManifestFile / destinyTasksFile / destinyTasksDir — the canonical
// layout of a destiny repo (docs/destiny): root manifest + tasks directory
// (entry point `tasks/main.yml`, include neighbors `tasks/<sub>.yml`).
const (
	destinyManifestFile = "destiny.yml"
	destinyTasksFile    = "tasks/main.yml"
	destinyTasksDir     = "tasks"
	// destinyVarsFile — destiny locals next to the manifest (docs/destiny/vars.md).
	// Optional: its absence is not an error (destiny without locals).
	destinyVarsFile = "vars.yml"
)

// DestinyRef — coordinates of a destiny repository to load. Symmetric to
// [ServiceRef]: Git is resolved from `default_destiny_source` (`{name}`
// substitution), Ref is a git tag/branch from `service.yml → destiny[]` by
// name (ADR-007).
type DestinyRef struct {
	Name string
	Git  string
	Ref  string
}

// DestinyArtifact — a materialized immutable snapshot of a destiny repository.
//
// Manifest — the parsed `destiny.yml` (carries the `input:` contract for
// apply.input isolation, ADR-009). Tasks — the parsed `tasks/main.yml` (a
// flat list of DSL-core tasks). The destiny render pass renders exactly
// Tasks in its own isolated CEL env.
type DestinyArtifact struct {
	Ref      DestinyRef
	SHA1     string
	LocalDir string
	Manifest *config.DestinyManifest
	Tasks    []config.Task
	// Vars — RAW destiny locals from `vars.yml` (docs/destiny/vars.md), without
	// schema validation (vars are untyped). nil if the file is absent. CEL
	// expressions `${ … }` in values are resolved in the render phase, not here.
	Vars map[string]any
}

// DestinyLoader loads destiny repositories into the cache under cacheRoot and
// parses their `destiny.yml` + `tasks/main.yml`. Safe for concurrent use
// (per-destiny Mutex, like [ServiceLoader]).
type DestinyLoader struct {
	snap snapshotter
}

// NewDestinyLoader creates a destiny loader with cache root cacheRoot.
func NewDestinyLoader(cacheRoot string, logger *slog.Logger) *DestinyLoader {
	return &DestinyLoader{snap: newSnapshotter(cacheRoot, logger)}
}

// Load materializes an immutable destiny snapshot at the commit ref resolves
// to, and parses `destiny.yml` (manifest) and `tasks/main.yml` (tasks).
func (l *DestinyLoader) Load(ctx context.Context, ref DestinyRef) (*DestinyArtifact, error) {
	sha1, dir, err := l.snap.snapshot(ctx, ref.Name, ref.Git, ref.Ref, "destiny")
	if err != nil {
		return nil, err
	}
	art := &DestinyArtifact{Ref: ref, SHA1: sha1, LocalDir: dir}

	manifest, err := l.parseManifest(art)
	if err != nil {
		return nil, err
	}
	art.Manifest = manifest

	tasks, err := l.parseTasks(art)
	if err != nil {
		return nil, err
	}
	art.Tasks = tasks

	vars, err := l.parseVars(art)
	if err != nil {
		return nil, err
	}
	art.Vars = vars

	return art, nil
}

// parseVars reads the snapshot's optional `vars.yml` (destiny locals,
// docs/destiny/vars.md). File absent → nil (destiny without locals).
// securejoin clamps escapes outside the snapshot; for not-exist it returns
// nil, so the file's optionality is preserved.
func (l *DestinyLoader) parseVars(art *DestinyArtifact) (map[string]any, error) {
	full, err := securejoin.SecureJoin(art.LocalDir, destinyVarsFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: unsafe path %s destiny %q: %w", destinyVarsFile, art.Ref.Name, err)
	}
	vars, err := config.LoadDestinyVars(full)
	if err != nil {
		return nil, fmt.Errorf("artifact: %s destiny %q: %w", destinyVarsFile, art.Ref.Name, err)
	}
	return vars, nil
}

// parseManifest reads and validates the snapshot's `destiny.yml`.
func (l *DestinyLoader) parseManifest(art *DestinyArtifact) (*config.DestinyManifest, error) {
	data, err := readSnapshotFile(art.LocalDir, destinyManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: reading %s destiny %q: %w", destinyManifestFile, art.Ref.Name, err)
	}
	manifest, _, diags, err := config.LoadDestinyManifestFromBytes(destinyManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: parsing %s destiny %q: %w", destinyManifestFile, art.Ref.Name, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s destiny %q invalid: %s", destinyManifestFile, art.Ref.Name, firstError(diags))
	}
	return manifest, nil
}

// parseTasks reads and validates the snapshot's `tasks/main.yml` as a flat
// list of DSL-core tasks, then expands within-destiny includes
// (`tasks/<sub>.yml`, destiny/tasks.md §4) into a flat list — before the
// render phase. Include targets resolve strictly inside the snapshot's
// `tasks/` directory (securejoin clamps escapes; `../` is already rejected at
// the validation phase). Cycles are detected by resolved path.
func (l *DestinyLoader) parseTasks(art *DestinyArtifact) ([]config.Task, error) {
	data, err := readSnapshotFile(art.LocalDir, destinyTasksFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: reading %s destiny %q: %w", destinyTasksFile, art.Ref.Name, err)
	}
	tasks, diags, err := config.LoadDestinyTasksFromBytes(destinyTasksFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: parsing %s destiny %q: %w", destinyTasksFile, art.Ref.Name, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s destiny %q invalid: %s", destinyTasksFile, art.Ref.Name, firstError(diags))
	}

	expanded, idiags := config.ExpandIncludes(tasks, destinyIncludeResolver(art.LocalDir))
	if diag.HasErrors(idiags) {
		return nil, fmt.Errorf("artifact: expanding includes in %s destiny %q: %s", destinyTasksFile, art.Ref.Name, firstError(idiags))
	}
	return expanded, nil
}

// destinyIncludeResolver — the within-destiny [config.IncludeResolver]:
// include targets stay strictly inside the snapshot's `tasks/` directory
// (destiny/tasks.md §4 — a neighbor in the same folder, escaping it is
// forbidden). securejoin inside readSnapshotFile clamps `..`/absolute
// paths/symlinks. The display path (`tasks/<sub>.yml`) is the cycle-detection
// key.
func destinyIncludeResolver(localDir string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		rel := path.Join(destinyTasksDir, name)
		data, err := readSnapshotFile(localDir, rel)
		if err != nil {
			return nil, "", err
		}
		return data, rel, nil
	}
}
