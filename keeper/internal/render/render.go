// Package render — Keeper-side render pipeline scenario-runner-а (architect-recon
// slice .f). Оркестрирует фазы ADR-010 над одним прогоном scenario:
//
//	vault-resolve → input-validation → CEL-render → выдача []RenderedTask + []DispatchPlan
//
// text/template-render для `.tmpl`-файлов здесь НЕ выполняется: по ADR-012(d) он
// делается Soul-side в `core.file.rendered`. Pipeline лишь переносит literal
// template-content + CEL-rendered vars в params задачи (RawTemplate-поле),
// фактический text/template-проход — на хосте.
//
// Pipeline переиспользует pilot-пакеты: vault-resolve через
// `keeper/internal/vault.Client`, CEL-фазу через `shared/cel.Engine`, резолв
// хостов (`on:`/`where:`) через roster из `keeper/internal/topology`.
//
// Pilot-объём DSL: sequential tasks + per-host fan-out + `apply: destiny`
// (изолированный render-проход destiny, V2 ADR-009 — destiny рендерится со своим
// input-scope, задачи вклеиваются в общий план) + `serial:`/`run_once:`
// (orchestration.md §2.2: run_once режет таргет до первого хоста по SID,
// serial вычисляет ширину волны в DispatchPlan — волновой dispatch делает
// scenario-orchestrator). `block:` (C1) и `loop:` (E1) реализованы — разворачиваются
// в render-фазе в плоский список RenderedTask (renderBlockTask / renderLoopTask).
// `include:` раскрывается ДО render (config.ExpandIncludes на loader-слое), render
// получает плоский список; нераскрытый include: → [ErrUnexpandedInclude]. Из исходной
// тройки вне pilot-объёма остаётся только `parallel:` → [ErrUnsupportedDSL] (явная
// ошибка, не silent-skip).
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
// [ADR-012]: docs/adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add
package render

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrUnsupportedDSL — scenario использует DSL-конструкцию вне pilot-объёма
// (в scenario-слое — `parallel:`). Это не ошибка автора scenario, а граница
// реализации pilot-а: caller отличает «не поддержано в pilot» от «scenario сломан»
// (симметрично cel.ErrUnsupported). serial:/run_once: больше сюда НЕ входят — они
// реализованы (slice D); block: (C1) и loop: (E1) — тоже реализованы и сюда НЕ
// входят (renderBlockTask / renderLoopTask). include: сюда НЕ входит — он раскрывается до render
// (config.ExpandIncludes), см. ErrUnexpandedInclude. on: keeper больше сюда НЕ
// входит — keeper-side задачи рендерятся в keeper-контексте (см. resolveOn /
// renderKeeperTask).
var ErrUnsupportedDSL = errors.New("render: DSL-конструкция вне pilot-объёма")

// KeeperTargetSID — синтетический «хост»-таргет keeper-side задачи (`on: keeper`,
// docs/keeper/modules.md). У keeper-side шага хостов нет: он исполняется на самом
// keeper-инстансе. Чтобы вписать его в общую модель «один apply_runs-row на
// (apply_id, sid)» и cross-host barrier (orchestration.md §7), keeper-задача
// получает единый стабильный target-SID. Совпадает с литералом `on: keeper`
// (docs/naming-rules.md): apply_runs-строка прогона с sid="keeper" — это
// keeper-локальное исполнение, не Soul. souls.sid = FQDN, поэтому коллизии с
// реальным хостом нет (composite PK (apply_id, sid)).
const KeeperTargetSID = "keeper"

// RunSentinelSID — run-level terminal-маркер для apply_runs, когда scenario-run
// абортнут ДО dispatch-фазы и реальных хостов нет (BAG-1, ADR-043/027/009).
// Ранний abort (no_hosts / scenario_load_failed / topology_failed /
// essence_failed / input_invalid / render_failed / keeper_dispatch_failed) не
// успевает вставить НИ ОДНОЙ строки apply_runs: dispatch ещё не стартовал.
// Voyage-awaiter (PgIncarnationAwaiter.pollOutcome) поллит до терминала ВСЕХ
// строк прогона и при пустом наборе вечно ждёт → Voyage зависает. Чтобы у
// каждого прогона была гарантированная терминальная строка даже при пустом
// roster-е, abort-путь вставляет одну sentinel-строку apply_runs с этим SID и
// status=failed. НЕ путать с [KeeperTargetSID] (`on: keeper`, keeper-side
// исполнение реальной задачи): RunSentinelSID — НЕ исполнитель, а плейсхолдер
// «прогон закрыт без единого хоста». Не-FQDN-форма (`__run__`) гарантирует
// отсутствие коллизии с реальным souls.sid=FQDN (composite PK (apply_id, sid)).
const RunSentinelSID = "__run__"

// ErrUnexpandedInclude — render встретил include-задачу. include раскрывается в
// плоский список ДО render (config.ExpandIncludes на loader-слое); его наличие
// здесь — программная ошибка (раскрытие не вызвано), не граница pilot-а. Отдельный
// sentinel от ErrUnsupportedDSL: include поддержан, просто должен прийти раскрытым.
var ErrUnexpandedInclude = errors.New("render: include-задача дошла до render нераскрытой")

// ErrAssertFailed — assert-задача (ADR-009 amendment 2026-06-23) не прошла:
// хотя бы один предикат `that[]` вычислился в false на render-фазе. Render
// обрывается ДО dispatch — ни одной задачи на Soul не уходит, прогон не стартует
// («fail на этапе модели»). Это НЕ баг автора рендера и НЕ граница pilot-а, а
// объявленная DSL-семантика: caller (scenario.run / trial) отличает провал
// инварианта от внутренней ошибки и сообщает оператору message + текст предиката.
var ErrAssertFailed = errors.New("render: assert не прошёл")

// IncarnationMeta — фактологические поля incarnation, доступные в CEL как
// `incarnation.<path>` ([ADR-010]). Pipeline разворачивает их в map для
// cel.Vars.Incarnation; host_count подставляется автоматически из числа
// targeted-хостов (используется в scenario-предикатах вида
// `size(register.x) < incarnation.host_count`, см. add_user/main.yml).
type IncarnationMeta struct {
	Name           string
	Service        string
	ServiceVersion string
}

// RenderInput — вход одного прогона render pipeline.
//
// Essence — effective-слой essence incarnation (host-инвариантный), доступен в
// CEL как `essence.<path>` во всех scenario-vars (params/where/when/loop items).
// В destiny-проходе (renderApplyDestiny) НЕ пробрасывается — destiny видит
// essence только через apply: input: (изоляция, slice A). Register —
// register-context от уже выполненных задач (register-name → payload),
// предоставляется orchestrator-ом (.g) при per-task-рендере; в pilot пуст
// (cross-task chaining внутри Render — future).
//
// RegisterByHost — per-host register-context, накопленный из TaskEvent-ов
// прогона ПОСЛЕ барьера (sid → register-name → payload). Используется только
// [Pipeline.RenderStateChanges] (`sets: ${ register.<task>.<поле> }`, слайс 2);
// фаза Render его не читает (там register — cross-task chaining, future).
// Пустой/nil — прогон без register: задач (sets без register-ссылок).
//
// Hosts — roster прогона (resolved topology.Resolver-ом): connected-souls
// incarnation с last-reported soulprint. Pipeline сам применяет per-task
// `on:`/`where:` поверх этого roster-а (см. dispatch.go).
//
// Destiny — резолвер destiny для apply:destiny-задач (per-run: несёт destiny[]-
// refs текущего service-снапшота). nil → apply:destiny → [ErrUnsupportedDSL].
// В destiny-проходе (renderApplyDestiny) НЕ пробрасывается — destiny не делает
// вложенный apply:destiny в пилоте (guardDestinyTask его отвергает).
//
// Templates — ридер `.tmpl`-файлов снапшота сервиса для шага
// `core.file.rendered` (двухуровневый резолв scenario-local→service-level,
// ADR-009). Pipeline читает через него literal-содержимое шаблона и кладёт его в
// `params.template_content` (text/template здесь НЕ исполняется — это Soul-side).
// nil → задача core.file.rendered с ссылкой на шаблон → ошибка (handoff не
// настроен). В destiny-проходе (renderApplyDestiny) подменяется ридером снапшота
// destiny — её `.tmpl` живут в её собственном снапшоте, не в снапшоте сервиса.
type RenderInput struct {
	Scenario       *config.ScenarioManifest
	Essence        map[string]any
	Input          map[string]any
	Register       map[string]any
	RegisterByHost map[string]map[string]any
	Incarnation    IncarnationMeta
	Hosts          []*topology.HostFacts
	Destiny        DestinyResolver
	Templates      TemplateReader

	// State — снимок incarnation.state на момент захвата row-lock прогона
	// (run.go: stateBefore = inc.State под FOR UPDATE). Read-only: проецируется в
	// CEL как `incarnation.state.<path>` (incarnationVars), доступен в
	// params/where/apply-input И в state_changes-контексте; eval НЕ мутирует его
	// (CEL читает, не пишет — Вариант A, ADR-009/010). ИНВАРИАНТ на все passages
	// staged-render-а: renderIn переиспользуется на P>0 с тем же State, поэтому
	// `incarnation.state.*` идентичен на P0 и P1+ (= pre-run stateBefore, НЕ
	// промежуточный результат state_changes). nil → ключ `state` не объявляется
	// (push/trial без State: `incarnation.state.x` = no-such-key, backward-compat).
	State map[string]any

	// Ctx — request-scoped контекст прогона, прокидываемый в CEL-функцию vault()
	// ([ADR-017]) для отмены/таймаута ReadKV. Устанавливается [Pipeline.Render]
	// из своего ctx-аргумента и [Pipeline.RenderStateChanges]-caller-ом (run.go);
	// дочерний destiny-проход (renderApplyDestiny) наследует. nil ⇒ vault()
	// читает с context.Background() (cel.Vars.Ctx-семантика).
	Ctx context.Context

	// DestinyVarsResolved — резолвленные destiny-локалы `vars.yml` (Вариант A,
	// docs/destiny/vars.md), per-host: sid → имя→значение. Заполняется ОДИН раз на
	// destiny-проход (renderApplyDestiny резолвит vars.yml над destiny-env
	// input+soulprint.self+incarnation, изолированно от scenario register/essence и
	// без видимости vars друг друга), затем используется как БАЗОВЫЙ слой `vars.*`
	// при рендере каждой destiny-задачи (resolveTaskVars мерджит task-level `vars:`
	// поверх него — task переопределяет одноимённый file-vars). Инвариантен по
	// задачам прохода (резолвится не per-task). nil/пустой для хоста → база `vars.*`
	// пуста (scenario-проход: file-vars не участвуют). Ключ — SID хоста;
	// синтетический пустой хост (where: отфильтровал всех) → ключ "".
	DestinyVarsResolved map[string]map[string]any

	// Compute — резолвленные scenario-level `compute:`-переменные (ADR-009
	// amendment 2026-06-23): имя→значение, вычисленные ОДИН раз на прогон в
	// рун-уровневом контексте (input/register/incarnation/essence — БЕЗ soulprint,
	// структурный барьер host-инвариантности). Заполняется [Pipeline.resolveCompute]
	// в начале [Pipeline.Render] и [Pipeline.RenderStateOps]; кладётся в каждый
	// per-host контекст (hostVars) и в state_changes-контекст (stateChangesVars) как
	// `compute.<name>`. В изолированном destiny-проходе (renderApplyDestiny) НЕ
	// пробрасывается — destiny видит результат compute только через apply.input
	// (ADR-009 V2). nil ⇒ `compute.<name>` = штатный no-such-key (scenario без
	// compute:, backward-compat бит-в-бит).
	Compute map[string]any

	// destinyIsolated помечает изолированный destiny-проход (renderApplyDestiny).
	// Неэкспортируемое: внешние caller-ы (scenario-runner, trial) всегда дают
	// scenario-проход (zero-value false → soulprint.hosts доступен и проецируется
	// из Hosts). В destiny-проходе renderApplyDestiny ставит true: soulprint.hosts
	// в destiny — ошибка изоляции (orchestration.md §4.1), проекция не пробрасывается.
	destinyIsolated bool

	// TaskPassage — passage-индекс (0-based) каждой top-level задачи плана прогона
	// (staged-render, ADR-056; результат [Stratify]). Render клеймит им каждую
	// порождённую [RenderedTask] (и её apply:destiny/loop-потомков) по
	// originating-задаче — orchestrator (run.go) фильтрует dispatch/barrier по
	// RenderedTask.Passage. nil → все задачи в Passage 0 (N=1 / не-staged caller:
	// Trial, Acolyte RenderForHost, CheckDrift) — поведение БИТ-В-БИТ. Длина обязана
	// совпадать с числом top-level задач после ExpandIncludes (caller гарантирует:
	// Stratify работает над тем же списком).
	TaskPassage []int

	// ActivePassage — индекс Passage, который stage-loop рендерит и диспатчит
	// СЕЙЧАС (staged-render, ADR-056 §в.1). Задачи будущих Passage (TaskPassage[i] >
	// ActivePassage) ещё не имеют собранного register — их `where:`/`params:`,
	// читающие register, НЕ резолвятся: Render эмитит для них placeholder
	// RenderedTask (корректный Index/Passage, params/таргет не вычислены) только
	// ради сквозной index-нумерации; orchestrator их в этом Passage не диспатчит
	// (фильтр по Passage). Когда их Passage станет активным, повторный Render с
	// накопленным register резолвит их полноценно. nil-TaskPassage → ActivePassage
	// игнорируется (не-staged: все задачи Passage 0 рендерятся как сейчас, БИТ-В-БИТ).
	ActivePassage int

	// Sealed — аккумулятор sealed-путей render-прогона (seal / sealed-paths,
	// [ADR-010] §7.4). Render помечает в нём путь ячейки params, чьё СЫРОЕ
	// `${ … }`-значение читает secret-источник (secret-input/vault()/транзитивно
	// vars). Caller (scenario.run) создаёт [NewSealedSet], кладёт сюда и после
	// Render использует Sealed.Paths() для seal-aware маскинга наблюдаемых каналов
	// (audit.MaskSecretsSealed). nil ⇒ коллекция выключена (push/trial/Acolyte/
	// CheckDrift — seal не нужен, поведение БИТ-В-БИТ). Указатель шарится между
	// passages staged-render-а: пути накапливаются по всем Passage одного прогона.
	Sealed *SealedSet
}

// RenderedTask — задача после Keeper-side CEL-рендера, промежуточное
// представление перед сборкой `proto/keeper/v1.ApplyRequest`.
//
// Отличие от proto-типа `keeperv1.RenderedTask`: тут есть Index (позиция в
// scenario.tasks[], связывает с DispatchPlan и TaskEvent.task_idx) и Register
// (имя register-результата для chaining-а), которых нет в wire-контракте —
// orchestrator (.g) использует их при per-host-диспатче и не кладёт в proto.
//
// Params — CEL-rendered, уже в форме `*structpb.Struct` (прямая стыковка с
// proto). Для шага `core.file.rendered` params несут literal `template_content`
// (содержимое прочитанного `.tmpl`, A1-вариант ADR-012(d): без proto-изменений),
// ключ `template` (путь) из params удалён — Soul-у он не нужен.
//
// RawTemplate — literal-содержимое `.tmpl` для `core.file.rendered` после
// CEL-фазы, до text/template-прохода (Soul-side); "" для прочих модулей. Дублирует
// `params.template_content` как типизированное поле для orchestrator-/диагностики;
// authoritative для wire — именно `params.template_content` (A1).
type RenderedTask struct {
	Index    int
	Name     string
	Module   string
	Params   *structpb.Struct
	Register string

	// Passage — passage-индекс (0-based) staged-render (ADR-056), унаследованный
	// от originating top-level задачи через RenderInput.TaskPassage. orchestrator
	// (run.go stage-loop) диспатчит и барьерит задачи строго по Passage:
	// ApplyRequest несёт только задачи одного Passage, его barrier ждёт терминалы
	// строк (apply_id, sid, passage=N). 0 = единственный Passage (N=1 / не-staged)
	// — БИТ-В-БИТ как до staged-render. apply:destiny/loop-потомки наследуют
	// Passage родителя (block — атомарная единица Passage, ADR-056).
	Passage int
	// ID — стабильный адрес задачи из DSL-ядра `id:` (config.Task.ID, T1):
	// альтернатива register для адресации задачи без захвата register-результата
	// (register∪id, T1 запрещает оба сразу). Orchestrator-only, как Index — в
	// wire-контракт (keeperv1.RenderedTask) НЕ идёт: Soul адресует задачи по
	// task_idx, id нужен только Keeper-side свёртке per-task-итога (changed_tasks,
	// T3). Протягивается из config.Task.ID наравне с Register.
	ID    string
	NoLog bool
	// Timeout — per-task жёсткий лимит на одну попытку Apply (DSL-ядро timeout:,
	// destiny/tasks.md §9), convention `duration` Soul Stack (Go-duration "30s"
	// ИЛИ суффикс `<N>d`); "" = нет per-task лимита. Формат валидируется при
	// ПАРСЕ destiny/scenario (config-валидатор); render только протягивает
	// config.Task.Timeout в proto keeperv1.RenderedTask.Timeout без изменения
	// формы и без повторной проверки (string-консистентность с Task.Timeout/RetrySpec.Delay).
	Timeout     string
	RawTemplate string

	// When/ChangedWhen/FailedWhen — flow-control CEL-предикаты (ADR-012(d)),
	// протягиваются КАК CEL-СТРОКИ (не вычисляются Keeper-ом): они зависят от
	// register.* — результатов предыдущих задач, известных только Soul-у во время
	// прогона. Soul вычисляет их sandboxed-движком shared/cel.NewFlowControl.
	// When — gating ДО Apply (пусто = безусловно); ChangedWhen/FailedWhen —
	// override changed/failed ПОСЛЕ Apply (Soul вычисляет в applyrunner.runTask).
	// Источник — config.Task.When/ChangedWhen/FailedWhen (CEL-строки как есть из
	// распарсенного task). → proto keeperv1.RenderedTask.
	When        string
	ChangedWhen string
	FailedWhen  string

	// Until/RetryCount/RetryDelay — DSL-ядро retry: (destiny/tasks.md §9),
	// протягиваются Keeper-ом КАК ЕСТЬ — энфорс retry-петли Soul-side
	// (applyrunner.runTaskWithRetry). Until — CEL-предикат выхода из петли
	// (тот же sandboxed-движок, что failed_when; вычисляется на Soul после каждой
	// попытки). RetryCount — максимум попыток включая первую (0/1 = одна попытка).
	// RetryDelay — пауза между попытками, convention `duration` (string как
	// Timeout, без повторной проверки — формат провалидирован validateRetryField
	// при парсе). Источник — config.Task.Retry (nil → все zero-value = одна попытка).
	Until      string
	RetryCount int
	RetryDelay string

	// FlowContext — литеральный per-host снапшот НЕ-register части CEL-контекста
	// flow-control-предикатов: { input, vars, essence, incarnation, self }. То же,
	// что строится для рендера params (hostVars), МИНУС soulprint.hosts и loop. Soul
	// читает его как ДАННЫЕ (биндит soulprint.self ← flow_context.self), внешнего
	// доступа не делает. Host-вариативен (self per-host) — исключён из per-host-
	// сверки host-инвариантности params (см. paramsHostInvariant). → proto
	// keeperv1.RenderedTask.FlowContext.
	FlowContext *structpb.Struct

	// OnChangesIdx — индексы задач-источников DSL-ядра `onchanges:`
	// (destiny/tasks.md §8) после резолва register-имён в Index по всему плану
	// прогона (resolveOnChanges, Variant A). nil/пусто = безусловный запуск;
	// иначе задача исполняется на Soul-е только если хотя бы у одного источника
	// register.changed == true. config.Task.OnChanges (имена) → этот slice
	// (индексы) → proto keeperv1.RenderedTask.OnchangesIdx.
	OnChangesIdx []int

	// onChangesNames — register-имена `onchanges:` исходной задачи, протянутые
	// renderTaskIter до финального резолв-прохода [Pipeline.Render] →
	// resolveOnChanges. Неэкспортируемое: имена живут только внутри одного
	// прогона render до превращения в OnChangesIdx; orchestrator/dispatch видят
	// только индексы (wire-форма). После resolveOnChanges не используется.
	onChangesNames []string

	// OnFailIdx — индексы задач-источников DSL-ядра `onfail:` (destiny/tasks.md §8)
	// после резолва register-имён в Index по всему плану прогона (resolveOnFail,
	// Variant A — зеркало OnChangesIdx). nil/пусто = не-onfail-задача (gating не
	// применяется); иначе задача исполняется на Soul-е только если хотя бы у одного
	// источника register.failed == true (rescue-семантика). config.Task.OnFail
	// (имена) → этот slice (индексы) → proto keeperv1.RenderedTask.OnfailIdx.
	OnFailIdx []int

	// onFailNames — register-имена `onfail:` исходной задачи, зеркало
	// onChangesNames: протягиваются renderTaskIter до резолв-прохода
	// resolveOnFail, после превращения в OnFailIdx не используются.
	onFailNames []string

	// AggregateOf — ГЛОБАЛЬНЫЕ сквозные Index ВСЕХ дочерних destiny-задач одной
	// applier-задачи (`apply:`+`register:`), сводный итог которой несёт ЭТА
	// синтетическая терминальная `core.noop.run` (orchestration.md §2.1.1,
	// материализация applier-register, Вариант B). Эмитится renderApplyDestiny ПОСЛЕ
	// дочерних задач, только если у applier был непустой register: Register этой
	// задачи = register applier-а, поэтому внешний `onchanges:[<applier>]` /
	// `when: register.<applier>.changed` резолвится в её Index (registerIndex
	// подхватывает автоматически — onchanges.go не трогается).
	//
	// Soul (applyrunner.aggregateRegisterData) строит register_data этой
	// задачи НЕ из её ApplyEvent (noop тривиально changed=false), а как
	// `changed=OR(registerByIdx[i].changed)`, аналогично failed/timed_out по этим
	// индексам. Index'ы РЕМАПЯТСЯ global→local при сборке proto (ToProtoTasks/
	// remapRequisites), как OnChangesIdx — они адресуют локальную позицию в срезе
	// ApplyRequest.tasks[]. nil/пусто = задача не агрегирует. → proto
	// keeperv1.RenderedTask.AggregateOf. Хранятся как []int (global Index, зеркало
	// OnChangesIdx) — remapRequisites переводит в []int32-local при сборке proto.
	AggregateOf []int
}

// RenderedOp — одна операция `state_changes` после Keeper-side CEL-рендера
// (значение/ключ/match уже вычислены, cross-host last-wins-свёртка применена).
// Упорядоченный список этих операций возвращает [Pipeline.RenderStateOps];
// применяет их к incarnation.state — scenario.mergeStateChanges / trial-зеркало
// (orchestration.md §7, новая list-форма грамматики state_changes).
//
// Verb различает применение:
//   - VerbSet — перезапись Field значением Value;
//   - VerbAdd — идемпотентное добавление Value в коллекцию Field (map: по Key;
//     list: дедуп по Match-предикату). OnConflict (skip|replace|error) — политика
//     при совпадении идентичности;
//   - VerbModify — патч ВСЕХ элементов Field, подходящих под Match. Patch —
//     map путь-в-элементе → CEL/литерал, протягивается КАК ШАБЛОН (не вычислен):
//     merge вычисляет его per-matched-элемент через [StateOpEvalFunc] (биндинги
//     elem/key/value + scenario-контекст Context);
//   - VerbRemove — удалить ВСЕ элементы Field, подходящие под Match.
//
// foreach в RenderedOp не попадает — он раскрыт в render-фазе в N RenderedOp
// (по элементам коллекции, биндинг as-имени уже подставлен в Value/Patch/Match).
//
// Match/Patch протягиваются КАК СТРОКА/ШАБЛОН: merge вычисляет их per-element
// (Keeper заранее не вычисляет — зависят от каждого элемента state). Value (а
// для map — Key) уже cross-host-свёрнуты last-wins по SID.
//
// Context — per-RUN снимок scenario-контекста (input/register/incarnation/
// soulprint.self/essence/vars), last-wins по SID (output.md). Нужен merge-time
// для вычисления modify-Match/Patch и remove-Match, которые видят полный
// sets-контекст поверх биндингов элемента (ADR-057 §b). Для set/add — nil
// (их Value/Key уже вычислены на render-стороне; add-Match — чистая функция
// elem+value, см. [StateMatchFunc]).
//
// Expect — опц. ассерт кратности match для modify/remove (ADR-057 §c). ""/any —
// без ассерта.
type RenderedOp struct {
	Verb       config.StateVerb
	Field      string
	Value      any
	Key        string
	Match      string
	OnConflict config.OnConflict

	Patch   map[string]any
	Expect  config.Expect
	Context map[string]any
}

// StateMatchFunc — вычислитель match-предиката идентичности list-элемента add-
// операции (см. [Pipeline.EvalStateMatch]). Передаётся в merge (scenario/trial),
// чтобы тот не держал собственный cel.Engine: предикат `elem.sid == value.sid`
// вычисляется per-existing-элемент против биндингов elem (существующий) / value
// (добавляемый). Возврат — bool «идентичны».
type StateMatchFunc func(predicate string, elem, value any) (bool, error)

// StateOpEvalFunc — вычислитель CEL для modify/remove merge-time (см.
// [Pipeline.EvalStateOpExpr]). В отличие от [StateMatchFunc] (изолированный
// elem/value для add-дедупа), здесь предикат/значение видит ПОЛНЫЙ scenario-
// контекст прогона (ctx — снимок input/register/incarnation/soulprint.self/
// essence/vars) ПЛЮС биндинги текущего элемента коллекции (binds — elem/key/
// value). Используется per-matched-элемент: match-предикат → bool (boolOut=true),
// patch-значение → any (boolOut=false). Так modify-match `key == input.username`
// видит и key (элемент), и input.* (контекст).
type StateOpEvalFunc func(expr string, ctx, binds map[string]any, boolOut bool) (any, error)

// DispatchPlan — на какие хосты идёт задача после резолва `on:`+`where:`.
// TaskIndex ссылается на RenderedTask.Index. TargetSIDs — отсортированный по
// SID slice (детерминизм прогона, scenario/orchestration.md). Пустой TargetSIDs
// — задача не таргетит ни одного хоста (where: отфильтровал всех); это не
// ошибка, orchestrator пропускает такую задачу.
//
// SerialWidth — ширина волны `serial:` (orchestration.md §2.2.1): число хостов в
// волне (≤N), уже вычисленное из `serial: N | "<N>%"` против числа таргетов
// (процент — округление вверх, минимум 1). 0 = `serial:` не задан (вся ширина
// таргета в одной волне). RunOnce уже применён к TargetSIDs (срез до одного
// хоста, см. resolveTargets), поэтому отдельного флага run_once: в плане нет —
// он выражается единичным TargetSIDs. serial: и run_once: взаимоисключающи
// (config-валидатор), поэтому SerialWidth>0 и len(TargetSIDs)==1-от-run_once не
// пересекаются.
type DispatchPlan struct {
	TaskIndex   int
	TargetSIDs  []string
	SerialWidth int

	// Keeper помечает keeper-side задачу (`on: keeper`, docs/keeper/modules.md):
	// исполняется ЛОКАЛЬНО на keeper-инстансе через keeper-side core-Registry, НЕ
	// диспатчится Soul-у. TargetSIDs у такого плана = [KeeperTargetSID] (единичный
	// синтетический target — keeper-инстанс). scenario-runner ветвит исполнение по
	// этому флагу (run.go::dispatchKeeperTasks). false → обычная Soul-side задача.
	Keeper bool
}
