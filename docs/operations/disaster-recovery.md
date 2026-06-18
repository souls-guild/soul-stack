# Disaster recovery

Сценарии отказа компонентов инсталляции и процедуры восстановления. По принципу «что отказывает → что наблюдается → что делать».

Архитектурный контекст: Keeper stateless ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)), PG = source of truth ([ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)), Redis = эфемерный hot-слой ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)).

## Матрица отказов

| Отказ | Степень тяжести | Восстановление автоматически? | Action оператора |
|---|---|---|---|
| Один из N Keeper-инстансов | Низкая | Да (LB drain, Conclave обновится, Souls failback) | Investigate / restart упавшего |
| Все Keeper-инстансы | Высокая | Нет (нет процессов) | Поднять один минимум |
| Redis | Средняя | Частично (presence/lease восстановится при reconnect) | Поднять Redis; existing-сессии могут продолжить, новые apply упадут на claim-фазе |
| Postgres primary (с replica) | Средняя | Patroni / managed failover делает promote replica | Keeper-инстансы переподключатся на новый primary |
| Postgres без replica | **Критическая** | Нет | Restore из backup |
| Vault sealed / unreachable | Высокая | Нет (fail-closed) | Поднять Vault; existing-сессии живы, новые операции upadut на резолве `vault:` ref |
| Полная катастрофа (всё умерло) | Критическая | Нет | Last-known-good restore из backup-ов |

## 1. Один Keeper-инстанс упал

Симптомы:

- `up{job="keeper",instance="<host>"} == 0` в Prometheus.
- `keeper:instance:<kid>` исчез из Redis через TTL (~30s).
- LB drain-нул backend через health-check fail.
- Souls со стримами на этом инстансе получили обрыв → failback на другие.

Что произошло:

- Conclave-presence снялся (graceful) или истёк (crash).
- `keeper_reaper_lease_held` для этого инстанса = 0; если он держал лидерство — переизбрание через `reaper.lock_ttl` (default 5m).
- Acolyte-claim-ы этого инстанса остались в БД в статусе `claimed`/`dispatched` (если был in-flight). Если `reclaim_apply_runs` включён — recovery-scan переклеймит `claimed` → `planned` на следующем тике Reaper-а; `dispatched` остаётся (Soul-reconcile орфанит на reconnect, [ADR-027(g)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amend).

Action:

1. Investigate через `journalctl -u keeper -n 200` / Prometheus / OTel-traces.
2. `systemctl start keeper` (если падание было разовое — systemd `Restart=on-failure` уже перезапустит).
3. После старта — Conclave-presence пишется, инстанс возвращается в LB по health-check.
4. Verify `keeper_grpc_streams_active` растёт по мере failback Souls.

## 2. Все Keeper-инстансы недоступны

Симптомы:

- Все Souls в `souls.status = 'disconnected'` (через `mark_disconnected` reconcile в PG, отстанет на `stale_after`).
- Operator API недоступен на всех `keeper.internal:8080`.
- Prometheus `up{job="keeper"} == 0` везде.

Что произошло:

Возможные причины:
- Network outage в DC.
- Misconfiguration после rolling-upgrade — все инстансы упали одновременно на новой версии.
- PG / Redis / Vault outage — все Keeper остановились на retry-loop резолва.
- Hardware-инцидент.

Action:

1. **Diagnose первопричину** перед перезапуском. Если все упали по одной причине (битая `keeper.yml`, неработающий Vault) — перезапуск без fix не поможет.
2. Поднять **один** инстанс с проверенной версией:
   - Проверить `keeper.yml` локально на синтаксис. Отдельной офлайн-команды валидации **нет** (CLI `keeper` имеет только `init` / `run` / `version` / `help`; `--check-config` не реализован, [soulctl](../../soulctl/README.md) keeper-конфиг не валидирует). Практика: `keeper run --config=/etc/keeper/keeper.yml` на изолированном dev-инстансе — невалидный конфиг падает на старте с понятной ошибкой; либо запуск с `logging.level=debug` и наблюдение `journalctl -u keeper`.
   - Если новая версия — откатить пакет до проверенной.
   - `systemctl start keeper` — следить за `journalctl -u keeper -f`.
3. После старта одного инстанса — Souls начнут переподключаться на него (потенциальный перегруз — единственный инстанс держит весь флот). Поднимать дополнительные инстансы по мере их готовности.
4. После recovery — investigate root cause и патч.

## 3. Redis outage

Симптомы:

- `keeper_reaper_lease_held` = 0 везде (потеря leader-lease).
- Soul-стримы могут продолжать работать (TCP уже установлен) — но без presence-renewal через ~30s SID-lease истекут, и:
  - `souls.status` через `mark_disconnected` отстанет (если включён reconcile с lease-fallback).
  - Conclave-presence пропадает.
  - `apply:summons` pub/sub не работает → новые apply будут ждать poll-fallback Acolyte (default 2s).
  - `rbac:invalidate` / `service:invalidate` не работает → снимки RBAC / service-registry stale до следующего TTL-poll.
- `keeper_rbac_invalidations_received_total` rate = 0.

Что произошло:

Redis недоступен. Существующие in-memory state Keeper-инстансов продолжает работать на TTL-poll fallback (RBAC / service-registry), но координация между инстансами потеряна.

**Acolyte-claim продолжает работать** — claim делается через PG `FOR UPDATE SKIP LOCKED`, не через Redis. Просто без Summons-сигнала apply будет ждать poll-фаза (latency растёт).

Action:

1. Поднять Redis. Если single-node — restore через restart + AOF replay.
2. После Redis-up Keeper-инстансы автоматически re-register Conclave, SID-lease создаётся на следующем app-сообщении / при reconnect Souls.
3. Reaper-leader переизбирается за `lock_ttl`.

**Backup Redis не нужен** — все ключи восстановимы естественно, см. [`infra.md` → Backup Redis](infra.md#backup--restore-redis).

## 4. Postgres primary потерян (с replica)

Симптомы:

- Patroni / managed-сервис делает automated promote replica.
- Keeper-инстансы получают connection errors на короткое время (`keeper_postgres_connection_errors_total`).
- После promote — пулы пересоздаются, операции возобновляются.

Action:

- **Если Patroni**: ждать failover завершения (обычно 30-60s). Verify через `psql` к new primary.
- **Если cloud-managed**: ждать failover (обычно 60-120s). Verify через provider console.
- **Watchman может среагировать** на короткую недоступность PG → `isolated` → закрытие стримов. Souls failback на оставшиеся-not-watchman'нутые инстансы; после возврата PG Watchman снимает isolated, Souls возвращаются естественно по priority.

После recovery:
- `keeper_postgres_connection_errors_total` rate возвращается к 0.
- `apply_runs` строки, висевшие в claim-фазе при отказе, могут потерять lease и быть переклеймены recovery-scan-ом (если включён) — естественное поведение.
- `state_history` snapshot за время outage — нет (но прогонов в этом окне быть не должно — Keeper не отвечал).

## 5. Postgres primary потерян без replica

Критический сценарий. **Restore процедура:**

1. Остановить все Keeper-инстансы.
2. Восстановить PG из backup (см. [`infra.md` → Restore](infra.md#restore-процедура)).
3. Очистить Redis (`FLUSHDB`).
4. Поднять Keeper-кластер.
5. Souls failback переподключатся.

**Окно потери данных** = от последнего backup-а до момента отказа. Минимизировать через:

- pgBackRest с WAL-archive (PITR с точностью до секунд).
- Регулярные pg_dump (час / сутки).

## 6. Vault outage

Симптомы:

- `keeper_vault_read_errors_total{kind="error"}` rate > 0.
- Старт нового Keeper-инстанса — упал на резолве `postgres.dsn_ref` / `signing_key_ref` / `vault.auth.method=approle` login.
- Уже стартовавшие инстансы продолжают работать (in-memory кэш зарезолвенных секретов).
- Новые операции, требующие Vault на лету:
  - `core.vault.kv-read` ([ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)) → fail (`ErrVaultKVNotFound` или транспортная ошибка).
  - CEL `vault(...)` в render → fail сценарий с render-error.
  - Bootstrap нового Soul-а → fail (`pki/sign/<role>` недоступен).
  - JWT issue для нового Архонта → fail.

Action:

1. Поднять Vault. Если sealed — unseal (auto-unseal предотвращает это; manual unseal требует кворума ключей).
2. После up — все операции возобновляются. Vault-token renewer ([`docs/keeper/prod-setup.md`](../keeper/prod-setup.md)) перезапросит токен.
3. **Verify** — `keeper_vault_read_errors_total` rate возвращается к 0.

**Sealed Vault — fail-closed by design.** Никакого `KEEPER_ALLOW_VAULT_DOWN`-флага нет («безопасность на первом месте», [requirements.md](../requirements.md)).

## 7. Полная катастрофа (PG + Redis + Keeper down)

Hardware-инцидент, DC outage. **Last-known-good restore:**

1. **Поднять инфру в новом DC / на новых хостах:**
   - PG из backup (физический pgBackRest предпочтительнее логического pg_dump — PITR + быстрее).
   - Redis — пустой, поднять (данные эфемерны).
   - Vault из raft-snapshot (см. [`infra.md` → Vault backup](infra.md#vault)).

2. **Verify сети** — Keeper-хосты видят PG / Redis / Vault.

3. **Поднять Keeper-кластер:**
   ```sh
   for h in keeper-1 keeper-2 keeper-3; do ssh $h systemctl start keeper; done
   ```

4. **Soul-флот** — переподключится автоматически:
   - SoulSeed-сертификаты в `/etc/soul/seed/` на хостах живы.
   - В `soul.yml::keeper.endpoints` указаны DNS / IP новых Keeper-хостов (или старые имена резолвятся в новые IP через DNS).
   - Soul-агенты сами retry-перезапрашивают bootstrap → failback по priority → mTLS-handshake к восстановленному CA.

5. **Verify recovery:**
   - `keeper_grpc_streams_active` суммарно = число активных Souls.
   - `souls.status = 'connected'` в реестре `souls` через `mark_disconnected` reconcile (за `stale_after * 2 = 3min`).
   - `incarnation.status` для всех incarnation — в `ready` (если перед катастрофой не было in-flight apply; если был — `applying`/`error_locked`, требует ручного триажа).
   - SQL: `SELECT count(*) FROM apply_runs WHERE status IN ('planned', 'claimed', 'dispatched')` — должен быть 0 или близко (in-flight apply за время катастрофы могут зависнуть; см. [`faq.md`](faq.md)).

### Что теряется при катастрофе

- **Аpply, бывший в process на момент катастрофы** — incarnation остаётся в `applying` / `error_locked`. Оператор ручным action перезапускает прогон.
- **Audit-events за время outage** — нет (Keeper не писал). Это естественно — нечего было audit-ить, Keeper не отвечал.
- **State_history snapshot за время outage** — нет (см. выше).
- **Live OTel-trace-данные за время outage** — нет (OTel-collector мог их получать от уцелевших Souls, но Keeper-spans отсутствуют).

### RTO / RPO

- **RTO** (recovery time objective): зависит от backup-стратегии. С pgBackRest PITR — 30 мин типично.
- **RPO** (recovery point objective): с PITR — секунды-минуты до момента катастрофы; с pg_dump — час / сутки.

## 8. Корректировка после восстановления

После любого crash-recovery — проверить состояние:

### Зависшие incarnation

```sql
SELECT name, status, applying_started_at, NOW() - applying_started_at AS stuck_for
FROM incarnation
WHERE status IN ('applying', 'migration_failed', 'error_locked', 'destroying')
ORDER BY applying_started_at;
```

- `applying` старше 15 минут — owned-by-dead-instance footgun (или валидный длинный прогон). Cross-check с `apply_runs`.
- `error_locked` — провалившийся прогон, требует ручного решения (см. [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) trade-offs).
- `migration_failed` — провалившаяся state_schema-миграция (см. [`upgrade.md` → Откат state_schema](upgrade.md#откат-state_schema)).

### Зависшие `apply_runs`

```sql
SELECT apply_id, sid, status, claim_at, claim_expires_at, attempt
FROM apply_runs
WHERE status IN ('claimed', 'dispatched')
  AND claim_at < NOW() - INTERVAL '1 hour'
ORDER BY claim_at;
```

- Если `reclaim_apply_runs` включён — `claimed` будут переклеймены автоматически.
- `dispatched` — обычно закрывается Soul-reconcile при reconnect Soul-а ([ADR-027(g)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amend). Если Soul тоже умер (post-MVP known-gap) — оператор закрывает вручную:
  ```sql
  UPDATE apply_runs SET status = 'failed', error_summary = 'manual closure after disaster recovery'
  WHERE apply_id = '<ULID>' AND status = 'dispatched';
  ```

### Orphaned Vault keys (если используется Sigil)

Если `reap_orphan_vault_keys` включён (report-only) — после восстановления может быть рост `keeper_reaper_rule_purged_total{rule="reap_orphan_vault_keys"}` (детекция сирот, не удаление). Investigate через лог Reaper-а; ручное удаление через `vault delete secret/keeper/sigil-keys/<key_id>` после verify, что ключ действительно осиротевший.

## Backup-чек-лист — что должно быть готово ДО катастрофы

Без этих артефактов восстановление невозможно:

- [ ] PG-backup стратегия настроена (pgBackRest или pg_dump cronjob).
- [ ] Vault raft-snapshot регулярно бэкапится.
- [ ] mTLS-материал Keeper и CA приватник — в Vault PKI (восстанавливается из Vault-snapshot).
- [ ] Vault unseal-keys / recovery-keys — в безопасном месте (НЕ на Keeper-хосте).
- [ ] `keeper.yml` / `soul.yml` — в git.
- [ ] DNS-записи / IP Keeper-хостов — документированы, чтобы Soul-конфиги можно было быстро направить на новые хосты.
- [ ] L4-LB конфиг — в git / IaC.
- [ ] Vault AppRole role_id / secret_id — оператор знает, где они лежат (НЕ только в Vault).
- [ ] Procedure restore — отрабатывали на staging.

## См. также

- [`infra.md`](infra.md) — backup / restore PG / Redis / Vault.
- [`docs/architecture.md` → ADR-005 / ADR-006 / ADR-027](../architecture.md) — обоснования отказоустойчивости.
- [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md#включение-recovery-recovery-enable) — `reclaim_apply_runs` гейт.
- [`faq.md`](faq.md) — типичные проблемы.

## Open questions (runbook)

Сверено с кодом (CLI `keeper`, реестр метрик); ниже — отсутствующие операционные удобства, которые могут понадобиться позже:

- **`keeper --check-config`** — отдельной подкоманды офлайн-валидации `keeper.yml` **нет** (CLI = `init` / `run` / `version` / `help`). Практика валидации — `keeper run` на изолированном dev-инстансе (невалидный конфиг падает на старте), см. шаг 2 выше. Кандидат на post-MVP-удобство.
- **`keeper issue-token`** — отдельного subcommand на manual JWT issue для существующего Архонта **нет**. Catastrophic identity recovery — через ротацию signing-key + bootstrap-like процесс (см. [bootstrap-rbac.md → Сброс к «единственному админу»](bootstrap-rbac.md#сброс-к-единственному-админу-catastrophic-recovery)). Кандидат на post-MVP.
- **Conclave-метрики** — метрики вида `keeper_conclave_live_count` в коде **нет** (реестр Keeper-метрик — [observability.md](../observability.md)). Живость инстансов смотрят по Redis-ключам conclave напрямую и по `journalctl`. Кандидат на post-MVP.
- **Conclave-deregister на crash** — graceful-shutdown снимает ключ; crash оставляет до TTL. Явной команды `keeper conclave-evict --kid=...` для «знаю, что хост точно мёртв, не ждать TTL» **нет** — оператор ждёт истечения TTL либо удаляет ключ из Redis вручную. Кандидат на post-MVP.
