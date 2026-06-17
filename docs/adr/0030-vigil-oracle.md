# ADR-030. Vigil + Oracle — event-driven мониторинг (beacons + reactor)

> **Это контракт + дизайн; имплементации event-loop-а нет.** Срез S0 (этот ADR) вводит ТОЛЬКО нормативный дизайн, proto-контракт (`proto/keeper/v1/beacon.proto` + only-add в `EventStream`-oneof) и имена в [naming-rules.md](../naming-rules.md). Soul-scheduler, Oracle-роутер, Postgres-реестры, миграции, OpenAPI/MCP, safety-механизмы — последующие срезы S1–S5 (см. ниже). Закрывает [open Q №23](../architecture.md#открытые-вопросы) и backlog-пункт [ADR-016(d)](0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) (event-driven контур, salt beacons/reactor-эквивалент). Незафиксированное предложение `SoulBeacon` (open Q №23) этим ADR заменено финальными именами.

- **Контекст.** Soul Stack — пакетный декларативный: прогон инициирует оператор (scenario через API/MCP) или планировщик, контур «состояние хоста изменилось → автоматическая реакция» отсутствует. Нет аналога Salt beacons (хост-триггеры «файл изменился / pid пропал / порог метрики достигнут») и reactor (правило «событие → действие»). Это блокирует self-healing-сценарии (упал сервис → перезапустить scenario-ом), реакцию на drift и событийную автоматизацию вообще. Open Q №23 откладывал контур до конкретного запроса; запрос появился. Имена сущностей в [naming-rules.md](../naming-rules.md) не были зафиксированы — нужен propose-and-wait.
- **Сущности (propose-and-wait пройден, внесены в [naming-rules.md](../naming-rules.md)).**
  - **Vigil** — Soul-side проверка (beacon-определение): «что наблюдать и как часто». **Read-only по конструкции** — Vigil НЕ мутирует хост, только наблюдает и поднимает событие. Источник правды — реестр Postgres `vigils` (managed через OpenAPI/MCP, toggle + RBAC; symmetric Augur `omens`/`rites` [ADR-025](0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)). Тело проверки — **встроенные core-beacon** (`core.beacon.file_changed` / `core.beacon.service_down` / …) + **plugin-kind `soul_beacon`** для community-проверок (S5). Набор активных Vigil резолвится по covens хоста и едет ему через `VigilSnapshot`.
  - **Portent** — событие beacon (Soul → Keeper). Едет only-add в `FromSoul.oneof` существующего `EventStream`-а. Payload — `google.protobuf.Struct` в MVP (typed-схема payload откладывается отдельным ADR, как Soulprint в [ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)). **Edge-triggered** — событие поднимается на **смену состояния**, а не на каждый тик проверки (иначе шторм + петли). SID в payload — echo (авторитет — mTLS peer cert, [ADR-012(i)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)).
  - **Oracle** — Keeper-side reactor-роутер. Приём Portent → match по реестру Decree → постановка named-scenario в work-queue ([ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Без собственного исполнения apply — только маршрутизация в существующий work-queue.
  - **Decree** — правило reactor (Postgres-реестр; managed OpenAPI/MCP). **Default-deny** (как Rite [ADR-025](0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)): нет матчащего Decree → событие не вызывает действия. Субъектная привязка — `coven` **XOR** `sid` (как Rite). **Обязательное `incarnation_name`**: scenario оперирует `incarnation.state` (ADR-009), поэтому Decree явно несёт incarnation, над которой прогоняется action — угадывать из subject coven/sid нельзя (хост несёт несколько incarnation-меток; угадывание = amplification-вектор от скомпрометированного Soul). `ServiceRef` резолвится **из** incarnation (ADR-007 — git-ref не дублируется в Decree); реакция ставится на **один SID-отправитель** Portent (не на весь coven); перед enqueue — **membership-check** (subjectSID обязан принадлежать incarnation Decree, иначе skip — fail-closed, fire/audit не пишутся). Опц. `where`-предикат — **keeper-local CEL-sandbox** с единственным корнем `event` (узкий предикат над недоверенным payload; `shared/cel` не расширяется, deny-list как у migration-CEL ADR-019 по духу). **Action = ТОЛЬКО named scenario** (whitelist) через work-queue; raw-команда отвергнута (см. инвариант безопасности).
- **Транспорт — only-add, без нового RPC.** Новый файл `proto/keeper/v1/beacon.proto`; `PortentEvent` добавляется в `FromSoul.oneof payload`, `VigilSnapshot` — в `FromKeeper.oneof payload` ([ADR-012(c)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) forward-compat only-add; [ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) «один стрим» соблюдён). `VigilSnapshot` несёт `repeated VigilDef vigils` (`name` / `interval` / `check` / `params`) и применяется Soul-ом **ReplaceAll** (как `SigilSnapshot` / `SigilTrustAnchors` [ADR-026(h)](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)): полный active-набор для SID заменяет локальный, отсутствующий Vigil Soul-scheduler останавливает. Минимально-расширяемо (only-add позже).
- **ОБЯЗАТЕЛЬНЫЕ ИНВАРИАНТЫ (нормативно, не опционально).**
  - **(a) Loop-prevention.** Триада: **cooldown** на Decree (минимальный интервал между срабатываниями одного правила) + **идемпотентный декларативный scenario-action** (повторный прогон сходится к тому же состоянию и гасит причину события) + **circuit-breaker** (N срабатываний за окно → авто-`disable` Decree + alert). Edge-triggered Portent (событие на смену состояния, не на тик) — первый барьер против шторма.
    - **Нормативный механизм circuit-breaker (S4 Part 1).** Хранилище — таблица Postgres `oracle_circuit(decree PK, window_start TIMESTAMPTZ, fire_count INT)` с FK `decree → decrees(name) ON DELETE CASCADE` (per-decree **fixed-window** счётчик; cooldown ADR-030(a) гасит per-(decree, subject), circuit-breaker считает срабатывания правила СУММАРНО). Инкремент — **атомарный UPSERT** (`INSERT … ON CONFLICT (decree) DO UPDATE` с CASE-ресетом окна при `window_start <= now - window`, `RETURNING fire_count`): один statement под row-lock-ом сериализует read-modify-write — **cluster-safe** без advisory-lock-ов (конкурентные инкременты с разных Keeper-инстансов не теряются). Вызывается в `evaluateDecree` ПОСЛЕ успешного enqueue+RecordFire (только реально поставленное срабатывание считается). При `fire_count >= max_fires` — trip: `UPDATE decrees SET enabled=false WHERE name=$1 AND enabled=true`; `RowsAffected==1` = **single-winner** (ровно один инстанс при конкурентном trip-е пишет metric `keeper_oracle_circuit_tripped_total` + audit `decree.circuit_tripped` + warn-лог). Пороги — глобальные в `keeper.yml`: `oracle_circuit_max_fires` (дефолт 5, **`0` = breaker OFF**, escape-hatch — `BumpCircuit` не вызывается) и `oracle_circuit_window` (дефолт 10m); per-Decree override — отдельный заход. **Re-enable провалившегося Decree (MVP)** = delete+recreate: каскад `ON DELETE CASCADE` чистит `oracle_circuit`, пересозданный Decree стартует с чистого окна (toggle-endpoint — отдельный заход). Само-чистится каскадом, БЕЗ отдельного Reaper-правила. Миграция `042_create_oracle_circuit`.
  - **(b) Security.** **Default-deny Decree** (нет правила → нет действия). **Whitelist = scenario-only** — action может быть ТОЛЬКО зарегистрированный named-scenario; **raw-команда отвергнута** как RCE-вектор: скомпрометированный Soul иначе мог бы поднять Portent, запускающий произвольную команду на Keeper-управляемом контуре. Команды доступны только через `core.exec.run` **внутри** scenario (тот же путь, что у обычного прогона, под теми же гарантиями). **Субъектная привязка** Decree (`coven` XOR `sid`) ограничивает, какие хосты вообще могут триггерить правило. **RBAC** — отдельные permissions на управление Vigil / Decree. **Audit** — категория `soul_grpc` ([ADR-022(b)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)), событие срабатывания Oracle (имя класса — `oracle.fired`-семья, нормируется при S3). **Beacon-событие = недоверенный вход**: Soul может быть скомпрометирован, поэтому Oracle относится к Portent как к input от потенциально враждебной стороны (default-deny + субъектная привязка + scenario-only — слои защиты).
- **Отличие от Augur ([ADR-025](0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)).** Augur = **pull внешнего значения ВО ВРЕМЯ apply** (request/reply, Soul просит — Keeper отвечает синхронно в рамках прогона). Vigil/Oracle = **push внутреннего event-loop по расписанию Soul** (fire-and-forget, Soul наблюдает сам и шлёт Portent, реакция асинхронна через work-queue). Не пересекаются по назначению; общее — только транспорт (оба only-add в `EventStream`-oneof).
- **Реконсиляция ADR.**
  - [ADR-012(c)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) — соблюдён буквально: only-add `PortentEvent` (FromSoul, field 7) + `VigilSnapshot` (FromKeeper, field 9), новые field-номера не reuse, нового RPC нет. Паттерн ReplaceAll-снимка перенят у [ADR-026(h)](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс).
  - [ADR-025](0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul) — ортогонален (pull/apply-time vs push/scheduled); общий транспорт-oneof, разные сообщения. Default-deny + субъект `coven` XOR `sid` Decree зеркалят Rite.
  - [ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) — Oracle не исполняет apply сам, а ставит scenario в существующий work-queue (`apply_runs` + `Summons`); Acolyte подхватывает как обычный прогон. Никакого второго исполнительного контура.
  - [ADR-016(d)](0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — backlog event-driven закрыт; community-проверки — через `soul_beacon` plugin-kind (4-й kind plugin-инфраструктуры, S5), реализация под Sigil-целостность ([ADR-026](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)).
- **Consequences.**
  - **Новый файл `proto/keeper/v1/beacon.proto`** (`PortentEvent` / `VigilDef` / `VigilSnapshot`) + committed `proto/gen/go/keeper/v1/beacon.pb.go`; only-add в `keeper.proto`-oneof.
  - **Имена в [naming-rules.md](../naming-rules.md)** — Vigil / Portent / Oracle / Decree (словарь), `PortentEvent` (FromSoul) / `VigilSnapshot` (FromKeeper) (proto Keeper↔Soul), `soul_beacon` как будущий 4-й plugin-kind.
  - **Будущие срезы вводят** (НЕ в этом ADR): Soul-scheduler + core-beacon (S1), Oracle-роутер + реестры Postgres `vigils`/`decrees` + миграции (S2), OpenAPI/MCP CRUD Vigil/Decree + RBAC-perms + audit (S3), safety-механизмы cooldown/circuit-breaker/метрики (S4), inotify + `soul_beacon`-плагины + Sigil-интеграция + typed-payload (S5).
- **Trade-offs.**
  - **Struct-payload в MVP** (не typed) — как Soulprint до [ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp): быстрее ввести контур, цена — нет статической схемы payload/params. Typed-схема — отдельным ADR при стабилизации набора core-beacon.
  - **Scenario-only action (no raw-команда)** — жертвуем гибкостью «реактор сразу выполняет команду» ради устранения RCE-вектора. Команды остаются доступны через `core.exec.run` в scenario — на один уровень косвенности больше, но под теми же RBAC/audit/idempotency-гарантиями.
  - **Push-модель (fire-and-forget) vs pull (Augur)** — два разных контура с общим транспортом; цена — две ментальные модели событий, выигрыш — каждая под свой use case без натягивания одной на обе.
- **Срезы реализации.** **S0 (этот срез)** — ADR + proto-контракт + имена (pilot). S1 — Soul-scheduler + 1–2 core-beacon (edge-triggered). S2 — Oracle-роутер + реестр Decree + миграция + постановка scenario в work-queue. S3 — OpenAPI/MCP CRUD Vigil/Decree + RBAC + audit. S4 — safety (cooldown / circuit-breaker / метрики). S5 — post-MVP (inotify / `soul_beacon`-плагины + Sigil / typed-payload). Pilot = S0 + S1 + S2.

#### Amendment 2026-05-26 (S5 closure)

Vigil S5 закрывает три open ends ADR-030 одной волной (commits `634b818` +
V5-2/V5-3 + V5-4 docs-amend). После S5 — ADR-030 feature-complete; (iv)
per-Decree cooldown override + (v) toggle-endpoint re-enable Decree + (vi)
metric-threshold-pull beacon — отдельные slices при появлении конкретного
запроса.

**S5 PM-decisions.**

1. **Typed PortentPayload (V5-1).** 6 typed-message (`FileChangedPortent` /
   `ServiceDownPortent` / `PortClosedPortent` / `DiskFullPortent` /
   `ProcessAbsentPortent` / `HttpUnhealthyPortent`) + `custom` Struct в
   `oneof payload` (fields 7..13). Parity с typed Soulprint Facts
   ([ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)). Поле `PortentEvent.data`
   (Struct, field 2) → `[deprecated=true]` физически остаётся; Soul-side
   1-release переходный период заполняет ОБЕ ветки (data + typed); hard-cut
   `data` в S5-final (post-1-release, parity с push S7-decision).

2. **`soul_beacon` plugin-kind (V5-2).** 4-й plugin-kind в plugin-инфре
   (после `SoulModule` / `CloudDriver` / `SshProvider`). Unary RPC
   `ValidateVigil` + `Check` (НЕ stream — сохраняет
   [ADR-020(d)](0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)
   one-shot lifecycle). Plugin spawn Soul-side через
   `soul/internal/pluginhost` (parity с keeper-host). Sigil-trust verify на
   Soul-host через уже доставляемый `SigilTrustAnchors`-snapshot
   ([ADR-026(h)](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)).
   Vigil routing через namespace в `VigilDef.check`: `core.beacon.*` →
   встроенный, остальное → plugin-discovery.

3. **inotify — 7-й core-beacon (V5-3).** Linux-only (build-tag), kernel
   inotify syscall (`golang.org/x/sys/unix`). Fold-adapter: background
   goroutine read inotify-fd, аккумулирует events за tick window, `Check`
   возвращает `state='quiet'/'events'` + payload `{path, events[], count}`.
   Darwin/Windows stub-impl с ошибкой `platform not supported`.
   `recursive=true` отложен (MVP source-of-bugs); `throttle` param
   accept-and-ignore (forward-compat). `InotifyPortent` typed (field 14).

**S5 invariants.**

- Все 7 core-beacon + plugin-beacon edge-triggered (state-change Portent),
  не level-triggered.
- Plugin-beacon Sigil-verify обязателен (default-deny при mismatch).
- Typed-payload + legacy Struct оба валидны для CEL-Decree-where (1-release
  переходный период).
- inotify Linux-only inv: запуск Vigil на non-Linux → error
  `platform not supported` в логи, scheduler пропускает тик, Vigil-row
  остаётся `state=unknown`.

**S5 ADR cross-links.**

- [ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) (typed Soulprint) —
  pattern-reference для typed PortentPayload.
- [ADR-020](0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)
  (plugin-инфра) — 4-й kind `soul_beacon` (`KIND_SOUL_BEACON=4`).
  `ManifestSpec.params_schema` переиспользуется для beacon-config.
- [ADR-026](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)
  (Sigil-trust) — anchors snapshot уже доставляется на Soul-host,
  переиспользуется без новой machinery.
- [ADR-012](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)
  (only-add proto) — `PortentEvent.data` deprecated, но физически остаётся;
  oneof field-numbers strictly only-add.

**Что отложено пост-S5.**

- (iv) Per-Decree cooldown override.
- (v) Toggle-endpoint re-enable Decree.
- (vi) Metric-threshold-pull beacon — отдельный contour (новый ADR).
- L3b live-тесты Vigil/Oracle — требует harness `CreateVigil` /
  `CreateDecree` / `WaitForOracleFires` (M2.5 harness-extension slice).
- inotify `recursive` + `throttle` — отдельный slice по запросу.
- `PortentEvent.data` hard-cut в S5-final после 1 production-релиза
  (parity с push S7-decision).
