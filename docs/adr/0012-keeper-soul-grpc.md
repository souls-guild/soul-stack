# ADR-012. Контракт Keeper↔Soul gRPC: один EventStream с oneof, Keeper-side рендер, forward-compat only-add

- **Контекст.** [ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) зафиксировал транспорт (gRPC bidi поверх mTLS, инициирует Soul), но не контракт сообщений. [Pilot scaffolding ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) положил placeholder `proto/keeper/v1/keeper.proto` с RPC `Ping`. Нужно одно консолидированное решение по структуре service-а, раскладке `.proto`-файлов, политике версионирования, месту рендера Destiny и соотношению `TaskEvent` с `ApplyEvent` от `SoulModule`. Часть пунктов пересекается с open Q №6 (Soulprint-схема), №7 (protobuf compat) и №12 (Heartbeat).
- **Решение.**

  **(a) Один `service Keeper` + один долгоживущий bidi-стрим с `oneof` payload.** Сервис состоит из двух RPC-методов:

  ```
  service Keeper {
    rpc Bootstrap(BootstrapRequest) returns (BootstrapReply);     // unary, server-only TLS
    rpc EventStream(stream FromSoul) returns (stream FromKeeper); // bidi, mTLS
  }
  ```

  `FromSoul` и `FromKeeper` — wrapper-сообщения с `oneof payload`, перечисляющим все типы по направлению. Соответствует формулировке ADR-002 «один долгоживущий bidirectional gRPC-стрим» и позволяет добавлять новые типы сообщений без breaking change.

  **(b) Тематическая раскладка `.proto`-файлов внутри `proto/keeper/v1/`:**

  ```
  proto/keeper/v1/
  ├── keeper.proto        # service Keeper; FromSoul / FromKeeper oneof
  ├── onboarding.proto    # BootstrapRequest / BootstrapReply
  ├── lifecycle.proto     # Hello / HelloReply, SeedRotationRequest / SeedRotationReply
  ├── apply.proto         # ApplyRequest, TaskEvent, RunResult, CancelApply
  ├── soulprint.proto     # SoulprintReport
  └── common.proto        # типы, переиспользуемые между файлами (RenderedTask, TaskError, ...)
  ```

  Один файл = одна семантическая ось. Все импортируются `keeper.proto`-ом верхнего уровня. Generated Go-код собирается `make gen` в `proto/gen/go/keeper/v1/` (см. [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)).

  **(c) Версионирование — forward-compat, only-add внутри `proto/keeper/v1/`. Закрывает open Q №7.**
  - Никогда не удалять поля и не reuse field-номера.
  - Все новые поля добавляются как optional (proto3 default behavior).
  - Breaking changes — только через новый пакет `proto/keeper/v2/`, не правкой `v1/`.
  - Это позволяет апгрейдить Keeper отдельно от Souls: типовая эксплуатационная схема «сначала Keeper, потом Souls волнами» становится поддерживаемой грамматикой контракта, не договорённостью.

  **(d) Граница рендера — по внешнему доступу, не по локации.** Security-инвариант ADR-012(d) **перепривязан** (причина: прод-блокер golden-path Real-Linux E2E `core.file.rendered: template_content missing` + решение пользователя+architect). Раньше формулировка была «весь рендер Keeper-side»; теперь граница проходит по тому, **кто ходит во внешние системы**, а НЕ по физической локации рендера:

  - **CEL-фаза (рендер params, `${ … }`-интерполяция, vault) — Keeper-side.** `vault(...)` / `soulprint.*` / `register.*` / `input.*` / `essence.*` в `params:`/`vars:`/`on:` резолвятся на Keeper-е. На хост не попадают Vault-токены — внешний доступ остаётся за Keeper-ом.
  - **text/template-COMPUTE — Soul-side.** Финальный проход `templates/<path>.tmpl` (Go text/template + sprig-allowlist) выполняется на Soul-е модулем `core.file.rendered`: это **локальный compute без I/O** (нет файлов/сети/Vault/окружения — три sandbox-барьера ADR-010 сохраняются на Soul, sprig-allowlist исключает `env`/`exec`/`getHostByName` и всё читающее FS/сеть). Soul тянет только `shared/tmpl` (text/template + sprig-allowlist).
  - **Flow-control CEL — Soul-side, sandboxed, без внешнего доступа.** Предикаты `when:`/`changed_when:`/`failed_when:` зависят от `register.*` (результатов предыдущих задач), известных только на Soul во время прогона, а не на Keeper во время рендера. Поэтому вычисляются Soul-side урезанной cel-go-песочницей (`shared/cel.NewFlowControl`). Согласуется с границей «по внешнему доступу»: flow-control CEL — локальный compute без I/O (как text/template-COMPUTE). Sandbox регистрирует только функции без внешнего доступа (`size`/`contains`/`has`/comprehensions/конверсии/операторы/`duration`) и переменные `register.*` (собирает Soul из результатов задач по register-имени) + `flow_context` (доставляет Keeper). Запрещены конструктивно (символ не зарегистрирован → compile-error): `vault(...)` и `now()` (внешний доступ/недетерминизм — симметрия с migration-CEL sandbox [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)), `soulprint.hosts`/`soulprint.where` (cross-host, scenario-only), любой `__`-идентификатор. Soul тянет `cel-go`, но НЕ `vault`-client: Vault-токенов на хосте по-прежнему нет, внешний доступ keeper-only. **Реализовано Soul-side:** `when:` (gating ДО Apply) + `changed_when:`/`failed_when:` (override `changed`/`failed` ПОСЛЕ Apply; порядок — `changed_when` затем `failed_when`, `failed` приоритетнее). `failed_when: false` на упавшем модуле = ignore_errors (статус НЕ FAILED, прогон не ломается, исходная ошибка сохраняется в `register.<name>.ignored_error` + `TaskEvent.error`); к `TIMED_OUT` НЕ применяется (таймаут — инфраструктурный fail-stop). Активация `changed_when:`/`failed_when:` дополнительно включает `register.self.*` (свежий результат текущей задачи). `until:` (retry-петля) — отложен.
  - **`flow_context` — литеральный per-host снапшот не-register части CEL-контекста** `{ input, vars, essence, incarnation, self }` (то же, что для рендера params, минус `soulprint.hosts` и loop), доставляется в `RenderedTask.flow_context`. Soul читает его как данные (биндит `soulprint.self` ← `flow_context.self`, остальное top-level), внешнего доступа не делает. Host-вариативен (`self` per-host), исключён из per-host-сверки host-инвариантности params. `register.*` в `flow_context` НЕ кладётся — его Soul строит сам из накопленных результатов задач (по `RenderedTask.register`-имени). Новые поля `RenderedTask` (only-add, [ADR-012(c)](#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)): `when`/`changed_when`/`failed_when` (CEL-строки), `flow_context`, `register` (register-имя результата).
  - **Keeper доставляет literal-содержимое шаблона + собранный per-host корень контекста.** `ApplyRequest` для шага `core.file.rendered` несёт literal `template_content` (Keeper прочитал `.tmpl` из снапшота сервиса/destiny после CEL-фазы, text/template НЕ исполнял) + `render_context` — собранный Keeper-ом **per-host** корень text/template-контекста по [templating.md §3.2](../templating.md): `{ vars, self, role, essence }` (CEL-rendered `params.vars` под ключ `vars`; `self` — та же `soulprint.self`-проекция, что в CEL-фазе, единая точка правды ADR-018; `role` — declared-роль хоста; `essence` — effective-слой). Soul передаёт `render_context` **корнем** в text/template, поэтому шаблон видит `.vars.*`/`.self.*`/`.role`/`.essence.*`. **A1-вариант (MVP):** и `template_content`, и `render_context` едут внутри `RenderedTask.params` как обычные ключи — **без изменений proto**; плоский `vars`-ключ и путь-ключ `template` из params Keeper удаляет (Soul читает корень только из `render_context`). **Host-инвариантность шаблонного контекста НЕ требуется:** `render_context` и `template_content` исключены из per-host-сверки params (`self`-зависимые шаблоны → у каждого хоста СВОЙ `render_context`). Для golden-path (один хост в прогоне) это тривиально; полный per-host dispatch self-зависимых шаблонов (отдельный `ApplyRequest` на хост) — отложенный orchestrator-слой. Корневая причина прод-блокера BUG-A (Real-Linux E2E `map has no entry for key "self"`): Soul подавал плоский `vars` корнем, шаблон `{{ .self.network.primary_ip }}` падал strict-mode. Двухуровневый резолв `.tmpl` (scenario-local→service-level, [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) делает Keeper при чтении.

  Фазы конвейера: `vault-resolve(авторские refs в params) → input-resolve(merge → scoped operator-input-vault-resolve → value-валидация) → CEL-render → доставка literal .tmpl` (Keeper) → `text/template-render` (Soul, в `core.file.rendered`). Полные фазы — [templating.md §4](../templating.md).

  - **Scoped-резолв `vault:`-ref в operator-input — отдельный ограниченный канал.** Значение secret-поля с объявленным `vault_scope` (input-контракт, [docs/input.md → «vault_scope»](../input.md#vault_scope-scoped-резолв-vault-ref-в-operator-input)) может быть `vault:`-ref; Keeper резолвит его keeper-side ОДИН раз в input-фазе с проверкой `vault_scope`-prefix + hard deny-list (`secret/keeper/*`/`secret/internal/*`, неотключаем конфигом, расширяется `keeper.yml → vault.input_deny_paths`). Default-deny: `vault:`-значение в поле без `vault_scope` — ошибка. Каждый резолв (ok/denied) аудируется (`input.vault_resolved`, путь — не секрет, значение секрета не логируется); denied — security-сигнал. value-валидация (`pattern`/`enum`) — на УЖЕ резолвнутом значении. Это **не** трогает доверенный авторский канал (`vault:`-ref в `params:` и `${ vault(...) }`) — он остаётся как есть.

  Преимущества (сохраняются): нет Vault-токенов на Soul-е (безопасность на первом месте); централизованный аудит CEL- и vault-резолва на Keeper-е; в `soul/go.mod` нет `cel-go`/`vault`-client.

  Trade-off: probe-шаги в scenario дают round-trip Keeper↔Soul после каждого probe — Keeper держит scenario-state и отправляет следующий шаг **после** получения per-host `register:` от Soul. Это и так делается: диспетчер scenario живёт на Keeper-е ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).

  **(e) `TaskEvent` ↔ SoulModule `ApplyEvent` — агрегация на Soul-е (для MVP).** Soul ждёт завершения SoulModule sub-process-а, формирует финальный `TaskEvent` (с `register.<name>.*` полями: `.changed`, `.failed`, `.timed_out`, плюс декларированные в destiny `output:` поля) и шлёт его Keeper-у. **Прогресс long-running шагов** в Keeper↔Soul стрим в MVP не уходит. Post-MVP — добавится через дублирование структуры `ApplyEvent` в `apply.proto`, **без** cross-import между `proto/keeper/v1/` и `proto/plugin/v1/` (запрет [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)).

  **(f) Bootstrap-RPC — отдельный listener, server-only TLS.** `rpc Bootstrap` слушает на отдельном порту с **server-TLS без client cert** (у Soul ещё нет SoulSeed, identity-claim даёт `bootstrap_token`). `rpc EventStream` — основной порт с mTLS. Явное разделение «нет identity» / «есть identity» упрощает RBAC, аудит и сетевую сегментацию.

  **(g) Soulprint payload — `google.protobuf.Struct` до закрытия open Q №6.** `SoulprintReport.facts` имеет тип `google.protobuf.Struct`; точная typed-схема будет добавлена после фиксации №6 как новое поле без breaking change (deprecation старого, only-add).

  **(h) Heartbeat — не вводится.** `last_seen_at` обновляется на любое app-сообщение в EventStream-е, плюс gRPC keepalive на транспорте. Open Q №12 остаётся открытым, но текущий контракт от него не зависит — выделенный `Heartbeat`-message добавим, если эксплуатация покажет, что нужен.

  **(i) SID в payload — echo, не identity-claim.** Авторитетный источник SID — `Subject Alternative Name` peer cert mTLS-сессии. В proto-комментариях соответствующих полей явно: `// SID echoed for logging only; authoritative source is the mTLS peer certificate`.

  **(j) Имена сообщений** зафиксированы в [naming-rules.md → раздел «Сообщения proto Keeper↔Soul»](../naming-rules.md#сообщения-proto-keepersoul). `StateReport` из ранних обсуждений отвергнуто как конфликт с зарезервированным «State» (= `incarnation.state`); финальное имя финального отчёта прогона — **`RunResult`**.

  **(k) Amendment (2026-05-25, `WardRoster` — Soul-reconcile).** Новое `FromSoul` **only-add** сообщение `WardRoster { repeated ActiveApply active }` (`ActiveApply { string apply_id; int32 attempt }`, field 3 reserved под `status`, never-reuse), `FromSoul.oneof` **field 8**. Soul на (re)connect (сразу после Hello, ReplaceAll-семантика) декларирует реально ведомые `apply_id` — Keeper терминалит осиротевшие `dispatched` ([ADR-027(g)](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) РЕАЛИЗОВАНО). Forward-compat: старый Soul не шлёт → Keeper sweep no-op (fail-safe). Имя — в naming-rules.

- **Consequences.**
  - В `proto/keeper/v1/` создаются шесть `.proto`-файлов (плюс существующий `keeper.proto` расширяется). `make gen` собирает в `proto/gen/go/keeper/v1/`, generated-код коммитится (ADR-011).
  - В `soul/go.mod` появляется `cel-go` (sandboxed flow-control CEL — `when`/`changed_when`/`failed_when`, через `shared/cel.NewFlowControl`), но НЕ `vault`-client (внешний доступ keeper-only). Это **осознанная перепривязка BUG-A** — симметричная уже сделанной для text/template-COMPUTE (граница «по внешнему доступу», а не «где compute»), а не тихая правка: flow-control-предикаты зависят от `register.*`, которого на Keeper-е во время рендера нет. `sprig` (через `shared/tmpl`) на Soul тоже **есть** — text/template-COMPUTE шага `core.file.rendered`. Внешнего доступа (Vault-токены / `vault`-client / сеть к Vault) на Soul по-прежнему нет.
  - Конфигурация Keeper-а формализует два sub-listener-а под `listen.grpc.{bootstrap,event_stream}` ([config.md → listen](../keeper/config.md#listen)). TLS-параметры независимы (Bootstrap — server-only TLS без `ca`; EventStream — mTLS с `ca`); одинаковые file paths для cert/key между sub-listener-ами разрешены грамматикой. Schema-валидация добавляет diag-коды `grpc_bootstrap_listener_required` / `grpc_event_stream_listener_required` (оба обязательны по ADR-002) и `bootstrap_eventstream_port_conflict` (адреса должны различаться).
  - `proto/keeper/v1/` не импортирует `proto/plugin/v1/`; структуры событий между ними дублируются — это сознательная цена за изоляцию плагинной экосистемы (ADR-011).
  - `soul-lint` не валидирует строго `SoulprintReport.facts` до закрытия open Q №6.
  - **Open Q №7 закрыт** (правило forward-compat only-add). Open Q №6 и №12 остаются открытыми, но не блокируют v1-контракт.
  - `service Keeper` живёт в одном service-блоке (см. (a)); push-режим ([keeper.push](../architecture.md#push-режим-keeperpush)) использует **другой** канал (SSH, не gRPC) и в `service Keeper` не присутствует.
- **Trade-offs.**
  - Один EventStream — простая модель, но требует careful ordering на оба направления (особенно при cancel/abort параллельно с долгим apply). FIFO-гарантии gRPC внутри одного направления это покрывают.
  - Keeper-side CEL + внешний доступ => probe-шаги делают round-trip Keeper↔Soul. Для типового сценария из 5–10 шагов это десятки ms RTT на шаг — приемлемо. Граница «по внешнему доступу» (ADR-012(d)): CEL с внешним доступом (vault/render params) — Keeper, локальный compute без I/O — Soul (text/template-COMPUTE + flow-control CEL `when:`/…); на Soul оставлен только compute, проигрыша по поверхности атаки нет (sandbox-барьеры ADR-010 + sandbox-by-undeclaration flow-control-env сохранены на Soul).
  - Forward-compat only-add со временем загромождает `v1/` deprecated-полями. Лучше, чем break wire-format при апгрейдах.
  - Bootstrap на отдельном порту = +1 listener в конфиге Keeper-а. Незначительный operational overhead, выигрыш в безопасности.
  - Дублирование `ApplyEvent`-структуры между `proto/keeper/v1/apply.proto` и `proto/plugin/v1/soulmodule.proto` (когда понадобится) — drift риск. Принимаем: ADR-011 явно запрещает cross-import.
