# Reaper / Жнец

Фоновая задача внутри `keeper`-бинаря, чистящая БД от мусора и поддерживающая инварианты реестра. **Не отдельный бинарь.**

Имя **Charon** (Харон) **зарезервировано** на случай, если scope Жнеца расширится за рамки cleanup-а (миграции таблиц, перенос архивных записей, GC холодных слоёв). Пока имя одно — Reaper / Жнец. См. [naming-rules.md](../naming-rules.md).

> **Reaper = cleanup-домен, scheduling — у Conductor.** Спавн [Cadence](../naming-rules.md#сущности-предметной-области)-расписаний ([ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)) **НЕ** правило Reaper. S0-дизайн ADR-046 планировал Reaper-правило `spawn_due_cadence` (`action: spawn`), но [ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний) (2026-06-02) вынес исполнение расписаний в отдельную leader-elected подсистему **[Conductor](../naming-rules.md#модули-и-подсистемы-внутри-keeper)** со своим lease `conductor:leader` и tick-interval (`cadence_scheduler.interval`, ~15–30s) — cleanup-домен Reaper (`interval` 1h) и scheduling-домен Cadence имеют разный естественный ритм. Поэтому Reaper-`action` остаётся cleanup-набором (`expire`/`delete`/`set_status`/`report`/`soft_delete`) **без `spawn`**, а перечень правил ниже не содержит `spawn_due_cadence`. Когда Cadence-спавн будет реализован (слайсы C1–C4 ADR-048) — он живёт в Conductor, не здесь.

## Свойства

- Живёт внутри `keeper`. Не отдельный бинарь.
- Работает **только на одном Keeper-инстансе одновременно**: лидер выбирается через Redis-lease `reaper:leader` с TTL = `lock_ttl` ([ADR-006 → (d)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)).
- Цикл по `interval` (default `1h`). Поддерживается dry-run.
- **Метрики на каждое правило:** см. [Метрики](#метрики) ниже.
- Любое правило отключается через `enabled: false` точечно, **без передеплоя бинаря** (hot-reload конфига — сквозное требование, см. [requirements.md](../requirements.md)).

## Граница: Postgres (+ read-only Vault metadata), не хосты

Жнец работает **над Postgres**; единственное исключение — cross-store правило `reap_orphan_vault_keys` (см. [Правила](#правила)), которое **только читает** metadata-слой Vault KV (`list` имён + `created_time`, без чтения значений секретов и без удаления). Он не ходит на хосты по SSH и не чистит локальные файлы. Это сознательно держит Reaper «при базе», а не «при хостах» — иначе пришлось бы дать ему SSH-права на весь парк, что плохо с точки зрения blast radius. Vault-доступ Жнеца ограничен read-only metadata-путём набора ключей подписи Sigil (Vault-policy — в описании правила ниже).

Хостовый cleanup `/var/lib/soul-stack/{bin,modules}/` устроен отдельно:

- **pull-режим** — `soul`-демон сам чистит локальный кеш по своему расписанию. См. [`../soul/modules.md`](../soul/modules.md).
- **push-режим** — опциональная чистка в той же SSH-сессии `keeper.push`. См. [push.md](push.md).

> Blast-radius-обоснование «Reaper при базе, а не при хостах» и read-only-доступ к Vault-metadata разобраны как security-граница в [`../security/threat-model.md`](../security/threat-model.md).

### Что Reaper НЕ чистит (само-чистится каскадом)

- **`oracle_circuit`** (circuit-breaker Oracle, [ADR-030(a)](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)) — per-decree fixed-window счётчик срабатываний. Отдельного Reaper-правила **нет**: таблица само-чистится через FK `decree → decrees(name) ON DELETE CASCADE` — строка живёт ровно столько, сколько живёт Decree, и уходит вместе с ним.
  - **Known-op: re-enable провалившегося (tripped) Decree (MVP)** = **delete + recreate**. circuit-breaker авто-`disable`-ит сорвавшееся в петлю правило (`decrees.enabled=false`); чтобы вернуть его в строй с **чистым окном**, оператор удаляет Decree (`DELETE /v1/decrees/{name}`) и создаёт заново (`POST /v1/decrees`). Каскад при delete уносит строку `oracle_circuit`, поэтому пересозданный Decree стартует с `fire_count` от нуля (а не сразу с порога). Toggle-endpoint (`enabled` без пересоздания) — отдельный заход.

## Конфиг

Полный пример блока `reaper:` из `keeper.yml`:

```yaml
reaper:
  enabled: true
  interval: 1h          # как часто Жнец просыпается
  dry_run: false        # true — только посчитать, ничего не удалять (для аудита)
  batch_size: 500       # сколько записей за один прогон (защита от длинных транзакций)
  lock_ttl: 5m          # TTL Redis-lease лидера

  rules:
    expire_pending_seeds:
      enabled: true
      max_age: 24h      # bootstrap-токен в pending с истёкшим expires_at — удаляем
      action: delete

    purge_used_tokens:
      enabled: true
      max_age: 90d      # сожжённые/истёкшие bootstrap-токены — удаляем (аудит хранится отдельно)
      action: delete

    purge_souls:
      enabled: true
      statuses: [disconnected, expired]
      max_age: 30d      # запись Soul без жизни N дней — удаляем
      action: delete

    purge_old_seeds:
      enabled: true
      statuses: [superseded, expired, revoked]
      max_age: 90d      # история сертификатов старше — удаляем
      action: delete

    mark_disconnected:
      enabled: true
      stale_after: 90s  # last_seen_at старше N + нет live-стрима → disconnected
      action: set_status
      target_status: disconnected

    purge_audit_old:
      enabled: true
      max_age: 365d     # записи audit_log старше — удаляем; alias на keeper.yml → audit.retention_days
      action: delete

    purge_apply_runs:
      enabled: true
      max_age: 30d      # завершённые apply-прогоны старше — удаляем (retention apply-истории)
      action: delete

    purge_voyages:
      enabled: true
      max_age: 30d      # завершённые Voyage-прогоны (история) старше — удаляем; окно ВЫРОВНЕНО на purge_apply_runs (drill «voyage → apply_runs»)
      action: delete

    purge_push_runs:
      enabled: true
      max_age: 30d      # завершённые push-прогоны (success/partial_failed/failed/cancelled) старше — удаляем; окно ВЫРОВНЕНО на purge_apply_runs; НЕ путать с purge_orphan_push_runs (зомби)
      action: delete

    purge_incarnation_archive:
      enabled: true
      max_age: 365d     # архив снесённых incarnation (incarnation_archive): строки с archived_at старше — удаляем; compliance-окно
      action: delete

    purge_state_history_archive:
      enabled: true
      max_age: 365d     # архив журнала state_history снесённых incarnation (state_history_archive): строки с archived_at старше — удаляем; compliance-окно
      action: delete

    purge_archived_state_history:
      enabled: true
      max_age: 365d     # soft-deleted-строки ЖИВОЙ state_history (archived_at IS NOT NULL) старше — физически удаляем; compliance-окно
      action: delete

    purge_apply_task_register:
      enabled: true
      max_age: 1h       # register-данные завершённого прогона старше grace — удаляем (транзиентный run-state)
      action: delete

    reclaim_apply_runs:
      enabled: false    # ВЫКЛЮЧЕНО ПО ДЕФОЛТУ — см. предупреждение ниже
      stale_after: 1m   # формальный lease-таймаут; в предикат НЕ входит
      action: set_status
      target_status: planned

    reclaim_voyages:
      enabled: true     # ВКЛЮЧЕНО ПО ДЕФОЛТУ (path-defaulting) — работает и при ОТСУТСТВИИ ключа; см. ниже
      stale_after: 1m   # формальный lease-таймаут; в предикат НЕ входит (lease зашит в claim_expires_at)
      action: set_status
      target_status: pending

    reconcile_orphan_applying:
      enabled: true     # ВКЛЮЧЕНО ПО ДЕФОЛТУ (path-defaulting, как reclaim_voyages) — работает и при ОТСУТСТВИИ ключа; см. ниже
      stale_after: 90s  # ВХОДИТ в предикат (cutoff = NOW()-stale_after); parity mark_disconnected
      action: set_status
      target_status: ready

    reap_orphan_vault_keys:
      enabled: false    # ВЫКЛЮЧЕНО ПО ДЕФОЛТУ — report-only, требует Vault + list-права
      max_age: 24h      # grace по возрасту Vault-секрета против гонки Introduce
      action: report    # report-only: только считает/метрит/логирует, НИЧЕГО не удаляет

    archive_state_history:
      enabled: true
      keep_last_n: 50                 # сколько новейших активных снимков оставлять на incarnation
      keep_version_bump_snapshots: true  # никогда не архивировать snapshots шагов state_schema-миграции
      action: soft_delete             # soft-delete: помечаем `archived_at = NOW()`, физически НЕ удаляем

    scry_background:
      enabled: false                  # ВЫКЛЮЧЕНО ПО ДЕФОЛТУ — opt-in, ADR-031 Slice C
      interval: 6h                    # рекомендуемая периодичность фонового скана
      max_concurrent_in_flight: 10    # верхняя граница одновременных dry_run-прогонов (cluster-wide)
      min_interval_per_incarnation: 0 # 0 = без нижнего throttle; ORDER BY last_drift_check_at NULLS FIRST round-robin-ит сам
      action: report                  # информационный: counts → incarnation.last_drift_summary, никаких удалений

    purge_orphan_ephemeral_tidings:
      enabled: true     # OFF без enabled:true (map-driven, как reclaim_apply_runs — нужен явный ключ)
      max_age: 5m       # grace ПОСЛЕ терминала Voyage; ВХОДИТ в предикат (как purge_apply_task_register)
      action: delete
```

> **`reap_orphan_vault_keys` по дефолту выключено** — это **report-only** cross-store reconcile: правило находит осиротевшие приватники подписи Sigil в Vault (`secret/keeper/sigil-keys/<key_id>` без строки в реестре `sigil_signing_keys`) и **только** считает/метрит/логирует их. Оно **ничего не удаляет из Vault** и **не читает значения секретов** (приватники) — берёт только имена (`list`) и `created_time` (metadata). Включать имеет смысл лишь там, где настроен Vault и выдана list-/read-policy на metadata-путь набора ключей (см. описание правила в [Правила](#правила)). При выключенном/ненастроенном Vault правило деградирует (логирует fail и пропускается), не мешая остальным.

> **Known behavior (alert-noise):** metadata-промахи orphan-скана (секрет удалён между `list` и read `created_time`) учитываются в общем `keeper_vault_read_errors_total{kind=notfound}` — при включённом правиле дашборд может слегка зашуметь; это ожидаемо.

> **`reclaim_apply_runs` по дефолту выключено и его НЕЛЬЗЯ включать в прод, пока на Soul-агентах не раскатан attempt-fencing** ([ADR-027(g)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), Phase 2 / S-P2.2). Recovery возвращает протухший Ward в `planned`; живой Acolyte его пере-claim-ит и отправит **второй** `ApplyRequest` на хост. Без Soul-side guard по `attempt`-epoch Soul не отсечёт устаревший дубль прежнего владельца — получим **два apply на один хост**. Включать `enabled: true` только после полной раскатки fencing-Soul. Это и есть механизм инварианта «recovery не в прод до fencing».

> **`reclaim_voyages` по дефолту ВКЛЮЧЕНО через path-defaulting** ([ADR-043 §8](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), 2026-05-30). Это **обратный дефолт** относительно `reclaim_apply_runs` (тот map-driven: отсутствие ключа в `reaper.rules` = OFF, требует явного `enabled: true`). Механизм — **path-defaulting в `reaper.dispatch`**: правило исполняется при **отсутствии** ключа `reclaim_voyages` в `reaper.rules` **ЛИБО** при явном `enabled: true`; выключается **только** явным `reclaim_voyages.enabled: false`. Правило возвращает осиротевший running-Voyage (владелец умер до финализации, lease протух) обратно в `pending` (attempt++) — другой Keeper-инстанс пере-claim-ит и доисполнит leg с сохранённого `current_batch_index`. **Почему ON по дефолту:** целевой масштаб до 100k Souls, Keeper — горизонтально масштабируемый stateless-кластер ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)), где рестарт/замена инстанса (деплой, OOM, авто-скейл) — штатное регулярное событие → осиротевшие running-Voyage регулярны → recovery не должен зависеть от того, вписал ли оператор правило в `reaper.rules`. **Почему безопасно (в отличие от `reclaim_apply_runs`):** финализация Voyage идёт под CAS-ownership-guard (`voyage.Finalize` пишет терминал `WHERE claimed_by_kid = $2`) — stale-воркер, потерявший lease, ловит `ErrLeaseLost`, дубль-commit невозможен (**exactly-once на уровне top-level Voyage**; per-leg apply наследует apply-fencing [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Требование корректности: renew-интервал воркера ≪ lease-TTL ([ADR-043 §8(d)](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)).

> **Voyage-orphan-lock-release (шов re-run): реклеймнутый VoyageWorker снимает осиротевший `incarnation.status='applying'` перед re-run** ([ADR-027(l)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Когда `reclaim_voyages` вернул осиротевший `kind=scenario`-Voyage в `pending` и другой Keeper-инстанс пере-claim-ит его, VoyageWorker перед re-run leg-а сталкивается с `incarnation.status='applying'`, оставленным **крашнутым прошлым владельцем** (тот умер до финализации, single-winner state-commit `applying`→терминал не отработал). VoyageWorker **снимает СВОЙ осиротевший `applying`** (только тот, что принадлежит реклеймнутому им Voyage) и продолжает re-run. **Без этого шва reclaimed Voyage зависает** — re-run упирается в «incarnation уже applying» и не может стартовать leg, то есть `reclaim_voyages`-recovery был бы сломан вхолостую (Voyage вернулся в `pending`, но никогда не доисполняется). **Этот шов — ВКЛЮЧЁН ВСЕГДА вместе с `reclaim_voyages`** (часть его re-run-пути), **НЕ за отдельным opt-in** — в отличие от `reclaim_apply_runs` (default-OFF, map-driven, требует явного гейта). Так и должно быть: `reclaim_voyages` сам по себе default-ON (path-defaulting, см. блок выше), а без orphan-lock-release Voyage-recovery нерабочее → шов не имеет собственного выключателя.
>
> **Double-apply класс Voyage-orphan-release = тот же приемлемый класс, что у `reclaim_apply_runs`.** Шов снимает `applying` и доисполняет leg → per-leg apply этого Voyage наследует apply-fencing ([ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). При **network-partition** (живой, но партиционированный прошлый владелец продолжает свой apply, пока наш re-run шлёт второй) на хост может уйти **двойная отправка задания**. От **порчи `incarnation.state`** это защищает не «один apply на хост», а два барьера: (1) **gate-1 attempt-fencing RunResult** — stale-`RunResult` прежнего владельца отсекается на приёме по `apply_runs.attempt` (epoch вырос при пере-claim), порченый коммит state в гонке не проходит; (2) **идемпотентность модулей** — повторная отправка того же задания на хост не должна менять результат при корректно написанном модуле. **Оператор должен знать:** Voyage-recovery, будучи **default-ON**, **уже несёт этот double-apply класс на проде** — в отличие от `reclaim_apply_runs`, где этот же класс приходит только после явного включения под гейтом. Полное операторское разъяснение — [recovery-reclaim-apply-runs.md → Voyage-orphan-release](../operations/recovery-reclaim-apply-runs.md#voyage-orphan-lock-release--тот-же-double-apply-класс-включён-иначе).

> **Известное ограничение (known-limitation): `GET /v1/voyages?status=running` показывает временно-осиротевшие прогоны.** Фильтр по `status` отдаёт значение **сырой PG-колонки `voyages.status` без lease-overlay**. Поэтому осиротевший Voyage (`claim_expires_at < NOW()`, владелец умер, но `reclaim_voyages` ещё не подобрал его на ближайшем тике) попадает в выборку `?status=running` — он остаётся `running` в БД в течение **окна осиротелости ≈ `reaper.interval`** (от истечения lease до ближайшего reaper-тика). Это **by-design, не баг**: при `reclaim_voyages` default-ON окно короткое, данные не теряются (Voyage будет подобран и доисполнен с сохранённого `current_batch_index`). В отличие от [`GET /v1/souls`](../soul/identity.md#статусы-soul-и-переходы), где presence деривируется через Redis-lease-overlay поверх снимка `souls.status`, **Voyage read-path lease-overlay не имеет** — это осознанный долг. Чтобы отличить живое владение от осиротевшего, смотреть `claim_expires_at` в detail-ответе [`GET /v1/voyages/{id}`](#) (`claim_expires_at < NOW()` ⇒ владелец мёртв, ждёт reclaim; `>= NOW()` ⇒ lease жив). Кандидат на устранение — overlay-в-проекции read-path-а (по образцу soul presence-overlay) при реальном запросе оператора.

> **Известное ограничение (known-limitation): реклейм `command`-Voyage → at-least-once-семантика дочернего Errand при failover.** См. [ADR-043 §8(d)](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) (рекомендация: идемпотентные command-модули `core.cmd.shell` / `core.exec.run`).

> **`reconcile_orphan_applying` по дефолту ВКЛЮЧЕНО через path-defaulting** ([ADR-027(m)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), recovery-completeness backstop). Механизм дефолта **идентичен `reclaim_voyages`**: правило исполняется при **отсутствии** ключа `reconcile_orphan_applying` в `reaper.rules` **ЛИБО** при явном `enabled: true`; выключается **только** явным `reconcile_orphan_applying.enabled: false`. **Что закрывает:** прямой (standalone, не под Voyage) `incarnation.run` ставит `incarnation.status='applying'` в `lockRun`; если Keeper-владелец прогона умирает до терминала — lock виснет **навсегда**. Voyage-путь закрыт `reclaim_voyages` + Voyage-orphan-lock-release ([ADR-027(l)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)) по back-link `voyage_targets.apply_id`, но у прямого run-а строки `voyage_targets` нет — back-link структурно недостижим; `reclaim_apply_runs` тоже не достаёт (он реклеймит протухший `claimed`-Ward в `apply_runs`, а applying-lock — отдельный флаг на строке `incarnation`). **Двухфазный детект:** (1) SQL-кандидаты — stale applying-строки (`status='applying' AND applying_since < NOW()-stale_after AND applying_by_kid IS NOT NULL`) по partial-индексу `incarnation_applying_scan_idx`; (2) presence-чек — для каждого кандидата `InstanceAlive(applying_by_kid)` в Conclave: жив → skip (прогон реально идёт), мёртв → снятие, ошибка presence-чека → fail-safe skip (флап Redis ⇒ НЕ объявлять мёртвым). Снятие — идемпотентный `ReleaseApplyingOrphan` (FENCING-1 no-live-rival + single-winner CAS `applying → ready` внутри). **`stale_after` ВХОДИТ в предикат** (cutoff = `NOW()-stale_after`, в отличие от lease-аргументов reclaim-правил), default **90s** (parity `mark_disconnected` — тот же класс «владелец долго молчит», presence-чек добивает решение). **Presence-gate = no-op без живого Conclave:** правило доказывает смерть владельца только через `InstanceAlive`; при недоступном Redis presence-чек кандидата падает → fail-safe skip (живой прогон не срывается). Per-row audit `reaper.reconcile_orphan_applying.executed` на каждое снятие. **Known-gap:** строки с NULL `applying_by_kid` (legacy/pre-082 либо rerun-create микроокно `UnlockForRerun`-tx ↔ epoch-write-tx) правило НЕ реклеймит — без presence-свидетеля смерти владельца снятие небезопасно; такой осиротевший lock снимается оператором вручную через `POST /v1/incarnations/{name}/unlock`. **Default-ON безопасен:** presence-gate (`InstanceAlive`=false обязателен) + FENCING-1 + single-winner CAS не дают снять живой lock; residual double-apply (network-partition живого владельца) — тот же приемлемый класс, что у `reclaim_apply_runs` / Voyage-orphan-release, под защитой attempt-fencing `RunResult` + идемпотентности модулей. Операторское разъяснение — [recovery-reclaim-apply-runs.md → standalone-orphan reconcile](../operations/recovery-reclaim-apply-runs.md#standalone-orphan-reconcile--тот-же-класс-для-прямого-run-а).

### Структура правила

Каждое значение в map-е `reaper.rules` — объект с полями ниже. Ключ map-а (`expire_pending_seeds`, `purge_souls`, …) — имя правила; оно одновременно идентифицирует **над какой таблицей** правило работает (привязка зафиксирована в [Правила](#правила) ниже). Семантика `max_age` / `stale_after` (от какого поля считается возраст) тоже зависит от таблицы и нормирована per-rule.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `enabled` | `bool` | `true` | Включить/выключить правило точечно, без передеплоя бинаря. |
| `action` | `enum{expire, delete, set_status, report, soft_delete}` | — | Что делать с записями, удовлетворяющими условию. `expire` — пометить `expired`; `delete` — удалить строку; `set_status` — перевести в `target_status`; `report` — **report-only**: только посчитать/залогировать находку, ничего не менять (используется cross-store reconcile-правилом `reap_orphan_vault_keys`); `soft_delete` — пометить `archived_at = NOW()`, физическое удаление запрещено (используется правилом `archive_state_history`, ADR-Q19 retention). Closed enum; расширение — propose-and-wait, не freeform. |
| `max_age` | `duration` | — | Возраст записи (от поля таблицы, см. [Правила](#правила)), после которого правило срабатывает. Обязателен для `action: expire` / `action: delete`. |
| `stale_after` | `duration` | — | Время с последнего `last_seen_at` (или эквивалента таблицы), после которого правило срабатывает. Обязателен для `action: set_status`. |
| `statuses` | `list<enum>` | — | Фильтр — применять правило только к записям в указанных статусах (опционально, см. таблицу обязательности ниже). Допустимые значения зависят от таблицы (см. cross-link в [Правила](#правила)). |
| `target_status` | `enum` | — | Целевой статус для `action: set_status`. Допустимые значения зависят от таблицы. |
| `keep_last_n` | `integer` | `50` | Только для `action: soft_delete`. Сколько новейших активных снимков оставлять на единицу (incarnation для `archive_state_history`). Положительное; `0` или меньше — ошибка конфигурации (нулевой keep = «архивировать всё»). |
| `keep_version_bump_snapshots` | `bool` | `true` | Только для `action: soft_delete` правила `archive_state_history`. `true` — снимки шагов state_schema-миграции (`scenario='migration'`) НЕ архивируются никогда, независимо от `keep_last_n`; restorable anchor для recovery схемы при rollback ADR-019. `false` — правило архивирует их наравне с обычными (явный opt-out оператора). |
| `max_concurrent_in_flight` | `integer` | `10` | Только для правила `scry_background` (ADR-031 Slice C). Cluster-wide верхняя граница одновременных фоновых dry_run-прогонов: `<= 0` глушит правило без снятия `enabled`. |
| `min_interval_per_incarnation` | `duration` | `0` (без throttle) | Только для правила `scry_background`. Минимальный интервал между фоновыми сканами одного incarnation; `0` или пусто — без нижней границы (естественный round-robin даёт ORDER BY `last_drift_check_at NULLS FIRST`). |

**Условная обязательность по `action`:**

| `action` | Обязательные поля | Опциональные поля |
|---|---|---|
| `expire` | `max_age` | `enabled`, `statuses` |
| `delete` | `max_age` | `enabled`, `statuses` |
| `set_status` | `stale_after`, `target_status` | `enabled`, `statuses` |
| `report` | `max_age` | `enabled` |
| `soft_delete` | — | `enabled`, `keep_last_n`, `keep_version_bump_snapshots` |

Допустимые значения `statuses` и `target_status` **в этом документе не нормируются** — они зависят от таблицы и определены в [`../soul/identity.md`](../soul/identity.md) (`souls` / `bootstrap_tokens` / `soul_seeds` и их статусы) и [storage.md](storage.md). Парсер `keeper.yml` отвергает значение, не входящее в enum таблицы конкретного правила, с ошибкой `unknown_status`.

### Правила

| Правило | Над чем работает | `action` | Обязательные поля | Что делает |
|---|---|---|---|---|
| `expire_pending_seeds` | `bootstrap_tokens` (см. [storage.md](storage.md), [`../soul/identity.md`](../soul/identity.md)) | `delete` | `max_age` (+ `enabled`, `statuses` опц.) | Удаляет неиспользованные (`used_at IS NULL`) bootstrap-токены с истёкшим `expires_at` — старше на `max_age` сверх момента expiry (default 24h). Истёкший pending-токен не может быть использован (`Burn` отвергает его), хранить его дальше смысла нет; долговременный аудит создания живёт в `audit_log`. У `bootstrap_tokens` нет колонки `status`, поэтому исторически `action: expire` (помечает `expired`) на этой таблице не применим — фактическая семантика правила — `delete`. |
| `purge_used_tokens` | `bootstrap_tokens` | `delete` | `max_age` (+ `enabled`, `statuses` опц.) | Удаляет сожжённые / истёкшие токены старше `max_age`. Долговременный аудит хранится отдельно. |
| `purge_souls` | `souls` | `delete` | `max_age`, `statuses` (+ `enabled` опц.) | Удаляет записи Soul в `disconnected` / `expired` старше `max_age` (default 30d). |
| `purge_old_seeds` | `soul_seeds` | `delete` | `max_age`, `statuses` (+ `enabled` опц.) | Удаляет историю сертификатов в `superseded` / `expired` / `revoked` старше `max_age` (default 90d). |
| `mark_disconnected` | `souls` | `set_status` | `stale_after`, `target_status` (+ `enabled`, `statuses` опц.) | **Двунаправленный lease-aware reconcile** снимка `souls.status` ([ADR-006(a)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). (1) `connected` → `disconnected`: `last_seen_at` старше `stale_after` (default 90s) **и** нет живого Redis SID-lease. (2) `disconnected` → `connected`: жив SID-lease (Soul реально online — реконнект захватил lease, а снимок остался `disconnected`). Каждое направление двухфазное: выбрать PG-кандидатов, сверить с lease `soul:<sid>:lock`, пометить. Так idle-Soul (PG `last_seen_at` stale, но стрим жив) не метится disconnected ложно, **и** снимок не латчится в `disconnected` после первого «обрыв+sweep» (реконнект чинит снимок через reconcile, т.к. eventstream presence в PG не пишет, а Bootstrap-RPC уже-онбордированного Soul-а не срабатывает). Без настроенного Redis (single-instance dev) — fallback на чисто-SQL **одностороннее** `mark_disconnected` (миграция 014), где stale `last_seen_at` ⇔ нет стрима по построению (один инстанс), и латча нет. |
| `purge_audit_old` | `audit_log` ([storage.md → Таблица `audit_log`](storage.md#таблица-audit_log), [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) | `delete` | `max_age` (+ `enabled` опц.) | Удаляет записи `audit_log` старше `max_age` (default 365d, считается от `created_at`). `max_age` — alias на `keeper.yml → audit.retention_days` ([config.md → audit](config.md#audit)); при расхождении значений парсер `keeper.yml` отвергает конфиг с `audit_retention_mismatch`. |
| `purge_apply_runs` | `apply_runs` (миграция 018, реестр apply-прогонов scenario-runner-а) | `delete` | `max_age` (+ `enabled` опц.) | Удаляет **завершённые** apply-прогоны (`status` ∈ `success` / `failed` / `cancelled` и `finished_at IS NOT NULL`) старше `max_age` (default 30d, считается от `finished_at`). Прогоны в `running` **никогда не удаляются** — это идущие/висящие прогоны, их триаж — отдельный механизм. |
| `purge_voyages` | `voyages` ([ADR-043 §8](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), миграция 059; SQL-функция `purge_voyages` — миграция 075, [ADR-046 §79](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)) | `delete` | `max_age` (+ `enabled` опц.) | **Retention растущей истории Voyage-прогонов.** Удаляет **завершённые** Voyage (`status` ∈ `succeeded` / `failed` / `partial_failed` / `cancelled` и `finished_at IS NOT NULL`) старше `max_age` (default **30d**, считается от `finished_at`). `voyages` — единственная run-history таблица без собственного retention: каждый ручной запуск и каждый спавн Cadence добавляет строку, рост без потолка. Прогоны в `scheduled` / `pending` / `running` **никогда не удаляются** — это незавершённые/идущие прогоны (терминал им проставит VoyageWorker.Finalize или `reclaim_voyages`, не Жнец). **Не путать с активными Cadence-расписаниями:** правило чистит только историю Voyage, таблицы `cadences` / `incarnation_choirs` (активная declared-топология) оно **не трогает** — их удаление было бы порчей данных и остаётся операторским действием ([ADR-046 §9](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). **Каскад:** `voyage_targets` (Leg-строки прогона) уносятся `ON DELETE CASCADE` (миграция 059). soft-link-и `voyage_targets.apply_id` / `errand_id` (на `apply_runs` / `errands`) и `tidings.voyage_id` (ephemeral) — **НЕ** FK на `voyages`; purge их не удаляет и не оставляет битых ссылок (apply_runs чистится `purge_apply_runs`, errands — `purge_old_errands`, ephemeral-Tiding-и снимаются раньше правилом `purge_orphan_ephemeral_tidings`). `voyages.cadence_id → cadences ON DELETE SET NULL` (миграция 066): purge детей не трогает расписание. **Корреляционный инвариант:** окно по умолчанию (30d) **выровнено на `purge_apply_runs`**, чтобы drill «voyage → его apply_runs» не терял одну из сторон (voyage удалён, а apply_runs ещё нужны для корреляции — или наоборот); менять одно окно без другого — рассинхрон. Параметр SQL-функции назван `batch_limit` (а не `batch_size`), т.к. колонка `voyages.batch_size` (размер батча прогона) дала бы `ambiguous` в `LIMIT`. |
| `purge_push_runs` | `push_runs` (миграция 051; SQL-функция `purge_push_runs` — миграция 076, [push.md](push.md)) | `delete` | `max_age` (+ `enabled` опц.) | **Retention растущей run-history push-прогонов.** Удаляет **завершённые** push-прогоны (`status` ∈ `success` / `partial_failed` / `failed` / `cancelled` и `finished_at IS NOT NULL`) старше `max_age` (default **30d**, считается от `finished_at`). `push_runs` — run-history таблица того же класса, что `apply_runs` / `voyages`: каждый запуск `keeper.push` добавляет строку, рост без потолка. Прогоны в `pending` / `running` **никогда не удаляются** — их терминализирует executeAsync или (если зомби) правило `purge_orphan_push_runs`. **Не путать с `purge_orphan_push_runs`** (TTL 1h, `set_status`/`cancelled` для висящих in-flight) — это правило сносит уже **завершённую** историю, то — терминализирует осиротевшие активные. **Каскада нет:** per-host результаты лежат inline в `push_runs.summary` (jsonb), дочерних FK на `push_runs` нет (миграция 051). **Корреляционный инвариант:** окно по умолчанию (30d) **выровнено на `purge_apply_runs`**, чтобы drill «push-run → его per-host summary» не терял хвост run-history-таблиц одного класса; менять одно окно без другого — рассинхрон. |
| `purge_incarnation_archive` | `incarnation_archive` (миграция 039; SQL-функция — миграция 077) | `delete` | `max_age` (+ `enabled` опц.) | **Retention архива снесённых incarnation.** Удаляет строки `incarnation_archive` с `archived_at` старше `max_age` (default **365d**, считается от `archived_at`). Архив — историко-compliance снимок snapshot-а incarnation на момент destroy (что было до удаления), поэтому окно по умолчанию **сознательно консервативнее** run-history-окон (30d): год удержания под аудит. Дочерних FK на архив нет (миграция 039) — каскада нет. Оператор настраивает `max_age` под свои compliance-требования. |
| `purge_state_history_archive` | `state_history_archive` (миграция 039; SQL-функция — миграция 077) | `delete` | `max_age` (+ `enabled` опц.) | **Retention архива журнала state_history снесённых incarnation.** Удаляет строки `state_history_archive` с `archived_at` старше `max_age` (default **365d**, считается от `archived_at`). Parity с `purge_incarnation_archive` — то же compliance-окно 365d; обе таблицы (`incarnation_archive` + `state_history_archive`) записываются одной транзакцией при destroy. Дочерних FK нет. **Не путать с `purge_archived_state_history`** (живая `state_history`, не архив-таблица). |
| `purge_archived_state_history` | `state_history` (миграция 006; колонка `archived_at` из 048; SQL-функция — миграция 077) | `delete` | `max_age` (+ `enabled` опц.) | **Физический снос soft-deleted-снимков из ЖИВОЙ `state_history`.** Удаляет строки `state_history` с `archived_at IS NOT NULL` (помеченные ранее правилом `archive_state_history`) старше `max_age` (default **365d**, считается от `archived_at`). **Не путать с `archive_state_history`** (миграция 049): то правило **только проставляет** soft-delete-флаг (`archived_at = NOW()`) сверх `keep_last_n` последних снимков, **это** — окончательно сносит уже помеченные по истечении compliance-окна, разгружая живую таблицу. Активные снимки (`archived_at IS NULL`) **не трогаются**. Так soft-delete (archive) и физический снос (этот purge) разнесены по времени: оператор может выгрузить архивные снимки до их физического удаления. |
| `purge_apply_task_register` | `apply_task_register` (миграция 022, накопитель register-данных задач прогона) | `delete` | `max_age` (+ `enabled` опц.) | Удаляет register-строки прогонов в **терминальном** статусе (`apply_runs.status` ∈ `success` / `failed` / `cancelled` и `finished_at IS NOT NULL`) старше `max_age` (default **1h**, считается от `finished_at`). Здесь `max_age` семантически = **grace** после завершения прогона: register нужен scenario-runner-у только до cross-host barrier-а, дальше это транзиентный plaintext (потенциально с секретами). register **активного** (`running`) прогона **не удаляется никогда** — критерий «терминал + grace», а не TTL по `created_at`, гарантирует это независимо от длительности прогона. См. также: FK `ON DELETE CASCADE` чистит register каскадом вместе с самим apply_run (правило `purge_apply_runs`, 30d) — это правило снимает register **раньше**, сокращая окно plaintext-хранения. |
| `reap_orphan_vault_keys` | Vault KV `secret/keeper/sigil-keys/<key_id>` ↔ реестр `sigil_signing_keys` (миграция 037, ADR-026(h)) | `report` | `max_age` (+ `enabled` опц.) | **Report-only cross-store reconcile** ([ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). Находит **осиротевшие** приватники подписи Sigil — секреты `secret/keeper/sigil-keys/<key_id>` в Vault, для которых **нет строки** в `sigil_signing_keys` ни в одном статусе (**и `active`, и `retired` считаются живыми** — retired-приватник нужен для verify ранее подписанных Sigil-ов). Сирота возникает, например, когда `Introduce` записал приватник в Vault, а PG-вставка следом упала: keyservice сознательно **не делает** reverse-cleanup. Правило **только считает/метрит/логирует** находку — **ничего не удаляет** и **не читает значения секретов** (приватники): берёт лишь имена (`list`) + `created_time` (metadata). Здесь `max_age` семантически = **grace** по возрасту Vault-секрета (default **24h**, считается от `created_time`): отсекает гонку с `Introduce`, где запись в Vault опережает PG-commit — свежий секрет ещё может получить строку в реестре, поэтому моложе grace в сироты **не записывается**. `batch_size` ограничивает число metadata-round-trip-ов за один прогон. Требует настроенного Vault и Vault-policy `path "secret/metadata/keeper/sigil-keys/*" { capabilities = ["list", "read"] }` — **без `delete`, без чтения data-пути** `secret/data/...`. При отсутствии Vault-клиента правило деградирует (логирует fail, прогон продолжается). По дефолту **выключено** (см. предупреждение в [Конфиг](#конфиг)). Метрика `keeper_reaper_rule_purged_total{rule="reap_orphan_vault_keys"}` = **число задетектированных сирот** (для report-only «purged» = «detected», ничего не удалено). |
| `scry_background` | `incarnation` (миграция 050: колонки `last_drift_check_at` / `last_drift_summary`) + `apply_runs` (work-queue dry_run) | `report` | `max_concurrent_in_flight` (опц., default 10), `min_interval_per_incarnation` (опц., default 0) | **Фоновое периодическое drift-сканирование** ([ADR-031](../architecture.md) Slice C, pilot). Итератор отбирает incarnation в статусах `ready`/`drift` без активного apply-прогона (`NOT IN apply_runs WHERE finished_at IS NULL`) и в порядке `last_drift_check_at NULLS FIRST` (новые сканируются первыми). Для каждого: short FOR UPDATE-tx проверяет, что статус ещё `ready`/`drift` (защита от гонки с operator-Run), затем вызывает тот же `scenario.Runner.CheckDrift`, что и on-demand Slice B (`POST /v1/incarnations/{name}/check-drift`): dispatches dry_run `converge` по всем хостам через work-queue (Acolyte рендерит и шлёт `ApplyRequest{dry_run:true}`, Soul зовёт `mod.Plan`), ждёт барьер, собирает DriftReport. **Counts-only в фоне**: полный DriftReport НЕ сохраняется — пишутся только counts-агрегаты в `incarnation.last_drift_summary` (`{hosts_drifted, hosts_clean, hosts_unsupported, hosts_failed, total_hosts, scanned_at}`) и `last_drift_check_at = NOW()`; полный отчёт on-demand из Slice B возвращается прямо в response. `max_concurrent_in_flight` ограничивает одновременные фоновые dry_run-прогоны cluster-wide (counter — `SELECT count(*) FROM apply_runs WHERE recipe->>'dry_run'='true' AND finished_at IS NULL`). `min_interval_per_incarnation > 0` дополнительно отсекает повторный скан той же incarnation раньше срока. Audit-event `incarnation.drift_checked` пишется с `source: background`, `archon_aid: NULL`. **Default OFF — opt-in**: правило отсутствует в base-конфиге, оператор включает явно. Не нужно держать вместе с Slice B как зависимость: оба используют один и тот же pipeline, но независимо. |
| `archive_state_history` | `state_history` (миграция 006, колонка `archived_at` из 048) | `soft_delete` | `keep_last_n` (опц., default 50), `keep_version_bump_snapshots` (опц., default true) | **Soft-delete retention журнала state_history** ([ADR-Q19](../architecture.md), PM-решение 2026-05). Помечает `archived_at = NOW()` активные снимки `state_history` сверх `keep_last_n` последних на incarnation (по `at DESC`); физическое удаление запрещено — soft-deleted-снимки остаются в таблице (опц. для внешнего bulk-выгрузчика). При `keep_version_bump_snapshots: true` (default) снимки шагов state_schema-миграции (`scenario='migration'`) НЕ архивируются никогда — restorable anchor для recovery схемы при rollback ADR-019. `batch_size` ограничивает число помеченных за один прогон; drain до 0 — последовательность прогонов loop-а. SQL-функция `archive_state_history(integer, boolean, integer)` (миграция 049). Чтение истории через [`HistorySelectByName`] по умолчанию исключает soft-deleted (`archived_at IS NULL`); Operator API может включить включение архивных снимков флагом `include_archived=true`. |
| `reclaim_apply_runs` | `apply_runs` (миграция 025, Ward-claim колонки) | `set_status` | `stale_after`, `target_status` (+ `enabled` опц.) | **Recovery-скан протухших Ward** ([ADR-027(i)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), Phase 2). Возвращает зомби-claim — задания мёртвого Acolyte (`status` ∈ `claimed` / `running` с истёкшим `claim_expires_at < NOW()`) — обратно в `planned` для пере-claim, сбрасывая `claim_by_kid` / `claim_at` / `claim_expires_at`. **`attempt` НЕ сбрасывается** — fencing-epoch инкрементит следующий claim, и Soul-guard отсекает stale-`ApplyRequest`. **Закрывает дыру «висячий applying»** (мёртвый владитель не финализирует прогон). Использует partial-индекс `apply_runs_claim_scan_idx`. `stale_after`/`target_status` — формальные поля action-схемы; в SQL-предикат **не входят** (recovery сравнивает `claim_expires_at < NOW()` напрямую, фактический lease зашит в `claim_expires_at` при захвате Ward). `target_status` всегда `planned`. **Включается ТОЛЬКО при attempt-fencing на Soul-агентах** (см. предупреждение в [Конфиг](#конфиг)), иначе задвоит apply. |
| `reclaim_voyages` | `voyages` ([ADR-043 §8](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), миграция 059 — `voyages_claim_scan_idx`) | `set_status` | `stale_after`, `target_status` (+ `enabled` опц.) | **Recovery-скан протухших Voyage-claim-ов** ([ADR-043 §8 п.7](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)). Возвращает осиротевший Voyage с истёкшим lease (`status='running' AND claim_expires_at < NOW()`) обратно в `pending` для пере-claim другим Keeper-инстансом, сбрасывая `claimed_by_kid` / `last_renewed_at` / `claim_expires_at`. **`attempt` инкрементится** (fencing-epoch parity с `reclaim_apply_runs`); `current_batch_index` НЕ трогается — Keeper-Б при подборе читает progress с этого поля и продолжает с того же Leg-а. Возврат в `pending` (НЕ в исходный `scheduled`): к моменту `running` `schedule_at` заведомо наступил, строка должна быть немедленно подбираема. Использует partial-индекс `voyages_claim_scan_idx` (`WHERE status='running'`) + `FOR UPDATE SKIP LOCKED` (защита от гонки с конкурентным claim/renew). `stale_after`/`target_status` — формальные поля action-схемы; в SQL-предикат **не входят** (фактический lease зашит в `claim_expires_at` при захвате через voyage.ClaimNext; предикат сравнивает `claim_expires_at < NOW()` напрямую). `target_status` всегда `pending`. Audit-event `voyage.reclaimed { voyage_id, last_renewed_at, attempt_after }` per-row (kind-agnostic — единое событие для `scenario`/`command`-Voyage). **По дефолту ВКЛЮЧЕНО через path-defaulting** (обратный дефолт относительно `reclaim_apply_runs`; работает при отсутствии ключа, выключается только явным `enabled: false`) — см. предупреждение в [Конфиг](#конфиг). Дубль-commit отсекает CAS-ownership-guard `voyage.Finalize` (`WHERE claimed_by_kid=$2` → `ErrLeaseLost` для stale-воркера; exactly-once на top-level Voyage). |
| `reconcile_orphan_applying` | `incarnation` (миграция 082, applying-epoch колонки `applying_apply_id`/`applying_attempt`/`applying_by_kid`/`applying_since` + partial-индекс `incarnation_applying_scan_idx`) | `set_status` | `stale_after`, `target_status` (+ `enabled` опц.) | **Снятие осиротевшего `incarnation.status='applying'` lock прямого (standalone) scenario-run** ([ADR-027(m)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Закрывает шов, симметричный Voyage-orphan-lock-release ([ADR-027(l)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)), но для прямого `incarnation.run` (у него нет back-link `voyage_targets`). **Двухфазно:** (1) SQL-кандидаты — stale applying-строки (`status='applying' AND applying_since < NOW()-stale_after AND applying_by_kid IS NOT NULL`) по partial-индексу `incarnation_applying_scan_idx`; (2) presence — для каждого `InstanceAlive(applying_by_kid)` в Conclave: жив → skip, мёртв → снятие через идемпотентный `ReleaseApplyingOrphan` (FENCING-1 no-live-rival + single-winner CAS `applying → ready`), ошибка presence-чека → fail-safe skip. **`stale_after` ВХОДИТ в SQL-предикат** (cutoff = `NOW()-stale_after`, в отличие от lease-аргументов reclaim-правил), default **90s** (parity `mark_disconnected`). `target_status` всегда `ready`. Audit-event `reaper.reconcile_orphan_applying.executed` per-row (`{incarnation, prev_kid, apply_id}`). **По дефолту ВКЛЮЧЕНО через path-defaulting** (как `reclaim_voyages`; выключается только явным `enabled: false`) — см. предупреждение в [Конфиг](#конфиг). **Known-gap:** NULL-`applying_by_kid` (legacy/pre-082, rerun-create микроокно) НЕ реклеймится — ручной `unlock`. **Presence-gate = no-op без живого Conclave** (нет живого Redis ⇒ presence-чек падает ⇒ fail-safe skip кандидата, не no-op всего правила). |
| `purge_old_errands` | `errands` (миграция 052, [ADR-033](../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)) | `delete` | `max_age` (+ `enabled` опц.) | `DELETE FROM errands WHERE ttl_at < NOW()`. Default TTL **7д** (`errands.ttl_at = started_at + 7d`); поле зашивается dispatcher-ом при INSERT-е (`errand.TTLDefault`). Параметр `max_age` правила в SQL-предикат **не входит** — TTL зашит в строку, аргумент держится для совместимости с общим duration-runner-ом и как documented override для будущих миграций ttl-логики. Индекс `errands_ttl_idx` (миграция 052) делает условие cheap-scanable. |
| `purge_orphan_ephemeral_tidings` | `tidings` (миграция 072 — partial-индекс `tidings_ephemeral_voyage_idx` `WHERE ephemeral`, [ADR-052(g)](../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)) | `delete` | `max_age` (+ `enabled` опц.) | **Снос осиротевших ephemeral-Tiding-ов** ([ADR-052(g)](../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов) amendment N2). Удаляет **разовые** правила (`ephemeral=true`), чей Voyage либо **в терминале** (`status` ∈ `succeeded`/`failed`/`partial_failed`/`cancelled` и `finished_at` старше grace) либо **не существует** (строка `voyages` удалена). Здесь `max_age` семантически = **grace ПОСЛЕ терминала** Voyage и **входит** в SQL-предикат (parity `purge_apply_task_register`: max_age-as-grace), default **5m**. Grace — условие корректности, а не косметика: dispatcher матчит терминальное событие против ephemeral-правила асинхронно (tap-consumer через bounded-канал), снос раньше окна tap-consumer-а удалил бы правило **до** того, как уйдёт уведомление о завершении прогона → терминальное уведомление потерялось бы. Один `DELETE` одним statement-ом по partial-индексу (ephemeral-правил мало — десятки на in-flight прогоны; постоянные правила в скан не попадают), `batch_size` не применяется. Постоянные Tiding (`ephemeral=false`) правило **не трогает**. Default-семантика **OFF без `enabled: true`** (map-driven, как `reclaim_apply_runs` — не path-defaulting `reclaim_voyages`); при отсутствии wire-up herald-стека правило деградирует (warn + пропуск), не мешая остальным. |

Точные пороги (`max_age`, `stale_after`) подбираются под инсталляцию через hot-reload.

`mark_disconnected` согласует снимок `souls.status` с фактом Redis SID-lease **в обе стороны**; каждое направление двухфазное (lease-aware, [ADR-006(a)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)).

**Направление disconnect** (`connected` → `disconnected`):

1. **Кандидаты по PG.** SQL-функция `select_disconnect_candidates(stale_after, batch)` (миграция 043) отбирает `connected`-souls с `last_seen_at` старше `stale_after`. Throttled-flush из EventStream-handler-а (не чаще раза в `stale_after / 3`, см. [storage.md → Redis — горячий слой и координация](storage.md#redis--горячий-слой-и-координация)) держит `last_seen_at` свежим, пока по стриму идёт трафик — это первый барьер.
2. **Сверка с Redis SID-lease.** Для каждого кандидата Purger проверяет наличие живого lease `soul:<sid>:lock` ([`SoulStreamAlive`](storage.md#redis--горячий-слой-и-координация)). Lease держится renewal-goroutine-ой handler-а, пока стрим жив, и пропадает при штатном Release либо по TTL после crash-а. Кандидат с живым lease **исключается** — это закрывает дыру с **idle-Soul**: хост, который шлёт лишь soulprint раз в `refresh_interval`, мог иметь stale `last_seen_at` внутри `stale_after` (ни одного app-сообщения в окне), но его стрим жив — и ложно метился бы disconnected без lease-проверки.

Пережившие обе фазы (stale `last_seen_at` **и** нет lease) метятся `disconnected` через `mark_disconnected_sids(text[])` (миграция 043).

**Направление reconnect** (`disconnected` → `connected`):

1. **Кандидаты по PG.** SQL-функция `select_reconnect_candidates(batch)` (миграция 043) отбирает `disconnected`-souls **любого** `last_seen_at` — без duration-предиката: онлайновость решает живой lease, а не свежесть PG-снимка (idle-Soul на живом стриме держит lease, но `last_seen_at` мог протухнуть).
2. **Сверка с Redis SID-lease.** Кандидат с **живым** lease — реально online (реконнект захватил lease, а снимок остался `disconnected`) → возврат в `connected` через `mark_connected_sids(text[])` (миграция 043, guard `status='disconnected'` защищает `revoked`/`destroyed` от перетирания). Без этого направления снимок латчился бы в `disconnected` навсегда после первого sweep-а: реконнект поднимает lease, но строку не двигал бы никто (eventstream presence в PG не пишет, Bootstrap-RPC уже-онбордированного Soul-а не срабатывает).

Ошибка Redis-проверки конкретного SID — fail-safe в **обе** стороны: Soul **не** метится ни в `disconnected`, ни в `connected` (живой стрим важнее своевременности снимка; следующий прогон повторит).

**Presence (online/offline) деривируется из Redis SID-lease, НЕ из `souls.status`** ([ADR-006(a)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). Авторитет «Soul online» — живой lease `soul:<sid>:lock`; именно его читает [таргет-резолвер](../scenario/orchestration.md) (двухфазно: SQL-кандидаты по Coven + не-terminal/не-онбординг status → отсев по живому lease). Синхронной записи presence в PG на connect/disconnect EventStream-а **нет** (она была бы hot-path-ом на 100k VM).

Поэтому `mark_disconnected` теперь — **ленивое двунаправленное согласование PG-снимка `souls.status`** (для Operator API «последнее известное»), а **не источник presence**. Снимок `souls.status` отстаёт от факта (online уже виден резолверу через lease, а строка ещё `disconnected`) — это допустимо: правило приводит снимок к факту фоном в обе стороны, прогон от него не зависит. Без настроенного Redis (single-instance dev / unit) правило деградирует в прежний чисто-SQL **одностороннее** `mark_disconnected` (миграция 014), где stale PG-`last_seen_at` ⇔ нет живого стрима по построению (один инстанс) и латча `disconnected` нет — реконнект сразу делает `last_seen_at` свежим; там же резолвер деривирует presence из SQL-снимка (`status='connected'`). Штатный teardown больше PG-presence не пишет (lease гаснет на Release/TTL — см. [`../soul/identity.md` → Статусы Soul](../soul/identity.md#статусы-soul-и-переходы)).

Колонка «Обязательные поля» в этой таблице — фактическое использование, а не отдельная норма: для `purge_souls` / `purge_old_seeds` `statuses` обязателен потому, что без фильтра по статусу `delete` снёс бы живые записи. Норма уровня грамматики правила — таблица «Условная обязательность по `action`» выше.

## Включение recovery (recovery-enable)

> **Operational guide:** [`docs/operations/recovery-reclaim-apply-runs.md`](../operations/recovery-reclaim-apply-runs.md) — пошаговый runbook (прод-гейты, hot-reload, валидация метрик, rollback). Этот раздел — нормативная фиксация гейта; runbook применяет её на проде.

`reclaim_apply_runs` по дефолту **выключено** и его нельзя просто перевести в `enabled: true` — это операционный шаг под гейтом ([ADR-027(i)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) + amend «GATE-1: deliver-once recovery», 2026-05-25). После GATE-1 гейт **смягчён**: lease-инвариант ослаблен, Acolyte lease-renew для recovery-границы больше не нужен. **Валидировано на масштабе** мега-приёмкой 2026-05-25 (3 keeper + 9-нодовый реальный redis-cluster, `acolytes: 2`, `reclaim_apply_runs` включён, fencing-Soul раскатан): двойного apply нет, leader-failover отрабатывает (переизбрание Reaper-лидера по TTL без split-brain), recovery возвращает протухшие Ward. То есть гейт-условие «fencing раскатан» (шаг 1) удовлетворяется деплоем текущего `soul`-билда (fencing уже в бинаре). Безопасное включение — по шагам:

> ⚠️ **WARN: НЕ включать `reclaim_apply_runs` при `acolytes: 0`.** Это **связанная пара**: `reclaim_apply_runs` и `acolytes > 0`. При `acolytes: 0` (прод-дефолт, Acolyte — opt-in) задания исполняются старым синхронным путём и пишутся прямо в `running`; на этом не-fenced пути reclaim сам по себе **небезопасен** — переклейм без epoch-защиты задвоит apply. Сужение reclaim до `claimed` означает, что `running`-строки старого пути Reaper и так **не восстанавливает** (это не регресс: recovery старого пути — это in-memory run-goroutine инстанса-владельца, а не Reaper-reclaim). Включение reclaim имеет смысл **только** в связке с поднятым пулом Acolyte (`acolytes > 0`) и раскатанным fencing-Soul. Это known-gap из amend (e)/(f).

**Шаг 1. Раскатать fencing-Soul по флоту.** Все `soul`-агенты должны нести attempt-fencing (gate-1 attempt-fencing — уже в коде: Soul-guard по `ApplyRequest.attempt` на исполнении + эхо `RunResult.attempt` для epoch-check на приёме). Без этого пере-claim протухшего Ward отправит **второй** `ApplyRequest` на хост, а не-fenced Soul не отсечёт устаревший дубль прежнего владельца → два apply на один хост. Раскатать обновлённый `soul`-бинарь на весь парк **до** включения правила.

**Шаг 2. Убедиться, что `acolyte_lease > max-РЕНДЕР`.** После GATE-1 reclaim берёт только Ward в фазе `claimed` (рендер/claim, **до** отдачи на хост); живой долгий apply сидит в `dispatched`, который reclaim не трогает. Поэтому достаточно, чтобы lease переживал фазу рендера — **дефолт `acolyte_lease: 30s` ок** (см. [config.md → `acolytes`](config.md#acolytes)). Старое требование `acolyte_lease > max(время-apply-одного-хоста)` и Acolyte lease-renew — **сняты** (гейт ослаблен после GATE-1, amend (e)). Проверять только что `acolytes > 0` (см. WARN выше).

**Шаг 3. Перевести `reclaim_apply_runs.enabled` в `true`.** Только после шагов 1–2 — hot-reload конфига (`enabled: true` без передеплоя бинаря). После включения проследить по метрикам, что recovery не задваивает apply: всплеск `keeper_runresult_stale_total` (epoch-check на приёме отсекает stale-`RunResult`) ожидаем и означает, что fencing работает; рост числа повторных apply на одних хостах без stale-отсечки — сигнал, что fencing раскатан не на весь флот (вернуться к шагу 1).

```yaml
reaper:
  rules:
    reclaim_apply_runs:
      enabled: true     # только после шагов 1–2 и при acolytes > 0
      stale_after: 1m   # формальный lease-таймаут; в предикат НЕ входит
      action: set_status
      target_status: planned
```

Перед включением в проде — отдельный architect re-review связки (так зафиксировано в [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amend).

**Soul-also-dead — known-gap (MVP).** Если Keeper и Soul оба умерли **после** отдачи задания, строка зависнет в `dispatched` (`RunResult` не придёт, reclaim её не трогает — `dispatched` не реклеймится by design). На MVP это документированный known-gap: оператор перезапускает прогон вручную. Закрытие — post-MVP через Soul-reconcile (Soul при reconnect сообщает реально ведомые `apply_id`, Keeper терминалит осиротевшие `dispatched`). Reaper dispatched-timeout сознательно **не делаем** (terminal-by-timeout без подтверждения от Soul небезопасен — amend (g)).

## Метрики

Регистрируются в Prometheus-registry Keeper-а только при `reaper.enabled: true` (если `false` — collectors не публикуются вовсе, cardinality-safe). Реализация — [`keeper/internal/reaper/metrics.go`](../../keeper/internal/reaper/metrics.go), вызов из `keeper/cmd/keeper/main.go`.

| Метрика | Тип | Метки | Смысл |
|---|---|---|---|
| `keeper_reaper_rule_executions_total` | counter | `rule` | Число запусков правила за весь uptime keeper-инстанса. Инкрементируется и при `purged=0`, и при ошибке — это «сколько раз правило вызывалось», не «сколько раз сработало». |
| `keeper_reaper_rule_purged_total` | counter | `rule` | Сумма обработанных записей (удалено `delete` / переведено в новый статус `set_status` / помечено `expire`). Для `action: report` (`reap_orphan_vault_keys`) — число **задетектированных** сирот (ничего не удалено; «purged» здесь = «detected»). Не инкрементируется при `dispatch_error`. |
| `keeper_reaper_rule_duration_seconds` | histogram | `rule` | Длительность одного запуска правила. `_count{rule}` совпадает с `keeper_reaper_rule_executions_total{rule}`. |
| `keeper_reaper_dispatch_errors_total` | counter | `rule` | Число ошибок диспетчеризации (Purger вернул error, PG/Redis недоступны, и т.п.). При срабатывании `executions_total{rule}` тоже инкрементится, `purged_total{rule}` — нет. |
| `keeper_reaper_lease_held` | gauge | — | `1` если этот инстанс держит Redis-lease `reaper:leader`, иначе `0`. Один gauge на keeper-инстанс. Cluster-wide инвариант: `sum(keeper_reaper_lease_held) == 1`. |

`rule` — canonical имя правила из таблицы [Правила](#правила) выше (`expire_pending_seeds` / `purge_used_tokens` / `purge_souls` / `purge_old_seeds` / `mark_disconnected` / `purge_audit_old` / `purge_apply_runs` / `purge_voyages` / `purge_push_runs` / `purge_incarnation_archive` / `purge_state_history_archive` / `purge_archived_state_history` / `purge_apply_task_register` / `reclaim_apply_runs` / `reclaim_voyages` / `reconcile_orphan_applying` / `reap_orphan_vault_keys` / `archive_state_history` / `scry_background` / `purge_old_errands` / `purge_orphan_ephemeral_tidings`). Расширение closed-enum — через propose-and-wait вместе с новым `keeper.yml::reaper.rules.<name>`.

Для `scry_background` `keeper_reaper_rule_purged_total{rule="scry_background"}` = **число incarnation, обработанных за текущий тик** (запущено goroutine-ов; включая incarnation, для которых `ErrConvergeMissing` — сам факт запуска проверки); счётчики per-host-drift живут в `incarnation.last_drift_summary` (читается через `GET /v1/incarnations/{name}`), не в Prometheus.

## См. также

- [storage.md](storage.md) — таблицы, над которыми работает Жнец.
- [push.md](push.md), [`../soul/modules.md`](../soul/modules.md) — хостовый cleanup (другая тема, другой механизм).
- [config.md](config.md) → блок `reaper:` в `keeper.yml`.
- [prod-setup.md](prod-setup.md) → прод-развёртывание Keeper-а (Vault-policy для `reap_orphan_vault_keys`, гейт recovery-enable).
- [`../soul/identity.md`](../soul/identity.md) — `souls` / `soul_seeds` / `bootstrap_tokens` и их статусы.
- [architecture.md → Reaper / Жнец](../architecture.md#reaper--жнец).
- [naming-rules.md](../naming-rules.md) — Reaper и зарезервированное Charon.
