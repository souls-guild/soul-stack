// Package scenario — главный orchestrator прогона scenario на Keeper-стороне
// (architect-recon slice .g). Замыкает scenario-runner: связывает git-artifact
// loader, topology-резолвер, essence-pipeline, render-pipeline, gRPC-Outbound и
// incarnation/applyrun-CRUD в один async-прогон.
//
// Жизненный цикл прогона (см. [Runner.Start] → run-goroutine в run.go):
//
//	SelectByName → status=applying → Load(service) → ParseScenario →
//	LoadIncarnationHosts → Resolve(essence) → Render → dispatch(per-task
//	cross-host barrier) → UpdateStateFromRun(commit | error_locked)
//
// Pilot-объём DSL (PM-decision): sequential tasks + per-host fan-out + apply:
// destiny + include (раскрывается ДО render в config.ExpandIncludes) +
// serial/run_once (slice D: run_once режет таргет в render, serial катит хосты
// волнами в dispatch). block/loop/parallel — вне pilot, гарантирует
// render.Pipeline (ErrUnsupportedDSL). Cross-host barrier (orchestration.md §7):
// state_changes коммитятся один раз после завершения ВСЕХ волн/задач на ВСЕХ
// хостах прогона, никогда по-волново.
//
// RunResult-collection — Вариант A (poll apply_runs.status, PM-decision):
// proще cluster-coordination, poll БД работает в кластере (subscribe — только
// local). Поллится [applyrun.SelectStatusesByApplyID] до терминала всех SID-ов
// либо до per-scenario timeout-а.
package scenario

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// scenarioMainFile — точка входа сценария в service-репо (orchestration.md §1):
// `scenario/<name>/main.yml`.
const scenarioMainFile = "scenario/%s/main.yml"

// CreateScenarioName — имя bootstrap-сценария, который incarnation.Create
// (REST + MCP) прогоняет первым при появлении новой incarnation. Lifecycle-kind
// (см. [LifecycleScenarioNames]).
const CreateScenarioName = "create"

// DestroyScenarioName — имя teardown-сценария в снапшоте сервиса, который
// [Runner.StartDestroy] прогоняет в режиме [TerminalDestroy] (S-D2b). Совпадает
// по значению с incarnation.destroyScenarioName / destroyScenarioLabel, но это
// разные роли (имя файла сценария vs метка перехода в state_history); держим
// отдельную константу, чтобы смена одной не утянула другую молча. Lifecycle-kind
// (см. [LifecycleScenarioNames]).
const DestroyScenarioName = "destroy"

// LifecycleScenarioNames — каноническая конвенция keeper: набор имён сценариев,
// которые keeper трактует как specialized scenario-kind соответствующей фазы
// жизненного цикла. create → bootstrap новой incarnation; destroy → teardown
// (TerminalDestroy). Все прочие сценарии — operational (свободные операции над
// state), запускаемые обычным run-ом. Единственный источник правды для DTO-поля
// [artifact.Scenario.Kind]: имя ∈ набор → lifecycle, иначе operational.
// Императивные проверки имени по keeper-у ссылаются на per-name константы
// (CreateScenarioName / DestroyScenarioName), а не на строковые литералы.
//
// `converge` в набор НЕ входит (amend ADR-031, 2026-06-10): это operational-
// сценарий — запускается обычным run-ом (Apply-reconcile) И служит dry-run
// target-ом check-drift. Drift-контур грузит scenario/converge/main.yml по
// константе имени [ConvergeScenarioName] (auto-discover), не через членство в
// этом наборе, поэтому вывод converge из набора его не задевает.
var LifecycleScenarioNames = map[string]struct{}{
	CreateScenarioName:  {},
	DestroyScenarioName: {},
}

// IsLifecycleScenario сообщает, входит ли имя сценария в [LifecycleScenarioNames]
// (lifecycle-kind). Используется при разметке [artifact.Scenario.Kind] в
// listing-handler-е.
func IsLifecycleScenario(name string) bool {
	_, ok := LifecycleScenarioNames[name]
	return ok
}

// IsRunnableScenario сообщает, можно ли запустить сценарий оператором из
// Run-формы (ADR-042 «тупой фронт»: UI читает признак из каталога, не зашивает
// список имён). Канон: lifecycle-create=true (bootstrap новой incarnation),
// lifecycle-destroy=false (удаление — спец-флоу DELETE /v1/incarnations/{name},
// не run), operational=true (свободная операция над state). Размечает
// [artifact.Scenario.Runnable] в listing-handler-е.
func IsRunnableScenario(name string) bool {
	return name != DestroyScenarioName
}

// scenarioTemplatePrefix — каталог scenario-local-слоя двухуровневого резолва
// ресурсов (orchestration.md §6): `scenario/<name>`. render.TemplateReader ищет
// `.tmpl` сначала тут (scenario-local), затем на service-level. name уже прошёл
// валидацию ScenarioNamePattern (traversal/мусор отсечены до резолва пути).
func scenarioTemplatePrefix(name string) string {
	return "scenario/" + name
}

// ScenarioNamePattern — каноническая форма имени scenario: snake_case
// (`create`, `add_user`, `update_acl`), та же грамматика, что register/loop/
// input-идентификаторы (shared/config). Отличается от incarnation kebab-case;
// валидация имени до резолва пути `scenario/<name>/main.yml` отсекает path-
// traversal (`/`, `..`) и мусор.
const ScenarioNamePattern = `^[a-z][a-z0-9_]*$`

var scenarioNameRe = regexp.MustCompile(ScenarioNamePattern)

// ValidScenarioName проверяет соответствие имени scenario [ScenarioNamePattern].
func ValidScenarioName(name string) bool { return scenarioNameRe.MatchString(name) }

// defaultPollInterval — период опроса apply_runs.status в cross-host barrier
// fan-in (PM-decision: 200ms). Оптимизация (subscribe/event-driven) — позже.
const defaultPollInterval = 200 * time.Millisecond

// defaultRunTimeout — потолок длительности одного прогона scenario. Защита от
// «вечного barrier» (Soul завис, RunResult не пришёл). По истечении —
// abort + error_locked (PM-decision: 5min).
const defaultRunTimeout = 5 * time.Minute

// deployBudget — добавка к ceiling-у онбординга для provision-from-zero-прогона
// (ADR-0061): effective run-timeout такого прогона = ResolvedMaxAwaitTimeout (потолок
// барьера `await_online`) + deployBudget. Покрывает стадию ПОСЛЕ онбординга — деплой
// роли на онбордившиеся хосты (apply redis и т.п.) в Passage за refresh-границей.
// Внутренняя const, НЕ config-ключ: ручка `run_timeout` отложена отдельно, здесь
// фиксируем щедрый запас (deploy редко превышает единицы минут). Effective timeout
// применяется ТОЛЬКО к плану с refresh-эмиттером ([config.HasRefreshEmitter]); обычный
// прогон держит defaultRunTimeout (вечный barrier по-прежнему обрывается).
const deployBudget = 10 * time.Minute

// Sentinel-ошибки [Runner.Start].
var (
	// ErrAlreadyRunning — для applyID уже зарегистрирован активный прогон,
	// либо incarnation в статусе applying (PM-decision: на pilot — отказ,
	// не очередь).
	ErrAlreadyRunning = errors.New("scenario: run already in progress")
	// ErrShuttingDown — Runner в процессе Shutdown, новые прогоны не
	// принимаются.
	ErrShuttingDown = errors.New("scenario: runner is shutting down")
	// ErrLocked — incarnation в статусе error_locked: следующий прогон
	// отклоняется до явного unlock (ADR-009, «Атомарность и error_locked»).
	// Проверка под тем же FOR UPDATE, что и перевод в applying — авторитет
	// gate-а в транзакции, не только в HTTP-handler-е (TOCTOU-safe).
	ErrLocked = errors.New("scenario: incarnation is error_locked")

	// ErrNotRunnable — статус incarnation не входит в allow-list прогона
	// (всё, кроме ready/applying/error_locked: destroying — идёт teardown;
	// migration_failed — залочена провалившейся миграцией, нужен unlock/upgrade;
	// любой будущий статус). lockRun — explicit allow-list (fail-closed):
	// новый статус по умолчанию ОТВЕРГАЕТСЯ, а не молча разрешается. Отдельный
	// от ErrLocked, чтобы лог и result отличали «оператор должен сделать unlock
	// error_locked» от «инстанс в нелётном статусе».
	ErrNotRunnable = errors.New("scenario: incarnation status does not permit a run")

	// ErrKeeperModulesNotConfigured — scenario несёт задачу с `on: keeper`, но
	// keeper-side core-Registry не сконфигурирован (Deps.KeeperModules == nil).
	// Программная ошибка wire-up-а либо тестовая сборка без keeper-side модулей;
	// прогон уходит в error_locked (reason: keeper_dispatch_failed).
	ErrKeeperModulesNotConfigured = errors.New("scenario: keeper-side module registry is not configured")

	// errCancelRequested — внутренний sentinel barrier-а (G1): прогон отменён
	// через cluster-wide Cancel-флаг (apply_runs.cancel_requested, миграция
	// 024). Выставить флаг мог любой Keeper-инстанс; run-goroutine-владелец
	// прерывает барьер и уходит в abort (error_locked) — то же поведение, что
	// при локальном ctx-Cancel. Не экспортируется: наружу прогон виден через
	// статус incarnation, не через эту ошибку.
	errCancelRequested = errors.New("scenario: cluster-wide cancel requested")
)

// TerminalMode — режим финала прогона scenario (S-D2b): определяет, что
// run-goroutine делает на успешном завершении teardown-/apply-цикла и как
// фиксирует провал. Нулевое значение [TerminalCommitState] = обычный прогон —
// существующие вызовы (Create / scenario-run) не меняют поведения.
type TerminalMode int

const (
	// TerminalCommitState — обычный прогон (apply/upgrade): success коммитит
	// state_changes в incarnation.state + статус ready; провал → error_locked.
	// Дефолт (zero value): все существующие прогоны идут этим путём.
	TerminalCommitState TerminalMode = iota

	// TerminalDestroy — teardown-прогон (scenario `destroy`, S-D2b): success
	// НЕ коммитит state и НЕ переводит в ready — incarnation остаётся в
	// `destroying`, физический снос строки делает S-D3. Провал teardown
	// (хост упал, barrier fail-closed) → статус `destroy_failed` (НЕ
	// error_locked): из него оператор повторяет destroy или анлочит в ready
	// (S-D2a дал Unlock destroy_failed→ready).
	TerminalDestroy
)

// RunSpec — параметры одного прогона scenario, передаются API-handler-ом в
// [Runner.Start].
//
// ApplyID — ULID прогона (уже сгенерирован caller-ом, идёт в apply_runs /
// state_history / RunResult-correlation). ServiceRef — git-координаты
// service-репо (caller резолвит из реестра сервисов по incarnation.service,
// ADR-029).
// Input — `incarnation.spec.input` оператора. StartedByAID — инициатор (AID
// Архонта из Operator API; пустая строка → NULL в apply_runs).
//
// TerminalMode — режим финала (S-D2b): нулевое значение [TerminalCommitState]
// = обычный прогон; [TerminalDestroy] — teardown scenario `destroy`.
type RunSpec struct {
	ApplyID         string
	IncarnationName string
	ServiceRef      artifact.ServiceRef
	ScenarioName    string
	Input           map[string]any
	StartedByAID    string
	TerminalMode    TerminalMode

	// FromLocked — старт прогона по уже зарезервированному статусу applying
	// (rerun-last: incarnation.UnlockForRerun под FOR UPDATE перевёл
	// error_locked→applying минуя ready, race-free). lockRun при FromLocked НЕ
	// транзитит статус повторно — обязан увидеть applying, иначе старт отклоняется
	// (fail-closed). Нулевое значение = обычный старт.
	FromLocked bool

	// CadenceID — back-link на Cadence-расписание, породившее этот прогон
	// (ADR-046 §2, T4b-фундамент). nil ⇒ ручной прогон (оператор/Voyage без
	// расписания); populated ⇒ дочерний Voyage расписания. Проброс из
	// voyages.cadence_id (BuildVoyage → VoyageWorker → ScenarioSpawner). Несётся
	// в payload терминального события incarnation.run_completed ТОЛЬКО когда
	// != nil (ручной прогон ключ cadence_id не несёт — консервативно, как
	// drift-payload), чтобы постоянное Tiding-правило с cadence-селектором ловило
	// результаты прогонов расписания.
	CadenceID *string

	// VoyageID — back-link на породивший прогон Voyage (voyages.voyage_id,
	// ADR-043). Проброс из VoyageWorker через ScenarioSpawner.SpawnScenarioRun
	// (production-spawner кладёт его сюда). nil ⇒ прогон НЕ через Voyage: прямые
	// пути scenario `create` (auto_create) / rerun-last / destroy и их
	// MCP-аналоги вызывают Runner напрямую, минуя voyage-orchestrator. Несётся
	// в payload терминального события incarnation.run_completed ТОЛЬКО когда
	// != nil (симметрия с CadenceID) — для visibility-фетча per-incarnation
	// run-событий вояжа на Voyage detail (ADR-052 amend §k: событие per-
	// incarnation с correlation_id=apply_id, а страница вояжа фильтрует по
	// voyage_id в payload).
	VoyageID *string
}

// ApplyDispatcher — узкая поверхность gRPC-Outbound, нужная orchestrator-у:
// отправка `ApplyRequest` одному Soul-у. Интерфейс (а не *grpc.Outbound) —
// для unit-тестов runner-а без подъёма EventStream / StreamManager.
//
// Реализуется [grpc.Outbound] (метод SendApply с той же сигнатурой).
type ApplyDispatcher interface {
	SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error
}

// SummonsPublisher — узкая поверхность Redis-публикации Summons-сигнала
// (ADR-027(a)): «появились planned-задания, проверьте очередь». Интерфейс (а не
// прямой импорт keeper/internal/redis) держит scenario-runner независимым от
// Redis-клиента — тот же приём, что [acolyte.SummonsSubscriber]. nil → dispatch
// нового пути не шлёт Summons, planned-задания подхватит poll-fallback Acolyte
// (best-effort, ADR-027(a)).
//
// Реализуется тонкой обёрткой над [redis.PublishSummons] при wire-up-е (1.4.4).
type SummonsPublisher interface {
	PublishSummons(ctx context.Context) error
}

// LeaseOwnerChecker — узкая поверхность Redis-чтения владельца SID-lease:
// «какой Keeper-инстанс держит EventStream к данному Soul-у». Интерфейс (а не
// прямой импорт keeper/internal/redis) держит scenario-runner независимым от
// Redis-клиента — тот же приём, что [SummonsPublisher].
//
// Нужен ТОЛЬКО multi-keeper-guard-у run-goroutine-пути (acolytes=0,
// [Runner.dispatchWave]): перед SendApply сверяет владельца lease с собственным
// KID и при расхождении печатает WARN (footgun «прогон зависнет в applying» —
// RunResult уйдёт владельцу стрима на другом инстансе). При acolytes>0 (work-
// queue ADR-027) этой проблемы нет, guard не вызывается.
//
// ok=false — lease-ключа нет (Soul ни у кого на стриме); ошибка — сетевой сбой
// (guard деградирует молча, warn не печатается). nil → guard выключен (нет
// Redis / unit-тест без координации).
//
// Реализуется тонкой обёрткой над [redis.SoulLeaseOwner] при wire-up-е.
type LeaseOwnerChecker interface {
	SoulLeaseOwner(ctx context.Context, sid string) (kid string, ok bool, err error)
}

// PassageCapabilityChecker — узкая поверхность Redis-проверки «какие SID-ы НЕ
// анонсировали passage-capability» (ADR-056 §S5 forward-compat). Интерфейс (а не
// прямой импорт keeper/internal/redis) держит scenario-runner независимым от
// Redis-клиента — тот же приём, что [LeaseOwnerChecker] / [SummonsPublisher].
//
// Нужен ТОЛЬКО staged-гейту run.go: ДО dispatch-а сценария, стратифицированного в
// N>1 Passage, проверяет, что КАЖДЫЙ таргет-хост умеет эхать ApplyRequest.passage.
// Хост без capability → прогон отвергается (soul_passage_unsupported, fail-closed):
// иначе barrier следующего Passage ждал бы терминал, которого старый бинарь не
// пришлёт (зависание в applying).
//
// Возвращает подмножество переданных SID-ов БЕЗ capability (пустой/nil → все
// поддерживают). Ошибка — сетевой сбой Redis: staged-гейт обязан отвергнуть
// прогон (а не угадать поддержку), поэтому ошибка проброшена наверх.
//
// nil в Deps → гейт деградирует fail-closed: staged-прогон без чекера (нет Redis /
// unit-тест) отвергается целиком (нельзя подтвердить поддержку — нельзя слать N>1).
// Реализуется тонкой обёрткой над [redis.SoulsLackingCapability] при wire-up-е.
type PassageCapabilityChecker interface {
	SoulsLackingPassage(ctx context.Context, sids []string) ([]string, error)
}

// KeeperModuleRegistry — узкая поверхность keeper-side core-Registry
// (keeper/internal/coremod), нужная scenario-runner-у для локального исполнения
// задач с `on: keeper` (ADR-017, docs/keeper/modules.md). Интерфейс (а не прямой
// *coremod.Registry) держит scenario-пакет тестируемым без сборки всех keeper-side
// модулей и их dep-ов (PG / Vault / PluginHost) — fake реализует один Lookup.
//
// Реализуется [coremod.Registry] (метод Lookup с той же сигнатурой). nil в Deps →
// задача с `on: keeper` отвергается ([ErrKeeperModulesNotConfigured]).
type KeeperModuleRegistry interface {
	Lookup(name string) (module.SoulModule, bool)
}

// ChangedTaskReader — узкая поверхность read-доступа к журналу аудита: множества
// (sid, plan_index) задач прогона, терминал-ивших со статусом CHANGED (T3,
// свёртка changed_tasks) либо FAILED/TIMED_OUT (ADR-056 R3, cross-passage
// onfail-rescue-gating). Источник — `task.executed`-события (events_taskevent.go),
// НЕ отдельная таблица. Интерфейс (а не прямой *auditpg.Reader) держит scenario-
// пакет тестируемым без PG — fake реализует два метода.
//
// Реализуется [auditpg.Reader] (методы SelectChangedTaskKeys / SelectFailedTaskKeys
// с той же сигнатурой). nil → терминальное событие incarnation.run_completed
// эмитится без changed_tasks (свёртка пропускается, финал прогона не валится);
// cross-passage onchanges/onfail-gating (R3) деградирует fail-closed —
// staged-прогон с cross-passage requisite отвергается (см. run.go).
type ChangedTaskReader interface {
	SelectChangedTaskKeys(ctx context.Context, applyID string) (map[auditpg.ChangedTaskKey]struct{}, error)
	SelectFailedTaskKeys(ctx context.Context, applyID string) (map[auditpg.ChangedTaskKey]struct{}, error)
}

// Deps — конструкторские зависимости [Runner]. Обязательны все, кроме Logger
// (nil → discard) и Destiny (nil → apply:destiny не поддержан, ErrUnsupportedDSL).
type Deps struct {
	Loader   *artifact.ServiceLoader
	Topology *topology.Resolver
	Essence  *essence.Resolver
	Render   *render.Pipeline
	Outbound ApplyDispatcher
	// Destiny — источник destiny-артефактов для apply:destiny (default_destiny_
	// source + DestinyLoader). nil → apply:destiny в scenario отвергается на
	// render-фазе (ErrUnsupportedDSL).
	Destiny *DestinySource
	// KeeperModules — keeper-side core-Registry (ADR-017): задачи с `on: keeper`
	// (`core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`)
	// исполняются локально на инстансе через него. nil → задача с `on: keeper`
	// отвергается на dispatch-фазе ([ErrKeeperModulesNotConfigured]); чисто
	// Soul-side прогон работает без неё.
	KeeperModules KeeperModuleRegistry
	// DB — пул для incarnation + applyrun CRUD (одна Postgres, ADR-005).
	DB     *pgxpool.Pool
	Logger *slog.Logger

	// Vault — общий keeper-vault-клиент для scoped-резолва `vault:`-ref в
	// operator-input (docs/input.md → «vault_scope»). nil → input-vault-refs не
	// резолвятся (поле со значением `vault:` отвергается на input-фазе как
	// строка, не проходя ReadKV). Тот же клиент, что у render-pipeline.
	Vault InputVaultReader
	// Audit — write-path для security-trail резолва input-vault-ref
	// (`input.vault_resolved`, ok/denied) и терминального события прогона
	// (`incarnation.run_completed`, T3). nil → trail не пишется (резолв работает,
	// но без аудита — допустимо для unit-тестов).
	Audit audit.Writer
	// AuditReader — read-доступ к журналу аудита для свёртки per-task changed
	// (T3): множество (sid, task_idx) CHANGED-задач прогона. nil → терминальное
	// событие incarnation.run_completed эмитится без changed_tasks (свёртка
	// пропускается). Тот же pool/Reader, что у Operator API (auditpg.NewReader).
	AuditReader ChangedTaskReader

	// InputDenyPaths — config-расширение system-floor hard deny-list
	// (keeper.yml → vault.input_deny_paths). Дополняет [config.VaultInputFloor],
	// не выключает его.
	InputDenyPaths []string

	// Metrics — keeper_scenario_*-collectors (ADR-024). nil → метрики прогона
	// выключены (nil-safe методы [ScenarioMetrics] — no-op). Должен быть тем же
	// дескриптором, что регистрируется в main на общий metricsReg.
	Metrics *ScenarioMetrics

	// AcolyteEnabled — cutover-флаг исполнения apply (ADR-027, Phase 1.4.2):
	// true → dispatch пишет planned-задания + Summons (новый путь, исполняет
	// Acolyte-пул), false → прямой Insert(running)+SendApply (старый путь).
	// Заполняется из cfg.Acolytes>0 при wire-up-е (1.4.4). serial-guard
	// (run.go::run) всё равно загоняет scenario с `serial:`-задачами в старый
	// путь — распределённый serial — Phase 3.
	AcolyteEnabled bool

	// KID — идентификатор Keeper-инстанса (origin Summons-сигнала, ADR-027(a)).
	// Нужен новому пути dispatch-а для [SummonsPublisher]. Заполняется при
	// wire-up-е (1.4.4); пустой → AcolyteEnabled-путь Summons не шлёт (poll-
	// fallback подхватит).
	KID string

	// Summons — публикатор Summons-сигнала planned-заданий (ADR-027(a)). nil →
	// dispatch нового пути Summons не шлёт (best-effort, poll-fallback Acolyte
	// подхватит). Заполняется при wire-up-е (1.4.4).
	Summons SummonsPublisher

	// LeaseOwner — чекер владельца SID-lease для multi-keeper-guard-а
	// run-goroutine-пути (acolytes=0). nil → guard выключен (нет Redis / unit-
	// тест). Заполняется при wire-up-е только при живом Redis: на чужом KID-
	// владельце [Runner.dispatchWave] печатает WARN о возможном зависании прогона
	// в applying (single-keeper-only footgun дефолта acolytes=0).
	LeaseOwner LeaseOwnerChecker

	// PassageCap — чекер passage-capability таргет-хостов для staged-гейта run.go
	// (ADR-056 §S5). nil → staged-прогон (N>1 Passage) отвергается целиком
	// (fail-closed: без Redis нельзя подтвердить поддержку). Заполняется при
	// wire-up-е тонкой обёрткой над redis.SoulsLackingCapability.
	PassageCap PassageCapabilityChecker

	// PollInterval / RunTimeout — переопределение дефолтов (для тестов).
	// Нулевое значение → дефолт.
	PollInterval time.Duration
	RunTimeout   time.Duration

	// MaxAwaitTimeoutFn — hot-reload-aware источник ceiling-а барьера онбординга
	// `await_online` ([config.KeeperConfig.ResolvedMaxAwaitTimeout], ADR-0061): база
	// provision-aware effective run-timeout (ceiling + deployBudget) для прогона с
	// refresh-эмиттером. Замыкание (а не значение) — чтобы оператор-override keeper.yml::
	// max_await_timeout подхватывался следующим прогоном без рестарта, тем же приёмом,
	// что MaxAwaitTimeout у coremod (daemon.go). nil → effective timeout считается от
	// [config.DefaultMaxAwaitTimeout] (30m) — unit-тест/L0 без config.Store; provision-
	// прогон всё равно получает расширенный потолок, просто без hot-reload-override-а.
	MaxAwaitTimeoutFn func() time.Duration
}

// Runner — singleton-orchestrator прогонов scenario. Инжектится в API-handler
// (incarnation.Create) и в будущий `/scenarios/{scenario}`-endpoint.
//
// active хранит cancel-функции активных прогонов по applyID — для
// [Runner.Cancel] и graceful [Runner.Shutdown]. wg считает живые
// run-goroutine-ы. shuttingDown закрывает приём новых прогонов.
type Runner struct {
	deps         Deps
	logger       *slog.Logger
	pollInterval time.Duration
	runTimeout   time.Duration

	// maxAwaitTimeoutFn — hot-reload-aware ceiling барьера онбординга (копия
	// Deps.MaxAwaitTimeoutFn), база provision-aware effective run-timeout (run.go::
	// effectiveRunTimeout). nil → fallback на [config.DefaultMaxAwaitTimeout].
	maxAwaitTimeoutFn func() time.Duration

	// acolyteEnabled / kid — cutover-флаг и origin-KID нового пути dispatch-а
	// (ADR-027, Phase 1.4.2): копии Deps.AcolyteEnabled / Deps.KID, читаются в
	// run.go::run при ветвлении пути dispatch-а.
	acolyteEnabled bool
	kid            string

	// leaseOwner — чекер владельца SID-lease (копия Deps.LeaseOwner) для
	// multi-keeper-guard-а старого пути dispatch-а (acolytes=0, dispatch.go).
	// nil → guard выключен.
	leaseOwner LeaseOwnerChecker

	// passageCap — чекер passage-capability таргет-хостов (копия Deps.PassageCap)
	// для staged-гейта run.go (ADR-056 §S5). nil → staged-прогон отвергается
	// fail-closed.
	passageCap PassageCapabilityChecker

	// keeperModules — keeper-side core-Registry (копия Deps.KeeperModules) для
	// локального исполнения задач `on: keeper` (run.go::dispatchKeeperTasks).
	// nil → задача с `on: keeper` отвергается ([ErrKeeperModulesNotConfigured]).
	keeperModules KeeperModuleRegistry

	mu           sync.Mutex
	active       map[string]context.CancelFunc
	wg           sync.WaitGroup
	shuttingDown bool
}

// NewRunner собирает Runner. Паникует на nil обязательных зависимостях —
// это программная ошибка wire-up-а (main), не runtime-условие.
func NewRunner(deps Deps) *Runner {
	if deps.Loader == nil || deps.Topology == nil || deps.Essence == nil ||
		deps.Render == nil || deps.Outbound == nil || deps.DB == nil {
		panic("scenario: NewRunner: required dependency is nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	pollInterval := deps.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	runTimeout := deps.RunTimeout
	if runTimeout <= 0 {
		runTimeout = defaultRunTimeout
	}
	return &Runner{
		deps:              deps,
		logger:            logger,
		pollInterval:      pollInterval,
		runTimeout:        runTimeout,
		maxAwaitTimeoutFn: deps.MaxAwaitTimeoutFn,
		acolyteEnabled:    deps.AcolyteEnabled,
		kid:               deps.KID,
		leaseOwner:        deps.LeaseOwner,
		passageCap:        deps.PassageCap,
		keeperModules:     deps.KeeperModules,
		active:            make(map[string]context.CancelFunc),
	}
}
