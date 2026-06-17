# ADR-021. Hot-reload конфига с write-back YAML

- **Контекст.** [`docs/requirements.md`](../requirements.md) формулирует **двухчастное** сквозное требование: (1) «горячее изменение конфигурации» — runtime-change без рестарта; (2) «перезапись конфигурации на диске после применения» — после in-memory-мутации оператором новое значение пишется обратно в YAML. Это **нестандартный hot-reload**: помимо классического `file → reload-on-signal` нужна API-driven мутация с write-back YAML, сохраняющая комментарии и порядок ключей. [`docs/keeper/config.md → ## Hot-reload`](../keeper/config.md#hot-reload) и [`docs/soul/config.md → ## Hot-reload`](../soul/config.md#hot-reload) до этого ADR были помечены как **«механизм архитектурно не закреплён»** — TBD. Раздел [`§ Hot-reload блока plugin_runtime:`](../keeper/config.md#hot-reload-блока-plugin_runtime) ([ADR-020(d)](0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)) уже фиксирует per-поле политику для одного блока, но общего механизма нет. Без нормирования имплементация [`shared/config`](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) (ADR-011) и keeper-side config-server будут угадывать поведение.

- **Решение.**

  **(a) Два пути изменения конфига.** В MVP поддерживаются оба и нормированы единообразно:

  | Путь | Триггер | Pipeline |
  |---|---|---|
  | **File-edit** | Оператор редактирует `keeper.yml` / `soul.yml` на хосте → шлёт `SIGHUP` процессу. | parse → schema-validate → semantic-validate → atomic swap → audit. |
  | **API/MCP** | Оператор вызывает OpenAPI/MCP-эндпоинт мутации конфига → host меняет in-memory, валидирует, atomic swap, **write-back YAML**, audit. | mutate → schema-validate → semantic-validate → atomic swap → write-back → audit. |

  Конкретные API endpoints / MCP tool-имена для пути API — **в этом ADR не нормируются**, отложены до имплементации Operator API (см. Consequences).

  **(b) Триггер file-edit — SIGHUP в MVP.** Стандартный POSIX-сигнал, всегда работает, минимальный overhead. **inotify / fsnotify** — post-MVP, опциональное расширение для auto-reload без сигнала; включение через флаг `hot_reload.enable_inotify: true` (нормативная типизация блока — [`docs/keeper/config.md → hot_reload`](../keeper/config.md#hot_reload) и [`docs/soul/config.md → hot_reload`](../soul/config.md#hot_reload)). Linux-only зависимость и watch-handle overhead — причина не делать дефолтом.

  **(c) Validation pipeline + atomic swap.** Атомарность гарантируется тремя этапами валидации **до** swap; swap — отдельный шаг.

  Validation pipeline:

  1. **Parse YAML** — синтаксис, типы YAML-нодов. Ошибка → audit-event `config.reload_failed` с `phase: parse`, in-memory state не трогается.
  2. **Schema-validate** — типы полей, regex, enum-значения по таблицам в [`docs/keeper/config.md`](../keeper/config.md) / [`docs/soul/config.md`](../soul/config.md). Ошибка → `phase: schema_validate`.
  3. **Semantic-validate** — кросс-поля и сетевые проверки (например, новый `vault.addr` доступен; новый Postgres DSN отвечает). Ошибка → `phase: semantic_validate`.

  После прохождения всех трёх этапов — **atomic swap** через `sync/atomic.Pointer[Config]` или эквивалент. Все читатели после swap видят новый снимок, читатели до swap дочитывают со старого без блокировки.

  Любая ошибка любого этапа → in-memory state неизменен, файл на диске не модифицируется (даже для API-path — см. (d) и Trade-offs), audit-event `config.reload_failed` с `validation_errors[]` (тип ошибки + path + сообщение).

  **(d) Write-back YAML — только для API/MCP-path.** File-edit path уже имеет валидный файл на диске, переписывать его нечего. API-path обязан:

  - **Round-trip preservation** — комментарии, порядок ключей, anchors сохраняются. Конкретная Go-библиотека (`gopkg.in/yaml.v3` / `goccy/go-yaml` / др.) — выбор имплементации `shared/config` в Tier 2; ADR-021 фиксирует только инвариант round-trip.
  - **Atomic rename** — write-to-tmp в той же директории + `rename(2)` → атомарная замена. Защита от частично записанного файла при crash или power-loss. Tmp-имя не имеет ABI-гарантий, но live-имя файла после `rename` целевое.
  - **Permissions** — наследуются от исходного файла (`stat` исходного + `chmod` нового перед rename).
  - **Write-back порядок:** swap → write-back. Если write-back упал (диск full, permission denied), in-memory state **уже сменён** — `config.reload_succeeded` фиксирует это. **MVP-поведение:** write-back-failure отражается дополнительным `config.reload_failed` с `phase: semantic_validate` и явным сообщением в `validation_errors[]` («write-back failed: <reason>») — отдельного `phase: write_back` в MVP **нет**. **Backlog:** добавление `phase: write_back` как отдельного значения enum-а — при первом реальном запросе после имплементации `shared/config`.

  **(e) Scope: reload-able vs require restart — общий принцип.** Per-блок таблицы нормируются в [`docs/keeper/config.md`](../keeper/config.md) / [`docs/soul/config.md`](../soul/config.md) по каждому блоку (как уже сделано для `plugin_runtime:` — [`Hot-reload блока plugin_runtime:`](../keeper/config.md#hot-reload-блока-plugin_runtime)). ADR-021 фиксирует общий принцип:

  - **Reload-able без рестарта** — параметры конкретного запуска / операции / прогона: timeouts, thresholds, policies, capabilities whitelist, retry-backoff, размер batch-а Reaper-а, и т.п. Меняются in-memory, новые операции видят новое значение, in-flight — дорабатывают со старым.
  - **Require restart** — внешняя поверхность процесса: listener-address, socket paths, TLS-сертификаты файлов keystore (без on-disk-наблюдения), БД connection-strings, log-rotation file paths. Менять во время работы без рестарта — нарушает invariants открытых соединений.

  Полные per-блок таблицы для всех блоков `keeper.yml` / `soul.yml` — **отложены отдельной задачей** (см. Consequences). В этом ADR — только принцип + краткое summary в `## Hot-reload` обоих config.md.

  **(f) Multi-host coordination — per-host без cross-host.** Каждый Keeper-инстанс в HA-кластере ([ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) читает свой локальный `keeper.yml` независимо. Конфиг-файлы хранятся per-host (не в shared storage), reload идёт отдельно на каждом инстансе. То же для Soul: каждый Soul-host перезагружает свой `soul.yml` независимо.

  Cross-host координация (cluster-wide «reload by event» через Redis pub/sub, две-фазный commit конфига между инстансами Keeper-а) — **отложена post-MVP**. В MVP оператор обновляет конфиг на каждом инстансе явно (через CI, SSH-rollout-скрипт или последовательные API-вызовы). Согласованность конфига между инстансами — operational concern, не runtime-инвариант.

  **(g) Audit-trail — два audit-event-имени.** Один event per reload-attempt:

  - **`config.reload_succeeded`** — поля `source` (`signal|api|mcp`, closed enum нормирован в [ADR-022(b)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention); `signal` — оператор у клавиатуры на хосте, `api`/`mcp` — Архонт через HTTP/MCP), `archon.aid` (для `source: api|mcp`; пусто для `source: signal`), `changed_paths` (список YAML-paths изменённых полей, например `["postgres.pool.max", "reaper.interval"]`), `correlation_id`.
  - **`config.reload_failed`** — поля `source`, `archon.aid` (если применимо), `validation_errors[]` (тип ошибки + path + сообщение), `phase` (`parse | schema_validate | semantic_validate`).

  Convention — `<area>.<action>` (lowercase, dots), как у permissions ([rbac.md → Формат permissions](../keeper/rbac.md#формат-permissions)). Каталог audit-event-имён — [`docs/naming-rules.md`](../naming-rules.md). Поле `correlation_id` — ULID, общий механизм бизнес-корреляции audit-цепочки (не OTel trace-id); форма и общий audit-pipeline (storage, schema, write-path, retention) нормированы в [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention).

  **(h) `shared/config`-пакет — общий для трёх бинарей.** По [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) `shared/config/` — Go-модуль, общий для `keeper` / `soul` / `soul-lint`:

  - Парсинг YAML с round-trip-aware-структурой (если будет реализован write-back).
  - Schema-валидация по таблицам в config.md.
  - Atomic swap helpers (`sync/atomic.Pointer[T]` обёртка с reader-helpers).
  - Write-back helpers (используются только `keeper`/`soul` server-side, не `soul-lint`).
  - `soul-lint` использует тот же парсер + валидатор без atomic swap / write-back — статическая валидация конфига без поднятия процесса.

  Конкретная имплементация и публичный API `shared/config/` — отдельная задача Tier 2.

  **(i) History изменений — git-blame + audit в MVP.** Для file-edit path — YAML-файл живёт в git репо оператора (CI-managed deploy), история строится git-blame. Для API-path — audit-trail в Postgres (`config.reload_succeeded` записи с `archon.aid` / `changed_paths`) даёт «кто, когда, что изменил».

  Отдельная БД-таблица `config_history` со snapshots **отложена post-MVP** — не блокирует MVP, добавляется без breaking changes при первом реальном запросе (compliance-аудит / rollback по таймстампу).

  **(j) Симметрия для Soul.** Soul-host в **pull-режиме** читает локальный `soul.yml` — hot-reload-механизм симметричен Keeper-side: SIGHUP / atomic swap / audit. **API-driven мутация на Soul-host-е в MVP не предусмотрена** — Soul admin-surface (локальный HTTP/MCP listener или push-команда `ConfigReload` через Keeper↔Soul gRPC) отложено post-MVP, см. [open Q №8](../architecture.md#текущие). Централизованный rollout `soul.yml` в MVP — через CI / Ansible / SSH-доставку нового файла + `SIGHUP`; этого достаточно для типовых scenarios конфиг-менеджмента.

  В **push-режиме** ([Push-режим (`keeper.push`)](../architecture.md#push-режим-keeperpush)) `soul.yml` на удалённом хосте не используется (Soul поднимается one-shot, конфигурация приходит от Keeper-а через stdin) — hot-reload **не применим** в push.

- **Consequences.**
  - **Два новых audit-event-имени** в [`docs/naming-rules.md`](../naming-rules.md) — `config.reload_succeeded`, `config.reload_failed`. Открывают каталог audit-events; имена остальных подсистем добавляются по мере появления.
  - **`shared/config`-пакет** — отдельная задача имплементации Tier 2 (parser, schema-validator, atomic swap, write-back). Используется тремя бинарями.
  - **Subscribe API на reload-swap.** Поверх poll-style `Store[T].Get()` `shared/config` предоставляет opt-in callback-API: `Store[T].OnReload(fn ReloadCallback[T]) (unsubscribe func())`. Callback вызывается **только** на `Swapped=true`, в отдельной goroutine per-subscriber (slow subscriber не блокирует других + recover-panic), порядок не гарантируется. Аргументы — `old`/`new *T`, snapshot-указатели, мутировать запрещено. Применение: убирает латентность «следующего tick-а / следующего запроса» там, где компонент должен реагировать на reload **немедленно** (например, `keeper/internal/reaper.Runner` — кэширует cfg в `atomic.Pointer` и обновляет из callback-а). Не ломает backward compat: существующие consumer-ы на `Get()` продолжают работать без изменений. **RBAC исключён из этого пути ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)):** после переноса RBAC-storage в Postgres reload RBAC = БД-мутация + Redis pub/sub-инвалидация снимка enforcer-а, а не `SIGHUP` / config-swap; `keeper/internal/rbac.Holder` перестраивается из БД-инвалидации, не из config-reload-callback-а.
  - **Per-блок reload-policy таблицы** — отдельная задача нормирования. В этом цикле дополняются только разделы `## Hot-reload` обоих config.md (нормативный текст + ссылка на ADR-021).
  - **Опциональный блок `hot_reload:`** в `keeper.yml` / `soul.yml` (поля `enable_signal: bool default true`, `enable_inotify: bool default false`, `audit_correlation_id: bool default true`) — нормирован в [`docs/keeper/config.md → hot_reload`](../keeper/config.md#hot_reload) и [`docs/soul/config.md → hot_reload`](../soul/config.md#hot_reload). Все три поля require restart (контролируют сам hot-reload-механизм — race на signal-handler / fsnotify watch). Defaults встроены в `shared/config`, блок опционален.
  - **API/MCP-эндпоинты мутации конфига** (`config.set` и подобные) — отложены до Operator API (Tier 2 / post-MVP).
  - Раздел [«Сквозные требования»](../architecture.md#сквозные-требования-и-где-они-приземляются) расшифровывает «hot-reload + write-back на диск» через ссылку на ADR-021.

- **Trade-offs.**
  - **YAML round-trip vs полная regeneration.** Round-trip требует round-trip-aware-парсера (тяжелее обычного `yaml.Unmarshal`), часть YAML-edge-cases (например, exotic anchors с aliases на сложные структуры) может терять fidelity. Полная regeneration — детерминистичный output, но теряются комментарии, которые оператор писал «зачем здесь это значение». Выбран round-trip — комментарии операционно ценнее идеальной детерминированности.
  - **Per-host vs cross-host координация.** Per-host проще (нет распределённого протокола), но допускает временную inconsistency между Keeper-инстансами кластера (один уже перезагрузил конфиг, другие ещё нет). Принимаем: конфиг-параметры — в основном «runtime tunables», не корреляция-зависимые инварианты; короткое окно расхождения операционно безопасно. Cross-host (через Redis pub/sub) — добавится post-MVP при первом реальном запросе.
  - **`config.reload_failed` rollback на in-memory.** При API-path после ошибки валидации **файл на диске не меняется** (write-back только на success). Оператор видит ошибку в HTTP/MCP-ответе и в audit; in-memory state неизменен. Это сознательная цена: проще, чем «писать, потом откатывать file» (catastrophic при power-loss между write и rollback). Альтернатива «write-first, validate-from-disk» — отвергнута: ломает атомарность.
