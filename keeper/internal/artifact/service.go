package artifact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// serviceManifestFile — имя корневого манифеста сервиса в репозитории.
const serviceManifestFile = "service.yml"

// migrationsDir — каталог цепочки state_schema-миграций в Service-репо
// (docs/migrations.md §Раскладка: `migrations/<NNN>_to_<MMM>.yml`).
const migrationsDir = "migrations"

// ErrMigrationChainBroken — в цепочке отсутствует ожидаемый шаг
// (`migrations/<NNN>_to_<MMM>.yml`): upgrade требует миграцию, файла нет.
// Симметрично statemigrate-кодам: snake_case-маркер для диагностики.
var ErrMigrationChainBroken = errors.New("artifact: migration_chain_broken")

// ServiceLoader загружает Service-репозитории в кеш под cacheRoot и парсит их
// `service.yml`. Безопасен для конкурентного использования: per-service Mutex
// сериализует git-операции над одним рабочим клоном (разные сервисы идут
// параллельно).
type ServiceLoader struct {
	snap snapshotter
}

// NewServiceLoader создаёт загрузчик с корнем кеша cacheRoot. Если logger nil,
// используется slog.Default.
func NewServiceLoader(cacheRoot string, logger *slog.Logger) *ServiceLoader {
	return &ServiceLoader{snap: newSnapshotter(cacheRoot, logger)}
}

// Load материализует immutable-снапшот сервиса на commit-е, в который
// резолвится ref, и парсит его `service.yml`.
func (l *ServiceLoader) Load(ctx context.Context, ref ServiceRef) (*ServiceArtifact, error) {
	sha1, dir, err := l.snap.snapshot(ctx, ref.Name, ref.Git, ref.Ref, "сервиса")
	if err != nil {
		return nil, err
	}
	art := &ServiceArtifact{Ref: ref, SHA1: sha1, LocalDir: dir}
	manifest, err := l.parseManifest(art)
	if err != nil {
		return nil, err
	}
	art.Manifest = manifest
	return art, nil
}

// parseManifest читает и валидирует `service.yml` снапшота через нормативный
// `shared/config`-парсер. Diagnostics с уровнем error трактуются как ошибка
// загрузки (битый манифест в репо).
func (l *ServiceLoader) parseManifest(art *ServiceArtifact) (*config.ServiceManifest, error) {
	data, err := l.ReadFile(art, serviceManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: чтение %s сервиса %q: %w", serviceManifestFile, art.Ref.Name, err)
	}
	manifest, _, diags, err := config.LoadServiceManifestFromBytes(serviceManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s сервиса %q: %w", serviceManifestFile, art.Ref.Name, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s сервиса %q невалиден: %s", serviceManifestFile, art.Ref.Name, firstError(diags))
	}
	return manifest, nil
}

// ReadFile читает файл из снапшота по relative-path. Путь резолвится через
// securejoin: выход за пределы LocalDir (через `..`/абсолютный путь/симлинк)
// исключён.
func (l *ServiceLoader) ReadFile(art *ServiceArtifact, file string) ([]byte, error) {
	return readSnapshotFile(art.LocalDir, file)
}

// LoadMigrationChain собирает цепочку state_schema-миграций from→to из
// снапшота сервиса (docs/migrations.md): для каждой версии v∈[from, to)
// читает `migrations/<NNN>_to_<MMM>.yml` (NNN = "%03d" v, MMM = "%03d" v+1),
// парсит через [statemigrate.Parse]. Forward-only (ADR-019): from > to →
// ошибка (downgrade unsupported), from == to → пустой Chain (no-op ref-bump).
//
// Отсутствующий файл шага → [ErrMigrationChainBroken] (upgrade требует
// миграцию, файла нет). Паттерн — [DestinyLoader.parseTasks]/[ReadFile]:
// чтение через securejoin, парсинг чистой функцией [statemigrate.Parse].
func (l *ServiceLoader) LoadMigrationChain(art *ServiceArtifact, from, to int) (statemigrate.Chain, error) {
	if from > to {
		// Downgrade — guard на уровне загрузчика (дублирует caller-side guard
		// в incarnation.UpgradeStateSchema; forward-only, ADR-019).
		return nil, fmt.Errorf("artifact: migration downgrade unsupported: from=%d > to=%d", from, to)
	}
	if from == to {
		return statemigrate.Chain{}, nil
	}

	chain := make(statemigrate.Chain, 0, to-from)
	for v := from; v < to; v++ {
		rel := path.Join(migrationsDir, fmt.Sprintf("%03d_to_%03d.yml", v, v+1))
		data, err := readSnapshotFile(art.LocalDir, rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s сервиса %q отсутствует", ErrMigrationChainBroken, rel, art.Ref.Name)
			}
			return nil, fmt.Errorf("artifact: чтение %s сервиса %q: %w", rel, art.Ref.Name, err)
		}
		m, err := statemigrate.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("artifact: парсинг %s сервиса %q: %w", rel, art.Ref.Name, err)
		}
		chain = append(chain, m)
	}
	return chain, nil
}

// ReadSnapshotFile читает файл из снапшота по абсолютному localDir (корень
// материализованного снапшота сервиса/destiny) и relative-path. Экспортируемая
// обёртка над общим securejoin-ридером — для caller-ов вне пакета (render-wiring
// строит из неё [render.TemplateReader] поверх конкретного снапшота, не зная
// внутренней раскладки кеша). Выход за пределы localDir (`..`/абсолютный путь/
// симлинк наружу) заклампен securejoin-ом.
func ReadSnapshotFile(localDir, relPath string) ([]byte, error) {
	return readSnapshotFile(localDir, relPath)
}

// readSnapshotFile читает файл из снапшота localDir по relative-path. Общий для
// service- и destiny-снапшотов: securejoin клампит выход за пределы localDir.
func readSnapshotFile(localDir, path string) ([]byte, error) {
	full, err := securejoin.SecureJoin(localDir, path)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь %q: %w", path, err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("artifact: чтение %s: %w", filepath.Base(path), err)
	}
	return data, nil
}

// firstError возвращает сообщение первой error-диагностики для краткого отчёта.
func firstError(diags []diag.Diagnostic) string {
	for i := range diags {
		if diags[i].Level == diag.LevelError {
			return diags[i].Message
		}
	}
	return "unknown validation error"
}
