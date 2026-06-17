package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	securejoin "github.com/cyphar/filepath-securejoin"
	yaml "gopkg.in/yaml.v3"
)

// scenarioDir — каноническая раскладка scenario в Service-репозитории
// (docs/scenario): каталог `scenario/<name>/main.yml` на сервис; имя scenario
// = имя поддиректории. Парный destinyTasksDir-у/migrationsDir-у contant —
// чтобы не плодить magic-string по пакету.
const scenarioDir = "scenario"

// scenarioMainFile — корневой YAML scenario внутри `scenario/<name>/`.
const scenarioMainFile = "main.yml"

// Scenario — одна запись listing-а scenario из материализованного снапшота
// Service-репозитория (`scenario/<name>/main.yml`). Лёгкая проекция top-level
// полей scenario.yml для UI-dropdown «Choose scenario» (handler-у не нужны ни
// tasks, ни state_changes, ни orchestration-дельта — только метаданные).
//
// Имена ключей JSON совпадают с UI-API ([ServiceScenariosListReply]); типы —
// минимально-достаточные: InputSchema хранится `map[string]any` (повторяет
// сырой YAML), чтобы UI мог отрисовать форму без серверной валидации (та живёт
// в render-pipeline). Description / Tags необязательны — у scenario может не
// быть top-level описания / тэгов; тогда поля останутся "" / nil.
type Scenario struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Runnable — признак «запускаем оператором из Run-формы» (ADR-042 «тупой
	// фронт»): create=true, destroy=false (удаление — спец-флоу DELETE),
	// operational=true. Размечает listing-handler по канону scenario-пакета
	// (scenario.IsRunnableScenario), сам ListScenarios поле не заполняет.
	Runnable    bool           `json:"runnable"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
}

// Значения [Scenario.Kind] — closed enum дискриминатора сценария для UI
// («тупой фронт» читает каталог, не хардкодит имена create/destroy).
// Разметку ставит listing-handler по канону scenario.LifecycleScenarioNames
// (artifact не зависит от scenario-пакета — направление импорта обратное), сам
// ListScenarios оставляет поле пустым.
const (
	// ScenarioKindLifecycle — имя сценария ∈ scenario.LifecycleScenarioNames
	// (create / destroy): keeper трактует его как specialized scenario-kind
	// фазы жизненного цикла.
	ScenarioKindLifecycle = "lifecycle"
	// ScenarioKindOperational — обычный сценарий (свободная операция над state),
	// запускаемый обычным run-ом. `converge` — operational (amend ADR-031,
	// 2026-06-10): выведен из lifecycle-набора, несёт двойную роль
	// Apply-reconcile-run + dry-run target check-drift.
	ScenarioKindOperational = "operational"
)

// scenarioYAML — узкое top-level подмножество scenario/<name>/main.yml,
// разбираемое для UI listing-а. Поля симметричны JSON-форме [Scenario];
// `input` и `input_schema` оба принимаются (docs/scenario/concept.md):
// исторически слово было `input_schema`, в свежих примерах — `input` (см.
// service-redis-monitored). Берём приоритет input_schema → input.
//
// Нестандартные top-level-поля игнорируются (yaml.Unmarshal в struct ловит
// только перечисленные), это соответствует stop-rule ТЗ.
type scenarioYAML struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Input       map[string]any `yaml:"input"`
	InputSchema map[string]any `yaml:"input_schema"`
	Tags        []string       `yaml:"tags"`
}

// ListScenarios сканирует `scenario/*/main.yml` в материализованном снапшоте
// service-репозитория (serviceRoot — абсолютный путь к снапшоту, обычно
// [ServiceArtifact.LocalDir]) и возвращает отсортированный по имени список
// scenario-метаданных.
//
// Семантика partial-success: каждое scenario обрабатывается изолированно.
// Неразобранный YAML / отсутствующий main.yml → warning в logger и пропуск,
// но НЕ возврат ошибки (UI dropdown должен отображать остальные scenario,
// даже если одно сломано в репо). Сам `scenarioDir` может отсутствовать —
// сервис без сценариев валиден (вернём пустой список). Логгер опционален
// (nil → slog.Default).
//
// Безопасность пути — securejoin на каждый дочерний join (защита от `..` /
// симлинк-побега из снапшота). Симлинки внутри снапшота не следуем по сути
// (read os.ReadDir + securejoin); серверная сторона держит снапшот immutable.
func ListScenarios(serviceRoot string, logger *slog.Logger) ([]Scenario, error) {
	if logger == nil {
		logger = slog.Default()
	}
	scenariosRoot, err := securejoin.SecureJoin(serviceRoot, scenarioDir)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь scenario-каталога: %w", err)
	}

	entries, err := os.ReadDir(scenariosRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Scenario{}, nil
		}
		return nil, fmt.Errorf("artifact: чтение scenario-каталога %s: %w", scenarioDir, err)
	}

	out := make([]Scenario, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		sc, ok := loadScenario(serviceRoot, name, logger)
		if !ok {
			continue
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// loadScenario читает один `scenario/<name>/main.yml` и парсит его в [Scenario].
// Возвращает (_, false) при отсутствующем main.yml или невалидном YAML
// (partial-success: warning логируется, caller пропускает запись).
//
// Имя scenario берётся из директории (надёжный источник: совпадает с тем,
// что пишут в `apply.scenario` / `apply.destiny`); top-level `name:` в YAML
// игнорируется, даже если задан — расхождение имени директории и YAML-имени
// — баг service-репо, а не listing-а.
func loadScenario(serviceRoot, name string, logger *slog.Logger) (Scenario, bool) {
	relPath := filepath.ToSlash(filepath.Join(scenarioDir, name, scenarioMainFile))
	mainPath, err := securejoin.SecureJoin(serviceRoot, relPath)
	if err != nil {
		logger.Warn("artifact: scenario пропущена — небезопасный путь",
			slog.String("scenario", name), slog.Any("error", err))
		return Scenario{}, false
	}

	data, err := os.ReadFile(mainPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			logger.Warn("artifact: scenario пропущена — нет main.yml",
				slog.String("scenario", name), slog.String("path", relPath))
		} else {
			logger.Warn("artifact: scenario пропущена — ошибка чтения main.yml",
				slog.String("scenario", name), slog.Any("error", err))
		}
		return Scenario{}, false
	}

	var raw scenarioYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		logger.Warn("artifact: scenario пропущена — невалидный YAML",
			slog.String("scenario", name), slog.Any("error", err))
		return Scenario{}, false
	}

	schema := raw.InputSchema
	if schema == nil {
		schema = raw.Input
	}
	return Scenario{
		Name:        name,
		Path:        relPath,
		Description: raw.Description,
		InputSchema: schema,
		Tags:        raw.Tags,
	}, true
}
