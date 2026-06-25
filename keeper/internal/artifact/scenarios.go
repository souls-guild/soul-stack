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
	// Form — опциональный презентационный слой формы (top-level `form:` scenario-
	// манифеста): секции с подписями полей для UI Run-modal. omitempty — нет form:
	// в YAML → поля нет в reply (бит-в-бит как до фичи); UI рисует input плоско
	// (forward-compat). Группировка/валидация ввода живёт в input_schema, form —
	// только презентация. Серверная сторона НЕ валидирует form (это делает
	// soul-lint/render-валидатор); listing отдаёт его как есть для UI.
	Form *ScenarioForm `json:"form,omitempty"`
}

// ScenarioForm / ScenarioFormSection / ScenarioFormField — JSON-проекция top-level
// `form:` для UI listing-а (симметрично [Scenario.InputSchema] как сырому input).
// Это презентационный слой: секции группируют поля input под подписями. Поля имён
// JSON совпадают с YAML-ключами манифеста; типы минимально-достаточные (UI рисует,
// не валидирует). Валидацию инвариантов (поле ∈ input, уникальность key) делает
// soul-lint (shared/config.validateFormLayout) — listing отдаёт form как есть.
type ScenarioForm struct {
	Sections []ScenarioFormSection `json:"sections,omitempty"`
}

// ScenarioFormSection — одна визуальная группа полей формы.
type ScenarioFormSection struct {
	Key         string              `json:"key"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Collapsed   bool                `json:"collapsed,omitempty"`
	Fields      []ScenarioFormField `json:"fields,omitempty"`
}

// ScenarioFormField — ссылка на поле input с опц. подписью.
type ScenarioFormField struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
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
// redis-monitored). Берём приоритет input_schema → input.
//
// Нестандартные top-level-поля игнорируются (yaml.Unmarshal в struct ловит
// только перечисленные), это соответствует stop-rule ТЗ.
type scenarioYAML struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Input       map[string]any `yaml:"input"`
	InputSchema map[string]any `yaml:"input_schema"`
	Tags        []string       `yaml:"tags"`
	// Form — опциональный презентационный слой (top-level `form:`). Нестандартные
	// под-ключи игнорируются (yaml.Unmarshal в struct ловит только перечисленные);
	// строгую валидацию формы делает soul-lint, не listing.
	Form *scenarioFormYAML `yaml:"form"`
}

// scenarioFormYAML / scenarioFormSectionYAML / scenarioFormFieldYAML — YAML-форма
// `form:` для listing-парса. Структурно совпадает с JSON-проекцией [ScenarioForm]
// (те же имена полей), но отдельный тип под yaml-теги: listing не тянет
// shared/config (изоляция артефакт-пакета, направление импорта обратное).
type scenarioFormYAML struct {
	Sections []scenarioFormSectionYAML `yaml:"sections"`
}

type scenarioFormSectionYAML struct {
	Key         string                  `yaml:"key"`
	Title       string                  `yaml:"title"`
	Description string                  `yaml:"description"`
	Collapsed   bool                    `yaml:"collapsed"`
	Fields      []scenarioFormFieldYAML `yaml:"fields"`
}

type scenarioFormFieldYAML struct {
	Name  string `yaml:"name"`
	Label string `yaml:"label"`
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
		Form:        scenarioFormProjection(raw.Form),
	}, true
}

// scenarioFormProjection переводит YAML-форму `form:` в JSON-проекцию [ScenarioForm]
// для reply. nil вход (нет ключа `form:`) → nil (поле omitempty опускается в reply —
// бит-в-бит как до фичи). Тривиальная перепись полей: listing не валидирует форму
// (это делает soul-lint), только отдаёт её UI как есть.
func scenarioFormProjection(in *scenarioFormYAML) *ScenarioForm {
	if in == nil {
		return nil
	}
	out := &ScenarioForm{Sections: make([]ScenarioFormSection, 0, len(in.Sections))}
	for _, s := range in.Sections {
		sec := ScenarioFormSection{
			Key:         s.Key,
			Title:       s.Title,
			Description: s.Description,
			Collapsed:   s.Collapsed,
			Fields:      make([]ScenarioFormField, 0, len(s.Fields)),
		}
		for _, f := range s.Fields {
			sec.Fields = append(sec.Fields, ScenarioFormField{Name: f.Name, Label: f.Label})
		}
		out.Sections = append(out.Sections, sec)
	}
	return out
}
