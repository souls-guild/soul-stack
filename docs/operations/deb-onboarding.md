# Онбординг кластера из deb-пакетов

Этот гайд — пошаговая инструкция «с нуля до connected Soul» для оператора, который ставит Soul Stack из наших deb-пакетов на свою инфраструктуру. Он покрывает установку, провижининг Vault, выпуск TLS-материала Keeper-а, заполнение конфигов, bootstrap первого Архонта и онбординг первого Soul.

Это **операционный туториал**, не reference-спека. Где нужна полная грамматика конфига или контракт RPC — даю ссылку на нормативный документ. Сам гайд держится на реальных файлах репозитория: пакеты — [`deploy/nfpm/`](../../deploy/nfpm/), юниты — [`deploy/systemd/`](../../deploy/systemd/), примеры конфигов — [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) / [`examples/soul/soul.yml`](../../examples/soul/soul.yml).

> **Рамка пакетов.** Наши deb-пакеты несут **только** бинари (`keeper` / `soul` / `soul-lint`), systemd-юниты и примеры конфигов. **Postgres, Redis и Vault мы НЕ пакуем** — это внешняя инфраструктура, которую оператор поднимает и эксплуатирует сам. Гайд предполагает, что они уже доступны. Прод-требования к ним — [infra.md](infra.md); отличия dev↔прод — [keeper/prod-setup.md](../keeper/prod-setup.md).

Связанные документы: общая дока развёртывания (топологии, LB, HA) — [deployment.md](deployment.md); прод-настройка Vault/AppRole/policy — [keeper/prod-setup.md](../keeper/prod-setup.md); RBAC и первый Архонт — [bootstrap-rbac.md](bootstrap-rbac.md); механизм bootstrap-токена и `soul init` — [soul/onboarding.md](../soul/onboarding.md).

## 1. Предпосылки

### Внешняя инфраструктура

Прежде чем ставить пакеты, у оператора должны быть доступны три внешних компонента. Минимальный обязательный контур — **Postgres + Redis**; Vault в проде нужен для PKI (SoulSeed) и хранения секретов.

| Компонент | Зачем | Чек-лист готовности |
|---|---|---|
| **Postgres** | Единственное холодное хранилище Keeper-кластера ([ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)): реестры `souls` / `operators`, Destiny-каталог, журналы | Доступен по сети с keeper-хостов; есть БД + роль с правами на DDL (миграции применяются идемпотентно при старте Keeper-а); снят DSN-строкой |
| **Redis** | Heartbeat-кэш, lease на SID, pub/sub между Keeper-инстансами, лидер Reaper ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)) | Доступен по сети; известны `addr` и пароль |
| **Vault** | PKI для выпуска SoulSeed (mTLS), хранение KV-секретов (DSN / Redis-пароль / JWT signing-key), persistent backend + auto-unseal | Unsealed, доступен по HTTPS; есть права завести PKI mount + AppRole (см. шаг 3) |

Прод-требования к каждому (версии, persistent backend, auto-unseal, least-privilege policy) — нормативно в [infra.md](infra.md) и [keeper/prod-setup.md → Vault](../keeper/prod-setup.md#vault-approle--persistent--auto-unseal). Здесь они уже подняты вашей командой эксплуатации.

### Порты и firewall

Keeper слушает несколько listener-ов на разных портах (значения из [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) — оператор может изменить):

| Порт | Listener | Протокол | Кто ходит |
|---|---|---|---|
| `9442` | `listen.grpc.bootstrap` | server-only TLS | Soul на фазе `soul init` (bootstrap-токен + CSR) |
| `8443` | `listen.grpc.event_stream` | mTLS | Soul на фазе `soul run` (долгоживущий EventStream-стрим) |
| `8080` | `listen.openapi` | HTTP | Операторы (Operator API), health-check (`/readyz`) |
| `8081` | `listen.mcp` | HTTP | MCP-клиенты |
| `9090` | `listen.metrics` | HTTP | Prometheus scrape (`/metrics`) |

На Soul-хостах наружу слушает только метрики-listener (`metrics.listen`, по умолчанию `127.0.0.1:9091` в [`examples/soul/soul.yml`](../../examples/soul/soul.yml) — локально, не наружу). Соединение к Keeper-у Soul инициирует сам ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)).

Firewall-правила:

- **На keeper-хостах** — открыть входящие `9442` и `8443` для подсети управляемых хостов; `8080` / `8081` — для операторской/MCP-сети; `9090` — для Prometheus.
- **С keeper-хостов наружу** — доступ к Postgres / Redis / Vault и (для git-резолва сервисов/плагинов) к git-хостингу.
- **С soul-хостов наружу** — доступ к keeper-ам на `9442` и `8443`.

Полная сетевая топология (LB, L4-probe `8443`, разнесение портов) — [deployment.md](deployment.md).

### FQDN и SID

`SID` управляемого хоста = его **FQDN** (Идентичность Soul, [soul/identity.md](../soul/identity.md)). `soul init` по умолчанию берёт SID из `os.Hostname()`, приводя к lower-case. Требования:

- FQDN хостов должны резолвиться (DNS / `/etc/hosts`) и матчить `^[a-z0-9][a-z0-9.-]{0,253}$` — иначе `soul init` упадёт с `invalid sid`.
- FQDN-ы keeper-ов, которые вы кладёте в `soul.yml::keeper.endpoints[].host`, должны входить в SAN серверного сертификата Keeper-а (см. шаг 4) — иначе TLS-верификация на bootstrap не пройдёт.

## 2. Установка пакетов

Три пакета (`make pkg` собирает deb + rpm в `dist/`):

| Пакет | Куда ставить | Что несёт |
|---|---|---|
| `soul-stack-keeper` | центральный узел (1+ инстанс) | `keeper` + systemd-юнит + env + пример конфига |
| `soul-stack-soul` | каждый управляемый хост | `soul` + systemd-юнит + env + пример конфига |
| `soul-stack-soul-lint` | рабочая станция оператора / CI | только `soul-lint` (CLI, без демона/конфига) |

### Keeper (на центральном узле)

```sh
sudo dpkg -i soul-stack-keeper_<version>_amd64.deb
```

Пакет ([`deploy/nfpm/keeper.yaml`](../../deploy/nfpm/keeper.yaml)) раскладывает:

| Путь | Что | Заметка |
|---|---|---|
| `/usr/local/bin/keeper` | бинарь, `0755` | — |
| `/etc/systemd/system/keeper.service` | systemd-юнит | `Type=exec`, `User=soul-stack`, hardening (`ProtectSystem=strict`, единственный writable `/var/lib/keeper`) |
| `/etc/keeper/keeper.env` | env-файл, `config|noreplace` | задаёт `KEEPER_CONFIG=/etc/keeper/keeper.yml`; upgrade его не перетирает |
| `/etc/keeper/keeper.yml.example` | пример конфига, `0640` | **рабочий конфиг создаёт оператор** копированием (шаг 5) |

> **Почему `.yml.example`, а не `.yml`.** Пакет намеренно кладёт пример отдельным именем, чтобы `dpkg upgrade` никогда не трогал ваш рабочий `/etc/keeper/keeper.yml`. Первичную копию делаете руками.

### Soul (на каждом управляемом хосте)

```sh
sudo dpkg -i soul-stack-soul_<version>_amd64.deb
```

Пакет ([`deploy/nfpm/soul.yaml`](../../deploy/nfpm/soul.yaml)) раскладывает симметрично: `/usr/local/bin/soul`, `/etc/systemd/system/soul.service`, `/etc/soul/soul.env` (`SOUL_CONFIG=/etc/soul/soul.yml`), `/etc/soul/soul.yml.example`.

> **Hardening soul-юнита мягче keeper-а.** Soul применяет Destiny (ставит пакеты, правит файлы, управляет сервисами) — это требует реальных привилегий на хосте, поэтому жёсткие `ProtectSystem=strict` / `MemoryDenyWriteExecute` для него **не** ставятся ([`deploy/systemd/soul.service`](../../deploy/systemd/soul.service), комментарий в шапке). Единственный writable-путь — `/var/lib/soul-stack` (кеш модулей по SHA-256 + SoulSeed).

### Системный пользователь и каталоги

Оба юнита работают под системным пользователем `soul-stack`. Пакет тянет `adduser` (deb) как зависимость, но **создание пользователя и каталогов состояния — за оператором** (юниты ожидают их готовыми). На каждом хосте один раз:

**На keeper-хосте:**

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
sudo install -d -o soul-stack -g soul-stack /etc/keeper /var/lib/keeper
```

**На soul-хосте:**

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
sudo install -d -o soul-stack -g soul-stack /etc/soul /var/lib/soul-stack
```

(Конкретные команды — в шапках [`deploy/systemd/keeper.service`](../../deploy/systemd/keeper.service) и [`deploy/systemd/soul.service`](../../deploy/systemd/soul.service).)

`soul-lint` — просто CLI без демона, ставится одним `dpkg -i`, никакой настройки не требует.

## 3. Провижининг Vault

Эти шаги оператор выполняет **на своём проде-Vault** (под токеном/политикой с правами на mount-ы). Команды переложены с реального dev-провижининга [`dev/provision.sh`](../../dev/provision.sh) на прод — в dev там dev-mode Vault и root-токен, в проде те же операции делаются под админ-доступом к Vault с persistent backend и auto-unseal.

> **dev↔прод.** В dev `provision.sh` пишет секреты под `root`-токеном в dev-mode (секреты в RAM). В проде: persistent backend, auto-unseal, узкая least-privilege policy для самого Keeper-а ([`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl)). Провижининг ниже — разовая admin-операция, отдельная от рантайм-доступа Keeper-а.

### 3.1. KV-секреты

Keeper резолвит DSN Postgres, пароль Redis и JWT signing-key через `vault:`-ref-ы из конфига. Записать их в KV (mount `secret/`):

```sh
# DSN внешнего Postgres оператора (поле `dsn`)
vault kv put secret/keeper/postgres \
  dsn="postgres://keeper:<password>@pg.internal:5432/keeper?sslmode=require"

# Пароль внешнего Redis оператора (поле, на которое указывает redis.password_ref)
vault kv put secret/keeper/redis \
  password="<redis-password>"

# JWT signing-key операторов (поле `signing_key`) — 32 байта рандома, base64.
# Сгенерировать ОДИН раз и зафиксировать: смена ключа инвалидирует все живые JWT.
vault kv put secret/keeper/jwt-signing-key \
  signing_key="$(openssl rand -base64 32)"
```

> **JWT signing-key не перегенерировать.** Если ключ уже записан — не трогать. Любая смена инвалидирует все ранее выпущенные операторские JWT (подпись не сойдётся). Ротация — отдельная плановая процедура ([keeper/prod-setup.md → Ротация signing-key](../keeper/prod-setup.md#jwt-signing-key-прод)).

### 3.2. PKI: mount + root + роль `soul-seed`

PKI выпускает SoulSeed-сертификаты (mTLS-пары Soul ↔ Keeper). В [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) mount — `pki/soulstack`; ниже для примера используем `pki/` (как в dev), под свой mount поправьте пути.

```sh
# 1. Включить PKI-engine и поднять max-lease-ttl
vault secrets enable -path=pki pki
vault secrets tune -max-lease-ttl=87600h pki

# 2. Сгенерировать root-сертификат (CN — общий якорь доверия для всех SoulSeed
#    и серверного cert-а Keeper-а; см. шаг 4)
vault write pki/root/generate/internal \
  common_name="soul-stack" ttl=87600h

# 3. Роль soul-seed: домены/SAN, разрешённые для выпускаемых сертификатов.
#    allowed_domains — под FQDN-схему ваших хостов (НЕ example.com/test из dev).
vault write pki/roles/soul-seed \
  allowed_domains="internal,<your-domain>" \
  allow_subdomains=true \
  max_ttl=720h
```

### 3.3. AppRole для рантайм-доступа Keeper-а

В проде Keeper аутентифицируется в Vault через AppRole (не root-токен). Создать роль, привязав least-privilege policy:

```sh
# Узкая policy (шаблон с покомментированными путями — examples/keeper/vault-policy.hcl)
vault policy write keeper-prod examples/keeper/vault-policy.hcl

# Роль с привязкой policy
vault write auth/approle/role/keeper-prod \
  token_policies=keeper-prod \
  secret_id_ttl=720h token_ttl=1h token_max_ttl=24h

# role_id — НЕ секрет, пойдёт в keeper.yml::vault.auth.role_id
vault read auth/approle/role/keeper-prod/role-id

# secret_id — СЕКРЕТ, положить в файл mode 0400 (шаг 5)
vault write -f auth/approle/role/keeper-prod/secret-id
```

`role_id` — идентификатор роли, не секрет (хранится открыто в `keeper.yml`). `secret_id` — секрет, в конфиге plaintext-ом **не хранится**, источник — локальный файл `secret_id_file` (`mode 0400`) или env `secret_id_env`. AppRole-credentials намеренно НЕ читаются из Vault (chicken-egg: именно ими Keeper логинится). Контракт — [keeper/prod-setup.md → AppRole](../keeper/prod-setup.md#approle-аутентификация-keeper-а).

## 4. TLS-материал Keeper-а

Это **самое аккуратное место онбординга** — описано явно, потому что здесь сходятся две независимые цепочки доверия.

### Что нужно положить

Keeper слушает оба gRPC-listener-а (bootstrap `9442` и event_stream `8443`) с серверным сертификатом. По [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) файлы лежат в `/etc/keeper/tls/`:

| Файл | Роль |
|---|---|
| `/etc/keeper/tls/server.crt` | серверный leaf-cert Keeper-а (предъявляется на bootstrap + event_stream) |
| `/etc/keeper/tls/server.key` | приватный ключ leaf-а |
| `/etc/keeper/tls/ca.crt` | CA для валидации **клиентских** SoulSeed-сертификатов на mTLS event_stream (`event_stream.tls.ca` = ClientCAs) |

### Почему один PKI-корень для всего

Серверный cert Keeper-а **обязан цепляться к тому же PKI-корню**, что и SoulSeed-сертификаты. Иначе на EventStream (mTLS) Soul не доверяет серверному cert-у Keeper-а, а Keeper не доверяет клиентскому cert-у Soul-а. Поэтому серверный leaf Keeper-а выпускается из той же роли `pki/issue/soul-seed`, что и SoulSeed (см. комментарий в [`dev/provision.sh`](../../dev/provision.sh), функция `issue_keeper_cert`).

### Процедура выпуска

Выпустить leaf из своего Vault PKI и разложить по файлам (по аналогии с `issue_keeper_cert` в [`dev/provision.sh`](../../dev/provision.sh), переложено на прод-FQDN keeper-а; `keeper.internal` — пример, подставьте реальный FQDN, под которым Soul-ы будут адресовать этот Keeper в `soul.yml::keeper.endpoints[].host`):

```sh
vault write -format=json pki/issue/soul-seed \
  common_name="keeper.internal" \
  alt_names="keeper.internal" \
  ip_sans="10.0.0.10" \
  ttl=720h > /tmp/keeper-issue.json
```

Из JSON-ответа разложить три поля в файлы (`certificate` → `server.crt`, `private_key` → `server.key`, `issuing_ca` → `ca.crt`) и выставить права:

```sh
sudo install -d -o soul-stack -g soul-stack -m 0750 /etc/keeper/tls
# certificate → server.crt, private_key → server.key, issuing_ca → ca.crt
sudo install -o soul-stack -g soul-stack -m 0640 server.crt /etc/keeper/tls/server.crt
sudo install -o soul-stack -g soul-stack -m 0600 server.key /etc/keeper/tls/server.key
sudo install -o soul-stack -g soul-stack -m 0640 ca.crt    /etc/keeper/tls/ca.crt
```

> **SAN обязателен.** Серверный cert должен содержать в SAN тот FQDN (или IP), который Soul-ы кладут в `keeper.endpoints[].host` — Soul проверяет hostname серверного cert-а на bootstrap-фазе. Несоответствие → `certificate validation failed` (см. шаг 9).

> **Ротация.** Leaf истекает по `ttl` (выше — 720h). Перевыпуск — повтор этой процедуры + рестарт keeper-а; CA-корень при этом не меняется, поэтому уже онбордженные Soul-ы не затрагиваются. Политика ротации — на стороне команды эксплуатации PKI.

## 5. Конфиг `keeper.yml`

Скопировать пример в рабочий путь и заполнить:

```sh
sudo cp /etc/keeper/keeper.yml.example /etc/keeper/keeper.yml
sudo chown soul-stack:soul-stack /etc/keeper/keeper.yml
sudo chmod 0640 /etc/keeper/keeper.yml
```

Обязательные к проверке/правке блоки (полный пример — [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml), нормативный контракт — [keeper/config.md](../keeper/config.md)):

```yaml
# Идентичность инстанса — уникальна в кластере (несколько keeper-ов = разные kid)
kid: keeper-eu-west-01

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

# Внешние хранилища оператора — через vault:-ref (значения положены на шаге 3.1)
postgres:
  dsn_ref: vault:secret/keeper/postgres
redis:
  addr: "redis.internal:6379"
  password_ref: vault:secret/keeper/redis

# Vault — AppRole (шаг 3.3) + PKI-mount (шаг 3.2)
vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod                        # role_id из шага 3.3 (не секрет)
    secret_id_file: /etc/keeper/vault-secret-id # secret_id, файл mode 0400
  pki_mount: "pki/soulstack"                    # ваш PKI-mount (в dev — pki/)

# JWT операторов (signing-key положен на шаге 3.1)
auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
    ttl_default: 24h
    ttl_bootstrap: 30d
```

Положить `secret_id` (из шага 3.3) в файл, на который указывает `secret_id_file`:

```sh
# вывод `vault write -f auth/approle/role/keeper-prod/secret-id` → поле secret_id
echo -n "<secret_id>" | sudo tee /etc/keeper/vault-secret-id >/dev/null
sudo chown soul-stack:soul-stack /etc/keeper/vault-secret-id
sudo chmod 0400 /etc/keeper/vault-secret-id
```

> Реестр сервисов и RBAC-каталог в `keeper.yml` **не настраиваются** — они живут в Postgres и управляются через Operator API / MCP после старта ([ADR-029](../adr/0029-service-registry.md), [ADR-028](../adr/0028-rbac-storage.md)). Появление ключей `services:` / `rbac:` в конфиге отвергается как `unknown_key`.

Прежде чем стартовать, можно проверить путь к конфигу в env-файле `/etc/keeper/keeper.env` (`KEEPER_CONFIG=/etc/keeper/keeper.yml`) — юнит читает путь оттуда.

## 6. Запуск Keeper

Каталоги и пользователь уже созданы (шаг 2). Включить и запустить:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now keeper
```

Проверить:

```sh
systemctl status keeper
journalctl -u keeper -n 100 --no-pager
# health-check (listener openapi.addr): 200 при готовности к приёму трафика
curl -fsS http://127.0.0.1:8080/readyz && echo OK
```

`/readyz` зависит от Postgres + Redis (и пишется заново при рестарте) — 200 означает, что инстанс готов принимать трафик. Для L4-балансировщика достаточно TCP-probe порта `8443` ([deployment.md](deployment.md)).

> **Keeper откажется стартовать**, если реестр `operators` пуст и не передан `--initialize`: `operators registry is empty; run 'keeper init …'`. Это нормально на самом первом запуске — выполните шаг 7. Семантика рестарта (`--initialize` / `KEEPER_INITIALIZE`) — [bootstrap-rbac.md → Restart-семантика](bootstrap-rbac.md#restart-семантика).

## 7. Bootstrap первого Архонта

Реестр операторов в свежей БД пуст — все API вернут 403, пока не создан первый Архонт. Bootstrap — administrative subcommand самого `keeper`-бинаря (не отдельный режим). Запускается **один раз на одном инстансе** ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)):

```sh
sudo -u soul-stack keeper init \
  --archon=archon-alice \
  --config=/etc/keeper/keeper.yml \
  --credential-out=/etc/keeper/archon-alice.jwt
```

Что происходит ([bootstrap-rbac.md → `keeper init`](bootstrap-rbac.md#keeper-init--первый-архонт)): под PG advisory lock проверяется, что `operators` пуст; создаётся первый Архонт с ролью `cluster-admin` (`permissions: ["*"]`); выпускается JWT (TTL = `auth.jwt.ttl_bootstrap`, default 30 дней) и пишется в `--credential-out` с `mode 0400`.

> **AID-формат.** `--archon` — kebab-case `^archon-[a-z0-9-]{1,62}$` (`archon-alice`, `archon-ops-01`). См. [naming-rules.md](../naming-rules.md).

> **Хранение bootstrap-JWT.** Файл `--credential-out` — исходный материал для первой настройки, не долговременное хранилище. Немедленно перенести в password manager / Vault оператора и **не оставлять** в `/etc/keeper/` надолго: это admin-credential с правами `*`. Дальнейших Архонтов (для людей, CI, machine-identity) создавать через Operator API — [bootstrap-rbac.md → Выпуск дополнительных Архонтов](bootstrap-rbac.md).

После этого Keeper стартует штатно (если был запущен с `--initialize` в read-only — начнёт обслуживать API; иначе — `systemctl restart keeper`).

Сохранить токен в переменную для следующих шагов:

```sh
TOKEN=$(sudo cat /etc/keeper/archon-alice.jwt)
```

## 8. Онбординг Soul

Онбординг душ — двусторонний: оператор регистрирует хост в Keeper-е и получает одноразовый bootstrap-токен; на хосте `soul init` обменивает токен + CSR на SoulSeed (mTLS-пару). Полная механика — [soul/onboarding.md](../soul/onboarding.md).

### 8.1. Регистрация хоста и выпуск токена

На стороне Keeper-а (через Operator API; SID = FQDN будущего Soul-хоста):

```sh
curl -s -X POST http://keeper.internal:8080/v1/souls \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sid": "host01.dc1.internal", "covens": ["demo"], "transport": "agent"}'
```

> **`transport` обязателен.** Поле `transport` должно быть `agent` (демон-онбординг, pull-режим) или `ssh` (push-режим по SSH). При отсутствии вернётся 422 Unprocessable Entity. В примере выше — `agent` (Soul-демон инициирует соединение к Keeper-у).

Запись в `souls` появляется в статусе `pending`; в ответе **один раз** возвращается plain bootstrap-токен (одноразовый, TTL по умолчанию 24h). Потеряли — не восстановить, только перевыпустить через `POST /v1/souls/{sid}/issue-token` ([onboarding.md → Восстановление](../soul/onboarding.md#восстановление-потерян-токен)).

### 8.2. Trust-материал на хосте: как Soul доверяет Keeper-у до `soul init`

Перед `soul init` Soul ещё не имеет SoulSeed, но bootstrap-фаза (`9442`) идёт по **server-only TLS** — Soul обязан проверить серверный cert Keeper-а. Доверие здесь устанавливается **не TOFU**, а явной предзагрузкой CA: Soul верифицирует серверный cert Keeper-а против CA-файла из `soul.yml::keeper.tls.ca` (в коде — `bootstrap.Run` → `tlsx.LoadClientTLS{CAPath: keeper.tls.ca}`, [`soul/internal/bootstrap/bootstrap.go`](../../soul/internal/bootstrap/bootstrap.go)). Если файл пуст/не указан — `soul init` падает с `keeper.tls.ca is empty`.

Поэтому **оператор обязан заранее положить на хост PKI-корень** (тот же `ca.crt` / `issuing_ca`, что и серверный cert Keeper-а, шаг 4) по пути из `soul.yml::keeper.tls.ca`:

```sh
sudo install -d -o soul-stack -g soul-stack -m 0750 /var/lib/soul-stack/seed
# CA из шага 4 (issuing_ca PKI-корня) — например, скопировать ca.crt с keeper-хоста
sudo install -o soul-stack -g soul-stack -m 0644 ca.crt /var/lib/soul-stack/seed/ca.crt
```

> **Две цепочки доверия — две роли одного CA.** `keeper.tls.ca` (предзагруженный файл) валидирует серверный cert Keeper-а на **bootstrap**-фазе. После успешного `soul init` Keeper возвращает PKI-цепочку (`BootstrapReply.ca_chain_pem`), которую Soul сохраняет в SoulSeed-каталог (`paths.seed/current/ca.pem`) и использует для верификации сервера на **EventStream**-фазе. Это разные файлы, оба от одного PKI-корня. Предзагруженный `ca.crt` нужен только до первого `soul init`; дальше Soul опирается на seed-CA.

### 8.3. Конфиг `soul.yml` и доставка токена

Скопировать пример и заполнить:

```sh
sudo cp /etc/soul/soul.yml.example /etc/soul/soul.yml
sudo chown soul-stack:soul-stack /etc/soul/soul.yml
sudo chmod 0640 /etc/soul/soul.yml
```

Минимум, что правится (полный контракт — [soul/config.md](../soul/config.md)):

```yaml
keeper:
  endpoints:
    - host: keeper.internal       # FQDN из SAN серверного cert-а (шаг 4)
      event_stream_port: 8443     # mTLS, фаза `soul run`
      bootstrap_port: 9442        # server-only TLS, фаза `soul init`
  tls:
    ca: /var/lib/soul-stack/seed/ca.crt   # предзагружен на шаге 8.2

paths:
  seed: /var/lib/soul-stack/seed          # сюда `soul init` положит SoulSeed
```

> `event_stream_port` и `bootstrap_port` **оба обязательны** и оба явные — молчаливого ухода bootstrap на event-stream-порт нет ([ADR-012(b)](../adr/0012-keeper-soul-grpc.md), [config.md → keeper.endpoints](../soul/config.md)). Несколько keeper-ов перечисляются как несколько записей `endpoints[]` с `priority`.

**Доставка токена.** Способ физической доставки bootstrap-токена на хост — выбор оператора ([onboarding.md → Способы доставки](../soul/onboarding.md#способы-доставки-токена)): `keeper.push`, Ansible-role, SSH/SCP, CI/CD, cloud-init. Рекомендация по безопасности: файл токена `mode 0400` owner `soul-stack`, директория `mode 0700`; на systemd ≥ 250 — `LoadCredential=` (токен в tmpfs, не на диск).

### 8.4. `soul init` — обмен токена на SoulSeed

`soul init` читает токен из stdin (приоритет) или env `SOUL_BOOTSTRAP_TOKEN` — флага с токеном нет намеренно, чтобы не светить в `ps`/history:

```sh
sudo -u soul-stack sh -c 'cat /run/soul-bootstrap-token | soul init --config=/etc/soul/soul.yml'
```

Команда: определяет SID (= FQDN), генерирует приватный ключ + CSR (ключ **никогда не покидает хост**), подключается к bootstrap-listener-у Keeper-а, предъявляет токен + CSR, получает подписанный SoulSeed и атомарно раскладывает его в `paths.seed`. Если SoulSeed уже есть — `init` падает (защита от случайного перевыпуска); перевыпуск — отдельная процедура ([identity.md → Ротация SoulSeed](../soul/identity.md#ротация-soulseed)).

### 8.5. Запуск демона и проверка

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now soul
systemctl status soul
journalctl -u soul -n 100 --no-pager
```

Soul инициирует EventStream-стрим к Keeper-у (mTLS, порт `8443`). Проверить, что хост перешёл в `connected`, со стороны Keeper-а:

```sh
curl -s http://keeper.internal:8080/v1/souls/host01.dc1.internal \
  -H "Authorization: Bearer $TOKEN"
# в ответе status: connected
```

После этого хост готов получать Destiny. Сборка первого сервиса end-to-end — [guides/first-service.md](../guides/first-service.md).

## 9. Troubleshooting

| Симптом | Вероятная причина | Что проверить |
|---|---|---|
| `soul init`: **connection refused** | Keeper не слушает bootstrap-порт / firewall режет `9442` | Keeper запущен (`systemctl status keeper`); открыт входящий `9442` с soul-хоста; `host`/`bootstrap_port` в `soul.yml` указывают на правильный keeper |
| `soul init`: **certificate validation failed** / x509 hostname mismatch | предзагруженный `keeper.tls.ca` не от того PKI-корня, что серверный cert; или FQDN keeper-а не в SAN серверного cert-а | `keeper.tls.ca` = `issuing_ca` PKI (шаг 8.2); FQDN из `endpoints[].host` входит в SAN серверного cert-а (шаг 4) |
| `soul init`: **keeper.tls.ca is empty** | не указан/не предзагружен CA-файл | заполнить `keeper.tls.ca` в `soul.yml` и положить файл (шаг 8.2) |
| `soul init`: **bootstrap token invalid / expired / used** (403) | токен сожжён (уже использован), истёк (TTL 24h) или SID не совпал | перевыпустить токен `POST /v1/souls/{sid}/issue-token` (`force` при активном); сверить SID = FQDN хоста |
| `soul init`: **invalid sid** | FQDN не матчит `^[a-z0-9][a-z0-9.-]{0,253}$` | привести hostname к валидному lower-case FQDN либо задать `sid:` явно в `soul.yml` |
| Keeper при старте: **Vault unreachable** / sealed | Vault недоступен по HTTPS, sealed, или неверные AppRole-credentials | Vault unsealed и доступен по `vault.addr`; `secret_id_file` существует с `mode 0400`; `role_id`/policy на месте (шаг 3.3) |
| Keeper: **operators registry is empty; refusing to start** | первый запуск без bootstrap-а | выполнить `keeper init` (шаг 7) |
| Keeper не стартует, **apply migrations** в логах | нет прав на DDL / Postgres недоступен | роль в DSN имеет права на DDL; Postgres доступен по `dsn_ref` |
| Soul стартует, но `status` остаётся `pending`/не `connected` | EventStream-фаза не проходит (mTLS на `8443`) | открыт входящий `8443`; SoulSeed разложен (`paths.seed/current/`); серверный cert и SoulSeed от одного PKI-корня (шаг 4) |

Расширенный набор инцидентов и метрик — [faq.md](faq.md) и [monitoring.md](monitoring.md).

## 10. Обновление

Пакеты обновляются обычным `dpkg -i` новой версии. Что важно знать:

- **Рабочие конфиги не перетираются.** `*.yml.example` приходит из пакета, но ваш `/etc/keeper/keeper.yml` и `/etc/soul/soul.yml` создавали вы сами — upgrade их не трогает. Env-файлы помечены `config|noreplace`. После апгрейда стоит сверить свой конфиг с новым `*.yml.example` на предмет новых ключей.
- **DDL-миграции схемы БД Keeper-а** применяются идемпотентно при старте Keeper-а (на `keeper run`, а также в `keeper init` перед bootstrap-ом — накат автоматический при рестарте демона новой версии, `migrate.Apply` в daemon.go:600). Перед upgrade Keeper-а — backup Postgres и проверка changelog на breaking-миграции.
- **state_schema-миграции инкарнаций** (ADR-019) — это **отдельная** оператор-инициированная операция через Operator API (`POST /v1/incarnations/{name}/upgrade`), forward-only, не запускается автоматически при рестарте Keeper-а. Не путать с миграциями схемы БД.
- **Hot-reload конфигурации.** Часть ключей `keeper.yml` перечитывается без рестарта; часть требует рестарта (TLS-файлы, listener-ы, подсистемы стартуют один раз). Карта «hot-reload / restart-required» по каждому ключу — [keeper/config.md](../keeper/config.md).

Полная процедура rolling-upgrade Keeper-кластера без downtime (drain LB, по одному инстансу, verify-метрики) и Soul-флота — нормативно в [upgrade.md](upgrade.md). Откат пакета: `dpkg -i` предыдущей версии + `systemctl restart`; учесть, что прошедшие state_schema-миграции назад не откатываются ([upgrade.md → Откат](upgrade.md#откат-state_schema)).
