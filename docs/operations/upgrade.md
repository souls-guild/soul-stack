# Upgrade procedure

Rolling upgrade Keeper-кластера и Soul-флота, state_schema-миграции, откат, совместимость.

## Принципы

| Принцип | Где зафиксировано |
|---|---|
| **Forward-compat only-add** в proto Keeper↔Soul ([ADR-012(g)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)) — никогда не удалять поля и не reuse field-номера. Breaking changes только через `proto/keeper/v2/`. | [ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) |
| **Forward-compat only-add** в Operator API (REST + MCP) — внутри `/v1/` только-add. Breaking — `/v2/`. | [`docs/keeper/operator-api.md` → conventions](../keeper/operator-api.md#conventions) |
| **State_schema migrations forward-only** в MVP — `down:` не поддерживается. Откат — через `state_history` snapshot. | [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) |
| **Hot-reload конфига** ([ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)) — большая часть `keeper.yml` reload-able через SIGHUP. Список restart-only полей — в [`docs/keeper/config.md` → Hot-reload](../keeper/config.md#hot-reload). | [ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) |
| **Совместимость Keeper N ↔ Soul N-1**: новый Keeper понимает proto-сообщения старого Soul-а; новый Soul понимает proto-сообщения старого Keeper-а (forward-compat); только-add поля — нулевые / `0` означают «старая версия, без фичи». | [ADR-012(g)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) |

## Rolling upgrade Keeper

Multi-keeper кластер позволяет обновлять инстансы по одному, без downtime.

### Процедура

1. **Pre-flight checks:**
   - Backup PG до апгрейда (см. [`infra.md` → Backup](infra.md#backup--restore)).
   - Проверить, что нет миграций breaking-формата (changelog между версиями).
   - Verify здоровья кластера: `keeper_reaper_lease_held` (sum=1), `keeper_grpc_streams_active` стабилен.

2. **Upgrade первого инстанса:**
   ```sh
   ssh keeper-1.internal
   # Drain LB
   # ... вывести из active backend в LB
   # Установить новую версию
   sudo dpkg -i /tmp/soul-stack-keeper_<new-version>_amd64.deb
   # systemd Restart=on-failure поднимет с новой версией
   systemctl restart keeper
   # Verify стартовало
   journalctl -u keeper -n 100 --no-pager
   ```

   **Что происходит** при рестарте:
   - graceful shutdown текущей версии: Acolyte-drain (`acolyte_drain_grace`), Conclave-snap, EventStream-стримы закрываются → Souls failback на оставшиеся инстансы.
   - старт новой версии: `state_schema` миграции применяются (см. [§ State_schema migrations](#state_schema-migrations)), Conclave-presence пишется заново.

3. **Verify** новый инстанс:
   - `redis-cli KEYS 'keeper:instance:*'` — N ключей.
   - `keeper_grpc_streams_active` начинает расти по мере failback Souls.
   - `keeper_rbac_snapshot_rebuild_errors_total` не растёт.
   - `/readyz` HTTP-probe возвращает 200.

4. **Вернуть в LB**, перейти к следующему инстансу. Между инстансами — пауза 30s-1min, чтобы Souls успели re-distribute.

5. **Финальная проверка**: после обновления всех инстансов — `keeper --version` на каждом, `keeper_grpc_streams_active` суммарно равен числу подключённых Souls.

### Откат при провале

Если новая версия не стартует / падает по `keeper_*`-метрикам:

1. **Не рестартить с новой**, не паниковать — старые инстансы продолжают обслуживать.
2. Откатить пакет: `sudo dpkg -i /tmp/soul-stack-keeper_<previous-version>_amd64.deb && systemctl restart keeper`.
3. Verify — startup-логи без ошибок, Conclave-presence пришла.
4. Investigate новую версию отдельно.

**Если миграция state_schema успела отработать** при подъёме новой версии — откат бинаря **не откатывает миграцию** (forward-only). Старая версия может не понять новую схему `incarnation.state`. Recovery — см. [§ Откат state_schema](#откат-state_schema).

## State_schema migrations

`state_schema` — версионируемая схема runtime-данных incarnation ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). Версия бампится в `service.yml::state_schema_version`; миграции живут в `service-репо` под `migrations/<NNN>_to_<MMM>/`.

### Когда применяются

При **обработке `incarnation.upgrade`** через Operator API — оператор-инициированно, **не lazy** на старте Keeper-а. Атомарно одной PG-транзакцией: `SELECT FOR UPDATE → in-memory in-Go применение → snapshot per-step в state_history → COMMIT` ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

При проблеме — миграция rollback'ит транзакцию, `incarnation.status` переходит в `migration_failed` (терминал; remediation = откат до целевой версии или fix миграции + replay).

### Что бэкапить перед `incarnation.upgrade`

Снимок `state_history` перед version-bump-ом **никогда не архивируется** Reaper-правилом `archive_state_history` при `keep_version_bump_snapshots: true` (default) — restorable anchor.

Дополнительно — full PG-backup перед массовой миграцией (если планируется upgrade сразу N incarnation-ов).

### Откат state_schema

`down:` в DSL не поддерживается ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). Recovery после неудачной миграции:

1. **`incarnation.status = migration_failed`** — incarnation locked.
2. **Восстановить state из snapshot:**
   ```sql
   -- найти snapshot до миграции (тот, что не архивируется)
   SELECT at, scenario, state_before, state_after
   FROM state_history
   WHERE incarnation_name = '<name>' AND scenario = 'migration'
   ORDER BY at DESC LIMIT 5;

   -- восстановить state в incarnation
   UPDATE incarnation
   SET state = (SELECT state_before FROM state_history WHERE at = '<chosen-snapshot-time>'),
       state_schema_version = <previous-version>,
       status = 'ready'
   WHERE name = '<name>';
   ```
3. **Зафиксить миграцию** в service-репо, выпустить новый ref сервиса.
4. **Повторить `incarnation.upgrade`** с fixed-миграцией.

Эта операция — **аварийная**, требует прямого доступа к PG. Альтернатива — оставить incarnation в `migration_failed` и пересоздать (если миграция была первой и stake не критичен).

## Rolling upgrade Soul-флота

Soul-агенты обновляются независимо от Keeper, потому что forward-compat работает **в обе стороны** ([ADR-012(g)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)):

- Новый Keeper принимает proto-сообщения старого Soul-а — пропущенные only-add поля = `0`/`nil` (forward-compat деградация: например, `WardRoster` от старого Soul-а = пустой → sweep no-op fail-safe, см. [ADR-027(g)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amend).
- Новый Soul принимает proto-сообщения старого Keeper-а — то же самое (`ApplyRequest.attempt = 0` → fencing деградирует до защиты по `apply_id`, см. [ADR-027(g)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)).

### Procedure (через Destiny)

Канонический способ обновить Soul-флот — сам Soul Stack:

```yaml
# destiny/soul-upgrade/destiny.yml
input:
  to_version: { type: string, required: true }

tasks:
  - name: install-new-binary
    module: core.pkg
    state: installed
    name: soul-stack-soul
    version: "${ input.to_version }"
  - name: restart-soul
    module: core.service
    state: restarted
    name: soul
```

Раскатить через `keeper.incarnation.run` на coven управляемых хостов.

### Procedure (вручную)

```sh
# на каждом Soul-хосте
sudo dpkg -i /tmp/soul-stack-soul_<new-version>_amd64.deb
systemctl restart soul
```

После рестарта Soul:
- Bootstrap НЕ происходит заново — SoulSeed уже выпущен, mTLS-сессия восстанавливается.
- Soul переподключается на любой Keeper из priority-листа.
- `WardRoster`-snapshot отправляется при reconnect (S6 Soul-reconcile, [ADR-027(g)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)) — Keeper сверяет dispatch-апплаи.

**Версия Soul** инжектится при сборке (`SOUL_LDFLAGS := -X .../soul.soulVersion=$(VERSION)`, [`Makefile`](../../Makefile)) и попадает в `Hello`/`BootstrapRequest` для аудита.

## `state_schema` миграции — workflow эксплуатации

Полная спека миграций — [`docs/migrations.md`](../migrations.md). Cycle для оператора:

1. **Service-разработчик** правит `service.yml::state_schema_version` (бамп) + кладёт `migrations/<N>_to_<M>/migration.yml` с DSL-операциями (`rename`/`set`/`delete`/`move`, опц. `foreach` для коллекций) + тесты `migrations/<N>_to_<M>/tests/<case>.yml`.
2. **CI** прогоняет `soul-trial` ([ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)) — миграция применяется на state-fixture-х, ассертит `state_after`.
3. **Service-репо** мерджится, выпускается новый git-ref ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)).
4. **Оператор** обновляет `service_registry.ref` через Operator API: `POST /v1/services/{name}` с новым `ref:`.
5. **Оператор** запускает `incarnation.upgrade` через Operator API на конкретную incarnation: `POST /v1/incarnations/{name}/upgrade`. Атомарная одна PG-транзакция (см. выше).
6. **Verify**: `GET /v1/incarnations/{name}` → `status: ready`, `state_schema_version: <M>`. История миграций — в `state_history` со `scenario: migration`.

При проблеме — `status: migration_failed`, см. [§ Откат state_schema](#откат-state_schema).

## Совместимость версий

### Keeper version N ↔ Soul version M

| Сценарий | Поведение |
|---|---|
| Keeper N, Soul N | Все фичи доступны. |
| Keeper N+1 (новый), Soul N (старый) | OK. `ApplyRequest.attempt` пишется, но Soul его игнорирует / не делает fencing. `WardRoster` от Soul-а пустой → orphan-sweep no-op. |
| Keeper N (старый), Soul N+1 (новый) | OK. Soul шлёт `WardRoster`, Keeper его не понимает (молча игнорирует unknown-oneof). `RunResult.attempt = 0` — Keeper не делает epoch-check на приёме. |

**Major-bump** (`/v1/` → `/v2/`) — единственный случай breaking. На момент написания не планируется; при появлении — отдельная процедура с side-by-side выпуском `proto/keeper/v2/`.

### Keeper version N ↔ Operator API клиенты

| Сценарий | Поведение |
|---|---|
| Old client (REST/MCP) | OK. Новые поля в response игнорируются (JSON unknown-field-tolerant), новые endpoints клиент просто не вызывает. |
| New client против old Keeper | OK для существующих endpoints. Новые endpoints (introduced в новой версии Keeper) — 404 на старом → клиент graceful-fallback. |

## Backup before major operations

Чек-лист перед любой нестандартной операцией (массовый upgrade, миграция, ротация ключей):

- [ ] PG backup (логический или физический PITR).
- [ ] Vault snapshot (raft snapshot).
- [ ] Конфиги `keeper.yml` / `soul.yml` всех инстансов — в git, committed.
- [ ] mTLS-материал и TLS-сертификаты — записаны в Vault PKI / в secret manager оператора.
- [ ] Audit-log — последняя выгрузка в холодный архив (для post-incident-debugging).

## См. также

- [`docs/architecture.md` → ADR-012 / ADR-019 / ADR-021](../architecture.md) — обоснования forward-compat и hot-reload.
- [`docs/migrations.md`](../migrations.md) — нормативная спека state_schema-миграций.
- [`docs/keeper/config.md` → Hot-reload](../keeper/config.md#hot-reload) — per-блок политика hot-reload.
- [`disaster-recovery.md`](disaster-recovery.md) — восстановление при провале миграции / отказе после upgrade.
