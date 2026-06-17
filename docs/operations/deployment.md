# Deployment

Раскатка бинарей Soul Stack на прод-хосты. Минимальный single-keeper, HA multi-keeper, требования к ОС, systemd-юниты, конфиги.

## Бинари

Соответственно [ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) — **три операторских бинаря** (`keeper` / `soul` / `soul-lint`), раскатываемых на инфраструктуру. Четвёртый артефакт `soul-trial` — офлайн тест-раннер ([ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)) authoring-цикла (CI / dev-машина), на прод-хосты не ставится:

| Бинарь | Роль | Что | Где запускается |
|---|---|---|---|
| `keeper` | операторский | Центральный сервер (gRPC bidi к Soul, OpenAPI, MCP, push-модуль, Reaper). | Keeper-хост (`/usr/local/bin/keeper`). |
| `soul` | операторский | Демон-агент. В push-режиме — тот же бинарь, запускается `soul apply` через SSH. | Управляемый хост (`/usr/local/bin/soul`). |
| `soul-lint` | операторский | Офлайн-линтер Destiny / Service / Manifest / Scenario. | CI / dev-машина. |
| `soul-trial` | тест-инструмент | Офлайн-раннер испытаний destiny/scenario/миграций ([ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)). Не операторский артефакт. | CI / dev-машина. |

### Где взять артефакты

| Способ | Команда / источник | Когда |
|---|---|---|
| Из исходников | `make build` ([`Makefile`](../../Makefile)) | dev / staging. Бинари в `<module>/bin/<name>`. |
| Нативные пакеты deb / rpm | `make pkg` (требует `nfpm`, см. [`deploy/README.md`](../../deploy/README.md)). Артефакты в `dist/pkg/`. | Прод-инсталляция на Linux. Конфиги nfpm — [`deploy/nfpm/`](../../deploy/nfpm/). |
| Docker-образы | `docker build -f deploy/docker/<name>.Dockerfile -t soul-stack/<name> --build-arg VERSION=$(git describe …) .` (multi-stage, distroless runtime; см. [`deploy/README.md`](../../deploy/README.md)). | Контейнерная раскатка. |
| SBOM | `make sbom` (CycloneDX через `cyclonedx-gomod`, режим `app`). Артефакты в `dist/sbom/`. | Требования compliance / supply-chain audit. |

`make pkg` пересобирает Linux-бинари под `PKG_ARCH` (`amd64` default, переопределяется `make pkg PKG_ARCH=arm64`) с `CGO_ENABLED=0 -trimpath -ldflags '-s -w'` и инжектит `VERSION` ldflags-ом (см. [`Makefile`](../../Makefile)).

Подпись образов (cosign / sigstore) — отложена до появления CI + registry (`make sign` — documented stub), см. [`deploy/README.md` § «Подпись образов»](../../deploy/README.md).

## Системные требования

### Keeper-хост

| Параметр | Минимум | Рекомендуется |
|---|---|---|
| ОС | Linux x86_64 / arm64, kernel 5.10+ | RHEL/Alma/Rocky 9, Debian 12, Ubuntu 22.04 LTS |
| systemd | 245+ (ProtectSystem=strict + LoadCredential) | 252+ |
| CPU | 2 vCPU | 4-8 vCPU per-keeper-инстанс |
| RAM | 1 GB | 4 GB per-keeper-инстанс (зависит от размера флота и Acolyte-пула) |
| Диск (root FS) | 5 GB | 20 GB |
| Диск `/var/lib/keeper` | 1 GB | 10 GB (TLS-материал, кеш плагинов, work-root git-резолва) |
| Сетевые порты | см. [§ Сетевые порты](#сетевые-порты) | — |

Хост запускается под отдельным системным пользователем `soul-stack` (см. шапку [`deploy/systemd/keeper.service`](../../deploy/systemd/keeper.service)). Hardening — жёсткий профиль (`ProtectSystem=strict`, `MemoryDenyWriteExecute`, `PrivateDevices`, …) — Keeper не меняет хост, ему изоляция не мешает.

### Soul-хост (управляемый)

| Параметр | Минимум | Рекомендуется |
|---|---|---|
| ОС | Linux x86_64 / arm64, kernel 5.4+ | любая поддерживаемая в [Soulprint OsFacts](../soul/soulprint.md) дистрибуция (debian/ubuntu/redhat-family/alpine) |
| systemd | 245+ | по дистро |
| CPU | 1 vCPU | 2 vCPU |
| RAM | 256 MB | 1 GB (на момент apply больше — зависит от модулей) |
| Диск `/var/lib/soul-stack` | 200 MB | 2 GB (кеш модулей по SHA-256, SoulSeed) |
| Сетевые порты | egress к Keeper EventStream + bootstrap-listener-у | — |

Hardening Soul — **мягкий профиль** (см. [`deploy/systemd/soul.service`](../../deploy/systemd/soul.service)): Soul применяет Destiny (ставит пакеты, правит файлы, управляет сервисами), поэтому запись в систему НЕ запрещена. **Не ужесточать** `ProtectSystem=strict` без проверки apply-цикла — сломает core-модули.

### Soul-host в push-режиме (без агента)

- SSH-доступ от Keeper-хоста.
- Linux + base utilities (bash, coreutils, систем-pkg-mgr).
- Каталог `/var/lib/soul-stack/{bin,modules}/` — Keeper кеширует там бинарь `soul` и модули по SHA-256, повторный прогон не докачивает (см. [`docs/keeper/push.md`](../keeper/push.md)).
- Кеш-каталог чистится опциональным шагом в той же SSH-сессии (см. [`docs/keeper/push.md`](../keeper/push.md)), не Reaper-ом.

## Сетевые порты

### Keeper (default listen-адреса из [config.md](../keeper/config.md#listen))

| Порт | Назначение | Listener | TLS |
|---|---|---|---|
| `9442` | Bootstrap-RPC (онбординг Soul-ов) | `listen.grpc.bootstrap.addr` | server-only TLS |
| `8443` | EventStream Keeper↔Soul (долгоживущий bidi) | `listen.grpc.event_stream.addr` | mTLS (валидация SoulSeed-сертификатов по `tls.ca`) |
| `8080` | OpenAPI Operator API | `listen.openapi.addr` | поверх HTTPS (терминируется LB или прямо Keeper-ом, зависит от инсталляции) |
| `8081` | MCP-сервер | `listen.mcp.addr` | то же |
| `9090` | Prometheus `/metrics` | `listen.metrics.addr` | без TLS; защита — Basic-auth (`metrics.auth.basic`) опционально |

В прод-инсталляции — обычно за L4-LB / VIP (см. [`scaling.md`](scaling.md)). Bootstrap-listener (`9442`) и EventStream (`8443`) **обязательно** проброшены до Soul-флота с правильным TLS-материалом.

### Soul

| Порт | Назначение | Listener |
|---|---|---|
| `9091` (default) | Soul-side `/metrics` | `metrics.listen` — **по умолчанию loopback `127.0.0.1`** ([config.md → metrics](../soul/config.md#metrics)). Защита — Basic-auth опционально, источник пароля — `password_file` (у Soul нет vault-клиента). |

Soul **никогда не слушает входящие соединения от Keeper-а** — все коммуникации инициирует Soul через EventStream к Keeper-у ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). Никаких inbound-портов на управляемых хостах открывать не нужно.

## Раскладка файловой системы

### Keeper-хост

```
/etc/keeper/
  keeper.yml                      # рабочий конфиг (mode 0640, owner soul-stack)
  keeper.env                      # KEEPER_CONFIG=/etc/keeper/keeper.yml (EnvironmentFile)
  vault-secret-id                 # ТОЛЬКО при vault.auth.method=approle, mode 0400
  tls/
    server.crt                    # серверный сертификат Keeper (Bootstrap + EventStream)
    server.key                    # приватник
    ca.crt                        # CA для валидации SoulSeed входящих Souls

/var/lib/keeper/                  # ReadWritePaths systemd-юнита
  state/                          # кеш / временные TLS-материалы (если используется)

/var/lib/soul-stack-keeper/
  plugins/                        # plugin cache_root (commit_sha-слоты резолвенных бинарей)
  plugin-src/                     # plugin work_root (git-клоны резолвера); СТРОГО вне plugins/

/var/run/soul-stack-keeper/
  plugins/                        # Unix-domain сокеты плагинов (mode 0700)

/var/log/keeper/
  keeper.log                      # при logging.file задан; ротация встроенная
  keeper-<timestamp>.log.gz       # архивы (rotation.max_files / max_age_days)
```

### Soul-хост

```
/etc/soul/
  soul.yml                        # рабочий конфиг
  soul.env                        # SOUL_CONFIG=/etc/soul/soul.yml
  bootstrap-token                 # одноразовый, ПОСЛЕ онбординга удаляется (см. soul/onboarding.md)
  seed/
    soul.crt                      # SoulSeed (выпускается через CSR; приватник не покидает хост)
    soul.key

/var/lib/soul-stack/              # ReadWritePaths
  bin/                            # кеш бинарей по SHA-256
  modules/                        # кеш модулей по SHA-256
```

## SystemD-юниты

Готовые юниты — в [`deploy/systemd/`](../../deploy/systemd/). Установка (одна за один к одному с инструкцией в шапке юнита):

```sh
# на Keeper-хосте
useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
install -m0755 dist/keeper /usr/local/bin/keeper           # или из deb/rpm
install -d -o soul-stack -g soul-stack /etc/keeper /var/lib/keeper
install -m0640 keeper.yml /etc/keeper/keeper.yml
install -m0644 deploy/systemd/keeper.service /etc/systemd/system/keeper.service
install -m0644 deploy/systemd/keeper.env     /etc/keeper/keeper.env
systemctl daemon-reload && systemctl enable --now keeper
```

Соответственно для Soul-хоста с `deploy/systemd/soul.service`.

Юниты вынесли путь к конфигу в `EnvironmentFile` (`/etc/keeper/keeper.env` → `KEEPER_CONFIG=…`) — оператор меняет файл, не правит юнит. `Restart=on-failure` + `StartLimit{IntervalSec=60s,Burst=5}` — перезапуск при падении, не зацикливаемся при битом конфиге.

### Hot-reload через SIGHUP

Изменения конфига применяются на лету через `systemctl reload keeper` ([ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). Per-блок политика — какие поля reload-able, какие требуют рестарта — в [`docs/keeper/config.md` → Hot-reload](../keeper/config.md#hot-reload).

При успешном reload — audit-event `config.reload_succeeded` (`source=signal`), при провале — `config.reload_failed` (старый снимок остаётся активным, ошибка в логах).

## Конфиг `keeper.yml` — обязательный минимум

Полный контракт — [`docs/keeper/config.md`](../keeper/config.md). Минимум для прод-инсталляции:

```yaml
kid: keeper-prod-01                       # стабильный человекочитаемый ID инстанса

listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"
      tls: { cert: /etc/keeper/tls/server.crt, key: /etc/keeper/tls/server.key }
    event_stream:
      addr: "0.0.0.0:8443"
      tls: { cert: /etc/keeper/tls/server.crt, key: /etc/keeper/tls/server.key, ca: /etc/keeper/tls/ca.crt }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }

postgres:
  dsn_ref: vault:secret/keeper/postgres   # plaintext DSN запрещён, см. config.md
  pool: { min: 5, max: 50 }

redis:
  addr: "redis.internal:6379"
  password_ref: vault:secret/keeper/redis

vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle                       # прод — НЕ token, см. docs/keeper/prod-setup.md
    role_id: keeper-prod
    secret_id_file: /etc/keeper/vault-secret-id
  pki_mount: "pki/soulstack"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    ttl_default: 24h
    ttl_bootstrap: 720h

otel:
  enabled: true
  exporter: otlp
  endpoint: "otel-collector.internal:4317"

logging:
  level: info
  format: json
  file: /var/log/keeper/keeper.log
  rotation: { max_size_mb: 100, max_age_days: 7, max_files: 10, compress: true }

reaper:
  enabled: true
  interval: 1h
  rules:
    expire_pending_seeds: { enabled: true, max_age: 24h, action: delete }
    purge_used_tokens:    { enabled: true, max_age: 90d, action: delete }
    purge_souls:          { enabled: true, statuses: [disconnected, expired], max_age: 30d, action: delete }
    purge_old_seeds:      { enabled: true, statuses: [superseded, expired, revoked], max_age: 90d, action: delete }
    mark_disconnected:    { enabled: true, stale_after: 90s, action: set_status, target_status: disconnected }
    purge_audit_old:      { enabled: true, max_age: 365d, action: delete }
    purge_apply_runs:     { enabled: true, max_age: 30d, action: delete }
    purge_apply_task_register: { enabled: true, max_age: 1h, action: delete }
    archive_state_history: { enabled: true, keep_last_n: 50, keep_version_bump_snapshots: true, action: soft_delete }
    # reclaim_apply_runs — ВЫКЛЮЧЕНО, включается только после раскатки fencing-Soul + acolytes>0,
    # см. docs/keeper/reaper.md → Включение recovery
    # reap_orphan_vault_keys — выключено, report-only; включать только при настроенном Vault list-policy
```

Полный эталонный пример — [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml). Vault AppRole + persistent storage + auto-unseal + JWT signing-key rotation подробно — [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md).

## Конфиг `soul.yml` — обязательный минимум

Полный контракт — [`docs/soul/config.md`](../soul/config.md). Минимум:

```yaml
sid: host-01.example.com                  # SID = FQDN, по умолчанию резолвится через hostname -f

keeper:
  endpoints:
    - { addr: "keeper-1.internal:8443", priority: 1 }
    - { addr: "keeper-2.internal:8443", priority: 1 }   # внутри priority — shuffle
    - { addr: "keeper-3.internal:8443", priority: 2 }   # резерв (другой DC)
  failback:
    interval: 1h
    spray: 5m

tls:
  cert: /etc/soul/seed/soul.crt
  key:  /etc/soul/seed/soul.key
  ca:   /etc/soul/seed/ca.crt              # CA Keeper-а

bootstrap_token_file: /etc/soul/bootstrap-token  # удаляется после онбординга

metrics:
  listen: "127.0.0.1:9091"

otel:
  enabled: true
  endpoint: "otel-collector.internal:4317"

logging:
  level: info
  format: json
  file: /var/log/soul/soul.log
  rotation: { max_size_mb: 50, max_age_days: 7, max_files: 5 }
```

Алгоритм подключения (priority + failback + shuffle) — [`docs/soul/connection.md`](../soul/connection.md).

## Multi-keeper HA — топология

Несколько Keeper-инстансов с разным `kid` поверх общей Postgres + Redis ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). Stateless — любой инстанс обслуживает любой запрос.

```
                    ┌────────────── L4-LB / VIP ──────────────┐
                    │                                          │
                    ▼                  ▼                  ▼
              ┌─────────┐        ┌─────────┐        ┌─────────┐
              │ keeper  │        │ keeper  │   …    │ keeper  │
              │  K1     │        │  K2     │        │  KN     │
              │ acolytes:N        │ acolytes:N      │ acolytes:N
              └────┬────┘        └────┬────┘        └────┬────┘
                   │                  │                  │
                   └──────────────────┼──────────────────┘
                                      │
                            ┌─────────┴─────────┐
                            ▼                   ▼
                      ┌──────────┐        ┌──────────┐
                      │  Redis   │        │ Postgres │
                      │ (cluster │        │  HA      │
                      │ / sentinel)       │ (Patroni)│
                      └──────────┘        └──────────┘
```

Раскатка multi-keeper подробно — [`scaling.md`](scaling.md). Главные инварианты:

- **Каждый инстанс — свой `kid`** (kebab-case, уникален в кластере; конфликт → `ErrConclaveKIDTaken`, см. [Conclave](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)).
- **`acolytes > 0` ОБЯЗАТЕЛЬНО** при N > 1 живых Keeper-инстансов ([ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Refuse-guard отказывается стартовать при нарушении.
- **JWT signing-key, mTLS-материал, Vault-конфигурация — одинаковы на всех инстансах** (signing-key из общего Vault KV, mTLS — один issuing CA).
- **`tls.cert` / `tls.key` могут отличаться** между инстансами, если за разными VIP-ами стоят разные SAN-ы, но обычно один wildcard-сертификат на весь кластер.

## L4 балансировщик

EventStream — долгоживущий gRPC bidi-стрим (часы / дни). Поэтому:

- **L4-балансировщик (TCP)**, не L7. gRPC через L7-proxy (envoy / haproxy в HTTP-mode) терпит, но не даёт ничего сверх — для bidi нужен прозрачный TCP.
- **least-connections** распределяет нагрузку EventStream равномерно при scale-out.
- **Sticky-session НЕ нужен** — присоединение Soul-а через SID-lease в Redis ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)) уже даёт «один Soul → один инстанс» на стороне Soul-Stack, инвариант не зависит от LB.
- **Health-check** — `/readyz` Keeper-а (зависит от listener-а `openapi.addr`; для L4-LB достаточно TCP-probe порта `8443`).
- **Drain при scale-down** — graceful shutdown Keeper-а отдаст SIGTERM (`shutdown_grace`); Conclave-presence снимается → Watchman-cascade на других инстансах НЕ срабатывает (Watchman реагирует только на изоляцию). Soul-ы получают EOF на текущем EventStream → failback на следующий endpoint из priority-листа.

OpenAPI / MCP (`8080` / `8081`) можно класть за L7-proxy (TLS termination + HTTP routing). Bootstrap-RPC (`9442`) — единичный unary, можно и за L4, и за L7.

## Раскатка по шагам — single-keeper (минимум для smoke)

1. **Инфра:** поднять Postgres + Redis + Vault (по [`infra.md`](infra.md)).
2. **Vault provision:** записать `secret/keeper/postgres` (поле `dsn`), `secret/keeper/jwt-signing-key` (поле `signing_key`), `secret/keeper/redis` (поле `password`). Создать AppRole `keeper-prod` + policy ([`docs/keeper/prod-setup.md`](../keeper/prod-setup.md)).
3. **TLS:** выпустить серверный сертификат Keeper и CA для SoulSeed.
4. **Keeper-хост:** установить deb/rpm, создать `/etc/keeper/keeper.yml` (минимум выше), запустить через systemd.
5. **Bootstrap первого Архонта:** `keeper init --archon=archon-alice --config=/etc/keeper/keeper.yml --credential-out=/etc/keeper/archon-alice.jwt` ([`bootstrap-rbac.md`](bootstrap-rbac.md)).
6. **Smoke:** `curl -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" https://keeper-1.internal:8080/v1/operators` — должен вернуть `200` со списком Архонтов.

## Раскатка по шагам — multi-keeper HA

После single-keeper:

1. **Подготовить N-1 хостов** — те же требования, тот же deb/rpm, та же `keeper.yml` (с разным `kid:` на каждом).
2. **acolytes > 0** в `keeper.yml` всех инстансов (см. [`scaling.md`](scaling.md)).
3. **L4-LB** перед EventStream / Bootstrap-портами + L7-proxy перед OpenAPI / MCP. Health-check — TCP-probe `8443`.
4. **Soul-конфиги** — указать все Keeper-endpoint-ы (можно через LB VIP, можно прямые адреса с priority). Зависит от модели failover в инсталляции.
5. **Conclave-проверка:** `redis-cli KEYS 'keeper:instance:*'` показывает N ключей через 10s после старта последнего инстанса (TTL 30s, renew 10s).
6. **Reaper-leader unique:** `sum(keeper_reaper_lease_held) == 1` в Prometheus (см. [`monitoring.md`](monitoring.md)).

## См. также

- [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md) — Vault AppRole + persistent + auto-unseal + JWT signing-key rotation.
- [`docs/keeper/config.md`](../keeper/config.md) — полный нормативный контракт `keeper.yml`.
- [`docs/soul/config.md`](../soul/config.md) — `soul.yml`.
- [`deploy/README.md`](../../deploy/README.md) — Docker / systemd / nfpm.
- [`scaling.md`](scaling.md) — multi-keeper / Acolyte / Conclave / Watchman.
- [`bootstrap-rbac.md`](bootstrap-rbac.md) — `keeper init`, второй+ Архонт, RBAC.
