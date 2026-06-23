package config

import "time"

// Лимиты размера ApplyRequest (контракт Keeper↔Soul по EventStream, ADR-012).
//
// Поле задаётся в МиБ симметрично `logging.rotation.max_size_mb` (один стиль
// size-полей в проекте, без отдельного human-readable парсера). Обе стороны
// делят дефолт и минимум: Keeper-send-лимит обязан быть ≤ Soul-recv-лимиту,
// дефолты совпадают (8 MiB), чтобы из коробки Keeper не отправлял того, что
// Soul отвергнет.
const (
	// DefaultMaxApplySizeMB — дефолт обоих лимитов (Keeper-send и Soul-recv)
	// при опущении поля. 8 MiB > gRPC-дефолта recv (4 MiB), которого мало для
	// крупного Destiny.
	DefaultMaxApplySizeMB = 8

	// MinMaxApplySizeMB — нижняя граница валидации. Меньше 1 MiB не пропускаем:
	// даже скромная пачка RenderedTask с inline-content шаблонов укладывается в
	// единицы МиБ, а сабмегабайтный лимит превратил бы любой реальный Destiny в
	// fail-fast (Keeper) / отказ (Soul).
	MinMaxApplySizeMB = 1

	// bytesPerMiB — множитель МиБ→байты для grpc-call-опций.
	bytesPerMiB = 1024 * 1024
)

// KeeperConfig — типизированное представление `keeper.yml`.
//
// Нормативная спека блоков — [docs/keeper/config.md]. Каждый блок —
// отдельная struct ниже. Все поля типа `duration` хранятся как строки и
// валидируются в semantic-фазе (`time.ParseDuration`), enum — как строки
// со списком допустимых значений в schema-фазе.
//
// `reactor:` сознательно отсутствует — на данный момент имя не зафиксировано
// ([open Q №23](docs/architecture.md)), парсер обязан отвергать ключ через
// `unknown_key` в strict-режиме.
//
// `rbac:` тоже отсутствует: RBAC-каталог перенесён в Postgres (ADR-028(g)
// hard-cut), управление — через `role.*` API/MCP. Ключ `rbac:` в keeper.yml
// отвергается тем же `unknown_key` (поля в структуре нет → reflect-walker
// в `walk.go` поднимает диагностику).
//
// `services:` / `default_destiny_source:` / `default_module_source:` тоже
// отсутствуют: реестр Service-ов и well-known скаляры перенесены в Postgres
// (`service_registry` + `keeper_settings`, ADR-029 hard-cut), управление — через
// `service.*` API/MCP. Источник правды — БД; потребители (scenario service-
// registry / destiny-source) читают runtime-снимок `serviceregistry.Holder`.
// `default_module_source:` упразднён без замены (потребителя не было). Все три
// ключа отвергаются тем же `unknown_key`.
type KeeperConfig struct {
	KID string `yaml:"kid"`

	Listen   KeeperListen   `yaml:"listen"`
	Postgres KeeperPostgres `yaml:"postgres"`
	Redis    KeeperRedis    `yaml:"redis"`
	Vault    KeeperVault    `yaml:"vault"`
	Auth     *KeeperAuth    `yaml:"auth,omitempty"`
	OTel     *KeeperOTel    `yaml:"otel,omitempty"`
	Logging  KeeperLogging  `yaml:"logging"`

	Metrics *KeeperMetrics `yaml:"metrics,omitempty"`

	Plugins       *KeeperPlugins `yaml:"plugins,omitempty"`
	PluginRuntime *PluginRuntime `yaml:"plugin_runtime,omitempty"`
	Sigil         *KeeperSigil   `yaml:"sigil,omitempty"`
	Audit         *KeeperAudit   `yaml:"audit,omitempty"`

	// Push — pilot wire-up SshDispatcher (S6, 2026-05-26). Pilot-path: inline
	// `targets[]` + `providers[]` + single `host_ca_ref` в keeper.yml,
	// single-provider routing. Long-term canon (S7): миграция в souls.ssh_target
	// jsonb + PG-table push_providers + push.host_ca_refs[] — отдельный slice,
	// этот блок будет deprecated сразу после миграции. Опц.: при отсутствии
	// блока (или пустых targets/host_ca_ref) push-orchestrator не поднимается —
	// `/v1/push/*` и `keeper.push.apply` возвращают «не сконфигурировано».
	Push *KeeperPush `yaml:"push,omitempty"`

	// SigilAnchorsReloadInterval — период TTL-fallback-перечита набора
	// trust-anchor-ключей подписи Sigil (ADR-026(h), R3 known-gap). Канал
	// `sigil:anchors-changed` (Redis pub/sub) — best-effort: пропущенный сигнал
	// оставил бы отставшую ноду со старым набором якорей до рестарта (fail-open
	// при Retire). Периодический re-read (`reloadAnchors` по тикеру, образец
	// rbac.DefaultRefreshInterval / Summons poll-fallback) самоисцеляет
	// пропущенный сигнал за интервал. Тип `duration`, пустое/0 → дефолт
	// [DefaultSigilAnchorsReloadInterval] (30s). Валидация формата — semantic-фаза,
	// диапазон (>0) — резолвом дефолта в daemon (стиль `acolyte_*`).
	SigilAnchorsReloadInterval string        `yaml:"sigil_anchors_reload_interval,omitempty"`
	HotReload                  *HotReload    `yaml:"hot_reload,omitempty"`
	Reaper                     *KeeperReaper `yaml:"reaper,omitempty"`

	// CadenceScheduler — Conductor, leader-elected исполнитель Cadence-расписаний
	// (ADR-048). Свой tick-interval, независимый от reaper.interval (scheduling-
	// домен Cadence требует частого тика ~15–30s, cleanup-домен Reaper — редкого
	// ~1h). Default-ON при наличии Redis (footgun-guard ADR-048 §5: Cadence без
	// работающего планировщика молча не спавнит Voyage). Опц.: при отсутствии
	// блока Conductor поднимается с дефолтами, если настроен Redis.
	CadenceScheduler *KeeperCadenceScheduler `yaml:"cadence_scheduler,omitempty"`

	// Acolytes — число воркеров пула исполнения apply (ADR-027, Acolyte).
	// Feature-flag: 0 (default) — пул не поднимается, исполнение идёт прежним
	// run-goroutine-путём scenario-runner-а; >0 — пул активен. Cutover на пул
	// — отдельный slice (Phase 1.4). Валидация (>= 0) — schema-фаза.
	Acolytes int `yaml:"acolytes,omitempty"`

	// AcolyteLease — TTL Ward-захвата planned-задания (ADR-027(d):
	// claim_expires_at = NOW()+lease). Просроченный Ward переклеймит
	// recovery-скан (Phase 2). Тип `duration` (Go-`time.ParseDuration` либо
	// `<N>d`), пустое → дефолт [DefaultAcolyteLease] (30s). Валидация формата —
	// semantic-фаза, диапазон (>0) — после парсинга в daemon.
	AcolyteLease string `yaml:"acolyte_lease,omitempty"`

	// AcolyteBatch — максимум planned-заданий, захватываемых одним claim-тиком
	// (LIMIT claim-запроса, ADR-027(d)). Воркеры разных инстансов делят очередь
	// через FOR UPDATE SKIP LOCKED — батч лишь ограничивает аппетит одного тика.
	// 0/опущено → дефолт [DefaultAcolyteBatch] (10). Валидация (>= 0) —
	// schema-фаза.
	AcolyteBatch int `yaml:"acolyte_batch,omitempty"`

	// AcolytePollInterval — период poll-tick-а воркера: fallback к Summons-сигналу
	// (ADR-027(a)). Даже при потере pub/sub-сигнала задание подхватится на
	// ближайшем тике. Тип `duration`, пустое → дефолт [DefaultAcolytePollInterval]
	// (2s). Валидация формата — semantic-фаза, диапазон (>0) — после парсинга.
	AcolytePollInterval string `yaml:"acolyte_poll_interval,omitempty"`

	// AcolyteDrainGrace — окно graceful-drain пула Acolyte при остановке Keeper
	// (ADR-027 Phase 2): от сигнала «больше не claim-ить» до жёсткой отмены
	// claim-ctx у не успевших in-flight-воркеров. Прерванный claim оставляет
	// Ward в БД (claimed/running) — подберёт recovery-скан.
	// Тип `duration`, пустое → дефолт [DefaultAcolyteDrainGrace] (5s). Валидация
	// формата — semantic-фаза, диапазон (>0) — после парсинга в daemon.
	AcolyteDrainGrace string `yaml:"acolyte_drain_grace,omitempty"`

	// OracleCircuitMaxFires — порог circuit-breaker-а Oracle (ADR-030(a),
	// beacons S4): сколько срабатываний одного Decree за окно
	// [OracleCircuitWindow] допустимо до авто-disable (enabled=false). Глобальный
	// порог на все Decree; per-Decree override — отдельный заход.
	//
	// РАЗЛИЧЕНИЕ «пусто» vs «явный 0» требует указателя: семантика поля —
	// `nil` (поле опущено) → дефолт [DefaultOracleCircuitMaxFires] (5),
	// резолвится в daemon; явный `0` (escape-hatch) → breaker OFF (BumpCircuit
	// не вызывается, Decree никогда не авто-disable). Плоский `int` эти два
	// случая не различил бы (оба = 0), а ТЗ требует «пусто=дефолт, 0=off» —
	// поэтому `*int` (паттерн overlay-`*int`, как было у MaxAgeDays до уплощения,
	// но тут различение «0 vs не задано» сознательно СОХРАНЯЕТСЯ). Валидация
	// (>= 0 при заданном) — schema-фаза (nil-safe).
	OracleCircuitMaxFires *int `yaml:"oracle_circuit_max_fires,omitempty"`

	// OracleCircuitWindow — длина fixed-window circuit-breaker-а Oracle
	// (ADR-030(a)): окно, в котором считаются срабатывания Decree до сравнения с
	// [OracleCircuitMaxFires]. Тип `duration`, пустое → дефолт
	// [DefaultOracleCircuitWindow] (10m). Валидация формата — semantic-фаза,
	// диапазон (>0) — резолвом дефолта в daemon (стиль `acolyte_*`).
	OracleCircuitWindow string `yaml:"oracle_circuit_window,omitempty"`

	// WatchmanInterval — период probe-тика Watchman (изоляция-детект +
	// soul-shedding S2): как часто инстанс пингует PG+Redis (те же зависимости,
	// что `/readyz`) на предмет изоляции. Тип `duration`, пустое → дефолт
	// [DefaultWatchmanInterval] (5s). Валидация формата — semantic-фаза, диапазон
	// (>0) резолвится дефолтом в daemon (стиль `acolyte_*` / `oracle_circuit_window`).
	WatchmanInterval string `yaml:"watchman_interval,omitempty"`

	// WatchmanFailThreshold — число подряд идущих провалов probe Watchman до
	// объявления изоляции и активного закрытия (shedding) всех локальных
	// EventStream-стримов. Debounce/flap-guard: единичный сетевой spike не
	// сбрасывает весь флот стримов (thundering-herd reconnect по кластеру).
	// 0/опущено → дефолт [DefaultWatchmanFailThreshold] (3). Валидация (>= 0) —
	// schema-фаза (симметрично `acolyte_batch`).
	WatchmanFailThreshold int `yaml:"watchman_fail_threshold,omitempty"`

	// AllowUnsafeSinglePathMultiKeeper — явный opt-out из refuse-guard-а
	// soul-shedding (Finding-A, ADR-027(h)): при `acolytes == 0` И присутствии
	// ДРУГИХ живых Keeper-инстансов в Conclave (`CountLive > 1`) Keeper по
	// дефолту ОТКАЗЫВАЕТСЯ стартовать — run-goroutine-путь (`acolytes: 0`)
	// single-keeper-only, иначе apply на Keeper-A c Soul-ом на стриме Keeper-B
	// навсегда зависает в `applying` (см. инвариант HA в docs/keeper/config.md).
	//
	// `true` — осознанный выбор оператора (напр. намеренный single-keeper-за-LB
	// на время миграции / rolling-restart, где «другой» инстанс — уходящий):
	// refuse заменяется громким WARN, старт продолжается. Дефолт `false`
	// (безопасно: refuse). Дублируется env-флагом `KEEPER_ALLOW_UNSAFE_MULTI_KEEPER`
	// (truthy-OR, для контейнер/CI-окружений), резолв — в daemon.
	//
	// `bool` любого значения валиден — schema-проверки диапазона нет; поле лишь
	// должно присутствовать в структуре, чтобы strict-walker не отверг ключ как
	// `unknown_key`.
	AllowUnsafeSinglePathMultiKeeper bool `yaml:"allow_unsafe_single_path_multi_keeper,omitempty"`

	// Toll — cluster-wide detector массового оттока Soul-ов (ADR-038). При
	// nil/пустом блоке — поднимается с дефолтами (включён): per-instance
	// tollwatcher + Redis-leader агрегатор. Поле `enabled: false` — явный
	// opt-out (dev-сборки без Redis / отладка); все остальные поля имеют
	// дефолты из [DefaultToll*]-констант. Toll работает ТОЛЬКО при non-nil
	// Redis — в single-instance/dev без Redis он сам деградирует в no-op
	// (gate в daemon, не в config).
	Toll *KeeperToll `yaml:"toll,omitempty"`

	// Voyage — параметры VoyageWorker-пула (ADR-043): claim+execute
	// унифицированных батчевых прогонов (kind=scenario|command). Отдельный
	// worker-pool (НЕ общий с Acolyte).
	//
	// Config-gated OFF ПО УМОЛЧАНИЮ: при nil/опущенном блоке pool НЕ
	// поднимается; воркер стартует ТОЛЬКО при явном `voyage.workers: N > 0`.
	// Дефолты остальных полей — [DefaultVoyage*]-константы.
	Voyage *KeeperVoyage `yaml:"voyage,omitempty"`

	// Tempo — per-AID rate-limiter resolver-тяжёлых write-эндпоинтов (ADR-050).
	// При nil → дефолты + enabled=true (поднимается ПРИ наличии Redis; без Redis
	// limiter=nil → middleware passthrough, gate в daemon). Явный `enabled: false`
	// — opt-out (dev/отладка). Поля `voyage_create.{rate,burst}` имеют дефолты из
	// [DefaultTempo*]-констант; hot-reloadable (atomic swap, новый лимит со
	// следующего запроса, [ADR-021]).
	Tempo *KeeperTempo `yaml:"tempo,omitempty"`

	// Herald — параметры claim-queue worker-а доставки уведомлений (ADR-052(d),
	// S3). При nil → дефолты + поднимается ПРИ наличии Redis (доставка живёт в
	// Redis-очереди, hot→Redis); без Redis доставка деградирует (job-ы дропаются,
	// keeper не падает — fail-open). Workers по умолчанию [DefaultHeraldWorkers].
	Herald *KeeperHerald `yaml:"herald,omitempty"`

	// WebUIEnabled — тоггл встроенного UI на маршруте `/ui` (ADR-055). `*bool`,
	// чтобы различить «не задано» от явного `false`: nil/опущено → default-ON
	// (true) — бета хочет single-binary UI из коробки; явный `false` → opt-out
	// (статика `/ui` НЕ монтируется, API `/v1` не затрагивается). Симметрия
	// footgun-guard-у соседних подсистем (`tempo.enabled`/Toll default-ON), но
	// БЕЗ зависимости от инфраструктуры: UI вшит в бинарь (go:embed), внешнего
	// бэкенда не требует. Hot-reloadable (ADR-021) — re-mount роутера. Резолв —
	// [KeeperConfig.WebUIEnabled].
	WebUIEnabled *bool `yaml:"web_ui_enabled,omitempty"`

	// CloudInit — параметры рендера cloud-init userdata для VM, создаваемых
	// `core.cloud.provisioned` (ADR-017(h) amendment 2026-05-27, B-flat
	// закреплён). При nil — userdata-генерация не доступна: сценарий с
	// `generate_userdata: true` валится с явной ошибкой; явный `userdata` в
	// params продолжает работать без изменений.
	//
	// Все поля резолвятся в момент GenerateUserdata-вызова (не на старте
	// daemon), поэтому hot-reload `keeper.yml` подхватывается следующим
	// cloud-create-шагом без рестарта — через `config.Store.Get()`.
	//
	// Userdata НЕ несёт bootstrap-токены: per-VM-токен генерируется после
	// Create в `applyCreated` и попадает в register-output для доставки
	// отдельным шагом scenario (типично `keeper.push` через SSH-провайдер).
	// См. ADR-017(h) amendment и docs/keeper/cloud.md → «Cloud-init bootstrap (MVP)».
	CloudInit *KeeperCloudInit `yaml:"cloud_init,omitempty"`
}

// WebUIMounted возвращает эффективный тоггл встроенного UI (`/ui`, ADR-055):
// nil/опущено → true (default-ON, footgun-guard как Tempo/Toll); явный `false`
// → false (opt-out). Чистый резолв указателя; UI вшит в бинарь — внешнего
// бэкенда не требует (в отличие от Tempo/Toll, которым нужен Redis). Имя
// отлично от поля WebUIEnabled (Go не допускает метод и поле с одним именем).
func (c *KeeperConfig) WebUIMounted() bool {
	if c == nil || c.WebUIEnabled == nil {
		return true
	}
	return *c.WebUIEnabled
}

// Дефолты VoyageWorker-pool (ADR-043). Применяются в daemon при пустом полe
// (после того как блок voyage уже присутствует и workers > 0). Источник истины
// семантики — пакет voyageorch; здесь объявлены ради config-резолва без
// import-цикла shared→keeper. ВАЖНО: дефолта `workers` НЕТ — отсутствие блока
// означает «pool OFF», явный opt-in через `voyage.workers: N`.
const (
	// DefaultVoyageLeaseTTL — дефолт TTL PG-claim-lease для строки в `voyages`
	// (claim_expires_at = NOW() + lease_ttl). 60s parity ErrandRun.
	DefaultVoyageLeaseTTL = 60 * time.Second

	// DefaultVoyageLeaseRenewInterval — дефолт периода renewal-CAS-UPDATE-а
	// (~1/3 TTL). 20s parity ErrandRun.
	DefaultVoyageLeaseRenewInterval = 20 * time.Second

	// DefaultVoyagePollInterval — дефолт периода idle-poll claim-loop-а. 5s
	// parity ErrandRun.
	DefaultVoyagePollInterval = 5 * time.Second

	// DefaultVoyageMaxScope — дефолтный верхний лимит размера резолвнутого scope
	// одного Voyage (число единиц прогона: инкарнаций для kind=scenario, хостов
	// для kind=command). DoS-guard S-med-3: без потолка один POST может
	// резолвнуть весь флот (100k) → 100k per-row INSERT в одной транзакции +
	// неконтролируемый blast-radius. 10000 — рекомендация architect, принята как
	// дефолт; оператор переопределяет в keeper.yml::voyage.max_scope.
	DefaultVoyageMaxScope = 10000

	// DefaultVoyageMaxBatchSize — дефолтный верхний предел размера батча/окна
	// одного Voyage (ADR-043 amendment 2026-06-01, S-W4): batch_size для barrier,
	// concurrency для window. DoS-guard parity voyage.max_scope — без потолка
	// оператор может задать гигантскую пачку/окно и снять смысл скользящей выкатки
	// (весь scope одной волной). Совпадает с [DefaultVoyageMaxScope] (10000):
	// батч не может быть больше всего scope-потолка; оператор переопределяет в
	// keeper.yml::voyage.max_batch_size.
	DefaultVoyageMaxBatchSize = 10000
)

// KeeperVoyage — параметры VoyageWorker-pool (ADR-043, S1).
//
// Опциональный блок. ОТЛИЧИЕ от ErrandRun: при nil/опущенном блоке pool НЕ
// поднимается (config-gated OFF по умолчанию — S1-фундамент рядом со старыми
// путями). Pool стартует ТОЛЬКО при явном `voyage.workers: N > 0`. Остальные
// поля при наличии блока резолвятся к [DefaultVoyage*]-константам.
type KeeperVoyage struct {
	// Workers — число воркеров VoyageWorker-пула на инстанс. 0/опущено → pool
	// НЕ поднимается (даже если блок voyage задан). Явный `N > 0` — поднимается
	// N воркеров (config-gated OFF по умолчанию).
	Workers int `yaml:"workers,omitempty" json:"workers,omitempty"`

	// LeaseTTL — TTL PG-claim-lease для строки в `voyages`. Тип `duration`,
	// пустое → дефолт [DefaultVoyageLeaseTTL] (60s).
	LeaseTTL string `yaml:"lease_ttl,omitempty" json:"lease_ttl,omitempty"`

	// LeaseRenewInterval — период renewal-CAS-UPDATE-а текущего lease-а. 0 rows
	// affected → ErrLeaseLost, VoyageWorker бросает работу. Тип `duration`,
	// пустое → дефолт [DefaultVoyageLeaseRenewInterval] (20s = ~1/3 LeaseTTL).
	LeaseRenewInterval string `yaml:"lease_renew_interval,omitempty" json:"lease_renew_interval,omitempty"`

	// PollInterval — период idle-poll claim-loop-а (когда pending-Voyage-ов
	// нет). Тип `duration`, пустое → дефолт [DefaultVoyagePollInterval] (5s).
	PollInterval string `yaml:"poll_interval,omitempty" json:"poll_interval,omitempty"`

	// MaxScope — верхний лимит размера резолвнутого scope одного Voyage
	// (DoS-guard S-med-3). `*int`, чтобы различить «не задано» (→ дефолт
	// [DefaultVoyageMaxScope] = 10000) от явного 0 («безлимит» — для тестов /
	// обратной совместимости). Превышение лимита на резолве target-а →
	// 422 voyage_scope_too_large (handler-инвариант, не CHECK). В отличие от
	// прочих полей блока, MaxScope действует НЕЗАВИСИМО от Workers: cap живёт в
	// API-handler-е (POST /v1/voyages), а не в pool-е, поэтому защищает даже при
	// `workers: 0`.
	MaxScope *int `yaml:"max_scope,omitempty" json:"max_scope,omitempty"`

	// MaxBatchSize — верхний предел размера батча/окна одного Voyage (ADR-043
	// amendment 2026-06-01, S-W4): эффективный batch_size для barrier, concurrency
	// для window. `*int` — различить «не задано» (→ дефолт
	// [DefaultVoyageMaxBatchSize] = 10000) от явного 0 («без предела»).
	// Превышение → 422 voyage_batch_size_too_large (handler-инвариант, parity
	// voyage_scope_too_large). Симметрично MaxScope, действует НЕЗАВИСИМО от
	// Workers (cap живёт в API-handler-е).
	MaxBatchSize *int `yaml:"max_batch_size,omitempty" json:"max_batch_size,omitempty"`
}

// ResolvedMaxScope возвращает эффективный потолок размера scope: не задано
// (nil блок / nil поле) → [DefaultVoyageMaxScope] (10000); явный 0 → 0
// (безлимит, для тестов / обратной совместимости); явное значение → оно само.
func (v *KeeperVoyage) ResolvedMaxScope() int {
	if v == nil || v.MaxScope == nil {
		return DefaultVoyageMaxScope
	}
	return *v.MaxScope
}

// ResolvedMaxBatchSize возвращает эффективный потолок размера батча/окна: не
// задано (nil блок / nil поле) → [DefaultVoyageMaxBatchSize] (10000); явный 0 →
// 0 (без предела); явное значение → оно само. Parity [ResolvedMaxScope].
func (v *KeeperVoyage) ResolvedMaxBatchSize() int {
	if v == nil || v.MaxBatchSize == nil {
		return DefaultVoyageMaxBatchSize
	}
	return *v.MaxBatchSize
}

// Дефолты Herald claim-queue worker-а доставки (ADR-052(d), S3).
const (
	// DefaultHeraldWorkers — число worker-горутин доставки на инстанс при наличии
	// Redis. Конкурентные клеймы безопасны (at-least-once). 2 — умеренный
	// параллелизм без шторма: уведомлений мало относительно прогонов.
	DefaultHeraldWorkers = 2

	// DefaultHeraldDeliveryTimeout — дефолтный общий таймаут одного webhook-POST-а
	// (dial+TLS+POST+чтение ответа). Зеркало [herald.DefaultDeliveryTimeout] (10s)
	// — держим строкой здесь, чтобы daemon резолвил единообразно с прочими
	// duration-полями (ParseDuration).
	DefaultHeraldDeliveryTimeout = "10s"
)

// KeeperHerald — параметры claim-queue worker-а доставки уведомлений (ADR-052(d),
// S3). Опциональный блок: при nil поля дефолтятся, worker поднимается ПРИ
// наличии Redis. `workers: 0` явно выключает доставку (job-ы накапливаются в
// Redis-очереди, но не доставляются).
type KeeperHerald struct {
	// Workers — число worker-горутин доставки на инстанс. nil/опущено →
	// [DefaultHeraldWorkers] (2). Явный 0 → доставка выключена.
	Workers *int `yaml:"workers,omitempty" json:"workers,omitempty"`

	// DeliveryTimeout — общий таймаут одного webhook-POST-а. Тип `duration`,
	// пустое/некорректное → [DefaultHeraldDeliveryTimeout] (10s).
	DeliveryTimeout string `yaml:"delivery_timeout,omitempty" json:"delivery_timeout,omitempty"`
}

// ResolvedWorkers возвращает эффективное число worker-ов: nil блок / nil поле →
// [DefaultHeraldWorkers]; явное значение (вкл. 0) → оно само.
func (h *KeeperHerald) ResolvedWorkers() int {
	if h == nil || h.Workers == nil {
		return DefaultHeraldWorkers
	}
	return *h.Workers
}

// Дефолты Tempo per-AID rate-limiter-а (ADR-050(e)). Применяются при опущенном/
// нулевом поле в момент чтения config-snapshot (middleware читает живой
// config.Store, hot-reload). Подобраны как «человеку/нормальному автоматону
// хватает с запасом, цикл-abuse режется».
const (
	// DefaultTempoVoyageCreateRate — refill-скорость бакета voyage-create,
	// токенов в секунду (rps).
	DefaultTempoVoyageCreateRate = 10.0

	// DefaultTempoVoyageCreateBurst — глубина бакета voyage-create.
	DefaultTempoVoyageCreateBurst = 20

	// DefaultTempoVoyagePreviewRate — refill-скорость бакета voyage-preview,
	// токенов в секунду (rps). Мягче create (preview read-like по эффекту —
	// без persist/audit), но НЕ безлимит: dry-resolve scope так же resolver-
	// тяжёл (Purview-резолв + page-CEL по флоту), поэтому цикл-abuse режется
	// отдельным, более широким бакетом (ADR-050 amendment 2026-06-17).
	DefaultTempoVoyagePreviewRate = 30.0

	// DefaultTempoVoyagePreviewBurst — глубина бакета voyage-preview.
	DefaultTempoVoyagePreviewBurst = 60
)

// KeeperTempo — Tempo per-AID rate-limiter (ADR-050).
//
// Опциональный блок. При nil → дефолты + enabled=true (default-ON, footgun-guard
// как Conductor/Toll). Явный `enabled: false` — opt-out. Поднимается фактически
// ТОЛЬКО при наличии Redis-клиента (limiter живёт в Redis); без Redis —
// middleware passthrough (gate в daemon, не в config). Поля
// `voyage_create.{rate,burst}` / `voyage_preview.{rate,burst}` при опущении
// резолвятся к [DefaultTempo*].
type KeeperTempo struct {
	// Enabled — флаг включения Tempo. nil/опущено → true (default-ON); явный
	// `false` — оператор выключает (dev/отладка). `*bool`, чтобы различить «не
	// задано» (→ default-on) от явного `false` (паттерн KeeperToll.Enabled).
	Enabled *bool `yaml:"enabled,omitempty"`

	// VoyageCreate — rate/burst bucket-а `POST /v1/voyages` (create). Опущенные
	// поля резолвятся к [DefaultTempoVoyageCreate*] в [ResolvedVoyageCreate].
	VoyageCreate KeeperTempoBucket `yaml:"voyage_create,omitempty"`

	// VoyagePreview — rate/burst bucket-а `POST /v1/voyages/preview` (dry-resolve
	// scope). ОТДЕЛЬНЫЙ bucket от voyage_create (ADR-050 amendment 2026-06-17):
	// preview read-like по эффекту, но resolver-heavy по стоимости → собственный,
	// более мягкий лимит, чтобы preview и create не делили квоту. Опущенные поля
	// резолвятся к [DefaultTempoVoyagePreview*] в [ResolvedVoyagePreview].
	VoyagePreview KeeperTempoBucket `yaml:"voyage_preview,omitempty"`
}

// KeeperTempoBucket — rate/burst одного логического Tempo-bucket-а.
//
// Rate — refill-скорость (токенов в секунду, rps). Burst — глубина бакета
// (capacity). Оба должны быть > 0 при заданном блоке (валидация в schema-фазе);
// 0/опущено резолвится к дефолту в [KeeperTempo.ResolvedVoyageCreate].
type KeeperTempoBucket struct {
	Rate  float64 `yaml:"rate,omitempty"`
	Burst int     `yaml:"burst,omitempty"`
}

// TempoEnabled возвращает эффективный флаг включения Tempo: nil-блок / nil-поле
// → true (default-ON, footgun-guard). Фактический подъём дополнительно требует
// Redis (gate в daemon).
func (t *KeeperTempo) TempoEnabled() bool {
	if t == nil || t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// ResolvedVoyageCreate возвращает эффективные rate/burst bucket-а voyage-create:
// опущенные/нулевые поля → дефолты [DefaultTempoVoyageCreate*]. Читается
// middleware на каждом запросе (hot-reload), поэтому дешёвый чистый резолв без
// аллокаций.
func (t *KeeperTempo) ResolvedVoyageCreate() (rate float64, burst int) {
	rate, burst = DefaultTempoVoyageCreateRate, DefaultTempoVoyageCreateBurst
	if t == nil {
		return rate, burst
	}
	if t.VoyageCreate.Rate > 0 {
		rate = t.VoyageCreate.Rate
	}
	if t.VoyageCreate.Burst > 0 {
		burst = t.VoyageCreate.Burst
	}
	return rate, burst
}

// ResolvedVoyagePreview возвращает эффективные rate/burst bucket-а voyage-preview:
// опущенные/нулевые поля → дефолты [DefaultTempoVoyagePreview*]. Отдельный от
// voyage-create bucket (ADR-050 amendment 2026-06-17). Читается middleware на
// каждом запросе (hot-reload), поэтому дешёвый чистый резолв без аллокаций.
func (t *KeeperTempo) ResolvedVoyagePreview() (rate float64, burst int) {
	rate, burst = DefaultTempoVoyagePreviewRate, DefaultTempoVoyagePreviewBurst
	if t == nil {
		return rate, burst
	}
	if t.VoyagePreview.Rate > 0 {
		rate = t.VoyagePreview.Rate
	}
	if t.VoyagePreview.Burst > 0 {
		burst = t.VoyagePreview.Burst
	}
	return rate, burst
}

// Дефолты параметров Acolyte-пула (ADR-027). Совпадают с прежними хардкод-
// значениями (main-константы / acolyte.defaultPollInterval), чтобы пустой
// конфиг не менял поведение. Применяются в daemon при пустом/нулевом поле.
const (
	// DefaultAcolyteLease — дефолт TTL Ward-захвата (claim_expires_at).
	// Умеренное окно: достаточно для render→MarkDispatched→SendApply, и
	// достаточно короткое, чтобы recovery-скан быстро вернул задание мёртвого
	// владельца в очередь.
	DefaultAcolyteLease = 30 * time.Second

	// DefaultAcolyteBatch — дефолт размера пачки одного claim-тика.
	DefaultAcolyteBatch = 10

	// DefaultAcolytePollInterval — дефолт периода poll-fallback-а воркера.
	// Достаточно частый для failover в единицы секунд, достаточно редкий, чтобы
	// не флудить PG пустыми claim-запросами на простаивающем кластере.
	DefaultAcolytePollInterval = 2 * time.Second

	// DefaultAcolyteDrainGrace — дефолт окна graceful-drain пула Acolyte при
	// остановке (ADR-027 Phase 2). Достаточно, чтобы уже начатый claim
	// (render → MarkDispatched → SendApply) добежал на здоровом PG/Soul, и не
	// настолько большое, чтобы тормозить SIGTERM-выход на зависшем in-flight
	// (его Ward переживёт рестарт — lease истечёт, подберёт recovery-скан).
	DefaultAcolyteDrainGrace = 5 * time.Second
)

// Дефолты circuit-breaker-а Oracle (ADR-030(a), beacons S4). Применяются в
// daemon при пустом (опущенном) поле; явный 0 в max_fires — НЕ дефолт, а
// escape-hatch «breaker OFF» (см. [KeeperConfig.OracleCircuitMaxFires]).
const (
	// DefaultOracleCircuitMaxFires — дефолт порога авто-disable Decree-а.
	// 5 срабатываний за окно: правило с idempotent-action + cooldown в норме не
	// доходит до 5 повторов; устойчивый выход на порог = правило сорвалось в
	// петлю и его пора глушить.
	DefaultOracleCircuitMaxFires = 5

	// DefaultOracleCircuitWindow — дефолт длины fixed-window. 10m достаточно
	// широкое, чтобы серия повторов «правило в петле» уложилась в одно окно
	// (cooldown между ними обычно минуты), и достаточно узкое, чтобы редкие
	// легитимные срабатывания за день не накапливались до ложного trip-а.
	DefaultOracleCircuitWindow = 10 * time.Minute
)

// Дефолты Watchman (изоляция-детект + soul-shedding S2). Применяются в daemon
// при пустом/нулевом поле. Совпадают с пакетными watchman.Default*-константами
// (источник истины дефолтов — пакет watchman, здесь дублируются для config-
// резолва без import-цикла shared→keeper).
const (
	// DefaultWatchmanInterval — дефолт периода probe-тика Watchman. 5s: баланс
	// между скоростью реакции на изоляцию (≈ interval × fail_threshold до
	// shedding-а) и нагрузкой ping-ов на PG/Redis.
	DefaultWatchmanInterval = 5 * time.Second

	// DefaultWatchmanFailThreshold — дефолт числа подряд идущих провалов probe до
	// shedding-а. 3: debounce от единичных spike-ов (один-два тика переживаются),
	// устойчивая потеря (>=3) триггерит.
	DefaultWatchmanFailThreshold = 3
)

// Дефолты Toll (cluster-wide detector массового оттока, ADR-038). Применяются
// в daemon при пустом/нулевом поле. Источник истины — пакет toll, здесь
// дублируются для config-резолва без import-цикла shared→keeper.
const (
	// DefaultTollThreshold — доля от baseline `souls.status='connected'`, при
	// превышении которой за окно Toll взводит cluster:degraded. 0.20 = 20%
	// (умеренный порог: не реагирует на единичные отключения малого
	// кластера, но ловит DC outage / massive split).
	DefaultTollThreshold = 0.20

	// DefaultTollWindow — длина sliding-окна Toll. 60s — баланс между
	// скоростью реакции на массовый отток и устойчивостью к burst-ам
	// (rolling restart одной партии Souls укладывается в окно).
	DefaultTollWindow = 60 * time.Second

	// DefaultTollDegradedTTL — TTL Redis-ключа `cluster:degraded`. Равен
	// длине окна: если leader умер и не успел продлить, флаг гаснет сам,
	// блокировка снимается.
	DefaultTollDegradedTTL = 60 * time.Second

	// DefaultTollClearGrace — устойчивое окно низкого rate-а до clearing
	// (asymmetric hysteresis по ADR-038): сработать на первом превышении,
	// снять только после grace под threshold-ом — защита от флапов.
	DefaultTollClearGrace = 60 * time.Second

	// DefaultTollLeaseTTL — TTL Redis-lease `cluster:toll:leader`. 30s
	// (симметрично Reaper-овскому): leader продлевает каждые TTL/3, при
	// crash-е следующий кандидат подхватит через ≤ TTL.
	DefaultTollLeaseTTL = 30 * time.Second

	// DefaultTollWarmup — окно immunity после старта инстанса. Disconnect-ы
	// первые 60s после старта tollwatcher-а считаются (метрика растёт), но
	// в Redis sorted-set НЕ публикуются (cluster restart false-positive
	// defense — все Souls reconnect-ят разом, это не отток).
	DefaultTollWarmup = 60 * time.Second

	// DefaultTollWebhookTimeout — потолок одного POST-вызова webhook-канала
	// (ADR-038 amendment 2026-05-27, extensions). 10s достаточно для PagerDuty
	// Events API / Slack incoming webhook (типичная latency — сотни мс),
	// достаточно коротко, чтобы повисший remote не задержал leader-tick.
	// Webhook — best-effort: тайм-аут в alert-канал не блокирует Set/Clear.
	DefaultTollWebhookTimeout = 10 * time.Second
)

// Допустимые форматы webhook-канала (ADR-038 amendment, extensions).
const (
	// TollWebhookFormatGeneric — generic JSON-POST с плоским payload-ом
	// `{event_type, leader_kid, rate, baseline_connected, threshold,
	// window_seconds, timestamp, coven_name?}`. Подходит под произвольный
	// HTTP-receiver (включая self-hosted alertmanager-relays).
	TollWebhookFormatGeneric = "generic"

	// TollWebhookFormatPagerDutyV2 — PagerDuty Events API v2 schema:
	// `{routing_key, event_action, dedup_key, payload:{summary, source,
	// severity, custom_details}}`. URL обязан быть
	// `https://events.pagerduty.com/v2/enqueue` (или integration-equivalent),
	// `routing_key` — integration-key из Vault KV (под полем `routing_key`).
	TollWebhookFormatPagerDutyV2 = "pagerduty_v2"

	// TollWebhookFormatSlack — Slack incoming webhook schema:
	// `{text, attachments:[{color, fields:[...]}]}`. URL — slack-issued
	// webhook (`https://hooks.slack.com/services/...`). Auth — в самом URL,
	// отдельных headers нет.
	TollWebhookFormatSlack = "slack"
)

// KeeperToll — Toll cluster-wide detector (ADR-038).
//
// Опциональный блок. При nil → дефолты + enabled=true (поднимается); явный
// `enabled: false` → выключается полностью. Все остальные поля имеют дефолты
// из [DefaultToll*]-констант; их можно тюнить без переписывания всего блока
// (опущенные поля резолвятся к дефолтам в daemon).
type KeeperToll struct {
	// Enabled — флаг включения Toll. nil/опущено → true (включено по дефолту);
	// явный `false` — оператор выключает (dev-режим / отладка). `*bool`,
	// чтобы различить «не задано» (→ default-on) от явного `false`.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Threshold — доля disconnect_rate / baseline_connected, при превышении
	// которой Toll-leader взводит cluster:degraded. 0/опущено → дефолт
	// [DefaultTollThreshold] (0.20). Допустимый диапазон (0, 1] — порог
	// «больше 100% от baseline» бессмыслен. Schema-проверка диапазона.
	Threshold float64 `yaml:"threshold,omitempty"`

	// WindowSize — длина sliding-окна (per-second buckets в Redis sorted-set).
	// Тип `duration`, пустое → [DefaultTollWindow] (60s). Формат — semantic-фаза.
	WindowSize string `yaml:"window_size,omitempty"`

	// DegradedTTL — TTL Redis-ключа `cluster:degraded`. Тип `duration`, пустое
	// → [DefaultTollDegradedTTL] (60s). Если leader умер и не продлил — флаг
	// сам гаснет.
	DegradedTTL string `yaml:"degraded_ttl,omitempty"`

	// ClearGrace — устойчивое окно низкого rate до clearing (asymmetric
	// hysteresis). Тип `duration`, пустое → [DefaultTollClearGrace] (60s).
	ClearGrace string `yaml:"clear_grace,omitempty"`

	// LeaseTTL — TTL Redis-lease `cluster:toll:leader`. Тип `duration`,
	// пустое → [DefaultTollLeaseTTL] (30s). Renew каждые TTL/3.
	LeaseTTL string `yaml:"lease_ttl,omitempty"`

	// WarmupDelay — окно immunity после старта инстанса (cluster restart
	// false-positive defense). Тип `duration`, пустое → [DefaultTollWarmup]
	// (60s).
	WarmupDelay string `yaml:"warmup_delay,omitempty"`

	// Webhook — опц. alert-канал на cluster.degraded_set / cluster.degraded_cleared
	// (ADR-038 amendment 2026-05-27, extensions). Поддерживает generic JSON-POST
	// + специфичные форматы PagerDuty Events API v2 / Slack incoming webhook.
	// При nil или `enabled: false` notifier не поднимается; degraded set/clear
	// идёт как было (audit + gauge + metrics) без alert-out-а. Webhook — best-
	// effort: ошибка POST-а логируется, но не блокирует Set/Clear (cluster
	// degraded — primary goal, webhook — secondary).
	Webhook *KeeperTollWebhook `yaml:"webhook,omitempty" json:"webhook,omitempty"`

	// PerCovenThresholds — опц. per-coven threshold-override (ADR-038 amendment
	// 2026-05-27, extensions). Если задан и непустой, leader дополнительно
	// считает disconnect_rate per-coven (ZRANGEBYSCORE → split member-value по
	// `|`) и взводит cluster:degraded если ЛИБО global threshold превышен ЛИБО
	// per-coven threshold по конкретной coven. В audit-payload trigger-а
	// сохраняется `coven_name` (если триггер per-coven). Ключ map — имя coven,
	// значение — порог (0, 1].
	//
	// Cardinality-риск ADR-038(п.5) митигирован тем, что подписка на per-coven
	// thresholds — явное opt-in решение оператора: список ключей конечен и
	// контролируется в keeper.yml. Per-coven counter в Prometheus
	// (keeper_toll_disconnects_total{coven}) уже несёт ту же cardinality.
	PerCovenThresholds map[string]float64 `yaml:"per_coven_thresholds,omitempty" json:"per_coven_thresholds,omitempty"`
}

// KeeperTollWebhook — webhook alert-канал Toll (ADR-038 amendment, extensions).
//
// При `enabled: false` notifier не поднимается (default-off — добавление поля
// не меняет поведение существующих конфигов). При `enabled: true` обязательны
// `url_ref` (vault-ref на secret-URL ИЛИ inline plain URL — выбор оператора) +
// `format` (валидируется schema-фазой по closed-enum
// [TollWebhookFormatGeneric] / [TollWebhookFormatPagerDutyV2] /
// [TollWebhookFormatSlack]).
//
// БЕЗОПАСНОСТЬ: integration-keys / Slack-webhook-URL — секреты (раскрывают
// pager). Рекомендуется `url_ref: vault:secret/keeper/toll-webhook-url` (поле
// в Vault KV — `url`). Inline URL допустим для совместимости с локальными
// receiver-ами без Vault, но для prod-формы — vault-ref.
type KeeperTollWebhook struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	URLRef  string `yaml:"url_ref" json:"url_ref"`
	Format  string `yaml:"format,omitempty" json:"format,omitempty"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// KeeperListen — четыре listener-а Keeper-а.
type KeeperListen struct {
	GRPC    KeeperListenGRPC   `yaml:"grpc"`
	OpenAPI KeeperListenSimple `yaml:"openapi"`
	MCP     KeeperListenSimple `yaml:"mcp"`
	Metrics KeeperListenSimple `yaml:"metrics"`
}

// KeeperListenGRPC — два sub-listener-а Keeper-а под ADR-012(b):
// `bootstrap` (server-only TLS, отдельный listener для Bootstrap-RPC,
// у Soul-а ещё нет SoulSeed-сертификата) и `event_stream` (mTLS,
// долгоживущий bidi-стрим после онбординга).
//
// TLS-параметры независимы по архитектуре, но грамматика допускает
// одинаковые пути для cert/key — один и тот же серверный сертификат
// можно использовать на обоих listener-ах.
type KeeperListenGRPC struct {
	Bootstrap   KeeperListenGRPCBootstrap   `yaml:"bootstrap"`
	EventStream KeeperListenGRPCEventStream `yaml:"event_stream"`
}

// KeeperListenGRPCBootstrap — Bootstrap-listener.
//
// CA сюда НЕ кладётся: Bootstrap по ADR-012(b) — server-only TLS, у
// Soul-а до онбординга нет клиентского сертификата. Появление ключа
// `listen.grpc.bootstrap.tls.ca` в YAML отвергается schema-фазой через
// `unknown_key`.
type KeeperListenGRPCBootstrap struct {
	Addr string                       `yaml:"addr"`
	TLS  KeeperListenGRPCBootstrapTLS `yaml:"tls"`
}

type KeeperListenGRPCBootstrapTLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// KeeperListenGRPCEventStream — EventStream-listener (mTLS).
//
// MaxApplySizeMB — потолок размера одного исходящего FromKeeper-сообщения,
// прежде всего `ApplyRequest` с пачкой отрендеренных `RenderedTask` (рендер
// Destiny — Keeper-side, ADR-012). Применяется как `grpc.MaxSendMsgSize` на
// EventStream-сервере: при попытке отправить больше Keeper падает fail-fast с
// понятной ошибкой вместо глухого отказа на стороне Soul. 0/опущено → дефолт
// [DefaultMaxApplySizeMB] (8 MiB). Это поле — НЕ recv-лимит входящих FromSoul
// (тот — внутренний инвариант `eventStreamMaxRecvMsgSize`, 1 MiB, конфигом не
// управляется).
//
// Инвариант контракта Keeper↔Soul: этот send-лимит должен быть ≤ Soul-recv-
// лимиту (`keeper.max_apply_size_mb` в `soul.yml`), иначе Keeper отправит то,
// что Soul отвергнет. Дефолты обеих сторон совпадают (8 MiB).
type KeeperListenGRPCEventStream struct {
	Addr           string                         `yaml:"addr"`
	TLS            KeeperListenGRPCEventStreamTLS `yaml:"tls"`
	MaxApplySizeMB int                            `yaml:"max_apply_size_mb,omitempty"`
}

// ResolvedMaxApplySize возвращает эффективный send-лимит EventStream-сервера в
// байтах: 0/опущено → [DefaultMaxApplySizeMB]. Валидация (>0, ≥ минимума) —
// в schema-фазе; здесь только резолв дефолта.
func (e KeeperListenGRPCEventStream) ResolvedMaxApplySize() int {
	mb := e.MaxApplySizeMB
	if mb <= 0 {
		mb = DefaultMaxApplySizeMB
	}
	return mb * bytesPerMiB
}

type KeeperListenGRPCEventStreamTLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
	// CA — корневой сертификат, которым валидируются SoulSeed входящих Souls.
	CA string `yaml:"ca"`
}

type KeeperListenSimple struct {
	Addr string `yaml:"addr"`
}

// KeeperPostgres — холодное хранилище.
type KeeperPostgres struct {
	DSNRef string             `yaml:"dsn_ref"`
	Pool   KeeperPostgresPool `yaml:"pool"`
}

type KeeperPostgresPool struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// KeeperRedis — горячий слой и координация.
//
// `mode` выбирает топологию Redis (ADR-006 amendment):
//   - `standalone` (default, пусто/опущено = standalone — forward-compat для
//     старых конфигов) — один узел, адрес в `addr`.
//   - `sentinel` — Redis Sentinel HA: клиент находит master через sentinel-узлы.
//     Требует `master_name` + `sentinels` (адреса sentinel-узлов host:port);
//     `addr` в этом режиме не используется. Пароль самих sentinel-узлов —
//     `sentinel_password_ref` (опц.), пароль Redis — `password_ref`.
//   - `cluster` — Redis Cluster: шардирование по слотам. Требует `nodes`
//     (адреса узлов кластера host:port для bootstrap-discovery); `addr` не
//     используется. `sentinel_*` к cluster-режиму не относятся.
//
// `password_ref` / `sentinel_password_ref` — vault-ref формы
// `vault:<mount>/<path>[#field]` (или plaintext в тестах); резолв — на
// `keeper/internal/redis.NewClient` через keeper-vault-клиент.
type KeeperRedis struct {
	Mode                string   `yaml:"mode,omitempty"`
	Addr                string   `yaml:"addr"`
	PasswordRef         string   `yaml:"password_ref"`
	MasterName          string   `yaml:"master_name,omitempty"`
	Sentinels           []string `yaml:"sentinels,omitempty"`
	Nodes               []string `yaml:"nodes,omitempty"`
	SentinelPasswordRef string   `yaml:"sentinel_password_ref,omitempty"`
}

// KeeperVault — обязательная зависимость Keeper-а.
//
// `auth.method` выбирает способ аутентификации Keeper-а в Vault:
//   - `token` (default, dev-shortcut) — статический токен из поля `token`;
//     `dev/docker-compose.yml` поднимает Vault в dev-режиме с root-токеном.
//   - `approle` — прод-путь (ADR-014): Keeper делает `auth/approle/login`
//     c `role_id` + `secret_id` и получает renewable client-token, который
//     дальше продлевает TokenRenewer (renewer.go).
//
// Forward-compat: keeper.yml без блока `auth` (или с пустым `auth.method`)
// трактуется как `method=token` — старые конфиги работают без правок.
//
// `kv_mount` — mount point для Vault KV v1/v2 secrets engine, default "secret"
// (версия определяется автоматически через `sys/internal/ui/mounts`; override —
// `vault.kv_version`).
//
// `kv_version` — опциональный escape-hatch: пусто/опущено → версия KV mount-а
// резолвится probe-ом через `sys/internal/ui/mounts/<mount>`; заданное `"1"`/`"2"`
// форсирует версию без probe (нужно, когда ACL закрывает probe-endpoint).
//
// `pki_mount` / `pki_role` — mount + role PKI engine, через который Keeper
// подписывает CSR Soul-ов при онбординге (`Bootstrap`-RPC, ADR-012(b)).
// Default `pki_role` пустой; semantic-фаза не валидирует ROLE — Vault
// сам отвергает запрос на несуществующий role.
type KeeperVault struct {
	Addr      string          `yaml:"addr"`
	Token     string          `yaml:"token,omitempty"`
	KVMount   string          `yaml:"kv_mount,omitempty"`
	KVVersion string          `yaml:"kv_version,omitempty"`
	Auth      KeeperVaultAuth `yaml:"auth"`
	PKIMount  string          `yaml:"pki_mount"`
	PKIRole   string          `yaml:"pki_role,omitempty"`

	// InputDenyPaths — опц. расширение hard deny-list для scoped-резолва
	// `vault:`-ref в operator-input (docs/input.md → «vault_scope», форк C).
	// Logical-path-префиксы (`<mount>/<prefix>`), которые НИКОГДА не резолвятся
	// через input-ref, даже если поле объявило покрывающий их `vault_scope`.
	// Дополняет system-floor [config.VaultInputFloor] (`secret/keeper/*`,
	// `secret/internal/*`); сам system-floor конфигом НЕ выключается, только
	// расширяется. Авторских `vault:`-refs в task params это НЕ касается.
	InputDenyPaths []string `yaml:"input_deny_paths,omitempty"`
}

// KeeperVaultAuth — выбор и параметры auth-метода Vault.
//
// AppRole-credentials НЕ читаются из Vault (`vault:`-ref) — это chicken-egg:
// именно этими credentials Keeper и логинится, чтобы потом резолвить любые
// vault-ref-ы (postgres.dsn_ref, signing_key_ref, …). Поэтому источник —
// локальный, до подъёма Vault-клиента:
//
//   - `role_id` — НЕ секрет (идентификатор роли), допустимо в открытом виде
//     прямо в keeper.yml.
//   - `secret_id` — секрет, plaintext в основном конфиге НЕ хранится. Источник
//     задаётся одним из (приоритет сверху вниз):
//   - `secret_id_file` — путь к mode-ограниченному файлу (рекомендуется
//     0400/0600), содержимое = secret_id (trailing newline снимается);
//   - `secret_id_env` — имя env-переменной с secret_id (dev/CI/инжектор
//     секретов вроде Vault Agent / k8s-secret-as-env).
//
// При `method=token` все approle-поля игнорируются.
type KeeperVaultAuth struct {
	Method       string `yaml:"method,omitempty"`
	RoleID       string `yaml:"role_id,omitempty"`
	SecretIDFile string `yaml:"secret_id_file,omitempty"`
	SecretIDEnv  string `yaml:"secret_id_env,omitempty"`
}

// AuthMethodToken / AuthMethodAppRole — допустимые значения `vault.auth.method`.
// Пустой method эквивалентен token (forward-compat).
const (
	AuthMethodToken   = "token"
	AuthMethodAppRole = "approle"
)

// ResolvedAuthMethod возвращает эффективный auth-метод: пустой → token.
func (a KeeperVaultAuth) ResolvedAuthMethod() string {
	if a.Method == "" {
		return AuthMethodToken
	}
	return a.Method
}

// KeeperAuth — JWT-аутентификация операторов (ADR-014).
// У Soul блока `auth:` нет — Soul аутентифицируется через mTLS / SoulSeed.
type KeeperAuth struct {
	JWT *KeeperAuthJWT `yaml:"jwt,omitempty"`
}

type KeeperAuthJWT struct {
	SigningKeyRef string `yaml:"signing_key_ref,omitempty"`
	Issuer        string `yaml:"issuer,omitempty"`
	TTLDefault    string `yaml:"ttl_default,omitempty"`
	TTLBootstrap  string `yaml:"ttl_bootstrap,omitempty"`
}

// KeeperSigil — подпись допусков плагинов (ADR-026, печать доверия Sigil).
//
// Optional-блок: если не задан (или signing_key_ref пуст), подпись недоступна —
// keeper стартует нормально, но allow-операция (slice S4) вернёт ошибку «sigil
// key not configured». Загрузка ключа — nil-safe.
//
// SigningKeyRef — config-путь к Vault KV с ed25519-приватником подписи
// (`vault:<mount>/<path>`, поле `signing_key`). Симметрично
// auth.jwt.signing_key_ref (ADR-014), но ключ АСИММЕТРИЧНЫЙ (ed25519): приватник
// подписывает на Keeper, публичная часть едет Soul-у в bootstrap как trust-anchor.
type KeeperSigil struct {
	SigningKeyRef string `yaml:"signing_key_ref,omitempty"`
}

// DefaultSigilAnchorsReloadInterval — дефолт TTL-fallback-перечита набора
// trust-anchor-ключей подписи Sigil (ADR-026(h), R3 known-gap). 30s — то же
// окно, что у `rbac.DefaultRefreshInterval`-семейства (мутации ключей редки,
// окно устаревания мало), достаточно короткое, чтобы пропущенный
// `sigil:anchors-changed` самоисцелился до прод-ротации, и достаточно редкое,
// чтобы не дёргать Vault/PG re-build-ом Signer-а на простаивающем кластере.
const DefaultSigilAnchorsReloadInterval = 30 * time.Second

// KeeperMetrics — дополнительные настройки `/metrics`-эндпоинта Keeper-а.
// Сам bind-адрес — `listen.metrics.addr` (выделенный listener, не openapi-
// роутер); этот блок несёт только опц. защиту эндпоинта (ADR-024).
//
// У Soul симметричной auth нет: у Soul-агента нет vault-клиента (ADR-012),
// которым можно зарезолвить password_ref; Soul-метрики защищены loopback-ом
// (`metrics.listen` = 127.0.0.1). Auth для Soul — отдельная будущая задача.
type KeeperMetrics struct {
	Auth *KeeperMetricsAuth `yaml:"auth,omitempty"`
}

type KeeperMetricsAuth struct {
	Basic *KeeperMetricsBasicAuth `yaml:"basic,omitempty"`
}

// KeeperMetricsBasicAuth — HTTP Basic-auth на `/metrics`.
//
// PasswordRef — vault-ref (`vault:<mount>/<path>`), резолвится тем же
// keeper-vault-клиентом, что читает JWT signing-key (поле `password` из KV).
// Plaintext-пароль в конфиге не допускается («безопасность на первом месте»):
// только vault-ref. При Enabled оба — Username и PasswordRef — обязательны
// (валидация в schema-фазе).
type KeeperMetricsBasicAuth struct {
	Enabled     bool   `yaml:"enabled"`
	Username    string `yaml:"username,omitempty"`
	PasswordRef string `yaml:"password_ref,omitempty"`
}

// KeeperOTel — общая для Keeper и Soul форма (отдельные структуры, чтобы
// при будущем дрейфе enum-ов не пересекались).
type KeeperOTel struct {
	Enabled  bool   `yaml:"enabled"`
	Exporter string `yaml:"exporter,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`

	// ExportMetrics — опц. push метрик по OTLP в дополнение к
	// Prometheus-scrape (ADR-024 §1.2 / observability.md §5). Заглушка
	// под Slice 2: читается из конфига, но OTLP-метрик-pipeline ещё не
	// поднимается (в Slice 0 экспортируются только трейсы).
	ExportMetrics bool `yaml:"export_metrics,omitempty"`
}

// KeeperLogging — логи с ротацией.
type KeeperLogging struct {
	Level    string           `yaml:"level,omitempty"`
	Format   string           `yaml:"format,omitempty"`
	File     string           `yaml:"file,omitempty"`
	Rotation *LoggingRotation `yaml:"rotation,omitempty"`
}

// LoggingRotation — общая для Keeper и Soul (разные дефолты, но идентичная схема).
//
// MaxAgeDays семантика: пусто/0 → дефолт билдера (7 дней), любое >0 — точное
// число дней. «0 = без age-based удаления» в схеме НЕ выражается — для этого
// MaxAgeDays перешёл с overlay-`*int` на плоский int, и различение «0 vs не
// задано» сознательно снято (см. shared/log → defaultMaxAgeDays).
type LoggingRotation struct {
	MaxSizeMB  int  `yaml:"max_size_mb,omitempty"`
	MaxFiles   int  `yaml:"max_files,omitempty"`
	MaxAgeDays int  `yaml:"max_age_days,omitempty"`
	Compress   bool `yaml:"compress,omitempty"`
}

// DefaultPluginFetchTimeout — дефолт `plugins.fetch_timeout`: потолок одной
// цепочки git-команд резолва плагина (clone→checkout→rev-parse, ADR-026
// F-fetch). git-egress — внешний вызов, поэтому таймуат обязателен; 120s
// покрывает крупный repo на медленном линке, не превращая старт Keeper-а в
// бесконечное ожидание на недоступном remote.
const DefaultPluginFetchTimeout = 120 * time.Second

// Лимиты размера резолва плагина (ADR-026(g) git-egress hardening). Защита
// диска keeper-host-а от враждебного/огромного репозитория: timeout
// ограничивает git-egress по времени, эти два поля — по объёму. Оба заданы в
// МиБ симметрично `listen.grpc.event_stream.max_apply_size_mb` (один стиль
// size-полей в проекте, без отдельного human-readable парсера). Превышение —
// fail-closed: слот не создаётся, плагину нечего допускать (Sigil).
const (
	// DefaultPluginMaxArtifactSizeMB — дефолт `plugins.max_artifact_size_mb`:
	// потолок размера одного извлекаемого бинаря `dist/<binary-name>` (и
	// manifest.yaml, который заведомо мельче). 256 MiB с запасом покрывает
	// реальные Go-бинари плагинов (десятки МиБ), отсекая мусорный артефакт,
	// которым враждебный репозиторий забил бы кеш.
	DefaultPluginMaxArtifactSizeMB = 256

	// DefaultPluginMaxCloneSizeMB — дефолт `plugins.max_clone_size_mb`: потолок
	// суммарного размера рабочего дерева клона (checkout + `.git`) перед
	// извлечением артефакта. Заведомо больше artifact-лимита: дерево несёт сам
	// артефакт плюс остальные файлы репозитория и shallow-`.git`. 1024 MiB —
	// крупный репозиторий с собранными бинарями, но не безразмерный.
	DefaultPluginMaxCloneSizeMB = 1024

	// MinPluginSizeMB — нижняя граница валидации обоих лимитов. Меньше 1 MiB не
	// пропускаем: сабмегабайтный потолок отверг бы любой реальный Go-бинарь
	// плагина (десятки МиБ), превратив hardening в постоянный fail-closed.
	MinPluginSizeMB = 1
)

// KeeperPlugins — каталоги Keeper-side плагинов.
//
// `CacheRoot` — путь к директории-кешу артефактов плагинов на keeper-host-е
// (см. [docs/keeper/plugins.md](../../docs/keeper/plugins.md)). Пустое
// значение — берётся `pluginhost.DefaultCacheRoot`. Должен быть абсолютным;
// валидация в schema-фазе.
//
// `WorkRoot` — корень рабочих git-клонов резолвера плагинов (ADR-026 F-fetch,
// A1-S1). СТРОГО вне `CacheRoot`: .git и checkout не должны попадать в
// кеш-каталог, который читают Discover/ReadSlot. Пустое → встроенный дефолт
// `/var/lib/soul-stack-keeper/plugin-src`. Должен быть абсолютным; валидация в
// schema-фазе.
//
// `FetchTimeout` — потолок одной цепочки git-команд резолва (тип `duration`,
// пустое → [DefaultPluginFetchTimeout] (120s)). Формат валидируется в
// semantic-фазе (стиль `acolyte_*`).
//
// `MaxArtifactSizeMB` / `MaxCloneSizeMB` — size-лимиты git-egress hardening
// (ADR-026(g)): потолок одного извлекаемого бинаря и суммарного рабочего дерева
// клона. Оба в МиБ (стиль `max_apply_size_mb`); 0/опущено → дефолты
// [DefaultPluginMaxArtifactSizeMB] / [DefaultPluginMaxCloneSizeMB]; заданное
// значение обязано быть ≥ [MinPluginSizeMB] (валидация в schema-фазе).
// Превышение на резолве — fail-closed (слот не создаётся).
type KeeperPlugins struct {
	CacheRoot         string               `yaml:"cache_root,omitempty"`
	WorkRoot          string               `yaml:"work_root,omitempty"`
	FetchTimeout      string               `yaml:"fetch_timeout,omitempty"`
	MaxArtifactSizeMB int                  `yaml:"max_artifact_size_mb,omitempty"`
	MaxCloneSizeMB    int                  `yaml:"max_clone_size_mb,omitempty"`
	CloudDrivers      []PluginCatalogEntry `yaml:"cloud_drivers,omitempty"`
	SSHProviders      []PluginCatalogEntry `yaml:"ssh_providers,omitempty"`
}

// ResolvedFetchTimeout возвращает эффективный `plugins.fetch_timeout`: пусто /
// некорректно → [DefaultPluginFetchTimeout]. Формат уже провалидирован
// semantic-фазой (checkDuration); дефолтуем на любой не-положительный результат
// на всякий случай (симметрия с резолверами acolyte_* в keeper/cmd/keeper).
func (p *KeeperPlugins) ResolvedFetchTimeout() time.Duration {
	if p == nil || p.FetchTimeout == "" {
		return DefaultPluginFetchTimeout
	}
	d, err := ParseDuration(p.FetchTimeout)
	if err != nil || d <= 0 {
		return DefaultPluginFetchTimeout
	}
	return d
}

// ResolvedMaxArtifactSize возвращает эффективный лимит размера одного бинаря в
// байтах: 0/опущено/некорректно → [DefaultPluginMaxArtifactSizeMB]. Диапазон
// (≥ минимума) валидируется schema-фазой; здесь только резолв дефолта.
func (p *KeeperPlugins) ResolvedMaxArtifactSize() int64 {
	mb := DefaultPluginMaxArtifactSizeMB
	if p != nil && p.MaxArtifactSizeMB > 0 {
		mb = p.MaxArtifactSizeMB
	}
	return int64(mb) * bytesPerMiB
}

// ResolvedMaxCloneSize возвращает эффективный лимит размера рабочего дерева
// клона в байтах: 0/опущено/некорректно → [DefaultPluginMaxCloneSizeMB].
func (p *KeeperPlugins) ResolvedMaxCloneSize() int64 {
	mb := DefaultPluginMaxCloneSizeMB
	if p != nil && p.MaxCloneSizeMB > 0 {
		mb = p.MaxCloneSizeMB
	}
	return int64(mb) * bytesPerMiB
}

type PluginCatalogEntry struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
	Ref    string `yaml:"ref"`
}

// PluginRuntime — общий для Keeper и Soul (ADR-020).
// Симметричная схема, разные дефолты `socket_dir`.
type PluginRuntime struct {
	SocketDir           string   `yaml:"socket_dir,omitempty"`
	StartupTimeout      string   `yaml:"startup_timeout,omitempty"`
	ShutdownGrace       string   `yaml:"shutdown_grace,omitempty"`
	AllowedCapabilities []string `yaml:"allowed_capabilities,omitempty"`
	ConflictPolicy      string   `yaml:"conflict_policy,omitempty"`
	EnableTLS           bool     `yaml:"enable_tls,omitempty"`
}

// KeeperAudit — блок аудита (ADR-022).
type KeeperAudit struct {
	Enabled       bool `yaml:"enabled"`
	OTelExport    bool `yaml:"otel_export"`
	RetentionDays int  `yaml:"retention_days"`
}

// HotReload — общий для Keeper и Soul блок управления триггерами reload-а
// (ADR-021).
type HotReload struct {
	EnableSignal       bool `yaml:"enable_signal"`
	EnableInotify      bool `yaml:"enable_inotify"`
	AuditCorrelationID bool `yaml:"audit_correlation_id"`
}

// Дефолты Conductor (ADR-048). Применяются в daemon при пустом/нулевом поле.
const (
	// DefaultCadenceSchedulerLockTTL — дефолт TTL Redis-lease conductor:leader.
	// 5m parity reaper lock_ttl: достаточно большой, чтобы пережить временный
	// stall лидера без потери лидерства, и достаточно короткий для быстрого
	// failover на другой инстанс при смерти лидера. renew идёт на lock_ttl/3
	// внутри leaderloop.
	DefaultCadenceSchedulerLockTTL = 5 * time.Minute

	// Профиль «Спокойный» адаптивного шага опроса Conductor (ADR-048 «Adaptive
	// interval», 2026-06-07). Шаг = clamp(derivedMinPeriod, poll_floor,
	// poll_ceiling); пустой реестр → poll_idle.

	// DefaultCadenceSchedulerPollFloor — нижняя граница шага опроса (30s). Это и
	// абсолютный минимум (poll_floor < 30s — конфиг-ошибка; DB-CHECK floor
	// interval_seconds ≥ 30 закрывает Pass B / ADR-046).
	DefaultCadenceSchedulerPollFloor = 30 * time.Second

	// DefaultCadenceSchedulerPollCeiling — верхняя граница шага опроса (60s):
	// редкие расписания (interval=1h) не растягивают опрос настолько, что
	// NextRunAnchored missed-slot становится единственной страховкой.
	DefaultCadenceSchedulerPollCeiling = 60 * time.Second

	// DefaultCadenceSchedulerPollIdle — шаг опроса при пустом enabled-реестре
	// (120s): спавнить нечего, опрашиваем реже обычного коридора.
	DefaultCadenceSchedulerPollIdle = 120 * time.Second
)

// KeeperCadenceScheduler — конфиг Conductor (ADR-048). Свой tick-interval и
// Redis-lease (`conductor:leader`), независимые от Reaper-а.
//
// Enabled — *bool ради различения «не задано» (→ default-ON при наличии Redis,
// footgun-guard ADR-048 §5) от явного `false` (оператор сознательно гасит
// планировщик целиком — выключение отдельной Cadence делается per-Cadence через
// `enabled: false` самой строки, ADR-046 §3, не глобальным гашением Conductor).
type KeeperCadenceScheduler struct {
	// Enabled — nil (опущено) → default-ON при наличии Redis; явный false →
	// Conductor не поднимается; явный true → поднимается (требует Redis для
	// lease-лидерства, как и Reaper).
	Enabled *bool `yaml:"enabled,omitempty"`

	// Interval — backcompat-alias верхней границы адаптивного опроса (ADR-048
	// «Adaptive interval»). До амендмента 2026-06-07 это был фиксированный период
	// тика. Теперь шаг адаптивный (clamp по poll_floor/poll_ceiling); `interval`
	// сохранён как alias: если задан, а `poll_ceiling` не задан → ceiling =
	// interval (старые keeper.yml не падают). Тип `duration`, hot-reload.
	Interval string `yaml:"interval,omitempty"`

	// LockTTL — TTL Redis-lease conductor:leader (hot-reload между re-acquire).
	// Тип `duration`, пустое → дефолт [DefaultCadenceSchedulerLockTTL] (5m).
	LockTTL string `yaml:"lock_ttl,omitempty"`

	// PollFloor / PollCeiling / PollIdle — коридор адаптивного шага опроса
	// (ADR-048 «Adaptive interval», профиль «Спокойный» 30s/60s/120s). Шаг =
	// clamp(derivedMinPeriod, poll_floor, poll_ceiling); пустой реестр → poll_idle.
	// Все три — тип `duration`, hot-reload (читаются из снимка config в IntervalFn,
	// как Interval/LockTTL). Пустое/невалидное → дефолт. Инварианты (semantic-
	// валидация): poll_floor ≥ 30s (абсолютный минимум), poll_floor ≤ poll_ceiling,
	// poll_idle ≥ poll_ceiling (idle не чаще обычного опроса).
	PollFloor   string `yaml:"poll_floor,omitempty"`
	PollCeiling string `yaml:"poll_ceiling,omitempty"`
	PollIdle    string `yaml:"poll_idle,omitempty"`
}

// CadenceSchedulerEnabled возвращает эффективный enabled Conductor с учётом
// footgun-guard-а (ADR-048 §5): не задано (nil блок / nil поле) → ON; явный
// `false` → OFF; явный `true` → ON. Фактический старт в daemon дополнительно
// требует non-nil Redis (lease-лидерство), как и Reaper.
func (c *KeeperCadenceScheduler) CadenceSchedulerEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ResolvedLockTTL возвращает эффективный TTL lease-ключа Conductor: пустое/
// невалидное → дефолт.
func (c *KeeperCadenceScheduler) ResolvedLockTTL() time.Duration {
	if c == nil || c.LockTTL == "" {
		return DefaultCadenceSchedulerLockTTL
	}
	d, err := ParseDuration(c.LockTTL)
	if err != nil || d <= 0 {
		return DefaultCadenceSchedulerLockTTL
	}
	return d
}

// ResolvedPollFloor — нижняя граница адаптивного опроса (ADR-048): пустое/
// невалидное → дефолт 30s.
func (c *KeeperCadenceScheduler) ResolvedPollFloor() time.Duration {
	if c == nil {
		return DefaultCadenceSchedulerPollFloor
	}
	return resolveDuration(c.PollFloor, DefaultCadenceSchedulerPollFloor)
}

// ResolvedPollCeiling — верхняя граница адаптивного опроса (ADR-048). Backcompat:
// если `poll_ceiling` не задан, но задан `interval` (старый формат) → ceiling =
// max(interval, poll_floor) — clamp ВВЕРХ до floor. Старый малый `interval`
// (dev-конфиги ставили суб-30s) НЕ роняет конфиг через инвариант
// `poll_floor ≤ poll_ceiling`: alias всегда ≥ floor. Эмиссию warning о подъёме
// делает semantic-фаза ([cadenceIntervalBelowFloorWarn]). Иначе пустое/невалидное
// → дефолт 60s.
func (c *KeeperCadenceScheduler) ResolvedPollCeiling() time.Duration {
	if c == nil {
		return DefaultCadenceSchedulerPollCeiling
	}
	if c.PollCeiling == "" && c.Interval != "" {
		ceiling := resolveDuration(c.Interval, DefaultCadenceSchedulerPollCeiling)
		if floor := c.ResolvedPollFloor(); ceiling < floor {
			return floor
		}
		return ceiling
	}
	return resolveDuration(c.PollCeiling, DefaultCadenceSchedulerPollCeiling)
}

// ResolvedPollIdle — шаг опроса при пустом enabled-реестре (ADR-048): пустое/
// невалидное → дефолт 120s.
func (c *KeeperCadenceScheduler) ResolvedPollIdle() time.Duration {
	if c == nil {
		return DefaultCadenceSchedulerPollIdle
	}
	return resolveDuration(c.PollIdle, DefaultCadenceSchedulerPollIdle)
}

// resolveDuration — общий резолвер duration-поля: пустое/невалидное/non-positive
// → fallback (стиль ResolvedInterval/ResolvedLockTTL).
func resolveDuration(val string, fallback time.Duration) time.Duration {
	if val == "" {
		return fallback
	}
	d, err := ParseDuration(val)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// KeeperReaper — фоновая чистка.
type KeeperReaper struct {
	Enabled   bool                  `yaml:"enabled"`
	Interval  string                `yaml:"interval,omitempty"`
	DryRun    bool                  `yaml:"dry_run,omitempty"`
	BatchSize int                   `yaml:"batch_size,omitempty"`
	LockTTL   string                `yaml:"lock_ttl,omitempty"`
	Rules     map[string]ReaperRule `yaml:"rules,omitempty"`
}

// ReaperRule — одно правило Жнеца. Схема правила слабо-типизирована,
// потому что 5 предопределённых rules имеют разные обязательные поля
// (`statuses`, `stale_after`, `target_status`); строгая per-rule типизация —
// отложенная задача нормирования reaper.md.
//
// KeepLastN / KeepVersionBumpSnapshots — поля правила
// `archive_state_history` (ADR-Q19 retention). Остальные правила их
// игнорируют. KeepLastN использует *int, чтобы отличить «не задано» (→ runner
// подставит дефолт 50) от явного 0 (semantic-validate отвергнет: 0 = «всё
// архивировать»). KeepVersionBumpSnapshots — *bool по той же причине:
// отличаем «не задано» (→ дефолт true, защита миграций) от явного false
// (оператор сознательно архивирует и version-bump-снимки).
type ReaperRule struct {
	Enabled                  bool     `yaml:"enabled"`
	MaxAge                   string   `yaml:"max_age,omitempty"`
	Action                   string   `yaml:"action,omitempty"`
	Statuses                 []string `yaml:"statuses,omitempty"`
	StaleAfter               string   `yaml:"stale_after,omitempty"`
	TargetStatus             string   `yaml:"target_status,omitempty"`
	KeepLastN                *int     `yaml:"keep_last_n,omitempty"`
	KeepVersionBumpSnapshots *bool    `yaml:"keep_version_bump_snapshots,omitempty"`

	// MaxConcurrentInFlight — поле правила `scry_background` (ADR-031 Slice C):
	// верхняя граница одновременно идущих dry_run-сканов, инициированных Reaper-
	// тиком. Прочие правила игнорируют. `*int`, чтобы различить «не задано» (→
	// runner подставит дефолт 10) от явного 0 («заглушить правило без снятия
	// enabled»). Counter активных сканов — по числу строк apply_runs с
	// recipe->>'dry_run'='true' и finished_at IS NULL.
	MaxConcurrentInFlight *int `yaml:"max_concurrent_in_flight,omitempty"`

	// MinIntervalPerIncarnation — поле правила `scry_background` (ADR-031 Slice
	// C): минимальный интервал между фоновыми скана­ми одного incarnation.
	// Прочие правила игнорируют. Пустая строка / нулевая duration = «без
	// нижней границы» (iterator-сортировка `last_drift_check_at NULLS FIRST`
	// сама естественно даёт round-robin между incarnation-ами).
	MinIntervalPerIncarnation string `yaml:"min_interval_per_incarnation,omitempty"`
}

// KeeperPush — pilot wire-up SshDispatcher (S6, 2026-05-26, [ADR-032 amendment]).
//
// Pilot-path сознательно вынесен в keeper.yml ради быстрого end-to-end-движения:
// per-host SSH-реквизиты в `targets[]` (без миграции `souls.ssh_target jsonb` —
// это S7), per-provider params в `providers[]` (без PG-table push_providers — S7).
// S7-3 ввёл multi-CA `host_ca_refs[]`; устаревший single-CA `host_ca_ref` остаётся
// под 1-release WARN deprecation window (auto-adapt в singleton, см. ниже).
// Multi-provider routing отложен — pilot поднимает дискаверенного первого
// ssh-плагина.
//
// Опц.: при отсутствии блока (или пустых полей) push-orchestrator не поднимается.
type KeeperPush struct {
	// Targets — per-SID SSH-реквизиты. Резолвер по SID lookup-ит запись и
	// возвращает [push.SSHTarget]; SID без записи → fail с `target_not_configured`.
	Targets []KeeperPushTarget `yaml:"targets,omitempty"`
	// Providers — per-provider params для env-payload `SOUL_SSH_<NAME>_PARAMS`
	// (ADR-020 amendment l). При spawn-е плагина `params` сериализуется в JSON
	// и кладётся в env-переменную с именем `SOUL_SSH_<UPPER(name)>_PARAMS`.
	// Записи без сопоставления в `plugins.ssh_providers[].name` игнорируются.
	Providers []KeeperPushProvider `yaml:"providers,omitempty"`
	// HostCARef — deprecated singular vault-ref на public host-CA (PEM-encoded
	// SSH public key, поле `public_key` в Vault KV). S7-3 ввёл multi-CA
	// `host_ca_refs[]` (ADR-032 amendment 2026-05-26); singular остался под
	// 1-release WARN deprecation window. На старте daemon auto-adapt-ит singular
	// в `HostCARefs[0]` с auto-name `default` и одноразовым WARN. Mutually
	// exclusive с `host_ca_refs[]` — одновременное присутствие отвергается
	// schema-фазой (`mutually_exclusive_keys`).
	//
	// Deprecated: use [HostCARefs] (multi-CA).
	HostCARef string `yaml:"host_ca_ref,omitempty" json:"host_ca_ref,omitempty"`
	// HostCARefs — мульти-CA для verify host-keys через SSH (S7-3, ADR-032
	// amendment 2026-05-26). Каждый элемент — vault-ref + operator-defined `name`
	// (для логов / OTel attrs / metrics cardinality). На handshake-е
	// `ssh.CertChecker.IsHostAuthority` делает OR-проверку по всем загруженным CA:
	// host-cert, подписанный любым из них — доверенный.
	//
	// Plaintext-inline-PEM отвергнут как нарушение security policy (симметрия с
	// `auth.jwt.signing_key_ref` / `sigil.signing_key_ref`); каждый `ref` обязан
	// быть vault-ref-ом. Имена в наборе должны быть unique (lookup по имени в
	// логах / метриках без двусмысленности).
	HostCARefs []KeeperPushCARef `yaml:"host_ca_refs,omitempty" json:"host_ca_refs,omitempty"`
	// AllowLegacyPushTargets — fallback-флаг S7-1 deprecation window (ADR-032
	// amendment 2026-05-26): PG-источник (souls.ssh_target jsonb) — canonical,
	// keeper.yml::push.targets[] — legacy. При false (default) запись отсутствует
	// в PG → `ErrTargetNotConfigured`; при true → fallback на ConfigTargetResolver
	// поверх Targets[] с одноразовым WARN на старте.
	AllowLegacyPushTargets bool `yaml:"allow_legacy_push_targets,omitempty" json:"allow_legacy_push_targets,omitempty"`
	// AllowLegacyPushProviders — fallback-флаг S7-2 deprecation window (ADR-032
	// amendment 2026-05-26): PG-источник (push_providers таблица) — canonical,
	// keeper.yml::push.providers[] — legacy. При false (default) plugin без
	// записи в PG → плагин стартует без env-payload (поведение зависит от
	// самого плагина: soul-ssh-static работает с дефолтами, soul-ssh-vault
	// требует params); при true → fallback на keeper.yml::push.providers[] с
	// одноразовым WARN на старте. Симметрично [AllowLegacyPushTargets].
	AllowLegacyPushProviders bool `yaml:"allow_legacy_push_providers,omitempty" json:"allow_legacy_push_providers,omitempty"`

	// AutoImportLegacyTargets — opt-in one-shot миграция inline
	// `push.targets[]` → `souls.ssh_target` jsonb при старте Keeper-а
	// (ADR-032 amendment 2026-05-26, S7-4). Default false (без явного
	// согласия оператора молчаливая миграция данных запрещена). При true
	// daemon на старте проходит по `Targets[]`: для каждого SID с
	// `ssh_target IS NULL` в `souls` пишет SSH-реквизиты и эмитит
	// audit-event `soul.ssh-target.imported_from_config` (source
	// `config_bootstrap`). Идемпотентно: запись с непустым PG-target
	// пропускается, повторный старт — no-op. Отсутствующая `souls`-row
	// — WARN-skip (не fatal).
	AutoImportLegacyTargets bool `yaml:"auto_import_legacy_targets,omitempty" json:"auto_import_legacy_targets,omitempty"`

	// AutoImportLegacyProviders — opt-in one-shot миграция inline
	// `push.providers[]` → PG-таблица `push_providers` при старте Keeper-а
	// (ADR-032 amendment 2026-05-26, S7-4). Default false. Симметрично
	// [AutoImportLegacyTargets]: записи, отсутствующие в PG, заводятся под
	// `archon-system`-AID; уже существующие имена пропускаются (запись в
	// PG canonical-источник, не перезаписываем). Audit-event —
	// `push-provider.imported_from_config`.
	AutoImportLegacyProviders bool `yaml:"auto_import_legacy_providers,omitempty" json:"auto_import_legacy_providers,omitempty"`

	// CovenDefaultProviders — Level 2 multi-provider routing (P2 W-4, ADR-032
	// amendment 2026-05-27). Карта coven-имя → имя SshProvider-плагина.
	// Используется, когда у Soul-а нет per-SID `ssh_target.ssh_provider`
	// (Level 1). Tiebreak при множественном coven-match — алфавитный порядок
	// имён ковенов (детерминизм). Пустая карта → переход к Level 3.
	//
	// Hot-reload поддерживается: на каждый config.Store.OnReload router
	// читает свежий снимок через RouterConfigSource.
	CovenDefaultProviders map[string]string `yaml:"coven_default_providers,omitempty" json:"coven_default_providers,omitempty"`

	// ClusterDefaultProvider — Level 3 multi-provider routing (P2 W-4). Имя
	// SshProvider-плагина по умолчанию для всех Souls, у которых ни Level 1,
	// ни Level 2 не дали match. Пусто → ErrProviderNotRouted (fail per-host).
	//
	// Hot-reload поддерживается (см. CovenDefaultProviders).
	ClusterDefaultProvider string `yaml:"cluster_default_provider,omitempty" json:"cluster_default_provider,omitempty"`
}

// KeeperPushCARef — один элемент multi-CA `push.host_ca_refs[]` (S7-3).
//
// `Ref` — vault-ref (`vault:<mount>/<path>`) на public host-CA (поле в Vault KV
// — `public_key`, симметрия с singular `host_ca_ref`).
//
// `Name` — operator-defined kebab-case-имя, используется как label-значение в
// `keeper_push_host_ca_used_total{ca_name=...}` и в diag-сообщениях. Должно быть
// уникально в наборе (валидация в schema-фазе).
type KeeperPushCARef struct {
	Ref  string `yaml:"ref"  json:"ref"`
	Name string `yaml:"name" json:"name"`
}

// DefaultHostCAName — auto-name при backward-compat auto-adapt singular
// `push.host_ca_ref` в `host_ca_refs[0]` (S7-3 deprecation window, ADR-032
// amendment 2026-05-26). Кебаб-case-имя, проходит ту же валидацию, что и
// operator-defined имена в multi-CA-наборе.
const DefaultHostCAName = "default"

// KeeperPushTarget — SSH-реквизиты одного push-хоста (`sid` = FQDN, тот же,
// что в реестре `souls`). Pilot-форма: inline в keeper.yml; S7 заменит на
// `souls.ssh_target jsonb`.
//
// Дефолты SSHPort / SSHUser / SoulPath применяются на резолве (см.
// keeper/internal/push.ConfigTargetResolver), а не в schema-фазе: оператор
// может опустить любое поле, и тогда подставится стандартное значение
// (22 / root / /usr/local/bin/soul).
type KeeperPushTarget struct {
	SID      string `yaml:"sid"                json:"sid"`
	SSHPort  int    `yaml:"ssh_port,omitempty" json:"ssh_port,omitempty"`
	SSHUser  string `yaml:"ssh_user,omitempty" json:"ssh_user,omitempty"`
	SoulPath string `yaml:"soul_path,omitempty" json:"soul_path,omitempty"`
}

// KeeperCloudInit — параметры рендера cloud-init userdata (ADR-017(h)
// amendment 2026-05-27, B-flat). Все поля обязательны на момент использования
// (сценарий с `generate_userdata: true`); пустое значение → fail-fast с ясной
// ошибкой при GenerateUserdata-вызове, а не молчаливый рендер «недо-userdata».
//
// `BootstrapEndpoint` — `host:port` LB, через который Soul-агент будет звонить
// на Bootstrap-RPC (ADR-012(b), отдельный listener) ПОСЛЕ установки. В userdata
// он рендерится в `keeper.endpoints[0]` (host + bootstrap_port; event_stream-
// порт указывается тем же — за LB-ом он один и тот же).
//
// `TLSCARef` — vault-ref (`vault:<mount>/<path>`) на PEM-CA Keeper-а. На
// GenerateUserdata-вызове резолвится через keeper-vault-клиент (поле в Vault KV —
// `ca`), результат запекается в userdata как `write_files: /etc/soul/tls/keeper-ca.pem`.
// CA — публичный материал (не секрет), но единый источник правды в Vault
// нужен для ротации без правок keeper.yml.
//
// `SoulBinaryURL` — HTTPS URL, с которого VM скачивает `soul`-бинарь (curl
// в runcmd). Должен использовать сертификат, верифицируемый pinned-CA из
// `TLSCARef` (или системного store-а — но для self-hosted Keeper-CA это и есть
// тот же ref). Plain http отвергается на этапе GenerateUserdata.
//
// `SoulVersion` — опц. строка, попадающая в userdata как комментарий (для
// диагностики); fingerprint-сверка отложена (см. ADR-017(h) amendment, soul-binary
// signature verification — отдельный slice).
type KeeperCloudInit struct {
	BootstrapEndpoint string `yaml:"bootstrap_endpoint"`
	TLSCARef          string `yaml:"tls_ca_ref"`
	SoulBinaryURL     string `yaml:"soul_binary_url"`
	SoulVersion       string `yaml:"soul_version,omitempty"`
}

// KeeperPushProvider — per-provider params для env-payload плагина SSH-провайдера.
// Pilot-форма: inline в keeper.yml; S7 заменит на PG-table `push_providers`.
//
// `Name` — ссылка на `plugins.ssh_providers[].name` (kebab-case); ровно та же
// строка, что использует git-резолвер каталога. `Params` сериализуется в JSON
// и инжектится в env-переменную `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` плагина
// (ADR-020 amendment l): `vault-bastion` → `SOUL_SSH_VAULT_BASTION_PARAMS`.
// Содержимое `Params` — opaque-форма самого провайдера (vault_addr/role/proxy_addr/…).
type KeeperPushProvider struct {
	Name   string         `yaml:"name"   json:"name"`
	Params map[string]any `yaml:"params" json:"params"`
}
