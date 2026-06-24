# Observability: метрики и OpenTelemetry

Нормативная спека сквозного слоя observability Soul Stack — публикации метрик и трейсинга. Фиксирует **конвенции и архитектуру**, общие для всех бинарей (`keeper` / `soul` / `soul-lint`), но не перечисляет конкретные метрики по списку — этот каталог наполняется при имплементации соответствующих подсистем.

Источник решения — [ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge). Сквозное требование, под которое это приземляется, — «у всех публикация метрик» + «поддержка OpenTelemetry из коробки» ([requirements.md](requirements.md), раздел «Общие требования»). Код observability-стека живёт в [`shared/obs/`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) ([ADR-011](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)) — единый модуль для обоих server-бинарей, чтобы метрики и трейсы собирались одним стеком без дублирования.

> **Почему cross-cutting (один файл), а не per-binary.** В таблице [«Сквозные требования»](architecture.md#сквозные-требования-и-где-они-приземляются) метрики и OTel помечены «Все три бинаря». Конвенции (namespace, единицы, суффиксы, resource-attrs) — общие; различение между Keeper и Soul делается **префиксом метрики** и значением `service.name`, а не отдельной спекой на каждый бинарь. Поэтому спека одна, как у `shared/obs` — один Go-модуль на оба бинаря.

## 1. Архитектура: Prometheus-primary + OTel-bridge

Два канала телеметрии с чётко разделёнными ролями:

| Канал | Что несёт | Модель | Роль |
|---|---|---|---|
| **Prometheus** | Метрики (counters / gauges / histograms). | **Pull** (scrape `/metrics`). | **Первичный** канал метрик. De-facto-стандарт мониторинга; у Keeper уже есть Prometheus-registry в [`shared/obs`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). |
| **OpenTelemetry** | Трейсы (spans) + опционально push метрик. | **Push** (OTLP в коллектор). | **Мост**: сквозной трейсинг (оператор → Keeper → Soul через propagation в gRPC-метаданных) + опциональный экспорт метрик в OTLP-коллектор для инсталляций без Prometheus-scrape. |

**Не «OTel-primary».** Метрики первично экспонируются Prometheus-эндпоинтом; OTLP-push метрик — опциональный мост, не замена scrape-канала. Трейсы — всегда через OTel (у Prometheus нет трейс-модели).

### 1.1. Prometheus — что экспонируется

- **Эндпоинт `/metrics`** на server-бинарях (`keeper`, `soul`), на **выделенном listener-е** (отдельный порт), не на openapi-роутере. Exposition-format — `text/plain; version=0.0.4` (Prometheus 2.x scrape-compatible), OpenMetrics-format пока не включён.
  - **Keeper:** `listen.metrics.addr` (обычно `0.0.0.0:9090`). Эндпоинт снят с openapi-роутера — scrape идёт на отдельный порт без auth-chain Operator API. keeper_http_*-метрики при этом по-прежнему собираются middleware на `/v1/*` и экспонируются здесь же (тот же registry). Опц. защита — HTTP Basic-auth (`metrics.auth.basic`, пароль из vault-ref, constant-time сравнение; [keeper/config.md](keeper/config.md#metrics)).
  - **Soul:** `metrics.listen` (default loopback `127.0.0.1:9091`). Опц. защита — HTTP Basic-auth (`metrics.basic_auth`, [soul/config.md](soul/config.md#metrics)), но **источник пароля — файл на диске** (`password_file`), а не vault-ref: у Soul нет vault-клиента ([ADR-012](adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)) для резолва. Сама constant-time проверка — тот же `obs.ServeMetrics`-хелпер, что у Keeper (helper источник-агностичен, §1.1 выше). При выключенном basic-auth (default) защита `/metrics` — loopback-bind.
- **Dedicated registry** ([`prometheus.NewRegistry()`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)), не глобальный `DefaultRegisterer` — чтобы две инстанции в одном процессе (например, в тесте) не делили состояние и не падали на re-register. На обоих бинарях один registry шарится между инструментацией (middleware / apply-цикл) и exposition-handler-ом metrics-listener-а.
- **Helper `obs.ServeMetrics(addr, reg, auth)`** ([`shared/obs`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)) — переиспользуемый обоими бинарями: поднимает выделенный listener для `GET /metrics`, опц. basic-auth (`auth=nil` → открыт). Helper **источник-агностичен**: caller передаёт уже зарезолвленный пароль (`BasicAuth{Username, Password}`); сам helper vault не резолвит ([ADR-011](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам): `shared/obs` не тянет vault-client). Graceful Shutdown в defer-цепочке main.
- **Базовые collectors** регистрируются явно: go-runtime (memory / goroutines / gc) и process (fds / cpu). Без них scrape бесполезен в production: одни application-метрики не отвечают на вопрос «кто течёт».
- `soul-lint` — офлайн-инструмент, `/metrics`-эндпоинта не имеет (нет долгоживущего процесса).

### 1.2. OTel — что экспортируется

- **Трейсы** — всегда, если включён `otel:`-блок конфига. Сквозная propagation trace-context через gRPC-метаданные EventStream-а: span Архонт-вызова на Keeper связывается со span-ом apply на Soul.
- **Метрики по OTLP** — **опционально**. Включается отдельным флагом в `otel:`-блоке конфига (нормативная типизация блока — в config.md каждого бинаря, отдельной задачей). По умолчанию метрики идут только через Prometheus-scrape; OTLP-метрики — для инсталляций, где нет Prometheus.
- **Endpoint OTLP-коллектора** задаётся в `otel:`-блоке конфига; локально поднимается через docker-compose ([dev-инфра](adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper), см. §1.3 ниже).

### 1.3. OTel в dev-стеке (otel-collector + Jaeger)

Локальный dev-стек ([`dev/docker-compose.yml`](dev/local-setup.md)) поднимает приёмник трейсов из двух сервисов:

| Сервис | Образ | Роль | Порт (host) |
|---|---|---|---|
| **otel-collector** | `otel/opentelemetry-collector-contrib` | Принимает OTLP gRPC от keeper/soul, batch → экспорт в Jaeger + debug-лог. Конфиг pipeline-а — [`dev/otel-collector.yaml`](dev/local-setup.md). | `4317` (OTLP gRPC) |
| **jaeger** | `jaegertracing/all-in-one` | Хранилище + UI трейсов (in-memory storage, теряется при restart). Принимает от коллектора OTLP внутри docker-сети. | `16686` (UI) |

- **keeper / soul → collector** — insecure-gRPC на `127.0.0.1:4317`. И dev-конфиг keeper-а ([`dev/keeper.dev.yml`](dev/local-setup.md): `otel.enabled: true`, `endpoint: 127.0.0.1:4317`), и dev-конфиг soul-а ([`dev/soul.dev.yml`](dev/local-setup.md)) указывают на этот эндпоинт. Insecure — потому что dev-collector поднят без TLS (`SetupOTel` всегда `WithInsecure`, см. [`shared/obs/otel.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)).
- **Смотреть трейсы** — Jaeger UI http://127.0.0.1:16686, service `keeper` / `soul`. Альтернатива без UI: `docker compose -f dev/docker-compose.yml logs -f otel-collector` (debug-exporter печатает принятые спаны).
- **Прод — НЕ этот стек.** `examples/keeper/keeper.yml` оставляет `otel:`-блок конфигурируемым (endpoint реального коллектора, TLS), без dev-hardcode. all-in-one Jaeger с in-memory storage — только для локальной отладки.

## 2. Namespace метрик: `soul_*` / `keeper_*`

Раздельные префиксы по компоненту:

| Префикс | Кто экспонирует | Пример |
|---|---|---|
| **`keeper_*`** | Keeper-side метрики. | `keeper_grpc_streams_active`, `keeper_http_requests_total` |
| **`soul_*`** | Soul-side метрики. | `soul_apply_tasks_total` |

**Различение по префиксу, а не по label.** Не вводим общий префикс с label `component="keeper|soul"` — отдельные префиксы короче, греппабельнее (`grep '^keeper_'`), и совпадают со стандартом Prometheus (per-exporter namespace). Один scrape-таргет = один компонент, смешения метрик в одном process-е нет.

> **Soul-side metric-naming замечание.** Префикс `soul_` относится к **роли компонента** (Soul-агент), не к namespace-словарю модулей (`core` / `wb` / …). Это разные пространства: `soul_apply_tasks_total` — метрика агента; `core.pkg.installed` — адрес модуля. Пересечения нет.

### 2.1. Конвенция именования

Стандарт Prometheus naming (он же — то, как уже названы метрики в [`shared/obs`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)):

- **`snake_case`**, ASCII, lowercase. Имя = `<prefix>_<subsystem>_<name>_<unit>[_suffix]`.
- **Суффикс единицы** обязателен там, где есть единица: `_seconds` (длительности — **всегда секунды**, не миллисекунды), `_bytes` (размеры — **всегда байты**). Не `_ms`, не `_mb` в имени метрики (внутренние МБ-поля Soulprint — это данные, не метрики).
- **Суффикс типа:**
  - `_total` — для counters (монотонно растущих): `keeper_http_requests_total`.
  - `_seconds` / `_bytes` без `_total` — для gauges/histograms.
  - Histogram-метрика именуется по измеряемой величине + единица: `keeper_http_request_duration_seconds` (Prometheus сам добавляет `_bucket` / `_sum` / `_count`).
- **Gauge мгновенного состояния** — без `_total`: `keeper_http_in_flight_requests`, `keeper_grpc_streams_active`.

### 2.2. Labels — кардинальность под контролем

- Labels — для разрезов, по которым реально задают вопросы оператора (`method` / `status` / `coven` / …), не для уникальных идентификаторов.
- **Нельзя класть в label** unbounded-значения: `sid` (FQDN, тысячи хостов), `aid`, `apply_id`, `audit_id`, raw URL-path. Это cardinality-blow-up — место для таких разрезов трейс/лог, не метрика.
  - Пример из кода: HTTP-`path` берётся из route-pattern (`/v1/operators/{aid}/revoke`), не из raw URL — иначе каждый AID породит новую серию.
- Доменная идентичность инстанса (`kid` / `sid`) живёт в **OTel resource-attributes** (см. §3), не в метрик-labels.

## 3. OTel resource-attributes

Каждый span/metric-export несёт resource-attributes, идентифицирующие источник:

| Attribute | Значение | Назначение |
|---|---|---|
| **`service.name`** | `"keeper"` \| `"soul"` | Стандартный OTel semconv-атрибут. Имя сервиса = имя бинаря (по словарю Soul Stack). |
| **`service.instance.id`** | generic instance-id | Стандартный OTel semconv-атрибут (если выставляется рантаймом). |
| **`soulstack.kid`** | [KID](naming-rules.md#идентификаторы) (Keeper ID) | **Кастомный** атрибут: доменная идентичность Keeper-инстанса в HA-кластере. Только на `service.name="keeper"`. |
| **`soulstack.sid`** | [SID](naming-rules.md#идентификаторы) (Soul ID = FQDN) | **Кастомный** атрибут: доменная идентичность Soul-агента. Только на `service.name="soul"`. |

**Зачем кастомные `soulstack.kid` / `soulstack.sid`, а не только generic `service.instance.id`.** Доменная идентичность Soul Stack — это KID и SID (по ним строится lease, аудит, таргетинг). Generic `service.instance.id` не даёт фильтра «трейсы конкретного Soul-а по его FQDN» или «события одного Keeper-инстанса по его KID» в терминах словаря системы. Префикс `soulstack.` — namespace кастомных атрибутов проекта, чтобы не конфликтовать с OTel semconv-зарезервированными именами.

**Wired.** `SetupOTel(ctx, OTelConfig{...})` ([`shared/obs`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)) собирает resource из `service.name` + кастомных `ResourceAttrs`. Keeper-main передаёт `ServiceName: "keeper"` + `{"soulstack.kid": cfg.KID}`; Soul-main — `ServiceName: "soul"` + `{"soulstack.sid": sid}`. `SetupOTel` вызывается **один раз за процесс** (ставит глобальный TracerProvider + propagator); `otel.*`-блок конфига — restart-required, hot-reload его не перечитывает.

> **Почему `soulstack.kid` / `soulstack.sid` — resource-attrs, а не metric-labels.** Высокая кардинальность (SID = FQDN, тысячи хостов): в Prometheus-labels это blow-up (§2.2). В OTel resource-attrs идентичность инстанса — штатное место, и трейсы по одному хосту фильтруются без раздувания метрик-серий.

## 4. Связь с requirements и кодом

- **requirements.md** — «у всех публикация метрик» закрывается Prometheus-`/metrics` на server-бинарях; «поддержка OpenTelemetry из коробки» — OTel-трейсами + опциональным OTLP-метрик-мостом. Оба — сквозные, в [`shared/obs`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам).
- **`shared/obs`** — единый Go-модуль observability-стека: `Registry` (+go/process-collectors), `MetricsHandler`, HTTP-middleware-инструментация, `ServeMetrics` (выделенный listener + опц. basic-auth), `SetupOTel` (trace-provider, OTLP-exporter, resource-attrs). Оба server-бинаря wire-up-ят его в `runDaemon` (keeper/soul `cmd`). OTLP-**метрик**-pipeline (push метрик) — ещё точка расширения, см. §5.
- **Конкретный список метрик** (что именно меряет Keeper и Soul) — **не нормируется здесь**: наполняется при имплементации каждой подсистемы (gRPC-stream, apply-цикл, Reaper, RBAC, …), как и [каталог audit-events](naming-rules.md#audit-events). Каждая метрика следует конвенциям §2.
- **Заведённые in-process spans** (через глобальный TracerProvider от `SetupOTel`; при OTel disabled — no-op, код не ветвится). Атрибуты span-ов несут доменные идентификаторы (`sid` / `apply_id` / incarnation/scenario name), которые в metric-labels запрещены (§2.2):
  - **Keeper:** `scenario.run` и `scenario.destroy_teardown` (tracer `keeper/scenario`; второй — дочерний на teardown-финал destroy: archive + DELETE строки incarnation), `grpc.bootstrap` и `grpc.apply_dispatch` (tracer `keeper/grpc`, пилот), `augur.request` (tracer `keeper/augur`; резолв + fetch `AugurRequest`, атрибуты без секретов/query), `sigil.anchors_reload` (tracer `keeper/sigil`; runtime-ротация trust-anchor-ключей подписи — re-build Signer + re-broadcast).
  - **Soul:** `apply.run` (tracer `soul/runtime`).
  - **Cross-process trace-propagation Keeper→Soul** — **реализована** через only-add proto-поле `ApplyRequest.trace_context` (W3C traceparent, ADR-012(c) forward-compat). Keeper инжектит trace-context span-а `grpc.apply_dispatch` в `req.TraceContext` (`SendApply`); Soul извлекает его перед `runner.Run`, и `apply.run` поднимается как child — сквозная трасса оператор → Keeper → Soul (ADR-024). В cluster-mode поле едет внутри protobuf-байтов через Redis pub/sub без отдельной обработки. Пустой `trace_context` (старый Keeper) → Extract noop, `apply.run` остаётся корнем собственной трассы (forward-compat деградация). EventStream-метаданные для этого не используются — traceparent в payload.

### 4.0. Где живёт Prometheus-collector подсистемы (правило размещения)

Нормативное правило, под которое инструментируются все подсистемы (тираж инструментации идёт строго по нему):

- **Collector подсистемы живёт рядом с подсистемой**, в `<bin>/internal/<subsys>/metrics.go`, с регистратором сигнатуры `Register<Subsys>Metrics(reg *obs.Registry) *<Subsys>Metrics`, если он специфичен одному бинарю/подсистеме. Все `keeper_*` / `soul_*`-метрики подсистем приземляются здесь, а не в `shared/obs`. Пилот — [`keeper/internal/grpc/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) (`RegisterGRPCMetrics`).
- **В `shared/obs` остаётся только сквозной фундамент**, нужный обоим бинарям без привязки к их internal-типам: registry-core (go/process collectors + exposition handler), `ServeMetrics`, `SetupOTel`, и параметризуемая HTTP-middleware (`HTTPMetrics` через injected path-extractor).
- **Критерий — НЕ префикс метрики, а зависимости collector-а:** тянет internal-типы / тяжёлый init подсистемы → subsystem-local (`<bin>/internal/<subsys>/metrics.go`); нейтрален и нужен обоим → `shared/obs`. Это та же граница, что между `shared/vault` (client-only) и `keeper/internal/vault` (server-side) по [ADR-011](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам).

> **`HTTPMetrics` в `shared/obs` — корректное применение правила, НЕ исключение.** Middleware параметризована injected path-extractor-ом (`MiddlewareForPath(func(*http.Request) string)`) и не знает ни одного keeper-typed-а — она нейтральна и нужна обоим бинарям, поэтому её место в `shared/obs`. Тиражёр инструментации **не должен** «причёсывать» её в `<bin>/internal/...`: по критерию зависимостей она уже размещена правильно.

> **`ReaperMetrics` живёт в `keeper/internal/reaper` — пример выполненного правила §4.0.** Reaper — keeper-only подсистема, тянущая internal-типы; её collector размещён рядом с подсистемой (`RegisterReaperMetrics` в [`keeper/internal/reaper/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)), а не в `shared/obs`. Это образцовое применение критерия «collector рядом с подсистемой»: миграция из `shared/obs` выполнена, отдельной backlog-задачи больше нет.

### 4.1. Каталог метрик (наполняется по подсистемам)

Метрики приземляются при инструментации соответствующей подсистемы. Имя/тип/labels — по конвенциям §2. Подсистемы, ещё не инструментированные, в каталоге не перечислены.

#### Keeper · EventStream (gRPC, ADR-012)

Эталонная инструментация-пилот ([ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). Регистратор — `RegisterGRPCMetrics(reg *obs.Registry) *GRPCMetrics` в [`keeper/internal/grpc/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам); дескриптор шарится между EventStream-handler-ом (streams/messages) и Outbound (apply-dispatch).

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_grpc_streams_active` | gauge | — | Число открытых EventStream-стримов Keeper↔Soul прямо сейчас. Inc после handshake (HelloReply доставлен), Dec в defer handler-а. Без label-ов: разрез по `sid` — cardinality-blow-up (§2.2); «сколько Soul-ов на инстансе» виден из метки `instance` Prometheus-target-а. |
| `keeper_grpc_messages_total` | counter | `direction` (`from_soul` / `to_soul`) | App-сообщения стрима по направлению: `from_soul` — принятые в receive-loop-е (вкл. handshake-Hello), `to_soul` — отправленные (HelloReply + send-loop). Тип payload в label не выносится — это вопрос для trace, а не для метрики. |
| `keeper_grpc_apply_dispatch_total` | counter | `result` (`ok` / `failed`) | Попытки отправить `ApplyRequest` в Soul (`Outbound.SendApply`): `ok` — enqueue/publish успешен, `failed` — `ErrSoulNotConnected` / `ErrOutboundQueueFull`. Рост `failed` — Soul-ы недоступны / очереди переполнены. |
| `keeper_grpc_bootstrap_total` | counter | `result` (`ok` / `failed`) | Онбординг-попытки Soul через unary `Bootstrap` (отдельный listener, server-only TLS — **не** EventStream): `ok` — seed выпущен и Soul присоединился, `failed` — любой не-ok исход (токен / CSR / Vault / tx). Анти-enum онбординга: детализация причины — в trace/log, не в label. |

**Spans (in-process).** Dispatch `ApplyRequest` оборачивается span-ом `grpc.apply_dispatch`; онбординг через `Bootstrap`-RPC — span-ом `grpc.bootstrap` (оба через глобальный tracer `otel.Tracer("keeper/grpc")`, провайдер поднят `SetupOTel`). Атрибуты span-а — `sid` / `apply_id` (доменные идентификаторы, в metric-labels запрещены §2.2, в trace-атрибутах — штатно); секретов не несёт. Span — на единицу dispatch-а / онбординга, не на весь long-lived стрим. При OTel disabled глобальный tracer no-op — span-ы бесплатны, код не ветвится. Cross-process trace-propagation Keeper→Soul (через gRPC-метаданные, §1.2) — отдельная задача (нужно proto-поле `traceparent`), в этот slice не входит.

#### Keeper · scenario (runner, ADR-009)

Регистратор — `RegisterScenarioMetrics(reg *obs.Registry) *ScenarioMetrics` в [`keeper/internal/scenario/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует Keeper-side прогон scenario (run-goroutine).

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_scenario_runs_total` | counter | `result` (`ok` / `failed` / `locked`) | Завершённые прогоны scenario по терминалу: `ok` — state закоммичен; `failed` — прогон провалился, incarnation переведена в `error_locked`; `locked` — прогон отклонён до старта (incarnation уже `applying` / `error_locked`). Имена incarnation/scenario в label НЕ кладём (cardinality-blow-up по числу инкарнаций/сценариев) — их разрез несёт span `scenario.run`. |
| `keeper_scenario_run_duration_seconds` | histogram (`DefBuckets`) | — | Длительность прогона scenario (от старта run-goroutine до терминала). Наполняется только реально стартовавшими прогонами; `locked` (отклонён до старта) в histogram не попадает. Разрез по результату не нужен — для p99 хватает общей серии. |

**Spans (in-process).** Прогон оборачивается span-ом `scenario.run` через tracer `otel.Tracer("keeper/scenario")`. Атрибуты — доменные идентификаторы прогона (incarnation/scenario name), запрещённые в metric-labels (§2.2).

#### Keeper · RBAC (ADR-028)

Регистратор — `RegisterRBACMetrics(reg *obs.Registry) *RBACMetrics` в [`keeper/internal/rbac/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует RBAC-подсистему: пересборку снимка каталога (`Holder.Refresh`), permission-проверки (`Holder.Check` — единственная точка проверки, через неё идут и api-middleware, и MCP) и cluster-инвалидацию (`Holder.WatchInvalidations`). Дескриптор инжектится сеттером `Holder.SetMetrics` в `setupMetricsRegistry` daemon-а (после создания registry, который поднимается позже `NewHolder` — init-order, паттерн `vault.Client.SetMetrics`).

**Кардинальность (инвариант).** Ни одного label с `aid` / `permission` / `role_name` / `resource` / `action` — только closed-enum (`kind`: `load`/`parse`; `result`: `allow`/`deny`). Кто и что проверял — в audit-log, не в метрику.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_rbac_snapshot_rebuild_duration_seconds` | histogram (`DefBuckets`) | — | Длительность одной пересборки снимка (`Holder.Refresh`: `src.Load` из БД → `NewEnforcerFromSnapshot`). Засекается на успехе и на отказе. |
| `keeper_rbac_snapshot_rebuild_errors_total` | counter | `kind` (`load` / `parse`) | Неуспешные пересборки: `load` — `src.Load` (БД недоступна / ошибка SELECT-ов), `parse` — `NewEnforcerFromSnapshot` (невалидная permission после рассинхрона версий каталога). Фаза известна caller-у точно — не угадывается по типу ошибки. |
| `keeper_rbac_snapshot_last_success_timestamp_seconds` | gauge (Unix-time) | — | Время последней УСПЕШНОЙ пересборки. **Возраст снимка считается в PromQL как `time() - keeper_rbac_snapshot_last_success_timestamp_seconds`** — отдельную `_age_seconds`-метрику сознательно не заводим (gauge-возраст «протух» бы между scrape-ами). |
| `keeper_rbac_snapshot_roles` | gauge | — | Число ролей в актуальном снимке (на каждый успешный `Holder.Refresh`). |
| `keeper_rbac_snapshot_operators` | gauge | — | Число операторов с ≥1 ролевой привязкой в актуальном снимке. AID без привязок = default-deny, в счёт не идёт. |
| `keeper_rbac_checks_total` | counter | `result` (`allow` / `deny`) | Permission-проверки `Holder.Check`: `allow` — `err==nil`; `deny` — любой не-nil error (явный `ErrPermissionDenied` и misconfigured-call сводятся к `deny`, наружу оба = 403). Горячий путь admin-API/MCP — инкремент только один nil-safe `Inc`. |
| `keeper_rbac_invalidations_received_total` | counter | — | Принятые cluster-wide RBAC-инвалидации (pub/sub-сигналы в `Holder.WatchInvalidations`, до запуска перечита). Self-origin отфильтрован источником. |

#### Keeper · service-registry (реестр Service-ов/keeper_settings, ADR-029)

Регистратор — `RegisterRegistryMetrics(reg *obs.Registry) *RegistryMetrics` в [`keeper/internal/serviceregistry/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Зеркало RBAC-Holder-метрик для снимка реестра Service-ов: пересборка снимка (`Holder.Refresh`: `ListServices` + `GetSetting` из БД) и cluster-инвалидация (`Holder.WatchInvalidations`). Дескриптор инжектится сеттером `Holder.SetMetrics` в `setupMetricsRegistry` daemon-а (init-order, паттерн `rbac.Holder.SetMetrics`). Геттер `Holder.Resolve` — синхронный горячий путь потребителей scenario; per-`Resolve` метрику/span сознательно НЕ заводим (overhead на каждый резолв; снимок-метрики на rebuild достаточны).

**Кардинальность (инвариант).** В label-ы НЕ кладём имя Service-а / git / ref / значение настройки — только closed-enum `kind` (`load`/`parse`). Снимок реестра — публичный каталог, не секрет, но cardinality по числу Service-ов в метриках недопустима.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_serviceregistry_snapshot_rebuild_duration_seconds` | histogram (`DefBuckets`) | — | Длительность одной пересборки снимка реестра (`Holder.Refresh`: `src.Load` из БД). Засекается на успехе и на отказе. |
| `keeper_serviceregistry_snapshot_rebuild_errors_total` | counter | `kind` (`load` / `parse`) | Неуспешные пересборки: `load` — `src.Load` (БД недоступна / ошибка SELECT-ов); `parse` зарезервирован под будущий типизированный декодер строк (текущий `PoolSource.Load` отдельной parse-фазы не имеет). |
| `keeper_serviceregistry_snapshot_last_success_timestamp_seconds` | gauge (Unix-time) | — | Время последней УСПЕШНОЙ пересборки. Возраст снимка — `time() - keeper_serviceregistry_snapshot_last_success_timestamp_seconds` в PromQL (симметрия с RBAC). |
| `keeper_serviceregistry_snapshot_services` | gauge | — | Число Service-ов в актуальном снимке (на каждый успешный `Holder.Refresh`). |
| `keeper_serviceregistry_invalidations_received_total` | counter | — | Принятые cluster-wide инвалидации реестра (pub/sub-сигналы в `Holder.WatchInvalidations`, до запуска перечита). Self-origin отфильтрован источником. |

#### Keeper · render (CEL+text/template-пайплайн, ADR-010)

Регистратор — `RegisterRenderMetrics(reg *obs.Registry) *RenderMetrics` в [`keeper/internal/render/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует Keeper-side render-пайплайн scenario (vault-resolve → CEL-render → резолв `on`/`where` → сборка плана). Дескриптор инжектится конструктор-параметром `NewPipeline` (один Pipeline на keeper-инстанс).

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_render_duration_seconds` | histogram (`DefBuckets`) | — | Длительность одного прохода `Pipeline.Render` в секундах (vault-resolve → CEL-render → резолв `on`/`where` → сборка плана) — самая тяжёлая Keeper-side фаза прогона (тот же горизонт, что у span-а `render.pipeline`). Разрез по результату не нужен — для p99 хватает общей серии; имена incarnation/scenario в label НЕ кладём (cardinality-blow-up) — их разрез несёт span. |
| `keeper_render_errors_total` | counter | — | Неуспешные проходы `Pipeline.Render` (любой не-nil error: `ErrUnsupportedDSL` / vault-resolve-fail / CEL-fail / host-инвариант-fail). Без label-а причины — детализация уходит в trace/log (span `render.pipeline` ставит `codes.Error`); counter держим для алерта на rate ошибок рендера. |

#### Keeper · vault (чтение KV v1/v2, ADR-017)

Регистратор — `RegisterVaultMetrics(reg *obs.Registry) *VaultMetrics` в [`keeper/internal/vault/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует keeper-side Vault-клиент (чтение KV v1/v2, версия mount-а определяется автоматически: CEL `vault()`, `vault:`-ref, `core.vault.kv-read`, чтение JWT-signing-key). Дескриптор инжектится сеттером `Client.SetMetrics` (init-order: registry поднимается позже клиента, тот же паттерн, что `Holder.SetMetrics`).

**Кардинальность (инвариант, ADR-024 §2.2 + «безопасность на первом месте»).** В label-ы НЕ кладём ни значение секрета, ни логический KV-путь (путь часто несёт имя секрета и высокую кардинальность). Разрез — только `mount` (closed enum, 1-2 значения на keeper: `secret`-default) и `kind` ошибки (closed enum `notfound`/`error`).

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_vault_read_duration_seconds` | histogram (`DefBuckets`) | `mount` | Латентность одного `Client.ReadKV` в секундах (round-trip до Vault), разрезанная по `mount`. Горячий путь резолва секретов. |
| `keeper_vault_read_errors_total` | counter | `mount`, `kind` (`notfound` / `error`) | Неуспешные `Client.ReadKV`: `notfound` — `ErrVaultKVNotFound` (путь отсутствует/удалён, штатный исход), `error` — транспортная/прочая ошибка чтения. Деталь причины (сам путь) — в log/trace caller-а, не в метрику; алертить на `notfound` и `error` надо по-разному. |

#### Keeper · Augur (брокер AugurRequest, ADR-025)

Регистратор — `RegisterBrokerMetrics(reg *obs.Registry) *BrokerMetrics` в [`keeper/internal/augur/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует Keeper-side брокер `AugurRequest` от Soul-а (EventStream-handler `handleAugurRequest`: авторизационный резолв + fetch из vault/prometheus/elk). Дескриптор регистрируется в `setupMetricsRegistry`, инжектится в `keepergrpc.AugurDeps.Metrics` в `setupGRPCEventStream` (паттерн `grpcMetrics`/`scenarioMetrics`).

**Кардинальность + безопасность (инвариант, augur.md §8).** В label-ы НЕ кладём `omen_name` / `query` / `sid` / `apply_id` / `request_id` / значение секрета — только closed-enum `source` (`vault`/`prometheus`/`elk`/`unknown`) и `decision` (`ok`/`denied`/`error`). Кто и что запрашивал — в audit-log/trace.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_augur_fetch_total` | counter | `source` (`vault` / `prometheus` / `elk` / `unknown`), `decision` (`ok` / `denied` / `error`) | Обработанные `AugurRequest`-ы: `ok` — доступ разрешён И fetch успешен; `denied` — резолв отклонил доступ; `error` — инфраструктурный сбой (резолв/fetch упал, concurrency-limit). `source=unknown` — тип Omen-а ещё не определён в момент учёта (Omen не найден / семафор переполнен до резолва). |
| `keeper_augur_fetch_duration_seconds` | histogram (`DefBuckets`) | `source` | Длительность обработки одного `AugurRequest` (резолв + fetch), по `source` (vault-KV дёшев, prom/elk — внешний HTTP). Разрез по decision не нужен — для p99 хватает per-source серии. |

**Spans (in-process).** Обработка `AugurRequest` оборачивается span-ом `augur.request` через tracer `otel.Tracer("keeper/augur")`. Атрибуты — `sid` + closed-enum `source_type` / `decision`; `omen_name` / `query` / значение секрета в span НЕ кладутся (augur.md §8, ADR-024 §2.2).

#### Keeper · Oracle (reactor-роутер beacons, ADR-030)

Регистратор — `RegisterOracleMetrics(reg *obs.Registry) *OracleMetrics` в [`keeper/internal/oracle/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует Keeper-side reactor-роутер Oracle (EventStream-handler `handlePortentEvent`: приём `PortentEvent` → match по реестру Decree → постановка named-scenario в work-queue, ADR-030 S2/S4). Дескриптор регистрируется в `setupMetricsRegistry`, инжектится в `keepergrpc.OracleDeps.Metrics` в `setupGRPCEventStream` (паттерн `augurMetrics`).

**Кардинальность + безопасность (ADR-024 §2.2).** В label-ы НЕ кладём `decree` / `sid` / `apply_id` / `beacon` / payload — это high-cardinality (десятки тысяч хостов × правил) и/или недоверенный вход (Soul может быть скомпрометирован). Все collector-ы — без label-ов (один Oracle-поток). Кто именно сработал — в audit-log (`oracle.fired` / `decree.circuit_tripped`) и trace.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_oracle_portents_received_total` | counter | — | Принятые `PortentEvent`-ы (с непустым `beacon_name`). Знаменатель прочих: сколько beacon-событий вообще дошло до reactor-а. |
| `keeper_oracle_decrees_matched_total` | counter | — | Decree-срабатывания, прошедшие весь фильтр (subject-match + membership + where-CEL + НЕ в cooldown) и дошедшие до постановки. Инкрементируется per-Decree (один Portent может сматчить несколько). Меньше `portents_received` из-за default-deny. |
| `keeper_oracle_scenarios_enqueued_total` | counter | — | Named-scenario, успешно поставленные в work-queue (ADR-027) Oracle-реакцией. Равно числу записанных fire-ов; расхождение с `decrees_matched` — сбои enqueue. |
| `keeper_oracle_cooldown_blocked_total` | counter | — | Decree-срабатывания, отсечённые cooldown-ом per-(decree, subject) (loop-prevention, ADR-030(a)). Рост — частые edge-события на одном правиле. |
| `keeper_oracle_circuit_tripped_total` | counter | — | Авто-disable Decree circuit-breaker-ом (ADR-030(a): N срабатываний за окно → `enabled=false` + alert). Любой ненулевой прирост — нештатная ситуация (правило сорвалось в петлю), alert-кандидат. |

#### Keeper · Sigil (ключи подписи + ротация, ADR-026(h))

Регистратор — `RegisterKeyMetrics(reg *obs.Registry) *KeyMetrics` в [`keeper/internal/sigil/keymetrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует реестр trust-anchor-ключей подписи Sigil (R3 multi-anchor ротация). Дескриптор регистрируется в `setupMetricsRegistry` (nil при выключенном Sigil — collectors не публикуются), один экземпляр шарится: gauge active-ключей обновляет `KeyService` (после мутации реестра), re-broadcast-наблюдаемость — daemon из `reloadAnchors` (pub/sub-сигнал И TTL-fallback-тик).

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_sigil_signing_keys_active` | gauge | — | Текущее число active trust-anchor-ключей подписи (`status='active'`). Closed-набор (единицы ключей на кластер), без разреза по label-ам. |
| `keeper_sigil_anchors_rebroadcast_total` | counter | — | Проходы re-broadcast-а набора якорей подключённым Soul-ам — на **каждый** `reloadAnchors` (pub/sub-сигнал `sigil:anchors-changed` + TTL-fallback-тик), независимо от того, скольким Soul-ам набор реально доехал. Сигнал «нода перечитала набор и разослала». |
| `keeper_sigil_anchors_last_delivered` | gauge | — | Число Soul-ов, которым **последний** re-broadcast набора якорей ушёл успешно (`Outbound.RebroadcastTrustAnchors` delivered). Операционный сигнал «новый набор разошёлся подключённым Soul-ам, **ПЕРЕД** Retire старого ключа» (Retire-инвариант ADR-026(h), R3-S7). |

**Spans (in-process).** Runtime-ротация (re-build Signer + обновление verify-наборов + re-broadcast) оборачивается span-ом `sigil.anchors_reload` через tracer `otel.Tracer("keeper/sigil")`. Атрибуты `active_anchors` / `rebroadcast_souls` — операционные counts (не label-cardinality, не материал ключа).

#### Keeper · Toll (cluster-wide отток-detector, [ADR-038](adr/0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls))

Имплементация — отдельным slice-ом (ADR зафиксирован 2026-05-26, код вне этого ADR). Метрики зарезервированы здесь для согласованности каталога.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_cluster_degraded` | gauge (0/1) | — | `1` — Toll-leader взвёл флаг `cluster:degraded` (rate disconnect > threshold за 60s окно); `0` — нормальное состояние. Set ТОЛЬКО leader-ом (Redis-lease `cluster:toll:leader` гарантирует exclusive setter; non-leader инстансы не публикуют этот gauge). Не fanout-метрика — closed-набор по cluster-уровню. |
| `keeper_toll_disconnects_total` | counter | `coven` | Не-graceful EventStream-disconnect-ы, наблюдённые tollwatcher-ом (post-filter graceful-shutdown / warmup-immunity). Per-coven cardinality безопасна на counter — это не fanout-флага cluster:degraded, а наблюдательный rate-источник Toll-окна. |

**Spans (in-process).** Fire-event (взвод флага `cluster:degraded`) оборачивается span-ом `toll.degraded_fired` через tracer `otel.Tracer("keeper/toll")` — дёшево, единичные срабатывания. Атрибуты `rate` / `baseline_connected` / `window_seconds` — операционные числа Toll-окна.

#### Keeper · Conductor (исполнитель Cadence, [ADR-048](adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний))

Регистратор — `RegisterConductorMetrics(r *obs.Registry) *ConductorMetrics` в [`keeper/internal/conductor/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует leader-elected исполнитель Cadence-расписаний ([conductor.md](keeper/conductor.md)): тик спавна созревших Cadence → Voyage. Collectors регистрируются **только в ветке поднятого Conductor** (default-ON при Redis и не `cadence_scheduler.enabled: false`) — при не-поднятом Conductor не публикуются вовсе (cardinality-safe, parity Reaper). Без label-ов (один Conductor-поток на инстанс; разрез по инстансу — Prometheus-метка `instance`).

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_conductor_lease_held` | gauge | — | `1` если этот инстанс держит Redis-lease `conductor:leader`, иначе `0`. Cluster-wide инвариант: `sum(keeper_conductor_lease_held) == 1` (при ровно одном лидере). Set ТОЛЬКО при смене лидерства (OnLeaseChange). Независим от `keeper_reaper_lease_held` — держатели lease-ов могут различаться. |
| `keeper_conductor_spawn_executions_total` | counter | — | Тики спавна Conductor-лидера за uptime инстанса. Инкрементируется на **каждый** тик, независимо от наличия due-расписаний. Знаменатель «эффективности»: много тиков при нулевом `spawned_total` = расписаний нет либо все `skip`/`queue`. |
| `keeper_conductor_spawned_total` | counter | — | Voyage, **реально заспавненные** из созревших Cadence (`skip`/`queue`-тики не считаются — affected = «сколько прогонов создано»). |
| `keeper_conductor_spawn_errors_total` | counter | — | Ошибки тика спавна (`Spawner.Run` вернул error: PG-сбой / резолв target). Выделено из `spawn_executions_total`, чтобы алертилось без histogram-а. Любой ненулевой прирост — alert-кандидат. |
| `keeper_conductor_spawn_duration_seconds` | histogram (`0.005…30s`) | — | Длительность тика спавна (`Spawner.Run`): SELECT due + per-row insert — единицы-десятки ms; верх 30s ловит аномально долгий тик. `_count` совпадает с `spawn_executions_total`. |

#### Keeper · Tempo (per-AID rate-limiter write-API, [ADR-050](adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api))

Регистратор — `RegisterTempoMetrics(reg *obs.Registry) *TempoMetrics` в [`keeper/internal/api/tempo_metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Counters инкрементируются middleware на каждом разрешённом / отклонённом запросе resolver-тяжёлого write-эндпоинта (конфиг — [config.md → tempo](keeper/config.md#tempo)). Регистрируются **безусловно** (registry всегда поднят): при выключенном Tempo (нет Redis / `tempo.enabled: false`) counters остаются на 0 — валидный сигнал «лимитер не активен».

Лейбл `endpoint` = логическое bucket-имя (`voyage_create`); **AID-лейбла НЕТ** — число операторов не ограничено, AID в лейбле взорвал бы кардинальность time-series. Кто именно превышает — видно в audit/логах по `claims.Subject`, не в метриках.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `keeper_tempo_allowed_total` | counter | `endpoint` | Запросы, пропущенные Tempo-лимитером (токен взят из per-AID-бакета). `endpoint` = bucket-имя (`voyage_create` обслуживает `POST /v1/voyages` + `/v1/voyages/preview`). |
| `keeper_tempo_rejected_total` | counter | `endpoint` | Запросы, отклонённые Tempo-лимитером (бакет пуст → `429 tempo-exceeded` + `Retry-After`). Рост — оператор бьёт API чаще `rate`+`burst`. Fail-open-проходы при Redis-сбое НЕ считаются rejected (passthrough) и НЕ увеличивают ни один из двух counter-ов. |

#### Soul · apply (apply-цикл, ADR-012/ADR-015)

Регистратор — `RegisterApplyMetrics(reg *obs.Registry) *ApplyMetrics` в [`soul/internal/runtime/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует apply-цикл Soul-демона.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `soul_apply_tasks_total` | counter | `result` (`ok` / `changed` / `failed`) | Завершённые задачи прогона: `changed` — задача внесла изменения; `failed` — упала / timeout / cancelled (терминальные не-успехи сводятся к `failed`); `ok` — без изменений. `apply_id` / `sid` в label НЕ кладём (cardinality §2.2) — их разрез несёт span `apply.run`. |
| `soul_apply_duration_seconds` | histogram (`0.05…300s`) | — | Длительность одного прогона (`Run` целиком). Buckets шире `keeper_http` — apply тяжелее HTTP-запроса (пакеты / файлы / сервисы), верх до 300s ловит хвост (компиляция / большой архив). |
| `soul_apply_task_retries_total` | counter | — | Повторные попытки `runTask` (DSL-ядро `retry:` / `until:`, [tasks.md §9](destiny/tasks.md)), без учёта первой попытки. Рост — нестабильные задачи / flaky-хосты. Без label-ов: разрез по task/apply_id — cardinality (§2.2). |
| `soul_apply_task_skipped_total` | counter | `reason` (`when` / `requisite` / `failed_run`) | Задачи, пропущенные gating-ом flow-control (`mod.Apply` не вызывался): `when` — `when:` дал false; `requisite` — `onchanges:` / `onfail:` не сработал; `failed_run` — прогон уже провален, не-`onfail`-задача пропущена fail-stop-ом. Closed enum — gating-цепочка Soul-а. |
| `soul_apply_task_timed_out_total` | counter | — | Задачи, завершившиеся таймаутом (`TASK_STATUS_TIMED_OUT`), по ФИНАЛЬНОМУ исходу (после исчерпания retry, не на каждую попытку). Выделена из общего `failed`-результата `soul_apply_tasks_total`: таймаут = особый сигнал «висит», отдельная серия удобна для алертов. |

**Spans (in-process).** Прогон оборачивается span-ом `apply.run` через tracer `otel.Tracer("soul/runtime")`. Атрибуты — `apply_id` / `sid` (в metric-labels запрещены §2.2).

#### Soul · EventStream (gRPC-клиент, ADR-002/ADR-012)

Регистратор — `RegisterEventStreamMetrics(reg *obs.Registry) *EventStreamMetrics` в [`soul/internal/grpc/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует Soul-side EventStream-клиент (connection-state).

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `soul_eventstream_connected` | gauge (0/1) | — | `1` — EventStream-сессия Soul↔Keeper установлена (handshake завершён); `0` — разрыв / реконнект. Connection-state Soul-агента одномерен (один Keeper-стрим за раз) — label-ов нет, разрез по KID/session уходит в trace/log. |
| `soul_eventstream_reconnects_total` | counter | — | Попытки реконнекта после первичного подключения (каждый `Dial` reconnect-loop-а). Рост — сигнал нестабильного канала / недоступности Keeper-кластера. |

#### Soul · soulprint (сбор фактов, ADR-018)

Регистратор — `RegisterSoulprintMetrics(reg *obs.Registry) *SoulprintMetrics` в [`soul/internal/soulprint/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует сбор фактов о хосте.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `soul_soulprint_collections_total` | counter | `result` (`ok` / `failed`) | Снимки фактов о хосте. `Collect` best-effort и error не возвращает (ADR-018) → сейчас инкрементируется только `ok`; `failed` зарезервирован под будущие fatal-сценарии сбора (closed enum). `sid` в label НЕ кладём — разрез по хосту в OTel resource-attrs (§3). |
| `soul_soulprint_collect_duration_seconds` | histogram (`0.001…5s`) | — | Длительность одного снимка фактов (`Collect`). Сбор лёгкий (чтение `/proc`, `/etc/os-release`, `net.*`) — узкие buckets внизу ловят норму; верх до 5s — на случай медленного FQDN/DNS-резолва на проблемном хосте. |

#### Soul · beacon (scheduler beacons, ADR-030)

Регистратор — `RegisterBeaconMetrics(reg *obs.Registry) *BeaconMetrics` в [`soul/internal/beacon/metrics.go`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам). Инструментирует per-process beacon-scheduler Soul-демона (Vigil-проверки → edge-triggered `PortentEvent` в канал, ADR-030 S1/S4). Дескриптор регистрируется на soul-registry в `main`, инжектится в `beacon.SchedulerConfig.Metrics`.

| Метрика | Тип | Labels | Смысл |
|---|---|---|---|
| `soul_beacon_portents_dropped_total` | counter | — | `PortentEvent`-ы, отброшенные при переполнении буфера канала (`Scheduler.emit` drop-ветка): writer-loop EventStream-а отстаёт либо нет активной сессии надолго. Дроп edge-triggered события — потеря одного перехода (следующая смена State снова поднимет Portent); ненулевой рост — сигнал «реакции теряются», alert-кандидат. Без label-ов: разрез по vigil-name — high-cardinality (§2.2). |

## 5. Что отложено

- **Конкретный каталог метрик** по подсистемам — по факту имплементации (см. §4).
- **OpenMetrics exposition-format** — не включён; включится при понятном triple-test от пользователя.
- **OTLP-push метрик** (`otel.export_metrics: bool`) — поле конфига заведено в обоих бинарях ([keeper/config.md](keeper/config.md#otel) / [soul/config.md](soul/config.md)), но **OTLP-метрик-pipeline ещё не поднимается**: в текущем slice через OTel экспортируются только трейсы, метрики — только через Prometheus-scrape. Реальный push метрик — отдельной задачей.
- **Sampling-конфиг** (`otel.sampler`) — поле не введено; дефолт зашит в коде (`ParentBased(AlwaysSample)` в [`shared/obs`](adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). Конфигурируемый сэмплер — при первом реальном запросе.
- **Exemplars** (связка histogram-bucket ↔ trace-id) — post-MVP, не блокирует MVP.
