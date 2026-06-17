# FAQ — типичные проблемы и триаж

Самые частые операционные ситуации и их быстрый разбор. Подробности — по cross-link-ам в архитектурную / нормативную документацию.

## «Souls в `disconnected`, хотя процессы живы»

**Симптомы.** В реестре `souls` через Operator API хост в `disconnected`, но `systemctl status soul` на хосте говорит `active (running)`, и `soul`-логи не показывают разрыва стрима.

**Корень.** `souls.status` — **ленивый snapshot** для Operator API, не источник presence ([ADR-006(a)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) amendment). Авторитет «Soul online» — живой SID-lease в Redis. Snapshot отстаёт от факта и должен подтягиваться `mark_disconnected` Reaper-правилом (двунаправленный reconcile, включая `disconnected → connected` для случая, когда lease живой а snapshot отстал).

**Триаж:**

1. Проверить, что `mark_disconnected` правило **включено** в `keeper.yml::reaper.rules.mark_disconnected.enabled: true`.
2. Проверить, что Reaper лидер вообще работает: `sum(keeper_reaper_lease_held) == 1`. Если 0 — никто не делает reconcile, snapshot латчится.
3. Проверить SID-lease в Redis напрямую:
   ```sh
   redis-cli EXISTS soul:host-01.example.com:lock
   # (integer) 1   — lease есть, Soul реально online
   ```
4. Если lease есть, а snapshot `disconnected` — ждать следующего Reaper-цикла (`reaper.interval`, default 1h). Для ускорения — `keeper.yml::reaper.interval: 5m` через hot-reload.
5. Если lease нет, а процесс живой — Soul не подключён к Keeper (TCP / mTLS / SoulSeed-проблема). Проверять Soul-логи на errors соединения.

См. также: [`docs/keeper/reaper.md` → `mark_disconnected`](../keeper/reaper.md), [`docs/architecture.md` → ADR-006 amendment](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis).

## «Apply висит в `applying` 30 минут+»

**Симптомы.** `GET /v1/incarnations/{name}` показывает `status: applying`, висит долго. Прогон вроде должен был завершиться.

**Возможные причины:**

### Случай 1: `acolytes: 0` в HA-кластере (footgun)

Прогон, созданный на keeper-A, но Soul-агент на стриме keeper-B. Keeper-A держит in-memory run-goroutine, ждёт `RunResult`; `RunResult` приходит на keeper-B (тот, кто держит EventStream Soul-а); keeper-B его не знает, что делать (нет в-memory владельца), молча игнорит. incarnation **навсегда** висит в `applying`.

**Verify через SQL:**

```sql
-- Найти strung-up incarnation
SELECT name, applying_started_at, started_by_aid FROM incarnation
WHERE status = 'applying' AND NOW() - applying_started_at > '15 minutes';

-- Найти apply_id и его хосты
SELECT apply_id, sid, status FROM apply_runs
WHERE apply_id = (SELECT apply_id FROM apply_runs ORDER BY created_at DESC LIMIT 1);

-- Если status = 'success' на всех — это case 1; incarnation не закрылся при `acolytes: 0`
```

**Action:**

1. Проверить, что Refuse-guard не сработал (`acolytes: 0` + multi-keeper → должен был отказаться стартовать; если стартовал — значит `allow_unsafe_single_path_multi_keeper: true`).
2. Зафиксировать `acolytes: > 0` в `keeper.yml` всех инстансов — это не reload-able изменение, требует restart Keeper-кластера ([scaling.md](scaling.md)).
3. Закрыть зависший прогон вручную:
   ```sql
   UPDATE incarnation SET status = 'ready' WHERE name = '<name>' AND status = 'applying';
   ```
4. Investigate через audit-log — кто и когда выставил `allow_unsafe_single_path_multi_keeper`.

### Случай 2: `claimed`/`dispatched` Ward после crash инстанса

Acolyte клеймил задание, инстанс умер. С включённым `reclaim_apply_runs` — recovery подберёт. Без — зависание.

**Verify:**

```sql
SELECT apply_id, sid, status, claim_by_kid, claim_at, claim_expires_at, attempt
FROM apply_runs WHERE status IN ('claimed', 'dispatched');
```

**Action:** включить `reclaim_apply_runs` по процедуре в [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md#включение-recovery-recovery-enable). Требует раскатки fencing-Soul (есть в коде) + `acolytes > 0`. Перед включением — architect re-review (см. ADR-027 amend).

### Случай 3: длинный прогон с `serial:` барьером и медленным хостом

Если scenario использует `serial:` — Acolyte клеймит весь serial-blok одним воркером, держит барьер. Один медленный хост может вытянуть длительность всего прогона.

**Verify:** OTel-трейсы (`scenario.run` span с child `apply.run` на каждый хост — найти медленный).

**Action:** оптимизировать scenario или принять, что прогон long-running.

## «check-drift возвращает `422 ErrConvergeMissing`»

**Симптомы.** `POST /v1/incarnations/{name}/check-drift` возвращает 422 с `"type": "/errors/converge-missing"`.

**Корень.** Сервис не поддерживает drift-детект — нет файла `scenario/converge/main.yml` в service-репо ([ADR-031 Slice B](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)).

**Action:** в service-репо добавить `scenario/converge/main.yml`, который реализует idempotent-проверку текущего состояния (типичный destiny-style scenario). После merge + service-ref bump — check-drift станет доступен.

См. [`docs/architecture.md` → ADR-031 Slice B](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile).

## «check-drift возвращает `422 ErrDriftInputMissing`»

**Симптомы.** `POST /v1/incarnations/{name}/check-drift` возвращает 422 с `"type": "/errors/drift-input-missing"`.

**Корень.** Converge-scenario требует input-параметр, который нельзя авто-резолвить из `incarnation.state` (нет такого имени) и нет значения в override-body запроса.

**Action:**

- Либо передать override в body запроса:
  ```sh
  curl -X POST .../check-drift -d '{"<param-name>": "<value>"}'
  ```
- Либо изменить converge-scenario, чтобы input-параметр имел default или брался из state по другому имени.

## «`Holder.Refresh` errors / RBAC snapshot stale»

**Симптомы.** Алерт `time() - keeper_rbac_snapshot_last_success_timestamp_seconds > 300`. Возможно `keeper_rbac_snapshot_rebuild_errors_total{kind=...}` растёт.

**Корень `kind=load`.** БД недоступна / ошибка SELECT-ов в `rbac_*`-таблицах. Investigate PG-связность.

**Корень `kind=parse`.** Невалидная permission в `rbac_role_permissions.permission` (рассинхрон версий каталога). Investigate последние записи в `rbac_role_permissions`:

```sql
SELECT role_name, permission FROM rbac_role_permissions ORDER BY role_name;
```

Найти permission, не соответствующий грамматике ([`docs/keeper/rbac.md` → Формат permissions](../keeper/rbac.md#формат-permissions)).

**Action:**

- `kind=load`: чинить PG.
- `kind=parse`: удалить / поправить невалидную permission через `role.update` (или прямой SQL после анализа).

После fix — `keeper_rbac_invalidations_received_total` rate растёт (через pub/sub `rbac:invalidate`), snapshot пересобирается.

## «Vault unreachable — что упадёт?»

**Симптомы.** `keeper_vault_read_errors_total{kind="error"}` растёт. Apply начинают падать на render-фазе.

**Что продолжает работать:**

- Existing EventStream-стримы (TCP уже установлен, in-memory кэш зарезолвенных секретов жив).
- Reaper, Conclave, Watchman.
- Operator API на read-операциях не требующих Vault (`GET /v1/operators`, `GET /v1/incarnations` без `state_history`-включения секретов).

**Что упадёт:**

- Старт нового Keeper-инстанса — на резолве `postgres.dsn_ref`. Если есть AppRole login — он сам не пройдёт без Vault.
- Любой apply, требующий `vault(...)` в CEL или `${ vault:... }` в шаблоне — render fail.
- Bootstrap нового Soul-а — PKI недоступен.
- `operator.issue-token` — JWT signing-key из Vault может уже быть закэширован, поэтому может работать; но `auth.jwt.signing_key_ref` resolve на старте Keeper уже сделан — должен работать.

**Action:** поднять Vault. Существующие сессии переживут (token-renewer прервётся, попытается перевыпустить токен; если Vault вернётся быстро — пользователи могут вообще не заметить).

## «Reaper не работает — `keeper_reaper_lease_held` везде 0»

**Симптомы.** Метрика 0 на всех инстансах, `apply_runs` / `audit_log` / `souls` начинают разрастаться.

**Корень.** Никто не держит Redis-lease `reaper:leader`. Причины:

1. Redis недоступен (см. [§ Redis outage](#redis-outage)).
2. `reaper.enabled: false` на всех инстансах.
3. Bug в Reaper-loop (рестарт инстанса должен помочь).

**Triage:**

1. `redis-cli GET reaper:leader` — должно вернуть `<kid>`, если кто-то лидер.
2. Если nil и Redis жив — investigate `journalctl -u keeper -n 200 | grep -i reaper`.
3. Проверить, что `reaper.enabled: true` в `keeper.yml`.

**Action:**

- Перезапустить любой Keeper-инстанс — следующий цикл должен взять lease (`SET reaper:leader <kid> NX EX <lock_ttl>`).
- Если все инстансы запускались в `reaper.enabled: false` — поменять через hot-reload.

## «Soul падает с `bootstrap token already used`»

**Симптомы.** При первом старте Soul-агента на новом хосте: `error: bootstrap token already used`.

**Корень.** Bootstrap-токен одноразовый — после успешного онбординга `bootstrap_tokens.used_at` выставляется, повторный запрос rejected ([`docs/soul/onboarding.md`](../soul/onboarding.md), [`docs/soul/identity.md`](../soul/identity.md)).

Возможные причины:

1. Токен реально использован — Soul-агент уже онбордился, file `/etc/soul/seed/soul.crt` существует.
2. Race-условие — два процесса параллельно пытаются провести bootstrap.

**Action:**

1. Если `/etc/soul/seed/soul.crt` существует — bootstrap уже прошёл; удалить файл `bootstrap-token` (он больше не нужен) и перезапустить `soul`.
2. Если нужно повторно онбордить — выпустить новый токен через Operator API:
   ```sh
   curl -X POST https://keeper.internal:8080/v1/souls/host-01.example.com/issue-token \
     -H "Authorization: Bearer $(cat /etc/keeper/archon.jwt)"
   ```

   Положить новый токен в `/etc/soul/bootstrap-token` (mode 0400).

## «Hot-reload `keeper.yml` упал»

**Симптомы.** `systemctl reload keeper` отработал без ошибки, но audit-event `config.reload_failed` появился, метрика подсказывает.

**Корень.** Невалидный конфиг в новой `keeper.yml` (синтаксис / диагностика парсера / некорректное значение в reload-able блоке).

**Поведение Keeper-а:** старый снимок конфига **остаётся активным** ([ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)) — Keeper не сломается, продолжит работать на старой конфигурации.

**Action:**

1. `journalctl -u keeper -n 50 --no-pager` — найти конкретную ошибку парсера (diagnostic-имя из [`docs/keeper/config.md`](../keeper/config.md)).
2. Исправить `keeper.yml`.
3. `systemctl reload keeper` снова. Audit-event `config.reload_succeeded` — verify.

## «Soul-стрим обрывается каждые 30 секунд»

**Симптомы.** `keeper_grpc_streams_active` колеблется, `soul_eventstream_reconnects_total` rate высокий.

**Возможные причины:**

1. **L4-LB сбрасывает по idle-таймауту.** gRPC keepalive должен держать соединение, но если LB агрессивный — обрывает. Проверить `timeout server 24h` в haproxy (см. [`scaling.md` → L4-LB](scaling.md#l4-балансировщик-настройки)).
2. **NAT / firewall** между Soul и Keeper — стейт NAT-таблиц истекает, соединение обрывается.
3. **gRPC keepalive** на стороне сервера / клиента misconfigured.

**Triage:**

1. На Soul-хосте — `ss -t -o state established | grep keeper-endpoint-port` показывает живые TCP-соединения и timer keepalive.
2. На Keeper — `keeper_grpc_messages_total{direction="from_soul"}` rate — есть ли app-сообщения по стриму? Если 0, и стрим всё-таки активен — Soul только держит keepalive, нет app-трафика.

**Action:** настроить LB / NAT под gRPC long-running streams (`timeout server 24h`+; NAT keepalive: increase TCP keepalive timeout на хостах).

## «Souls делают много reconnect-ов после rolling-restart Keeper»

**Симптомы.** При rolling-restart `soul_eventstream_reconnects_total` rate спайк, потом нормализуется.

**Это нормальное поведение.** Soul-агенты перенаправляются по failback на новые / другие инстансы. Спайк длится секунды-минуты. После завершения rolling-upgrade — стрим-counts re-distribute.

**Если спайк затягивается** (>5 минут) — investigate:

- Conclave-presence новых инстансов не появляется в Redis — restart prereqs не пройдены.
- `keeper.yml` нового инстанса невалиден — fail-stop при старте.
- L4-LB не возвращает новый инстанс в backend (health-check fails).

См. [`upgrade.md` → Rolling upgrade Keeper](upgrade.md#rolling-upgrade-keeper).

## «`incarnation.run` возвращает `409 incarnation already applying`»

**Симптомы.** Запрос `POST /v1/incarnations/{name}/run` возвращает 409.

**Корень.** Атомарность модели apply ([architecture.md → Атомарность и error_locked](../architecture.md)): нельзя запустить новый apply на incarnation, пока предыдущий не завершился.

**Action:**

1. Проверить статус: `GET /v1/incarnations/{name}` → `status: applying`. Если так — ждать завершения.
2. Если висит долго — см. [§ Apply висит в applying](#apply-висит-в-applying-30-минут).
3. Если статус `error_locked` — investigate последний `state_history.changed_by_aid` и причину; обычно требует ручного решения (см. [ADR-027 trade-offs](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)).

## «`POST /v1/operators` возвращает 409 `would lock out`»

**Симптомы.** Попытка ревокации Архонта возвращает 409 с `would lock out the cluster`.

**Корень.** Инвариант [ADR-013(c)](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) — нельзя удалить последнего оператора с `*`-permission.

**Action:** сначала создать другого Архонта с `cluster-admin`, потом ревокать старого. См. [`bootstrap-rbac.md` → Защита от self-lockout](bootstrap-rbac.md#защита-от-self-lockout).

## «Sigil verify failed — плагин не запускается»

**Симптомы.** Apply, использующий community-плагин (`soul-mod-*` / `soul-cloud-*` / `soul-ssh-*`), падает с `sigil verify failed`. Audit-event `plugin.verify_failed`.

**Корень.** Sigil-подпись плагина не сошлась — либо плагин не допущен через `plugin.allow`, либо был отозван (`revoked_at`), либо SHA-256 бинаря не совпадает с записью в `plugin_sigils`.

**Action:**

1. Verify запись в `plugin_sigils`:
   ```sql
   SELECT namespace, name, ref, sha256, revoked_at FROM plugin_sigils
   WHERE namespace = 'cloud' AND name = 'soul-cloud-aws' AND revoked_at IS NULL;
   ```
2. Verify SHA-256 фактического бинаря:
   ```sh
   sha256sum /var/lib/soul-stack-keeper/plugins/cloud/soul-cloud-aws/<commit_sha>/soul-cloud-aws
   ```
3. Если совпадение есть — verify trust-anchor-набор на Soul (re-broadcast мог не дойти). См. [`docs/observability.md` → keeper_sigil_anchors_last_delivered](../observability.md).
4. Если плагин обновился и SHA изменился — нужно явно допустить новый через Operator API (`plugin.allow`).

См. [ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), [`docs/keeper/plugins.md` → Integrity-model](../keeper/plugins.md).

## См. также

- [`disaster-recovery.md`](disaster-recovery.md) — сценарии полного отказа компонентов.
- [`monitoring.md`](monitoring.md) — какие метрики смотреть в каждом сценарии.
- [`docs/architecture.md`](../architecture.md) — обоснования всех описанных инвариантов.
- [`docs/keeper/reaper.md`](../keeper/reaper.md) — Reaper-правила (включая `reclaim_apply_runs`).
- [`docs/keeper/rbac.md`](../keeper/rbac.md) — RBAC.
