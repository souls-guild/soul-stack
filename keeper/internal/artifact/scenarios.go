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

	"github.com/souls-guild/soul-stack/shared/config"
)

// scenarioDir — каноническая раскладка scenario в Service-репозитории
// (docs/scenario): каталог `scenario/<name>/main.yml` на сервис; имя scenario
// = имя поддиректории. Парный destinyTasksDir-у/migrationsDir-у contant —
// чтобы не плодить magic-string по пакету.
const scenarioDir = "scenario"

// upgradeDir — второй канал авто-дискавери сценариев (ADR-0068 §3): каталог
// `upgrade/<slug>/main.yml` рядом со scenarioDir. Держит version-к-версии
// upgrade-сценарии отдельно от scenario/ (в обычных списках не мелькают).
const upgradeDir = "upgrade"

// scenarioMainFile — корневой YAML scenario внутри `<dir>/<name>/`.
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
	// Create — дискриминатор «сценарий годен как стартовый (bootstrap новой
	// incarnation)»: top-level `create: true` в main.yml (механизм нескольких
	// create-сценариев). UI фильтрует по нему список «выбрать стартовый сценарий»
	// в Create-форме; default-выбор — сценарий с именем `create` (back-compat).
	// omitempty — false (non-create сценарий) опускается из reply бит-в-бит как до
	// фичи. `destroy` этим флагом НЕ помечается (teardown — спец-флоу DELETE).
	Create bool `json:"create,omitempty"`
	// FromVersions — self-describing список версий-источников upgrade-сценария
	// (top-level `from:` в `upgrade/<slug>/main.yml`, ADR-0068 §3): из каких пинов
	// сценарий умеет апгрейдить. Заполняется только через [ListUpgrades]; у обычных
	// scenario/-записей nil → omitempty опускает поле бит-в-бит как до фичи.
	FromVersions []string `json:"from_versions,omitempty"`
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
//
// ShowWhen — опц. CEL-предикат над input.* для условной видимости секции. КАВЕТ:
// это ПРЕЗЕНТАЦИЯ, НЕ валидационный гейт — listing отдаёт строку как есть, eval
// делает UI client-side (вариант A); backend предикат НЕ вычисляет. Скрытие секции
// не отменяет валидацию её полей backend-ом. omitempty — нет ключа → видимо всегда.
type ScenarioFormSection struct {
	Key         string              `json:"key"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Collapsed   bool                `json:"collapsed,omitempty"`
	ShowWhen    string              `json:"show_when,omitempty"`
	Fields      []ScenarioFormField `json:"fields,omitempty"`
}

// ScenarioFormField — ссылка на поле input с опц. подписью и UX-подсказками.
//
// ShowWhen — условная видимость поля (семантика/кавет — как у секции: презентация,
// client-side eval, не гейт). Placeholder / Hint — чистая презентация виджета
// (текст в пустом поле / подсказка под полем), НЕ дублируют input-контракт. Все
// три omitempty — отсутствие любого бит-в-бит как до фичи.
type ScenarioFormField struct {
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	ShowWhen    string `json:"show_when,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Hint        string `json:"hint,omitempty"`
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
	// Extends — имя covenant-фрагмента (`extends:`), общий контракт секций
	// которого сценарий наследует (ScenarioManifest.Extends). Для UI-listing-а
	// значим ТОЛЬКО ради input: covenant.yml.input сливается в InputSchema
	// (mergeCovenantInputRaw) — иначе сценарий с нулевой input-дельтой (вся схема
	// в covenant, как create_from_souls) отдал бы пустую форму. Пустая строка /
	// нет ключа = нет наследования (forward-compat). Резолв — на raw-уровне
	// (см. ListScenarios): listing держит InputSchema сырым map-ом под UI, а
	// типизированный covenant-merge (config.ResolveScenarioCovenant) даёт другую
	// форму (InputSchemaMap без json-тегов) — она бы сломала фронт.
	Extends string `yaml:"extends"`
	// Create — top-level `create:` флаг стартового сценария. `*bool` ради
	// различения «не задано» (nil → не стартовый) от явного `create: false`;
	// listing проецирует в Scenario.Create через !=nil && *Create (см.
	// loadScenario). Строгую валидацию типа делает soul-lint (config-валидатор);
	// тут best-effort проекция для UI — невалидный тип останется nil → false.
	Create *bool `yaml:"create"`
	// FromVersions — top-level `from:` upgrade-манифеста (ADR-0068). Проецируется в
	// Scenario.FromVersions только на upgrade-пути ([ListUpgrades]); для scenario/
	// ключа нет → nil. Строгую валидацию типа делает soul-lint (config-валидатор).
	FromVersions []string `yaml:"from"`
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
	ShowWhen    string                  `yaml:"show_when"`
	Fields      []scenarioFormFieldYAML `yaml:"fields"`
}

type scenarioFormFieldYAML struct {
	Name        string `yaml:"name"`
	Label       string `yaml:"label"`
	ShowWhen    string `yaml:"show_when"`
	Placeholder string `yaml:"placeholder"`
	Hint        string `yaml:"hint"`
}

// ListScenarios сканирует `scenario/*/main.yml` в материализованном снапшоте
// service-репозитория (serviceRoot — абсолютный путь к снапшоту, обычно
// [ServiceArtifact.LocalDir]) и возвращает отсортированный по имени список
// scenario-метаданных для операционного UI-dropdown.
//
// Это ТОЛЬКО scenario/-канал: upgrade/<slug>/ здесь НЕ появляется (ADR-0068 §3 —
// upgrade-сценарии не пугают оператора в обычных списках сценариев; их отдаёт [ListUpgrades]).
func ListScenarios(serviceRoot string, logger *slog.Logger) ([]Scenario, error) {
	return listFromDir(serviceRoot, scenarioDir, logger)
}

// ListUpgrades — зеркало [ListScenarios] для второго канала авто-дискавери
// (ADR-0068 §3): сканирует `upgrade/<slug>/main.yml` того же снапшота и отдаёт
// список с заполненным [Scenario.FromVersions]. Отсутствие каталога `upgrade/` →
// пустой список (сервис без upgrade-сценариев валиден), как у ListScenarios с
// отсутствующим `scenario/`.
func ListUpgrades(serviceRoot string, logger *slog.Logger) ([]Scenario, error) {
	return listFromDir(serviceRoot, upgradeDir, logger)
}

// listFromDir — общий скан каталога сценариев (`scenario/` или `upgrade/`) в
// снапшоте сервиса: `<dir>/<name>/main.yml` → отсортированный по имени список
// метаданных. Единственное различие между [ListScenarios] и [ListUpgrades] —
// каталог `dir`; всё прочее (partial-success, securejoin, type-catalog-резолв)
// общее.
//
// Семантика partial-success: каждый сценарий обрабатывается изолированно.
// Неразобранный YAML / отсутствующий main.yml → warning в logger и пропуск,
// но НЕ возврат ошибки (UI dropdown должен отображать остальные, даже если один
// сломан в репо). Сам каталог `dir` может отсутствовать — сервис без сценариев/
// апгрейдов валиден (вернём пустой список). Логгер опционален (nil → slog.Default).
//
// Безопасность пути — securejoin на каждый дочерний join (защита от `..` /
// симлинк-побега из снапшота). Симлинки внутри снапшота не следуем по сути
// (read os.ReadDir + securejoin); серверная сторона держит снапшот immutable.
func listFromDir(serviceRoot, dir string, logger *slog.Logger) ([]Scenario, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dirRoot, err := securejoin.SecureJoin(serviceRoot, dir)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь каталога %s: %w", dir, err)
	}

	entries, err := os.ReadDir(dirRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Scenario{}, nil
		}
		return nil, fmt.Errorf("artifact: чтение каталога %s: %w", dir, err)
	}

	// Каталог переиспользуемых именованных типов сервиса (`types.yml`) — общий
	// для всех сценариев, читается один раз. $type-ссылки в InputSchema каждого
	// сценария резолвятся backend-side ДО проекции в reply (см. loadScenario):
	// UI получает уже подставленную схему + аннотацию x-type, а не сырой $type.
	catalog := loadTypeCatalog(serviceRoot, logger)

	out := make([]Scenario, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		sc, ok := loadScenario(serviceRoot, dir, name, logger)
		if !ok {
			continue
		}
		sc.InputSchema = resolveScenarioTypeRefs(sc.InputSchema, catalog)
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// loadScenario читает один `<dir>/<name>/main.yml` (dir — scenarioDir либо
// upgradeDir) и парсит его в [Scenario]. Возвращает (_, false) при отсутствующем
// main.yml или невалидном YAML (partial-success: warning логируется, caller
// пропускает запись).
//
// Имя scenario берётся из директории (надёжный источник: совпадает с тем,
// что пишут в `apply.scenario` / `apply.destiny`); top-level `name:` в YAML
// игнорируется, даже если задан — расхождение имени директории и YAML-имени
// — баг service-репо, а не listing-а.
func loadScenario(serviceRoot, dir, name string, logger *slog.Logger) (Scenario, bool) {
	relPath := filepath.ToSlash(filepath.Join(dir, name, scenarioMainFile))
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
	// covenant-merge ПЕРЕД отдачей input_schema: сценарий с `extends:` наследует
	// covenant.yml.input (тот же add-only merge, что runtime LoadScenarioManifest
	// Resolved → config.ResolveScenarioCovenant). Без него сценарий с нулевой
	// input-дельтой (вся схема в covenant — create_from_souls) отдал бы UI пустую
	// форму. Резолв raw-уровневый (covenant-input остаётся сырым map-ом под UI),
	// $type-ссылки covenant-полей резолвятся ПОСЛЕ — в ListScenarios, вместе с
	// локальными (одним проходом resolveScenarioTypeRefs).
	if raw.Extends != "" {
		schema = mergeCovenantInputRaw(serviceRoot, raw.Extends, schema, logger)
	}
	sc := Scenario{
		Name:        name,
		Path:        relPath,
		Create:      raw.Create != nil && *raw.Create,
		Description: raw.Description,
		InputSchema: schema,
		Tags:        raw.Tags,
		Form:        scenarioFormProjection(raw.Form),
	}
	// Изоляция канала ФИЗИЧЕСКАЯ, не только по каталогу (ADR-0068 §3): стрэй `from:`
	// в scenario/<name>/main.yml не должен просочиться в reply — FromVersions
	// несёт только upgrade/-канал.
	if dir == upgradeDir {
		sc.FromVersions = raw.FromVersions
	}
	return sc, true
}

// mergeCovenantInputRaw читает covenant.yml по имени из `extends:` и сливает его
// `input:`-секцию (raw-map) в `local` ADD-ONLY: covenant — БАЗА, дельта сценария
// дополняет (поля local НЕ перезаписываются — параллель config.mergeInputSections,
// но на raw-уровне под UI). Это listing-проекция того же covenant-резолва, что
// делает runtime (LoadScenarioManifestResolved → config.ResolveScenarioCovenant) —
// без неё сценарий с нулевой input-дельтой (вся схема в covenant) отдаёт пустую форму.
//
// Имя covenant валидируется тем же [config.ValidExtendsName] (единый источник
// правды формы), путь — securejoin от serviceRoot (traversal-кламп поверх грамматики
// имени). Partial-success как у всего listing-а: невалидное имя / отсутствующий /
// битый covenant.yml → warning + local как есть (UI получит хотя бы локальную дельту,
// а не 500; полную ошибку covenant_extends_* поднимет runtime-load и soul-lint).
//
// Конфликт ключа (поле и в covenant, и в local) на runtime — section_key_conflict;
// здесь listing НЕ валит набор: local-схема побеждает (covenant не перезаписывает),
// форма всё равно отрисуется. Возвращает map для отдачи в InputSchema (может быть
// nil, если ни covenant, ни local не дали полей).
func mergeCovenantInputRaw(serviceRoot, extends string, local map[string]any, logger *slog.Logger) map[string]any {
	if !config.ValidExtendsName(extends) {
		logger.Warn("artifact: covenant-input пропущен — недопустимое имя extends",
			slog.String("extends", extends))
		return local
	}
	covenantFile := extends + ".yml"
	data, err := readSnapshotFile(serviceRoot, covenantFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("artifact: covenant-input пропущен — ошибка чтения covenant.yml",
				slog.String("extends", extends), slog.Any("error", err))
		}
		// Отсутствие covenant.yml при заявленном extends — ошибка репо
		// (covenant_extends_target_not_found на runtime); listing не валит форму,
		// отдаёт локальную дельту как есть.
		return local
	}

	var frag struct {
		Input map[string]any `yaml:"input"`
	}
	if err := yaml.Unmarshal(data, &frag); err != nil {
		logger.Warn("artifact: covenant-input пропущен — невалидный YAML covenant.yml",
			slog.String("extends", extends), slog.Any("error", err))
		return local
	}
	if len(frag.Input) == 0 {
		return local
	}

	if local == nil {
		local = make(map[string]any, len(frag.Input))
	}
	for name, schema := range frag.Input {
		if _, dup := local[name]; dup {
			// Конфликт ключа — local побеждает (covenant add-only НЕ override).
			// runtime поднимет section_key_conflict; listing не валит форму.
			continue
		}
		local[name] = schema
	}
	return local
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
			ShowWhen:    s.ShowWhen,
			Fields:      make([]ScenarioFormField, 0, len(s.Fields)),
		}
		for _, f := range s.Fields {
			sec.Fields = append(sec.Fields, ScenarioFormField{
				Name:        f.Name,
				Label:       f.Label,
				ShowWhen:    f.ShowWhen,
				Placeholder: f.Placeholder,
				Hint:        f.Hint,
			})
		}
		out.Sections = append(out.Sections, sec)
	}
	return out
}
