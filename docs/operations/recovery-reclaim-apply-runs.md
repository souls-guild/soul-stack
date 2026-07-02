# Включение `reclaim_apply_runs` в проде

Операционализация GATE-1 (deliver-once recovery) из [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim). Не отдельный ADR — переключение уже реализованного правила, по списку прод-гейтов.

**Что делает.** Reaper-правило `reclaim_apply_runs` ([keeper/internal/reaper/runner.go](../../keeper/internal/reaper/runner.go)) реклеймит зависшие `apply_runs.status='claimed'` обратно в `planned`, если `claim_expires_at < NOW()`. Закрывает дыру «висячий applying» на фазе **до отдачи** (Keeper умер на рендере/claim, задание Soul-у ещё не ушло): живой Acolyte подхватит строку с инкрементом `attempt`, а Soul-side fencing отсечёт устаревший дубль прежнего владельца. Фаза **`dispatched`** (после отдачи) правилом **не трогается** — пере-claim уже отданного = двойной apply.

Без правила Keeper-инстанс, упавший в фазе `claimed`, оставляет `apply_run` застрявшим навсегда — operator должен снимать вручную.

**По дефолту `enabled: false`** (ADR-027 amend (e), [docs/keeper/reaper.md → Конфиг](../keeper/reaper.md#конфиг)). Включение — операционный шаг под гейтом, не одиночный `enabled: true`.

## Прод-гейты (все три обязательны)

### 1. Fencing-Soul раскатан на весь флот

ADR-027 amend GATE-1 + (g) + (e). Все `soul`-агенты должны нести attempt-fencing: Soul-guard по `ApplyRequest.attempt` на исполнении + эхо `RunResult.attempt` для epoch-check на приёме. Без этого пере-claim протухшего Ward отправит второй `ApplyRequest` на хост, а не-fenced Soul не отсечёт устаревший дубль.

**Текущий `soul`-билд несёт fencing** (gate-1 attempt-fencing уже в бинаре, [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amend (b)/(g) реализован). Достаточно убедиться, что **на весь парк** раскатан этот билд (а не легаси без attempt-поля).

**Проверка:** под flood-тестом устаревших `RunResult` метрика `keeper_runresult_stale_total` стабильно ненулевая — epoch-check на приёме отсекает stale-`RunResult`. Если 0 при заведомых stale-сценариях — fencing раскатан не везде.

### 2. `acolytes > 0` на ВСЕХ Keeper-инстансах

ADR-027 amend (f) + (e). `reclaim_apply_runs` и `acolytes > 0` — **связанная пара**. При `acolytes: 0` задания исполняются старым синхронным путём и пишутся прямо в `running`; reclaim на не-fenced пути без epoch-защиты задвоит apply.

**Проверка:** на каждом keeper-узле кластера `keeper.yml::acolyte.workers > 0` (см. [config.md → acolytes](../keeper/config.md#acolytes)). Через [Conclave](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis): refuse-startup-guard уже отказывается стартовать в опасном `acolytes:0 + CountLive>1`-сетапе ([ADR-027(h)/(k)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)), но оператор всё равно подтверждает явно.

### 3. Soul-reconcile (S6) активен для `dispatched`-сирот

ADR-027 amend (g), реализован 2026-05-25 — `WardRoster` ([ADR-012(k)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). Закрывает окно «Keeper и Soul оба мертвы после отдачи»: Soul на (re)connect шлёт `WardRoster` (ReplaceAll-снимок ведомых `apply_id`+attempt), Keeper в `OrphanDispatched` epoch-fenced single-winner-сверкой терминалит `dispatched`-строки этого SID, которых нет в наборе → новый терминал `orphaned` (миграция 044).

Без этого `dispatched`-строки на флоте с убитыми парами Keeper+Soul зависали бы навсегда — `reclaim` их не трогает by design.

**Проверка:** алерт **dispatched stuck (post-recovery-enable)** ([monitoring.md](monitoring.md)) — SQL `COUNT(*) FROM apply_runs WHERE status = 'dispatched' AND claim_at < NOW() - INTERVAL '1 hour'` должен оставаться около 0 на стенде с симулированным kill keeper+soul. Если копит — Soul-reconcile не работает (старый `soul`-билд без `WardRoster`, форвард-compat no-op fail-safe).

### Без гейтов — НЕ включать

- Без шага 1 (fencing-Soul) → race-condition «два Keeper-а параллельно выкатывают один apply_run на хост».
- Без шага 2 (`acolytes > 0`) → reclaim возвращает в `planned`, но никто не подберёт; на не-fenced пути reclaim вообще небезопасен.
- Без шага 3 (`WardRoster`) → `dispatched`-сироты копят бесконечно.

## Включение

В `keeper.yml`:

```yaml
reaper:
  interval: 30s              # tick-частота Reaper-цикла; рекомендуется <= acolyte.claim_lease / 2
  rules:
    reclaim_apply_runs:
      enabled: true          # default false
      stale_after: 1m        # формальный lease-аргумент action-схемы; в SQL-предикат НЕ входит
      action: set_status
      target_status: planned
```

`acolyte_lease > max-РЕНДЕР` — дефолт `30s` ок (lease-инвариант **ослаблен** после GATE-1, [ADR-027 amend (e)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). `acolyte_lease > max(время-apply-одного-хоста)` и Acolyte lease-renew — **больше не требуются** для recovery-границы.

Применить через hot-reload (`SIGHUP` или API/MCP-мутация конфига, [config.md → Hot-reload](../keeper/config.md#hot-reload)) — без рестарта keeper-процесса.

## Валидация после включения

1. **Метрики Reaper-правила** ([reaper.md → Метрики](../keeper/reaper.md#метрики)):
   - `keeper_reaper_rule_executions_total{rule="reclaim_apply_runs"}` — растёт по tick-у Reaper-цикла (правило вызывается). Это «сколько раз вызывалось», не «сколько раз сработало».
   - `keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"}` — суммарное число реклеймнутых строк (set_status → planned). Под нагрузкой с симулированными kill-Keeper во время `claimed` — растёт. На стабильном кластере без kill — должна быть около 0.
   - `keeper_reaper_dispatch_errors_total{rule="reclaim_apply_runs"}` — должна быть 0. Рост = PG-сбой при reclaim.

2. **Audit-events** ([naming-rules.md → Audit-events](../naming-rules.md#audit-events), область `reaper.*`):
   - `reaper.reclaim_apply_runs.executed` — пишется при срабатывании. SQL: `SELECT * FROM audit_log WHERE event_type = 'reaper.reclaim_apply_runs.executed' ORDER BY created_at DESC LIMIT 20`.

3. **Epoch-check на приёме** ([monitoring.md](monitoring.md)):
   - `keeper_runresult_stale_total` — всплески при пере-claim прежнего Ward (Soul прислал устаревший `RunResult` с прежним `attempt`). Это **ожидаемо** и означает, что fencing работает.
   - **Линейный рост числа повторных apply на одних хостах без stale-отсечки** = тревога: fencing раскатан не на весь флот, вернуться к гейту 1.

4. **Алерт `dispatched stuck`** — Soul-reconcile должен орфанить `dispatched`-строки после смерти Soul-владельца ([monitoring.md → Warning](monitoring.md#warning-триаж-в-рабочее-время)). Рост — Soul-reconcile не активен.

## Откат (rollback)

```yaml
reaper:
  rules:
    reclaim_apply_runs:
      enabled: false
```

Hot-reload. Безопасно в любой момент — правило идемпотентно (только status-update `claimed → planned`, не data-mutation, не удаление). После отката `claimed`-строки протухшего владельца снова застревают, пока правило выключено.

## Voyage-orphan-lock-release — тот же double-apply класс, включён иначе

Этот runbook — про `reclaim_apply_runs` (per-host Ward, default-OFF, гейт выше). Рядом живёт **второй recovery-механизм** с тем же приемлемым double-apply классом, но **включённый принципиально иначе** — оператор обязан понимать разницу.

**Что это.** `reclaim_voyages` ([ADR-043 §8](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), [reaper.md → Конфиг](../keeper/reaper.md#конфиг)) возвращает осиротевший `running`-Voyage (владелец-Keeper умер до финализации, lease протух) обратно в `pending`; другой Keeper-инстанс пере-claim-ит его и доисполняет с сохранённого `current_batch_index`. Для `kind=scenario`-Voyage при re-run leg-а реклеймнутый VoyageWorker сталкивается с осиротевшим `incarnation.status='applying'`, оставленным крашнутым прошлым владельцем (single-winner state-commit `applying`→терминал у него не отработал — он умер). **Voyage-orphan-lock-release** ([ADR-027(l)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)) — шов, которым VoyageWorker **снимает СВОЙ осиротевший `applying`** перед re-run. Без него reclaimed Voyage зависает («incarnation уже applying»), и сам `reclaim_voyages` оказывается нерабочим.

**Включается иначе, чем `reclaim_apply_runs`:**

| | `reclaim_apply_runs` (этот runbook) | Voyage-orphan-lock-release |
|---|---|---|
| Default | **OFF** (map-driven, нужен явный `enabled: true`) | **ON всегда** — встроенный re-run-путь `reclaim_voyages`, нет отдельного выключателя |
| Гейт перед включением | да, три прод-гейта выше | нет — приходит вместе с `reclaim_voyages` (тот сам default-ON, path-defaulting) |
| Кто включил на проде | оператор сознательно, под гейтом | **уже включено по умолчанию** |

`reclaim_voyages` default-ON осознанно (целевой масштаб 100k Souls, рестарт/замена Keeper-инстанса — штатное событие, осиротевшие Voyage регулярны → recovery не должен зависеть от ручной записи правила). А раз без orphan-lock-release Voyage-recovery сломан — шов не имеет собственного opt-in.

**Тот же double-apply класс.** При network-partition (живой, но партиционированный прошлый владелец продолжает свой apply, пока наш re-run шлёт второй) на хост может уйти **двойная отправка задания** — ровно как у `reclaim_apply_runs`. От **порчи `incarnation.state`** защищают те же два барьера, что описаны в этом runbook-е для per-host reclaim:

1. **gate-1 attempt-fencing `RunResult`** — stale-`RunResult` прежнего владельца отсекается на приёме по `apply_runs.attempt` (epoch вырос при пере-claim), всплеск `keeper_runresult_stale_total` (см. [Валидация → Epoch-check](#валидация-после-включения)) при failover Voyage ожидаем и означает, что fencing работает;
2. **идемпотентность модулей** — повторная отправка того же задания на хост не меняет результат при корректно написанном модуле (та же рекомендация, что для command-Voyage в [ADR-043 §8(d)](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)).

**Практический вывод оператору.** Этот double-apply класс присутствует на проде **независимо от того, включил ли ты `reclaim_apply_runs`** — он приходит вместе с default-ON `reclaim_voyages`. Раскатка fencing-Soul (гейт 1 выше) и идемпотентность модулей — условие корректности **обоих** механизмов, не только per-host reclaim. Если `reclaim_voyages` по какой-то причине выключен явным `reaper.rules.reclaim_voyages.enabled: false` — orphan-lock-release уходит вместе с ним, но тогда осиротевшие Voyage зависают без recovery (нежелательно на проде).

Нормативная фиксация — [reaper.md → блок Voyage-orphan-lock-release](../keeper/reaper.md#конфиг) и [ADR-027(l)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim).

## standalone-orphan reconcile — тот же класс для прямого run-а

Voyage-orphan-lock-release (выше) закрывает осиротевший `applying` только для прогонов **под Voyage** — у них есть back-link `voyage_targets.apply_id`. **Прямой (standalone) `incarnation.run`** — запуск сценария без батч-обёртки Voyage — строки `voyage_targets` не имеет, поэтому крах его Keeper-владельца оставлял бы `incarnation.status='applying'` навсегда. Этот шов закрывает Reaper-правило **`reconcile_orphan_applying`** ([ADR-027(m)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), recovery-completeness backstop).

**Что делает.** Снимает осиротевший прямой applying-lock: `applying → ready`, после чего incarnation снова запускаема. Детект двухфазный — (1) SQL-кандидаты: stale applying-строки (`applying_since` старше `stale_after`, 90s по умолчанию) с НЕпустым epoch (`applying_by_kid`); (2) presence-чек: жив ли `applying_by_kid` в Conclave (`InstanceAlive`). Lock снимается **только** если владелец доказанно мёртв (presence=false); живой владелец (прогон реально идёт) пропускается, ошибка presence-чека (флап Redis) — fail-safe skip (живой прогон не срывается).

**default-ON, как `reclaim_voyages`** (path-defaulting): работает при отсутствии ключа в `reaper.rules`, выключается только явным `reconcile_orphan_applying.enabled: false`. Отдельного прод-гейта (в отличие от `reclaim_apply_runs`) у него нет.

**Чем отличается от соседних recovery-правил:**

| Правило | Что реклеймит | Default |
|---|---|---|
| `reclaim_apply_runs` | протухший `claimed`-Ward в `apply_runs` (фаза до отдачи на хост) | **OFF**, три прод-гейта |
| `reclaim_voyages` (+ Voyage-orphan-lock-release) | осиротевший `running`-Voyage → `pending`; реклеймнутый воркер снимает `applying` под-Voyage | **ON** (path-defaulting) |
| `reconcile_orphan_applying` | осиротевший `applying`-lock **прямого** `incarnation.run` (вне Voyage) → `ready` | **ON** (path-defaulting) |

**Known-gap — NULL-epoch + FromLocked-микроокно → ручной `unlock`.** Правило реклеймит только строки с известным epoch (`applying_by_kid IS NOT NULL`). Два класса остаются за бортом:

- **legacy/pre-082** — applying-строки, поставленные до миграции 082 (epoch-колонок ещё не было), несут NULL `applying_by_kid`;
- **rerun-last микроокно** — `UnlockForRerun` транзитит `error_locked → applying` БЕЗ epoch, epoch дописывается следующей tx; краш точно в зазоре между этими двумя tx оставляет NULL-epoch.

Без presence-свидетеля смерти владельца (нет `applying_by_kid`) снятие небезопасно — такой lock правило сознательно НЕ трогает. Снимается оператором вручную: `POST /v1/incarnations/{name}/unlock` ([operator-api/incarnations.md → unlock](../keeper/operator-api/incarnations.md#post-v1incarnationsnameunlock--снять-error_locked)) после разбора, что прогон действительно мёртв. Диагностика `applying`-stuck — [faq.md](faq.md).

**Residual double-apply класс — тот же, что у `reclaim_apply_runs`.** При network-partition (живой, но партиционированный владелец продолжает apply, пока reconcile снял lock и incarnation перезапустили) на хост может уйти второй прогон. От порчи `incarnation.state` защищают те же два барьера: gate-1 attempt-fencing `RunResult` + идемпотентность модулей (см. [Voyage-orphan-lock-release](#voyage-orphan-lock-release--тот-же-double-apply-класс-включён-иначе) выше). Раскатка fencing-Soul (гейт 1) — условие корректности и этого механизма.

Нормативная фиксация — [reaper.md → `reconcile_orphan_applying`](../keeper/reaper.md#правила) и [ADR-027(m)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim).

## presence-gated force-release SID-lease — сокращение окна невидимости Soul-а

Отдельный recovery-backstop ([ADR-027(n)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), S2) — **не Reaper-правило**, а шов в EventStream-handler-е reconnect-а Soul-а. Касается не applying-lock-а инкарнации, а SID-lease стрима (`soul:<sid>:lock`).

**Проблема.** Soul переподключается к **другому** Keeper-инстансу после смерти прежнего holder-а. SID-lease прежнего holder-а ещё держится (TTL **60s**), и без шва новый Keeper отдавал бы Soul-у `AlreadyExists` — Soul ретраил бы до истечения TTL, оставаясь **невидимым** для флота всё это окно (до 60s).

**Что делает шов.** Вместо отказа handler presence-gated перехватывает lease у **доказанно-мёртвого** prev-holder-а: проверяет `InstanceAlive(prev_kid)` в Conclave (presence-ключ keeper-инстанса, TTL **30s**); если prev-holder мёртв (`InstanceAlive=false`) — CAS-by-prev-holder `ForceAcquireSoulLease` перезахватывает ключ на новый KID. Окно невидимости сокращается с **60s** (ждать TTL SID-lease) до **≤30s** (TTL Conclave-presence — пока прежний holder не отвалится из Conclave, он считается живым). Security-event `eventstream.lease_force_released {sid, prev_kid, new_kid}` (`source: soul_grpc`) — на каждый успешный перехват.

**Split-brain-безопасность через presence-gate.** Перехват происходит **только** при `InstanceAlive=false` (prev-holder доказанно мёртв). Если prev-holder жив, presence-чек упал (флап Redis), prev-holder == self (reconnect к тому же keeper-у) или ключ сменился на третьего между чеком и CAS — шов **не** перехватывает, отдаёт Soul-у `AlreadyExists` (тот ретраит). Эти отказы НЕ аудируются — это штатное «отдать Soul-у ретраить», не инцидент. Поэтому два живых Keeper-а не могут одновременно держать один SID-lease: владение получает ровно один, через presence-доказательство смерти другого.

**Residual — окно ≤Conclave-TTL не ноль.** Шов сокращает окно невидимости, но не обнуляет: prev-holder остаётся «живым» в Conclave до истечения его presence-TTL (30s), и до этого момента новый Keeper честно отдаёт `AlreadyExists`. Это by-design — раньше доказать смерть нельзя без риска split-brain.

**Soul-сторона комплементарна.** Пока keeper отдаёт `AlreadyExists` (lease ещё держится), Soul различает этот lease-held soft-failure от transport-сбоя и ретраит с модест-backoff-cap-ом (3s), а не общим transport-cap-ом (30s) — чтобы переподключиться в пределах секунд после force-release, а не долбить выживших keeper-ов всё presence-окно. См. [docs/soul/connection.md → Lease-held soft-failure](../soul/connection.md#lease-held-soft-failure-reconnect-после-краха-holder-а).

Нормативная фиксация — [naming-rules.md → `eventstream.lease_force_released`](../naming-rules.md#audit-events) и [ADR-027(n)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim).

## Связанное

- [ADR-027 — Acolyte / Ward / recovery](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), amend GATE-1 (deliver-once recovery, 2026-05-25); (l) — Voyage-orphan-lock-release.
- [ADR-043 §8 — Voyage failover / `reclaim_voyages` default-ON](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон).
- [docs/keeper/reaper.md → Включение recovery](../keeper/reaper.md#включение-recovery-recovery-enable) — нормативная фиксация гейта, источник правды.
- [docs/keeper/reaper.md → Метрики](../keeper/reaper.md#метрики) — canonical-имена метрик Reaper.
- Параллельные Reaper-правила, которые **не блокируют** включение этого: `mark_disconnected` (Soul heartbeat-выпадение, 90s), `reconcile_orphan_applying` (standalone-orphan applying-lock, default-ON, см. раздел выше), `purge_audit_old`, `purge_apply_runs` (30d retention завершённых), `purge_apply_task_register` (1h grace), `purge_old_errands`.
- [ADR-027(m)/(n)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) — recovery-completeness backstop: standalone-orphan reconcile (`reconcile_orphan_applying`) + presence-gated force-release SID-lease (`eventstream.lease_force_released`).
- [monitoring.md](monitoring.md) — алерт `dispatched stuck (post-recovery-enable)`.
- [faq.md](faq.md) — диагностика `applying`-stuck.
