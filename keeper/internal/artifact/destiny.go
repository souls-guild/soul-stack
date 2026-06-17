package artifact

import (
	"context"
	"fmt"
	"log/slog"
	"path"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// destinyManifestFile / destinyTasksFile / destinyTasksDir — каноническая
// раскладка destiny-репо (docs/destiny): корневой манифест + каталог задач
// (точка входа `tasks/main.yml`, include-соседи `tasks/<sub>.yml`).
const (
	destinyManifestFile = "destiny.yml"
	destinyTasksFile    = "tasks/main.yml"
	destinyTasksDir     = "tasks"
)

// DestinyRef — координаты destiny-репозитория для загрузки. Симметрично
// [ServiceRef]: Git резолвится из `default_destiny_source` (подстановка `{name}`),
// Ref — git tag/branch из `service.yml → destiny[]` по name (ADR-007).
type DestinyRef struct {
	Name string
	Git  string
	Ref  string
}

// DestinyArtifact — материализованный immutable-снапшот destiny-репозитория.
//
// Manifest — распарсенный `destiny.yml` (несёт `input:`-контракт для изоляции
// apply.input, ADR-009). Tasks — распарсенный `tasks/main.yml` (плоский список
// задач DSL-ядра). Render-проход destiny рендерит именно Tasks в своём
// изолированном CEL-env.
type DestinyArtifact struct {
	Ref      DestinyRef
	SHA1     string
	LocalDir string
	Manifest *config.DestinyManifest
	Tasks    []config.Task
}

// DestinyLoader загружает destiny-репозитории в кеш под cacheRoot и парсит их
// `destiny.yml` + `tasks/main.yml`. Безопасен для конкурентного использования
// (per-destiny Mutex, как [ServiceLoader]).
type DestinyLoader struct {
	snap snapshotter
}

// NewDestinyLoader создаёт загрузчик destiny с корнем кеша cacheRoot.
func NewDestinyLoader(cacheRoot string, logger *slog.Logger) *DestinyLoader {
	return &DestinyLoader{snap: newSnapshotter(cacheRoot, logger)}
}

// Load материализует immutable-снапшот destiny на commit-е, в который
// резолвится ref, парсит `destiny.yml` (манифест) и `tasks/main.yml` (задачи).
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

	return art, nil
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
