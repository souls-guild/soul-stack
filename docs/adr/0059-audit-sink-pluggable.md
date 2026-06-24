# ADR-059. Pluggable audit sink — выбор бэкенда выгрузки аудита (PG / Kafka / off)

> **Статус: proposed / deferred (design-only, 2026-06-24).** Дизайн-ADR. Решение направления принято пользователем (на целевом масштабе аудит не пишется синхронно в PG, выгружается в Kafka, под тумблером), но **кода по этому ADR в бете нет** — это план на пост-бету, фиксируется ДО реализации (документация впереди кода). Дизайн — architect. До реализации обязательна развязка зависимости Herald/`changed_tasks` от audit-в-PG (см. open question (n)). Рабочее имя сущности — **«audit sink»** (ключ конфига `audit.sink`); тематическое имя **Chronicle** — опциональная альтернатива, в [naming-rules.md](../naming-rules.md) НЕ зафиксировано (за PM/пользователем).

**Контекст.** [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) нормировал audit-pipeline на единственном бэкенде: `audit_log` в общей Postgres ([ADR-005](0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)), single source of truth, retention через Reaper-правило `purge_audit_old`. Trade-offs ADR-022 уже отметили: «Postgres-only vs отдельный audit-store... растущий объём (365 дней × все подсистемы × HA-кластер) — со временем upcoming концерн». [known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету) честно фиксирует: на целевом масштабе **100k VM** объём прогонов упирается в INSERT-rate и размер таблицы — `audit_log` главный потребитель объёма PG.

Синхронная запись каждого audit-события в PG не масштабируется: один прогон на 100k хостов даёт сотни тысяч `task.executed`-INSERT-ов. Решение пользователя (2026-06-24): на целевом масштабе аудит **не писать синхронно в PG** и **не строить in-app предпросмотр** (предпросмотр = read-нагрузка на PG, тот же концерн со стороны чтения) — события **выгружать в Kafka**, под явными тумблерами. Это план, не код в бете: бета остаётся на PG-sink (малый флот, `audit_log` хватает с запасом).

Точка абстракции уже существует и не требует переделки: `shared/audit.Writer` — async/best-effort интерфейс; `MultiWriter` даёт tap-точку (её использует [Herald](0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов) (c)). Чего **не зафиксировано:** что бывают разные sink-реализации, как они выбираются в конфиге, какие гарантии доставки даёт Kafka-sink (audit compliance-критичен — нельзя терять события), как это ложится на tier-модель ([ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей)), и как при Kafka-only-режиме (без audit-в-PG) живут подсистемы, которые сегодня **деривят данные из audit-в-PG** (Herald, `changed_tasks`).

**Решение (направление; реализация отложена).**

**(a) Sink-абстракция за `shared/audit.Writer`.** Конкретный backend audit-выгрузки — это выбор **реализации** `audit.Writer`, не новый интерфейс и не новая сущность словаря. Существующая абстракция (`Writer` async/best-effort + `MultiWriter` tap) — достаточна. Sink выбирается в `setupAudit` ([keeper/cmd/keeper/daemon.go](../../keeper/cmd/keeper/daemon.go), точка врезки) по конфигу и собирается один раз на старте. tap-цепочка Herald (`MultiWriter`-декоратор) **остаётся над выбранным sink-ом** — Herald навешивается поверх любого backend-а (но см. ограничение (n): сегодня Herald деривит `changed_tasks` SQL-запросом по `audit_log`, а не из tap-события).

**(b) Три значения `audit.sink`.** Конфиг `keeper.yml → audit.sink: pg | kafka | off`:

| Значение | Backend | Поведение |
|---|---|---|
| **`pg`** | `audit_log` в Postgres ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) | **default**, текущее поведение; реализация — существующий PG-writer (рабочее имя пакета — `auditpg`). Source of truth, retention через Reaper. |
| **`kafka`** | Kafka-топик | новый sink; события сериализуются и публикуются в Kafka, downstream-потребитель кладёт их в долговременное хранилище (data lake / SIEM / ClickHouse — вне Soul Stack). PG `audit_log` **не пишется** (или пишется усечённо — см. (n)). |
| **`off`** | — | audit не выгружается никуда. Осознанный выбор оператора (dev / эфемерный стенд); **не** default. |

Расширение enum (например `s3`/`elasticsearch`) — propose-and-wait, additive, без breaking. `off` — отдельное значение, **не** `enabled: false` (ADR-022(i) `audit.enabled` остаётся ортогональным master-тумблером; `sink: off` явно говорит «backend = никуда», что читается как осознанный выбор, а не «забыли настроить»).

**(c) Конфиг-блок (расширение `audit:` ADR-022(i)).** Форма (нормативная типизация — [config.md → audit](../keeper/config.md#audit), docs-writer при реализации):

```yaml
audit:
  enabled: true            # ADR-022(i), master-тумблер (ортогонален sink)
  sink:    pg              # pg | kafka | off  (default pg)
  otel_export: true        # ADR-022(f)
  retention_days: 365      # ADR-022(d), релевантен только sink=pg

  kafka:                   # читается только при sink=kafka
    brokers:  ["broker-1:9092", "broker-2:9092"]
    topic:    "soul-stack.audit"
    acks:     all          # at-least-once гарантия — НЕ ослаблять (см. (d))
    # секреты подключения (SASL/TLS) — vault-ref, НЕ cleartext (паттерн Herald secret_ref)
    sasl_ref: "secret/keeper/kafka-audit/sasl"
    tls_ref:  "secret/keeper/kafka-audit/tls"
```

`audit.kafka.*` читается только при `sink: kafka`. Секреты подключения к Kafka — **vault-ref** (паттерн [ADR-052(e)](0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов) `secret_ref` / [ADR-025](0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul) `auth_ref`), не PG/диск cleartext.

**(d) Гарантии доставки — Kafka at-least-once, деградация fail-closed.** Audit compliance-критичен ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) — SOC2/ISO: identity/authn/authz обязаны журналироваться) — **терять audit-события нельзя**. Kafka-sink даёт **at-least-once**, не at-most-once:

- **Producer `acks=all`** (синхронное подтверждение всех in-sync реплик) — обязательно, не ослаблять до `acks=1`/`acks=0`.
- **Деградация при недоступности Kafka — fail-closed**, не fail-open. Концепция fail-open/fail-closed — из [ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей) (Sigil/Purview = fail-closed, Tempo = fail-open). Audit fail-closed означает: при невозможности подтвердить запись audit-события sink выбирает **локальный durable-fallback** (см. ниже) или блокирует write-path, а не молча роняет событие. **Какой именно fail-closed-механизм — открытый дизайн-вопрос (m).**
- **Дедуп — downstream по `audit_id`.** at-least-once → возможен дубль события в топике. `audit_id` — ULID, уже PK в схеме ([ADR-022(a)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — служит ключом дедупликации на стороне потребителя (consumer идемпотентен по `audit_id`). Дедуп — забота downstream-консьюмера, не Soul Stack.

**(e) Гарантия записи в Kafka — два кандидата (открытый выбор, m).** at-least-once на оператор-критичном пути требует, чтобы успех бизнес-операции не обгонял запись её audit-факта. Два паттерна (выбор отложен до реализации):

1. **Transactional outbox** — audit-событие пишется в PG-таблицу-outbox в **той же транзакции**, что и бизнес-изменение; отдельный relay-worker (claim-queue parity [ADR-027(d)](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)/[Herald-доставка](0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)) перекладывает в Kafka и помечает доставленным. Гарантия сильнейшая (атомарность с бизнес-tx), но **не снимает нагрузку с PG INSERT** (outbox-INSERT того же порядка, что и `audit_log`-INSERT) — противоречит мотиву «снять PG-write». Outbox-таблица меньше живёт (truncate после relay), но write-rate тот же.
2. **Direct-producer `acks=all`** — синхронная публикация в Kafka на write-path без PG. Снимает PG-write полностью (мотив выполнен), но требует собственного durable-fallback на случай недоступности Kafka (иначе at-least-once нарушается — fail-closed (d)).

Trade-off (m): outbox = сильная гарантия ценой сохранения PG-write (мотив частично не выполнен); direct = выполненный мотив ценой сложного fallback. Выбор — при реализации, не в этом ADR.

**(f) Tier — Kafka строго ОПЦИОНАЛЕН, не 4-й required.** По [ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей): обязательный контур остаётся **PG + Redis + Vault**. Default `audit.sink: pg` — обязательный контур цел, никакой новой зависимости. Kafka — **OPTIONAL-with-degradation**: появляется как требование только при явном `sink: kafka`. Правило ADR-053 «новый required-компонент — только через явное решение пользователя» соблюдено: Kafka не становится четвёртым required by-default. Строка в OPTIONAL-таблице ADR-053 добавлена. Деградация Kafka-sink — **fail-closed** (d) — осознанный security-trade-off, явно зафиксирован (audit нельзя терять).

**(g) Переключение sink — restart-required.** Смена `audit.sink` (как и `audit.kafka.*`) — **не hot-reload**, требует рестарта Keeper-инстанса; паттерн `web_ui_enabled` ([ADR-055](0055-embed-ui-bundle.md)). Sink выбирается и собирается один раз в `setupAudit` на старте; смена backend-а на лету (пере-wiring write-path всех инициаторов + tap-цепочки) — лишняя сложность ради редкой операции. В per-block reload-policy таблице [config.md → Hot-reload](../keeper/config.md#hot-reload) `audit.sink`/`audit.kafka.*` помечаются restart-required (`audit.enabled`/`otel_export`/`retention_days` остаются reload-able по ADR-022). docs-writer при реализации.

**(h) shared остаётся pgx-free.** Инвариант [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам): `shared/` — поперечный Soul-safe код без серверных зависимостей (без pgx). Sink-абстракция (`Writer`/`MultiWriter`) живёт в `shared/audit` и остаётся бэкенд-агностичной. **Обе** конкретные реализации — в `keeper/internal`: PG-sink (рабочее имя `auditpg`) и новый Kafka-sink. Kafka-клиент тянется в `keeper/` (server-side), не в `shared/`. Это сохраняет: `shared` без pgx и без Kafka-драйвера, Soul-изоляцию ([ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) — Soul не тянет ни PG, ни Kafka).

**(i) Соотношение с backlog audit-scaling.** [known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету) перечисляет варианты для крупных флотов: партиционирование, hot-cold, batched-INSERT, **Redis-Stream-буферизация**. Kafka-sink — точка на оси **write-throughput**:

- **Вытесняет Redis-Stream-вариант** — Kafka покрывает ту же задачу (буферизация audit-потока с разгрузкой PG-write) полноценнее: durable-лог с downstream-консьюмерами, дедуп по `audit_id`, не нагружает Redis (Redis — hot-слой [ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis), его роль ≠ долговременный audit-буфер). Redis-Stream-вариант из backlog снимается.
- **batched-INSERT остаётся** как более дешёвая альтернатива на оси write-throughput для инсталляций, которым Kafka избыточна, но синхронный per-event INSERT уже жмёт: батчирование PG-INSERT (sink=pg + батч-flush) снижает write-rate без новой инфраструктуры. Это ортогональная оптимизация PG-sink-а, не вытесняется Kafka.
- **Партиционирование / hot-cold** — оси **storage size** PG-sink-а, к Kafka-sink-у не относятся (при Kafka-only PG `audit_log` не растёт), остаются в backlog для `sink=pg`.

**(j) `off` — границы.** `sink: off` отключает выгрузку audit полностью. Допустимо для dev / эфемерных стендов. Operations-нота (docs-writer при реализации): `off` снимает compliance-гарантию ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — лог на старте обязан внятно предупредить, что audit не пишется (паттерн «деградация внятна» [ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей)). `off` ≠ тихий no-op.

**Consequences (при реализации; в бете кода нет).**

- **`shared/audit`** — sink-абстракция уже есть (`Writer`/`MultiWriter`), новый код не требуется; меняется только wiring в `setupAudit`. pgx-free инвариант (h) сохраняется.
- **`keeper/internal` — новый Kafka-sink** рядом с PG-sink (`auditpg`); Kafka-клиент в `keeper/` go.mod (h).
- **`setupAudit` ([daemon.go](../../keeper/cmd/keeper/daemon.go))** — точка выбора sink по `audit.sink`; tap-цепочка Herald навешивается поверх выбранного sink (с учётом (n)).
- **Конфиг `audit:`** — поля `sink` + блок `kafka.*` (c); restart-required (g); [config.md](../keeper/config.md#audit) (docs-writer).
- **[known-limitations.md](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету)** — уточнение: Kafka-sink спроектирован (этот ADR), в бете не реализован; вытесняет Redis-Stream-вариант, batched-INSERT остаётся (i).
- **[ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей)** — строка в OPTIONAL-таблице (Kafka-sink, fail-closed).
- **[ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** — amendment-указатель: PG остаётся default + source-of-truth ПОКА, sink абстрагирован, Kafka opt-in (этот ADR).
- **Имена** — рабочее `audit sink` / ключ `audit.sink` / значения `pg`/`kafka`/`off`. Тематическое **Chronicle** — кандидат-альтернатива, **не** зафиксирован в [naming-rules.md](../naming-rules.md) (за PM/пользователем). Фиксация имени в naming-rules — отдельным шагом по propose-and-wait при выборе тематического варианта.

**Open questions / зависимости (ДО реализации).**

- **(n) КРИТИЧНО — Herald и `changed_tasks` сегодня деривят данные из audit-в-PG.** Это блокирующая зависимость, не замалчивается:
  - **`incarnation.run_completed` → `changed_tasks`** ([ADR-052 §k](0052-herald-notifications.md#k-терминальное-событие-прогона-incarnationrun_completed-несёт-changed_tasks)) собирается так: `scenario.Runner` на финале прогона **читает агрегат `audit_log`** SQL-запросом (`WHERE correlation_id = apply_id AND event_type = 'task.executed' AND payload->>'status' = 'TASK_STATUS_CHANGED'`) и сворачивает в `changed_tasks`. Источник changed-факта — **журнал аудита в PG** (явно: «Источник `changed` — журнал аудита, не новая таблица»).
  - **`GET /v1/audit` read-API** ([ADR-022(j)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) и **audit-фильтры** Herald-visibility (`payload_voyage`/`payload_herald`, [ADR-052 §k visibility](0052-herald-notifications.md#amend-k-visibility-2026-06-12-опциональный-voyage_id--audit-фильтр-payload_voyage)) — SQL по `audit_log`.

  При **`sink: kafka` без записи в PG** этот источник исчезает: `audit_log` пуст (или усечён) → `changed_tasks`-свёртка пустая → Herald-уведомления про per-task changed-разбивку **молча ломаются**, `GET /v1/audit` ничего не возвращает. **Это должно быть решено ДО реализации Kafka-sink.** Кандидаты направления (выбор — отдельный дизайн-заход с architect/пользователем, НЕ в этом ADR):
    1. **Альтернативный источник changed-факта** — `scenario.Runner` собирает `changed_tasks` из in-memory-агрегата прогона (он и так держит `[]RenderedTask` и видит per-`(sid, task_idx)`-статусы), не из SQL по `audit_log`. Развязывает свёртку от backend-а аудита. Это самостоятельная развязка, полезная и при `sink: pg`.
    2. **Гибрид sink** — `task.executed` (и терминалы прогонов) пишутся в PG **всегда** (это «горячий» операционный слой, нужный Herald/`changed_tasks`/`GET /v1/audit`), а в Kafka уходит **полный** поток для compliance/SIEM. Тогда `sink: kafka` означает «Kafka **дополнительно**», а не «вместо PG» — но мотив «снять PG-write» выполнен лишь частично (горячий операционный поток остаётся в PG).
    3. **Read-API и Herald мигрируют на Kafka-consumer-проекцию** — отдельная read-модель из Kafka. Тяжёлый вариант, отдельная подсистема.

  Без выбора по (n) Kafka-only-режим **нельзя реализовывать** — он тихо ломает Herald-уведомления и audit-чтение. Зафиксировано как hard-зависимость.

- **(m) Гарантия записи в Kafka — outbox vs direct-producer** (см. (e)): сильная гарантия ценой PG-write vs выполненный мотив ценой durable-fallback. Конкретный fail-closed-fallback (d) (local spool-файл? блокировка write-path? буфер с alerting?) — часть этого вопроса. Выбор — при реализации.

- **(o) In-app предпросмотр аудита.** Решение пользователя — предпросмотра не строить (read-нагрузка на PG = тот же концерн). Текущий `GET /v1/audit` ([ADR-022(j)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) и UI Audit Log ([known-limitations.md](../known-limitations.md), Oracle fires → audit-фильтр) при `sink: kafka` без PG лишаются источника — пересекается с (n). Поведение read-API при не-PG-sink (404? пустой? проекция?) — решить вместе с (n).

**Trade-offs.**

- **Pluggable sink vs hardcoded PG.** Pluggable — масштаб 100k VM требует разгрузки PG-write; абстракция уже есть (`Writer`), цена низкая. Цена — конфиг-сложность + зависимость (n). PG остаётся default → бета и малые инсталляции не платят ничего.
- **Kafka vs Redis-Stream (backlog).** Kafka — durable-лог, дедуп по `audit_id`, downstream-консьюмеры, не нагружает Redis (hot-слой). Redis-Stream разгрузил бы PG-write, но Redis — не место для долговременного compliance-буфера ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis) hot→Redis инвариант). Kafka вытесняет Redis-Stream-вариант (i).
- **fail-closed vs fail-open при недоступности Kafka.** fail-closed — audit compliance-критичен, потеря события недопустима ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). Цена — write-path деградирует при отказе Kafka (медленнее/блокируется/spool), а не «продолжаем без аудита». Осознанный security-trade-off, противоположный Tempo fail-open ([ADR-050](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)).
- **restart-required vs hot-reload sink.** restart — пере-wiring всего write-path + tap на лету ради редкой операции (смена backend-а аудита) не оправдан; паттерн `web_ui_enabled` ([ADR-055](0055-embed-ui-bundle.md)).
- **`sink: off` — отдельное значение vs `enabled: false`.** Отдельное значение — `off` читается как осознанный «никуда не выгружать», а `enabled: false` — как master-выключатель audit-логики; разведены, чтобы конфиг был явным.

**Связь с ADR.**

- **[ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** — нормировал PG-sink (`audit_log`); этот ADR абстрагирует backend, PG остаётся default + source-of-truth ПОКА. Amendment-указатель в ADR-022.
- **[ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей)** — Kafka-sink = OPTIONAL-with-degradation (fail-closed); PG+Redis+Vault остаются единственным required-контуром; правило «новый required — только через решение пользователя» соблюдено.
- **[ADR-052](0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)** — Herald tap поверх sink + `changed_tasks`-зависимость от audit-в-PG (open question (n)).
- **[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)** — Redis hot-слой ≠ долговременный audit-буфер (почему Kafka, не Redis-Stream).
- **[ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)** — `shared` pgx-free + бэкенд-агностичен; обе sink-реализации в `keeper/internal` (h).
- **[ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)** — claim-queue паттерн для outbox-relay-кандидата (e).
- **[ADR-055](0055-embed-ui-bundle.md)** — паттерн restart-required тумблера (`web_ui_enabled`).
- **[ADR-050](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)** — контраст fail-open vs audit fail-closed.
