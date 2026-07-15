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

// parseVars читает опциональный `vars.yml` снапшота (destiny-локалы,
// docs/destiny/vars.md). Отсутствие файла → nil (destiny без локалов).
// securejoin клампит выход за пределы снапшота; для not-exist возвращает nil,
// чтобы опциональность файла сохранилась.
func (l *DestinyLoader) parseVars(art *DestinyArtifact) (map[string]any, error) {
	full, err := securejoin.SecureJoin(art.LocalDir, destinyVarsFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь %s destiny %q: %w", destinyVarsFile, art.Ref.Name, err)
	}
	vars, err := config.LoadDestinyVars(full)
	if err != nil {
		return nil, fmt.Errorf("artifact: %s destiny %q: %w", destinyVarsFile, art.Ref.Name, err)
	}
	return vars, nil
}

// parseManifest читает и валидирует `destiny.yml` снапшота.
func (l *DestinyLoader) parseManifest(art *DestinyArtifact) (*config.DestinyManifest, error) {
	data, err := readSnapshotFile(art.LocalDir, destinyManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: чтение %s destiny %q: %w", destinyManifestFile, art.Ref.Name, err)
	}
	manifest, _, diags, err := config.LoadDestinyManifestFromBytes(destinyManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s destiny %q: %w", destinyManifestFile, art.Ref.Name, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s destiny %q невалиден: %s", destinyManifestFile, art.Ref.Name, firstError(diags))
	}
	return manifest, nil
}

// parseTasks читает и валидирует `tasks/main.yml` снапшота как плоский список
// задач DSL-ядра, затем раскрывает within-destiny include (`tasks/<sub>.yml`,
// destiny/tasks.md §4) в плоский список — до render-фазы. Include-цели резолвятся
// строго внутри каталога `tasks/` снапшота (securejoin клампит выход; `../`
// запрещён ещё на фазе валидации). Циклы детектируются по resolved-пути.
func (l *DestinyLoader) parseTasks(art *DestinyArtifact) ([]config.Task, error) {
	data, err := readSnapshotFile(art.LocalDir, destinyTasksFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: чтение %s destiny %q: %w", destinyTasksFile, art.Ref.Name, err)
	}
	tasks, diags, err := config.LoadDestinyTasksFromBytes(destinyTasksFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s destiny %q: %w", destinyTasksFile, art.Ref.Name, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s destiny %q невалиден: %s", destinyTasksFile, art.Ref.Name, firstError(diags))
	}

	expanded, idiags := config.ExpandIncludes(tasks, destinyIncludeResolver(art.LocalDir))
	if diag.HasErrors(idiags) {
		return nil, fmt.Errorf("artifact: раскрытие include в %s destiny %q: %s", destinyTasksFile, art.Ref.Name, firstError(idiags))
	}
	return expanded, nil
}

// destinyIncludeResolver — within-destiny [config.IncludeResolver]: include-цели
// строго в каталоге `tasks/` снапшота (destiny/tasks.md §4 — сосед в той же
// папке, выход за её пределы запрещён). securejoin внутри readSnapshotFile
// клампит `..`/абсолютный путь/симлинк. display-путь (`tasks/<sub>.yml`) — ключ
// cycle-detection.
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
