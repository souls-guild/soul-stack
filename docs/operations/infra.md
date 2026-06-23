# Инфра-зависимости: Postgres, Redis, Vault

Soul Stack эксплуатирует три обязательные внешние зависимости ([ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres) / [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) / [requirements.md](../requirements.md)). Прод-инсталляция должна поднять их **отдельно** от Keeper-кластера — Soul Stack их не управляет, только потребляет.

Эта зона документации фокусируется на **операционной части**: что бэкапить, как восстанавливать, какие настройки критичны для корректной работы Keeper-а. Архитектурное «зачем» — в соответствующих ADR.

## Postgres

Source of truth Keeper-кластера. Stateless Keeper-инстансы пишут сюда всё, что переживает рестарт: реестры (`souls` / `operators` / `rbac_*` / `service_registry` / …), `incarnation.state` + `state_history`, `apply_runs`, `audit_log`, `plugin_sigils`. Полный каталог таблиц — [`docs/keeper/storage.md`](../keeper/storage.md).

### Версия и режим

| Параметр | Минимум | Рекомендуется |
|---|---|---|
| Версия PostgreSQL | 14 | 16 (LTS-окно длиннее) |
| Connection-encryption | TLS (`sslmode=require`) — обязательно в прод | `sslmode=verify-full` с server-cert от внутренней PKI |
| Search path | default `public` | default `public` |
| Encoding | `UTF8` | `UTF8` |
| Locale | `C` / `en_US.UTF-8` | `en_US.UTF-8` (для текстового поиска) |

DSN передаётся через Vault KV (`postgres.dsn_ref: vault:secret/keeper/postgres`, поле `dsn`) — plaintext-DSN в `keeper.yml` запрещён парсером ([`docs/keeper/config.md` → postgres](../keeper/config.md#postgres)).

### HA-топология

| Решение | Когда |
|---|---|
| **Single instance** + регулярный backup | dev / staging. Прод **не рекомендуется** — single-point-of-failure. |
| **Patroni** (streaming replication + automated failover через etcd/consul/raft) | Прод on-premise. Промоут реплики при отказе primary — автоматический, Keeper-инстансы переподключаются (пул `postgres.pool.min/max`). |
| **Managed**: RDS PostgreSQL (Multi-AZ), Cloud SQL (regional), Yandex Managed PG, Azure Database (zone-redundant) | Прод в облаке. Failover управляется провайдером. |

**Промоут реплики — вне Soul Stack** ([ADR-027(k)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Keeper не управляет своей БД-топологией — при отказе PG-primary failover делает Patroni / managed-сервис, Keeper-инстансы реконнектятся на новый primary (пул пересоздаётся). Изолированные от PG Keeper-инстансы — Watchman ([scaling.md](scaling.md)) сбросит их Soul-стримы → Soul-ы failback-ом перейдут на здоровые инстансы.

### Пул соединений

```yaml
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 5, max: 50 }
```

Общий поток в PG = `pool.max × N_keeper_инстансов`. Например, кластер из 3 инстансов с `pool.max: 50` потенциально удерживает **150** активных соединений к PG. PG `max_connections` должен быть выше с запасом на бэкап-инструменты + сторонние клиенты (минимум `150 × 1.5 = 225`, default PG 100 — слишком мало).

### Размер таблиц (приблизительная оценка)

Расчёт под целевой масштаб ([многократно упоминается в ADR](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) — invariant «100k VM»):

| Таблица | Строк на инсталляцию | Размер строки | Объём (примерно) |
|---|---|---|---|
| `souls` | ~ число управляемых хостов (1k…100k) | ~ 200 B | 200 KB … 20 MB |
| `soul_seeds` | ~3-5× строк `souls` (история ротаций SoulSeed) | ~ 200 B | ~ 100 MB на 100k VM |
| `operators` + `rbac_*` | десятки строк | малый | <1 MB |
| `service_registry` | десятки строк | малый | <1 MB |
| `incarnation` | ~ число развёрнутых сервисов (десятки-сотни) | ~ 5-50 KB (jsonb `spec`/`state`) | до сотен MB |
| `state_history` | active snapshots × incarnation; **archive_state_history** держит keep_last_n=50 на incarnation | ~ 5-50 KB | до GB на больших инсталляциях |
| `apply_runs` | per-(apply_id, sid) на каждый прогон; retention 30d (`purge_apply_runs`) | ~ 500 B | сотни MB при активной эксплуатации |
| `apply_task_register` | per-(apply_id, sid, task_idx); retention 1h (`purge_apply_task_register`) | jsonb register_data, переменно | 10s-100s MB |
| `audit_log` | все Архонт-действия + Reaper + push + cloud + soul_grpc; retention 365d (`purge_audit_old`) | ~ 1 KB + jsonb payload | GBs при активной эксплуатации |
| `plugin_sigils` | единицы-десятки записей | ~ 1 KB | <1 MB |

**Дисковый sizing**: 20-50 GB достаточно для типичной инсталляции до 10k VM; 100k VM — планировать 100-200 GB с запасом на audit-log за год.

`audit_log` — главный потребитель объёма при росте. Партиционирование по `created_at` (BRIN-индекс или declarative partitioning по месяцам) — расширение post-MVP ([ADR-022 trade-offs](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)), не breaking.

### Backup / Restore

**Source of truth — всё в PG**, поэтому регулярный backup критичен. Восстановление из бэкапа возвращает кластер к момента snapshot-а.

#### Логический backup (pg_dump)

Для совместимости с инструментами CI / cold-archive:

```sh
PGPASSWORD=$(vault kv get -field=password secret/keeper/postgres-backup) \
pg_dump \
  --host=pg.internal \
  --username=keeper_backup \
  --format=custom \
  --compress=9 \
  --file=/backup/keeper-$(date -u +%Y%m%dT%H%M%SZ).dump \
  keeper
```

- Формат `custom` (бинарный, сжатый) — быстрее восстанавливается, поддерживает `pg_restore -j`.
- Отдельный read-only пользователь `keeper_backup` (`GRANT pg_read_all_data`) — не использовать app-пользователя `keeper`.
- **Регулярность**: ежечасно для активной инсталляции, ежесуточно для статичной.
- **Retention**: 30 дней локально + 1 год в холодном архиве (S3 / GCS / Yandex Object Storage) — соответствует `audit_log.purge_audit_old.max_age` по умолчанию.

#### Физический backup (pgBackRest / barman)

Рекомендуется для прод-инсталляции — WAL-archive + point-in-time recovery (PITR):

```ini
# /etc/pgbackrest/pgbackrest.conf (примерная конфигурация)
[global]
repo1-path=/var/lib/pgbackrest
repo1-retention-full=7
repo1-retention-archive=7

[keeper]
pg1-host=pg.internal
pg1-port=5432
pg1-database=keeper
pg1-user=postgres
```

```sh
# Полный бэкап раз в сутки
pgbackrest --stanza=keeper backup --type=full
# Инкрементальный — каждый час
pgbackrest --stanza=keeper backup --type=incr
```

Преимущество — **PITR**: восстановление на конкретный timestamp до момента инцидента (например, до случайного `DELETE` через API). Без PITR оператор теряет всё после последнего snapshot-а.

#### Restore-процедура

1. **Остановить весь Keeper-кластер** — иначе восстановленный PG будет писаться поверх (split state):
   ```sh
   for h in keeper-1 keeper-2 keeper-3; do ssh $h systemctl stop keeper; done
   ```
2. **Восстановить PG** из выбранного backup-а:
   - Физический (pgBackRest):
     ```sh
     pgbackrest --stanza=keeper --type=time --target="2026-05-26 14:30:00" restore
     ```
   - Логический (pg_dump):
     ```sh
     dropdb keeper && createdb keeper
     pg_restore -d keeper -j 4 /backup/keeper-2026-05-26.dump
     ```
3. **Очистить Redis** — кэшированный presence Souls / Conclave / SID-lease ссылается на pre-restore состояние и может рассинхронить:
   ```sh
   redis-cli FLUSHDB
   ```
   Это безопасно — все ключи Redis эфемерны (presence восстановится при reconnect Souls), см. [§ Redis](#redis).
4. **Поднять Keeper-кластер** обратно:
   ```sh
   for h in keeper-1 keeper-2 keeper-3; do ssh $h systemctl start keeper; done
   ```
5. **Souls сами переподключатся** (failback-loop в `soul.yml::keeper.endpoints`). После переподключения SID-lease восстанавливается, `souls.status` приходит в `connected` через `mark_disconnected`-reconcile (двунаправленный, ADR-006 amendment).
6. **Verify**: `keeper_grpc_streams_active` (Prometheus) на каждом инстансе должен расти до ожидаемого числа Souls. `apply_runs.status = 'running'` строк до восстановления — могут зависнуть в `dispatched`/`claimed`; recovery-scan Reaper их подберёт (если `reclaim_apply_runs` включён) или администратор перезапустит вручную.

#### Что **не** восстанавливается из PG-backup-а

- **Vault-секреты** (JWT signing-key, DSN, SoulSeed CA приватник, Sigil signing-key) — отдельный бэкап Vault (см. [§ Vault](#vault)).
- **Redis-состояние** — by design (эфемерно, восстанавливается естественно).
- **mTLS-материал на хостах** (`/etc/keeper/tls/`, `/var/lib/soul-stack/seed/`) — бэкапить через стандартные file-backup-инструменты или ротировать заново через Vault PKI после restore.

### Retention и housekeeping

Keeper сам чистит свои таблицы через Reaper-правила ([`docs/keeper/reaper.md`](../keeper/reaper.md)):

| Таблица | Правило | Default `max_age` |
|---|---|---|
| `bootstrap_tokens` (pending) | `expire_pending_seeds` | 24h |
| `bootstrap_tokens` (used) | `purge_used_tokens` | 90d |
| `souls` (disconnected/expired) | `purge_souls` | 30d |
| `soul_seeds` (superseded/expired/revoked) | `purge_old_seeds` | 90d |
| `audit_log` | `purge_audit_old` | 365d |
| `apply_runs` (терминальные) | `purge_apply_runs` | 30d |
| `apply_task_register` (после терминала) | `purge_apply_task_register` | 1h (grace) |
| `state_history` (активные) | `archive_state_history` (`soft_delete`) | `keep_last_n: 50` на incarnation; миграционные snapshots не архивируются |

Дополнительно вручную (вне Reaper-а):

- **`VACUUM ANALYZE`** — autovacuum в PG включён по умолчанию; для больших таблиц (`audit_log`, `state_history`) при долгой эксплуатации можно запускать `VACUUM FULL` в окно обслуживания для дефрагментации.
- **Soft-deleted `state_history` (archived_at IS NOT NULL)** — Keeper их не удаляет физически. Если место кончается — отдельный bulk-выгрузчик в S3 + DELETE (см. [ADR-Q19](../architecture.md#открытые-вопросы)).

## Redis

Горячий слой и координационная шина Keeper-кластера. **Эфемерное хранилище** — все ключи восстановимы естественно при reconnect Souls и продлении lease-renew goroutine-ами.

### Версия и режим

| Параметр | Минимум | Рекомендуется |
|---|---|---|
| Версия Redis | 6.2 | 7.x (улучшен ACL, доп. команды) |
| Топология | single instance + AOF (`redis.mode: standalone`) | Sentinel (рекомендуемый HA on-premise) или Cluster (горизонтальное масштабирование) — см. [§ HA Redis](#ha-redis) |
| Authentication | пароль через `redis.password_ref` в `keeper.yml` (vault-ref или plaintext) | vault-ref `vault:secret/keeper/redis` (резолв из Vault) + ACL-пользователь с минимальным набором команд (post-MVP) |
| Persistence | AOF (everysec) или RDB-snapshot | AOF — лишнее: данные эфемерны, восстановятся через reconnect |
| TLS | опционально | желательно — Redis ⇄ Keeper по TLS, особенно cross-DC |

Пароль Redis (`redis.password_ref`) поддерживает обе формы (см. [config.md → redis](../keeper/config.md#redis)):

- **vault-ref** `vault:secret/keeper/redis[#field]` — резолвится из Vault keeper-vault-клиентом на старте (как `postgres.dsn_ref` / `auth.jwt.signing_key_ref`). Default-поле KV-секрета — `password`; другое поле выбирается суффиксом `#field`. Это рекомендуемая прод-форма.
- **plaintext-строка** — работает как есть (dev / тесты без Vault-fixture).
- **пустое** — подключение к Redis без пароля.

Записанный ниже в Vault KV `secret/keeper/redis` (шаг bootstrap, таблица ротации) — **действующая ссылка**: достаточно указать в `keeper.yml` `password_ref: vault:secret/keeper/redis`.

### Что лежит в Redis

| Префикс ключа | Что | TTL | Когда обновляется |
|---|---|---|---|
| `soul:<sid>:lock` | SID-lease (какой Keeper-инстанс держит EventStream данного Soul-а) | 30s | renew каждые 10s renew-goroutine-ой handler-а; гаснет при `Release` (graceful) или TTL (crash). |
| `keeper:instance:<kid>` | [Conclave](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) — presence Keeper-инстанса | 30s (`DefaultConclaveTTL`) | renew каждые 10s (`DefaultConclaveRenewInterval`); снимается на graceful-shutdown. |
| `reaper:leader` | Лидерский lease Reaper-а | `reaper.lock_ttl` (default 5m) | renew переизбирается по TTL; один live-Reaper в кластере. |
| `apply:summons` | pub/sub-сигнал «появились planned-задания» ([ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)) | без TTL (pub/sub) | Эфемерный сигнал; потеря компенсируется poll-fallback Acolyte-ов. |
| `events:shard:<n>` | cluster-routing apply/run-событий (`TaskEvent`/`RunResult`/`ErrandResult`) между Keeper-инстансами ([ADR-006(c.1)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis), `applybus`) | без TTL (pub/sub) | Эфемерный; фикс. множество из 256 шардов (`n = fnv32a(apply_id) % 256`), bridge per-shard. Потеря событий допустима (fire-and-forget SSE). |
| `sigil:anchors-changed` | pub/sub-сигнал «trust-anchor-набор обновился» ([ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)) | без TTL | Эфемерный; потеря компенсируется TTL-fallback-перечитом (`sigil_anchors_reload_interval`, default 30s). |
| `rbac:invalidate` | pub/sub-сигнал «RBAC-снимок обновился» ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)) | без TTL | TTL-poll fallback на стороне `Holder`. |
| `service:invalidate` | pub/sub-сигнал «реестр Service-ов обновился» ([ADR-029](../adr/0029-service-registry.md#adr-029-реестр-service-ов--postgres)) | без TTL | TTL-poll fallback. |
| `cancel:<apply_id>` | cluster-wide cancel-сигнал прогона ([cluster-wide cancel](../keeper/storage.md#cluster-wide-cancel-прогона)) | короткий TTL | при `POST /v1/incarnations/{name}/cancel`. |

Все ключи имеют **fallback-механизм** в коде ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis), [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)): потеря pub/sub-сигнала покрывается TTL-poll-ом, потеря lease — TTL и переизбранием.

### Параметры

```redis.conf
# Persistence — лёгкая, нужна только для перезапуска Redis без потери lease-окна.
appendonly yes
appendfsync everysec

# Память — нет жёсткого ограничения, но безопасно ограничить, чтобы не вытеснить OS:
maxmemory 1gb
maxmemory-policy noeviction
```

**Eviction policy — `noeviction`**, не `allkeys-lru`. Обоснование: каждый ключ Redis в Soul Stack имеет **собственный TTL** и **смысловой fallback** в коде; eviction LRU вытеснил бы lease посреди жизни (split-brain Reaper / двойной handler одного SID). `noeviction` + `maxmemory` достаточно с большим запасом — суммарный объём всех ключей на 100k VM:

- `soul:<sid>:lock` × 100k = ~10 MB
- `keeper:instance:<kid>` × десятки = <1 KB
- `apply:summons` / `events:shard:*` / `cancel:*` — pub/sub, в Redis не хранятся (только трансляция)
- pub/sub-каналы — не хранятся, только трансляция

**1 GB `maxmemory` с большим запасом** для целевого масштаба.

### HA Redis

Клиент Keeper-а поддерживает все три топологии **нативно** через `redis.mode` (slot-routing для Cluster, master-discovery для Sentinel — на стороне клиента, без внешней прокси). Полная нормативная схема полей блока — [config.md → redis](../keeper/config.md#redis).

| Решение | `redis.mode` | Обязательные поля | Когда |
|---|---|---|---|
| **Single instance** + AOF | `standalone` (default) | `addr` | dev / staging, малые инсталляции, один узел. |
| **Sentinel** (1 master + N replica + Sentinel quorum) | `sentinel` | `master_name` + `sentinels[]` | **Рекомендуемый HA-путь для типового on-premise** — автоматический failover; single-master, проще и безопаснее Cluster. |
| **Cluster** (sharded, 3+ master + replicas) | `cluster` | `nodes[]` | Горизонтальное масштабирование под большой объём ключей. См. кавеат про pub/sub ниже. |
| **Managed**: ElastiCache, Cloud Memorystore, Yandex Managed Redis | `standalone` (за единым endpoint-ом) или `cluster`/`sentinel` по форме managed-сервиса | по выбранному `mode` | Прод в облаке. |

Cluster validated на мега-приёмке 2026-05-25 (3 keeper + 9-нодовый реальный redis-cluster): leader-failover Reaper-а по TTL без split-brain, presence Souls / Conclave, реальные сценарии прогонов.

!!! note "Рекомендация: Sentinel для типового HA"
    Для типового on-premise HA выбирайте **Sentinel**, а не Cluster. Sentinel — single-master с автоматическим failover: проще в эксплуатации, без шардирования и slot-rebalance, и снимает кавеат про cluster pub/sub (ниже). Cluster берите осознанно — когда объём ключей перерос один master.

!!! warning "Cluster pub/sub: broadcast на очень больших флотах"
    В режиме `cluster` Redis-pub/sub — классический broadcast (сообщение доходит до всех узлов кластера). Это **корректно** (координационные сигналы доезжают), но на **очень больших флотах** broadcast не снимает известный bottleneck pub/sub-нагрузки. Sharded pub/sub — план на GA. Для типового масштаба и для on-premise-HA через Sentinel этот кавеат не актуален.

### Backup / restore Redis

**Не нужен** в нормальной операционной модели. Все ключи восстановимы:

- SID-lease — renew-goroutine handler-а; на reconnect Soul-а lease создаётся заново.
- Conclave — на старте Keeper-инстанса `RegisterInstance` пишет presence заново.
- Reaper-leader — переизбрание по TTL.
- pub/sub — эфемерный, потеря компенсируется fallback.

**Восстановление инфраструктуры** — поднять новый Redis, направить туда блок `keeper.yml::redis` (`addr` для `standalone`, `sentinels[]`/`nodes[]` для `sentinel`/`cluster`) через hot-reload или systemd reload. Souls продолжают работать, presence пере-фиксируется в новом Redis за TTL (~30s).

## Vault

Хранилище **всех секретов** инсталляции: PG DSN, JWT signing-key, mTLS PKI (Keeper-side + SoulSeed), SSH-CA (для `keeper.push` через Vault SSH provider), Sigil signing-key, Essence-секреты сервисов, credentials cloud-driver-ов. **Обязательная зависимость** ([requirements.md](../requirements.md), [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).

Прод-настройка Vault (AppRole + persistent backend + auto-unseal + least-privilege policy) подробно описана в [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md). Здесь — операционная часть.

### Engines Keeper-а

| Engine | Mount | Что хранится | Использование |
|---|---|---|---|
| KV (v1/v2) | `secret/` | `keeper/postgres` (DSN), `keeper/jwt-signing-key`, `keeper/redis` (password), `keeper/sigil-signing-key`, `keeper/sigil-keys/<key_id>` (R3 multi-anchor), Essence-секреты сервисов | резолв `vault:` ref в конфиге и в CEL `vault(...)`. Версия KV mount-а определяется автоматически (probe), работают v1 и v2; провижининг ниже поднимает v2 как рекомендованный дефолт. **Sigil multi-anchor (R3, `sigil-keys/<key_id>`) — list/metadata-операции, требуют KV v2.** |
| PKI | `pki/` (или `pki/soulstack/`) | Root + intermediate CA для SoulSeed mTLS | `Bootstrap`-RPC подписывает CSR Soul-агента через `pki/sign/<pki_role>` |
| SSH | `ssh/` (опционально) | SSH CA для `keeper.push` через `soul-ssh-vault` provider | `keeper.push` запрашивает signed SSH cert на конкретный хост перед SSH-сессией |
| Transit | (опционально) | Подпись JWT без экспорта ключа | post-MVP, см. [ADR-014(b)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon) |

!!! note "TLS к Vault: custom-CA bundle в конфиге не параметризован (post-MVP)"
    HTTPS к Vault (`vault.addr: https://…`) работает, **если серверный сертификат Vault цепляется к системному trust-store** хоста Keeper-а (либо передан Vault SDK через стандартные переменные окружения, напр. `VAULT_CACERT`). Отдельного поля под кастомный CA-bundle для Vault в `keeper.yml` сейчас **нет** — клиент строится через `vaultapi.DefaultConfig()`. Параметризация custom-CA для Vault — post-MVP; пока вносите CA Vault-а в системный trust-store хоста.

### Bootstrap Vault для Keeper

Минимальный набор операций — провижининг секретов и engines:

```sh
export VAULT_ADDR=https://vault.internal:8200
export VAULT_TOKEN=<root-or-admin-token>

# KV — клиент Keeper-а работает и с v1, и с v2 (версия определяется автоматически).
# v2 рекомендован (versioning + metadata; Sigil multi-anchor list/metadata требует v2)
# и в dev-mode активен по умолчанию на mount `secret`.
vault secrets enable -version=2 -path=secret kv 2>/dev/null || true

# Записать обязательные секреты Keeper-а
vault kv put secret/keeper/postgres \
  dsn="postgres://keeper:<password>@pg.internal:5432/keeper?sslmode=verify-full"
# Redis: пароль читается из Vault (keeper.yml::redis.password_ref:
# vault:secret/keeper/redis). Default-поле KV-секрета — `password`.
vault kv put secret/keeper/redis password="<redis-password>"
vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"

# PKI engine для SoulSeed
vault secrets enable -path=pki/soulstack pki
vault secrets tune -max-lease-ttl=87600h pki/soulstack
vault write -field=certificate pki/soulstack/root/generate/internal \
    common_name="Soul Stack SoulSeed CA" ttl=87600h > /tmp/ca.crt
vault write pki/soulstack/roles/soul-seed \
    allowed_domains="example.com,internal" \
    allow_subdomains=true \
    max_ttl=720h

# AppRole для Keeper (прод)
vault auth enable approle
vault policy write keeper-prod /path/to/vault-policy.hcl   # see examples/keeper/vault-policy.hcl
vault write auth/approle/role/keeper-prod \
    token_policies=keeper-prod \
    secret_id_ttl=720h token_ttl=1h token_max_ttl=24h
vault read auth/approle/role/keeper-prod/role-id           # role_id → keeper.yml
vault write -f auth/approle/role/keeper-prod/secret-id     # secret_id → /etc/keeper/vault-secret-id mode 0400
```

Полный шаблон policy с покомментированными путями — [`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl). Подробности AppRole — [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md).

### Лёгкий Vault для малых инсталляций

Vault — обязательная зависимость ([ADR-053](../adr/0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей)), но это **не значит «нужен тяжёлый Vault-кластер»**. Для малой инсталляции достаточно одного процесса Vault с file-storage — операционно это сопоставимо с эксплуатацией Redis (один бинарь, локальный каталог под данные).

```hcl
# /etc/vault/vault.hcl — single-binary, file-storage
storage "file" { path = "/var/lib/vault/data" }
listener "tcp" {
  address     = "127.0.0.1:8200"
  tls_disable = false              # прод: реальный TLS, не disable
}
api_addr = "https://vault.internal:8200"
```

Первый запуск — `vault operator init` (сохранить unseal-keys и root-token в безопасном месте), затем `vault operator unseal` нужным числом ключей. Дальше — секреты и engines по разделу [Bootstrap Vault для Keeper](#bootstrap-vault-для-keeper) выше.

**dev-mode (`vault server -dev`) для прода непригоден** — он держит данные только в памяти и **теряет их при каждом рестарте** (а также unseal-ит сам себя и слушает по HTTP). Годится только для локальных экспериментов. Для прода — **file- или raft-storage + явный unseal** (в проде — [auto-unseal](#backup--restore-vault), иначе каждый рестарт требует ручного ввода кворума ключей).

Детали single-binary-конфигурации, raft-storage и auto-unseal — в [официальной документации Vault](https://developer.hashicorp.com/vault/docs/configuration); прод-настройка для Keeper — [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md).

### Backup / restore Vault

Vault бэкапит **storage backend**, не сам бинарь. Зависит от выбранного backend-а:

- **Raft** (рекомендуется): `vault operator raft snapshot save /backup/vault-$(date -u +%Y%m%dT%H%M%SZ).snap`. Снимок включает все KV, политики, токены, конфигурацию engines.
- **Consul / etcd**: бэкап соответствующего storage по их инструментам.
- **Cloud-managed (HCP Vault, AWS Secrets Manager, …)**: бэкап управляется провайдером.

**Ротация unseal-keys / recovery-keys** — по политике безопасности организации, обычно ежегодно. См. [Vault docs → Key rotation](https://developer.hashicorp.com/vault/docs/concepts/rotation).

**Auto-unseal** ([ADR в prod-setup](../keeper/prod-setup.md)) обязателен в прод — иначе каждый рестарт Vault требует ручного unseal с кворумом ключей. Cloud KMS (AWS KMS / GCP KMS / Azure Key Vault) или HSM — стандартные варианты.

### Ротация секретов

| Секрет | Как часто | Процедура |
|---|---|---|
| JWT signing-key (`secret/keeper/jwt-signing-key`) | По компрометации / по политике (раз в 6-12 мес) | `vault kv put` → `systemctl reload keeper` → перевыпуск всех JWT (старые невалидны). См. [§ Ротация signing-key в prod-setup.md](../keeper/prod-setup.md#jwt-signing-key-прод). |
| PG password (`secret/keeper/postgres`) | По политике (раз в 90 дней) | `ALTER USER keeper PASSWORD …` в PG → `vault kv put secret/keeper/postgres dsn=…` → `systemctl reload keeper` (пул пересоздаётся с новым DSN). Окно атомарности — кратковременный `connection failed` пока пул переcoздаётся. |
| Redis password (`secret/keeper/redis`) | По политике | `CONFIG SET requirepass …` в Redis → `vault kv put secret/keeper/redis password=…` (если `redis.password_ref` — vault-ref) → `systemctl reload keeper`. Резолв пароля Redis из Vault реализован — `password_ref: vault:secret/keeper/redis` действует. |
| SoulSeed (mTLS-cert Soul-а) | Регулярно (TTL `pki/soulstack/roles/soul-seed`, до 30d) | автоматически по живому стриму ([`docs/soul/onboarding.md`](../soul/onboarding.md)). |
| Vault AppRole `secret_id` | По TTL `secret_id_ttl` (default 720h в нашем шаблоне) | `vault write -f auth/approle/role/keeper-prod/secret-id` → переписать `/etc/keeper/vault-secret-id` (mode 0400) → `systemctl reload keeper` (или restart — auth-блок reload-able? см. [config.md hot-reload](../keeper/config.md#hot-reload)). |
| Vault root-token | По завершении bootstrap | `vault token revoke <root-token>` — после настройки AppRole не нужен; для plurpose `keeper init` уже отработал. |
| Sigil signing-key (`secret/keeper/sigil-signing-key`) | По компрометации (редко) | R3 multi-anchor allows graceful rotation: добавить новый ключ → re-broadcast trust-anchors → Retire старого; см. [ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс). |

**Sealed Vault → Keeper fail-closed.** При запечатанном Vault Keeper не резолвит ни `vault:` ref (DSN / signing-key / passwords), ни выпустит SoulSeed через PKI. Существующие сессии (с уже резолвленными секретами и активным PG-пулом) продолжают работать; новые операции, требующие Vault, падают. Это часть инварианта «безопасность на первом месте» — `KEEPER_ALLOW_VAULT_DOWN`-флага нет и не планируется.

## См. также

- [`docs/keeper/storage.md`](../keeper/storage.md) — полный каталог таблиц Postgres + ключей Redis.
- [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md) — Vault AppRole, least-privilege policy, persistent + auto-unseal, JWT signing-key rotation.
- [`docs/keeper/reaper.md`](../keeper/reaper.md) — Reaper-правила (retention).
- [`docs/architecture.md` → ADR-005 / ADR-006 / ADR-022 / ADR-026 / ADR-029](../architecture.md) — обоснования.
- [`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl) — шаблон policy.
- [`disaster-recovery.md`](disaster-recovery.md) — сценарии полной катастрофы (PG+Redis+Vault+Keeper).
