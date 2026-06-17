package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// StateSchemaInfo — проекция state_schema-метаданных одного снапшота Service-
// репо для UI Schema explorer-а (`GET /v1/services/{name}/state-schema`):
// текущая `state_schema_version`, опциональная декларация структуры state
// (`state_schema:` mapping из service.yml) и плоский список найденных миграций
// в `migrations/<NNN>_to_<MMM>.yml`. Content миграций не парсится — только
// metadata (from / to / относительный путь), чтобы UI мог построить graph
// «версия → версия» без серверной валидации DSL.
//
// Имена JSON-полей совпадают с UI-API (`ServiceStateSchemaReply`); типы —
// минимально-достаточные: Schema хранится `map[string]any` (повторяет сырой
// YAML), Migrations — отсортированный по `to` ASC список [Migration].
type StateSchemaInfo struct {
	Version    int            `json:"state_schema_version"`
	Schema     map[string]any `json:"schema,omitempty"`
	Migrations []Migration    `json:"migrations"`
}

// Migration — одна запись цепочки state_schema-миграций (metadata-only):
// номер версий-источника и -приёмника + относительный путь файла в снапшоте.
// Content (DSL-операции) НЕ парсится — UI Schema explorer-у нужен только
// граф `from → to` (грамматику миграции пользователь смотрит в git-репо).
type Migration struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Path string `json:"path"`
}

// reMigrationFile — каноническое имя файла миграции в `migrations/`
// (docs/migrations.md → `<NNN>_to_<MMM>.yml`). NNN/MMM — три цифры с
// ведущими нулями; иные файлы (`README.md`, тесты-каталоги, прочее) — игнорим.
var reMigrationFile = regexp.MustCompile(`^(\d{3})_to_(\d{3})\.yml$`)

// ListStateSchema собирает [StateSchemaInfo] из материализованного снапшота
// service-репо (serviceRoot — абсолютный путь, обычно [ServiceArtifact.LocalDir]).
//
// Алгоритм:
//  1. Парсит `service.yml` через нормативный [config.LoadServiceManifestFromBytes];
//     валидацию manifest-уровня НЕ перепроверяет — error-диагностика == битый
//     манифест в репо, поднимаем ошибку выше (caller отдаст 502).
//  2. Достаёт `state_schema_version` (≥1; ADR-019: monotonic int) и `state_schema:`
//     (опционально; service.yml МОЖЕТ декларировать структуру через MVP JSON
//     Schema подмножество — type/required/properties/items/additionalProperties,
//     см. validateStateSchema). Если поля нет — Schema=nil, и в JSON оно
//     omitempty-выкидывается; UI трактует как «структура не задекларирована».
//  3. Сканирует `migrations/` (если каталога нет — пустой список, не ошибка;
//     parity со [ListScenarios]). Файлы вида `<NNN>_to_<MMM>.yml` парсятся
//     только по имени (regex [reMigrationFile]); прочие сущности (subdir-ы
//     `<NNN>_to_<MMM>/tests/`, README, и т.п.) пропускаются молча. Сортировка —
//     by `to` ASC (граф цепочки растёт по версиям).
//
// Логгер опционален (nil → slog.Default). Stop-rules ТЗ:
//   - state_schema_version отсутствует в манифесте → согласно spec MVP это
//     ошибка валидации (config.schemaValidateService), которая поднимется
//     выше через diag.HasErrors → возвращаем error.
//   - migrations/ нет → empty list, без ошибки.
//   - YAML битый → error (caller отдаёт 502 bad-gateway).
func ListStateSchema(serviceRoot string, logger *slog.Logger) (*StateSchemaInfo, error) {
	if logger == nil {
		logger = slog.Default()
	}

	manifestPath, err := securejoin.SecureJoin(serviceRoot, serviceManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь %s: %w", serviceManifestFile, err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("artifact: чтение %s: %w", serviceManifestFile, err)
	}
	manifest, _, diags, err := config.LoadServiceManifestFromBytes(serviceManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s: %w", serviceManifestFile, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s невалиден: %s", serviceManifestFile, firstError(diags))
	}

	info := &StateSchemaInfo{
		Version: manifest.StateSchemaVersion,
		Schema:  manifest.StateSchema,
	}

	migrations, err := scanMigrations(serviceRoot, logger)
	if err != nil {
		return nil, err
	}
	info.Migrations = migrations
	return info, nil
}

// scanMigrations читает `migrations/`-каталог снапшота и возвращает
// отсортированный по `to` ASC список найденных шагов. Отсутствующий каталог
// → пустой список (сервис без миграций валиден). Файлы, не подпадающие под
// `<NNN>_to_<MMM>.yml`, пропускаются молча; subdir-ы (тесты миграций) —
// тоже (мы НЕ заходим внутрь, content не парсится).
func scanMigrations(serviceRoot string, logger *slog.Logger) ([]Migration, error) {
	migRoot, err := securejoin.SecureJoin(serviceRoot, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь %s: %w", migrationsDir, err)
	}
	entries, err := os.ReadDir(migRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Migration{}, nil
		}
		return nil, fmt.Errorf("artifact: чтение %s: %w", migrationsDir, err)
	}

	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			// `<NNN>_to_<MMM>/tests/` и прочие subdir-ы внутрь не сканируем.
			continue
		}
		m := reMigrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		from, ferr := strconv.Atoi(m[1])
		to, terr := strconv.Atoi(m[2])
		if ferr != nil || terr != nil {
			// Невозможно по regex, но guard на случай change-а grammar-а.
			logger.Warn("artifact: миграция пропущена — неразборные NNN/MMM",
				slog.String("file", e.Name()))
			continue
		}
		out = append(out, Migration{
			From: from,
			To:   to,
			Path: migrationsDir + "/" + e.Name(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].To < out[j].To })
	return out, nil
}
