// Package trial — герметичный раннер испытаний (Trial, [ADR-023]) Destiny/
// Scenario, бинарь `soul-trial`. Этот пакет реализует уровень L0
// (render-only): прогон Keeper-side render-пайплайна (`keeper/internal/render`)
// на fixtures без хоста и без внешней инфры, сверка отрендеренного плана
// (`[]RenderedTask`) с `assert.rendered_tasks`, сбор trial coverage по
// CEL-веткам через cel.CoverageSink.
//
// L0-секции assert: rendered_tasks (плоский план задач), state_changes
// (отрендеренные sets) и state_after (детерминированный итоговый
// incarnation.state). assert.dispatch — секция уровня L3 (multi-host
// оркестрация, [ADR-023]); на single-host один синтетический хост, поэтому она
// осмысленна только на multi-host и в L0 не реализуется (strict-декод отвергает
// её как unknown-key — это намеренно, см. AssertBlock).
//
// [ADR-023]: docs/adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage
package trial

// Case — один файл испытания `case.yml` ([ADR-023], формат — расширение
// migration-эталона). Структура read-only после загрузки.
//
// Fixtures задают весь герметичный контекст прогона (input/essence/soulprint/
// vault). Mocks.Register — register-контекст probe-шагов для `where:`/`when:`
// (в L0-пилоте проброс готового register-payload-а, без исполнения probe).
// Assert — ожидаемый результат; L0 сверяет RenderedTasks, StateChanges и
// StateAfter (см. AssertBlock).
type Case struct {
	Name     string      `yaml:"name"`
	Fixtures Fixtures    `yaml:"fixtures"`
	Mocks    Mocks       `yaml:"mocks,omitempty"`
	Assert   AssertBlock `yaml:"assert,omitempty"`

	// ExpectRenderError — кейс ОЖИДАЕТ, что Keeper-side render ОБОРВЁТСЯ ошибкой,
	// содержащей эту подстроку (ADR-023 amendment 2026-06-23). Enabler fail-кейсов
	// механизмов, падающих на render: assert: (ADR-009 amendment) и будущий
	// required_when. Render-успех при заданном ExpectRenderError → FAIL кейса;
	// render-ошибка без подстроки → FAIL; render-ошибка с подстрокой → PASS.
	//
	// Взаимоисключим с assert.rendered_tasks: «ожидаем abort» и «ожидаем план» —
	// противоположные исходы (validate отвергает оба в одном кейсе). При
	// ExpectRenderError секция assert пуста (плана нет). Опционально (omitempty):
	// обычные L0-кейсы его не несут — путь рендера БИТ-В-БИТ.
	ExpectRenderError string `yaml:"expect_render_error,omitempty"`
}

// Fixtures — герметичный вход прогона. Все поля опциональны; пустое поле =
// пустой контекст соответствующей переменной CEL.
//
// Soulprint в L0 — факты ОДНОГО хоста (карта `soulprint.self.<path>`),
// single-host сахар: harness строит roster из одного синтетического хоста.
// Hosts — multi-host roster прогона (N хостов, render-инварианты топологии:
// `soulprint.hosts`/`.where(...)`/`size()`/nodes-детерминизм). Soulprint и Hosts
// ВЗАИМОИСКЛЮЧЕНЫ: оба в одном кейсе → strict-ошибка (validate), в духе
// strict-декода harness. Соответствует уровню L0 render-only — какой хост
// реально исполняет (dispatch) остаётся L3 ([ADR-023] amendment 2026-06-22).
//
// Vault — мок vault-резолва: ключ = logical-path секрета (`secret/<...>`),
// значение = map полей секрета (форма KV v2 `data.data`).
//
// State — базовый `incarnation.state` ДО прогона сценария (для операций,
// накапливающих state поверх существующего — add_user/update_acl/…). Нужен
// только для assert.state_after, где ожидаемый итог = State + отрендеренные
// state_changes.sets; для сценариев create (state «с нуля») опускается.
//
// DefaultDestinySource — L0-аналог keeper.yml::default_destiny_source (то же
// имя ключа): шаблон URL с подстановкой {name}, по которому apply:destiny
// резолвит destiny-зависимость. В L0 источник обязан быть герметичным
// (`file://`-схема, путь относительно service-root кейса, напр.
// `file://../../destiny/{name}`); per-entry `destiny[].git` override побеждает
// шаблон, но в L0 тоже обязан быть `file://`. Пустое значение допустимо для
// кейсов без apply:destiny — резолвер тогда не дёргается.
type Fixtures struct {
	Input                map[string]any            `yaml:"input,omitempty"`
	Essence              map[string]any            `yaml:"essence,omitempty"`
	Soulprint            map[string]any            `yaml:"soulprint,omitempty"`
	Hosts                []HostFixture             `yaml:"hosts,omitempty"`
	Vault                map[string]map[string]any `yaml:"vault,omitempty"`
	State                map[string]any            `yaml:"state,omitempty"`
	DefaultDestinySource string                    `yaml:"default_destiny_source,omitempty"`
}

// HostFixture — одна запись multi-host roster-а L0 (`fixtures.hosts[]`).
// Зеркало стабильных полей topology.HostFacts, видимых в render
// (`soulprint.hosts[]`): sid/covens/role/soulprint/choirs.
//
// SID обязателен; Covens обязаны нести incarnation.name-метку (имя сценария
// кейса) — зеркало прод-roster (`rosterSQL WHERE $1 = ANY(coven)`), иначе хост
// не попадёт в таргет `on:`/`where:`. Role/Soulprint/Choirs опциональны.
// Порядок roster-а в `soulprint.hosts` детерминирован сортировкой по SID
// (harness, не порядок в YAML).
type HostFixture struct {
	SID       string         `yaml:"sid"`
	Covens    []string       `yaml:"covens,omitempty"`
	Role      string         `yaml:"role,omitempty"`
	Soulprint map[string]any `yaml:"soulprint,omitempty"`
	Choirs    []string       `yaml:"choirs,omitempty"`
}

// Mocks — моки для шагов, обращающихся к среде хоста.
//
// Register — карта register-name → payload probe-шага. В L0-пилоте payload
// подставляется как готовый register-результат для `where:`/`when:` без
// исполнения самого probe (probe — Soul-side, в L0 хоста нет).
type Mocks struct {
	Register map[string]any `yaml:"register,omitempty"`
}

// AssertBlock — ожидаемый результат прогона. Подсекции независимы и
// опциональны ([ADR-023]); L0 реализует RenderedTasks, StateChanges и
// StateAfter.
//
// StateChanges — ожидаемый результат рендера `state_changes.sets` сценария
// (поле → значение после CEL-свёртки, симметрично RenderedTasks для tasks).
// Опционально: даже без этой секции state_changes ВСЕГДА рендерятся при
// прогоне кейса, и ошибка рендера (например незащищённый `${ input.X }` по
// optional-без-default input → CEL «no such key») — фейл кейса. Секция нужна,
// когда хочется зафиксировать конкретные значения, а не только сам факт
// успешного рендера.
//
// StateAfter — ожидаемый детерминированный итоговый `incarnation.state` после
// прогона: базовый `fixtures.state` + отрендеренные `state_changes.sets`
// (зеркало прод-коммита, orchestration.md §7.1). Герметично, без хоста (L0).
// Сверка ПОЛНАЯ (как L1-migration): лишний ключ в итоге — тоже расхождение,
// state фиксируется целиком. Опционально: кейс выбирает state_after, когда
// важен сам факт итогового state, а не только дельта sets (state_changes —
// частичная сверка дельты, state_after — полная сверка результата).
type AssertBlock struct {
	RenderedTasks []ExpectedTask `yaml:"rendered_tasks,omitempty"`

	// TaskPresent/TaskAbsent — assert-by-presence-форма (PILOT новой модели L0,
	// решение пользователя 2026-06-24): тест проверяет НАЛИЧИЕ/ОТСУТСТВИЕ
	// вызова задачи в плане, не его ПОЗИЦИЮ. Сосуществует с позиционной
	// rendered_tasks на время миграции (взаимоисключения нет — формы независимы,
	// см. compareTaskPresence). Для каждой записи TaskPresent в плане обязана быть
	// ≥1 задача matching {module== ∧ params_subset⊆params ∧ опц.when== ∧
	// опц.id==(register∪id)}; 0 совпадений → fail. Для TaskAbsent — ≥1 совпадение
	// → fail. Дизамбигуаторы when/id разрешают коллизию >1 совпадения на
	// task_present.
	TaskPresent []ExpectedTask `yaml:"task_present,omitempty"`
	TaskAbsent  []ExpectedTask `yaml:"task_absent,omitempty"`

	StateChanges map[string]any `yaml:"state_changes,omitempty"`
	StateAfter   map[string]any `yaml:"state_after,omitempty"`
	// Dispatch — секция уровня L3 (multi-host оркестрация, [ADR-023]): на
	// single-host один синтетический хост, dispatch-план осмыслен только на
	// топологии. Поле НЕ объявлено, чтобы strict-декод отверг опирающиеся на
	// него кейсы явной ошибкой unknown-key, а не молча-пропуском (тест —
	// TestLoadCase_RejectsUnknownSection).
}

// ExpectedTask — ожидание на одну отрендеренную задачу плана. Обслуживает обе
// формы L0-ассерта tasks: позиционную (assert.rendered_tasks, по Index) и
// presence (assert.task_present/task_absent, по совпадению атрибутов).
//
// Позиционная форма: Index — позиция в scenario.tasks[] (связь с
// RenderedTask.Index). Module — ожидаемый module-адрес. Params — ожидаемые
// CEL-rendered params (deep-compare). Params опциональны: если не заданы —
// сверяются только index+module. ParamsSubset/When/ID в этой форме не
// используются.
//
// Presence-форма: Index игнорируется (позиция не сверяется). Module обязателен —
// module-адрес искомой задачи. ParamsSubset — ПОДМНОЖЕСТВО ожидаемых params
// (по-ключно, поддерживает <present>-маркер; лишние ключи рендера не мешают
// совпадению — та же семантика, что Params в позиционной сверке). When/ID —
// опциональные ДИЗАМБИГУАТОРЫ при коллизии нескольких совпадений: When —
// точное равенство CEL-строки RenderedTask.When; ID — равенство
// RenderedTask.Register ИЛИ RenderedTask.ID (register∪id, T1).
type ExpectedTask struct {
	Index  int            `yaml:"index"`
	Module string         `yaml:"module"`
	Params map[string]any `yaml:"params,omitempty"`

	ParamsSubset map[string]any `yaml:"params_subset,omitempty"`
	When         string         `yaml:"when,omitempty"`
	ID           string         `yaml:"id,omitempty"`
}
