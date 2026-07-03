# Cloud-интеграция (`keeper.cloud`)

Модуль внутри `keeper`-бинаря, отвечающий за cloud-операции (создание / удаление / опрос VM). Динамическое создание VM реализовано как **шаг сценария с `on: keeper`** через CloudDriver-плагин ([plugins.md](plugins.md)). Service не знает специфики облаков — он знает «нужен шаг создания VM с параметрами»; Keeper выбирает driver и исполняет.

## Provider и Profile в Postgres

**Provider** — настроенная учётка облака (AWS-аккаунт, GCP-проект, OpenStack-tenant). Хранится в Postgres ([storage.md](storage.md)), управляется через OpenAPI / MCP. CRUD-поверхность **реализована**:

| Метод + путь | Permission | MCP-tool | Назначение |
|---|---|---|---|
| `POST /v1/providers` | `provider.create` | `keeper.provider.create` | Создать Provider; `409 provider-already-exists` на дубль `name`. |
| `GET /v1/providers` | `provider.read` | `keeper.provider.list` | Перечислить (paged `offset`/`limit`). |
| `GET /v1/providers/{name}` | `provider.read` | `keeper.provider.get` | Прочитать один; `404 not-found`. |
| `DELETE /v1/providers/{name}` | `provider.delete` | `keeper.provider.delete` | Удалить; `404 not-found`; `409 provider-has-profiles` при привязанных Profile-ях (FK RESTRICT, миграция 020). |
| `POST /v1/profiles` | `profile.create` | `keeper.profile.create` | Создать Profile; `409 profile-already-exists` на дубль `name`; `422 validation-failed` на ссылку на несуществующий Provider (FK). |
| `GET /v1/profiles` | `profile.read` | `keeper.profile.list` | Перечислить (опц. фильтр `provider=`). |
| `GET /v1/profiles/{name}` | `profile.read` | `keeper.profile.get` | Прочитать один; `404 not-found`. |
| `DELETE /v1/profiles/{name}` | `profile.delete` | `keeper.profile.delete` | Удалить; `404 not-found`. |

**Иммутабельность.** `update`-операции **нет**: Provider/Profile неизменяемы, смена параметров = `delete` + `create`. Это защита от частичной мутации `spec` уже-живущих VM (нельзя на лету подменить регион/credentials под работающим флотом). Поэтому в каталоге [rbac.md](rbac.md#cloud-6--cloudmd) — только `create`/`read`/`delete` (без `update`), а MCP-tools — `create`/`list`/`get`/`delete`.

**`credentials_ref` — только vault-путь.** Поле принимает строку `vault:<mount>/<path>`; сами credentials API **НЕ резолвит и НЕ возвращает** — отдаёт `credentials_ref` как путь (секрет-гигиена, симметрия с jwt-signing-key-ref). Резолв vault-секрета происходит на scenario-слое при вызове `core.cloud.provisioned` (см. [Credentials-flow](#credentials-flow)), не в CRUD.

`provider.created` / `provider.deleted` и `profile.created` / `profile.deleted` пишутся в audit-журнал ([rbac.md](rbac.md)); read-роуты audit не пишут. Форма тел Provider/Profile описана ниже.

```yaml
keeper.provider.create
  name=aws-prod
  type=aws
  region=eu-west-1
  credentials_ref=vault:secret/cloud/aws-prod
```

**Profile** — шаблон VM, многоразовый. Тоже в Postgres:

```yaml
keeper.profile.create
  name=redis-medium-eu
  provider=aws-prod
  params={
    image:         ami-0abc123,
    instance_type: t3.medium,
    disk:          { size_gb: 50, type: gp3 },
    network:       { subnet: subnet-xyz, security_groups: [sg-redis] }
  }
  cloud_init=...
```

Параметры профиля валидируются против `profile_schema`, который driver публикует через RPC `Schema()` ([plugins.md](plugins.md)).

**Default essence в git как подложка.** Дефолтные параметры сервиса лежат в `essence/` репо сервиса (см. [architecture.md → Essence: pipeline сборки](../architecture.md#essence-pipeline-сборки)); оператор переопределяет их в `incarnation.spec` через API. Сами Provider/Profile — runtime-state, и потому в Postgres, а не в git ([architecture.md → Артефакты Soul Stack](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд)).

## Cloud-create как шаг сценария

В сценарии сервиса cloud-create — обычный шаг с `on: keeper`, использующий keeper-side core-модуль `core.cloud.provisioned` ([ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read), [keeper/modules.md](modules.md)). Прежний паттерн «core-destiny `cloud-provision`» **отменён ADR-017**: это не пакет задач для Soul, а keeper-side операция реестра, поэтому она — core-модуль с диспетчером `on: keeper`, а не destiny.

```yaml
- name: provision
  on: keeper                             # шаг исполняется на keeper-е
  module: core.cloud.created             # base core.cloud + state created (state — последний сегмент адреса)
  params:
    provider: "${ input.spawn.provider }"  # ИМЯ Provider-а из реестра providers
    profile:  "${ input.spawn.profile }"
    count:    "${ input.spawn.count }"
    userdata: "${ input.spawn.cloud_init }" # опц. cloud-init blob для bootstrap soul
```

> **★ State — последний сегмент `module:`-адреса**, не отдельный ключ. Пишется
> `module: core.cloud.created` / `core.cloud.destroyed`; формы `module:
> core.cloud.provisioned` или отдельного `state: created`-ключа **не существует**
> (реестр делит адрес `core.cloud.created` на base `core.cloud` + state `created`,
> поля `state:` в задаче нет).

Что делает `core.cloud` (state `created`):

1. **Резолвит Provider-реестр.** `params.provider` — это имя строки `providers` (не имя CloudDriver-плагина). Keeper читает строку, берёт `type` (= имя плагина `soul-cloud-<type>`), `region` и `credentials_ref`.
2. **Резолвит credentials.** По `credentials_ref` (`vault:<mount>/<path>`) Keeper читает секрет из Vault KV тем же keeper-side Vault-клиентом, что `core.vault.kv-read`, и кладёт plain-секрет + `region` в `CreateRequest.credentials`. Драйвер в Vault **НЕ ходит** (см. [Credentials-flow](#credentials-flow) ниже).
3. **Дёргает `CloudDriver.Create`** через PluginHost (spawn one-shot, ADR-020): провайдер создаёт VM, стримит прогресс, ждёт готовности (running + IP/DNS) и возвращает `VmInfo` с заполненным `fqdn` (= SID).
4. Для каждой VM создаётся запись в `souls` со `status: pending` и выписывается bootstrap-токен под её FQDN (plain-токен попадает только в register-output — `register.<step>.hosts[i].bootstrap_token`, в БД — hash).
5. Cloud-init на VM (через `userdata`) ставит **только setup**: `soul`-бинарь, CA, `soul.yml`, systemd-unit — **без токена**. Токен доставляет отдельный keeper-side шаг `module: core.bootstrap.delivered` ([ADR-063](../adr/0063-bootstrap-token-delivery.md), [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered)); он же делает redeem (`soul init`) на VM. После init Soul поднимает EventStream и переходит в `connected`.

Шаги 4–5 — режим **B-flat (default)**. При `self_onboard: true` порядок другой: токены выписываются **ДО** create и запекаются в userdata — VM онбордится сама, шаг доставки не нужен (см. [Self-onboard «Вариант T»](#self-onboard-вариант-t)).

State `destroyed` симметричен: Keeper резолвит тот же Provider (для credentials) и зовёт `CloudDriver.Destroy(vm_ids, credentials)`, затем — cascade-транзакция реестров ([ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)).

### Credentials-flow

Вариант **A** (зафиксирован): **Keeper резолвит секрет из Vault и передаёт plain в драйвер; драйвер в Vault не ходит.**

- Секрет хранится в Vault, в реестре `providers` — только `credentials_ref` (`vault:<mount>/<path>`).
- На каждый `Create`/`Destroy` Keeper читает секрет (keeper-side Vault-клиент), складывает его поля + `region` в `CreateRequest.credentials` / `DestroyRequest.credentials` (`google.protobuf.Struct`, only-add поля в [`clouddriver.proto`](plugins.md)).
- `region` живёт **внутри** credentials/profile Struct, а не отдельным типизированным полем: он provider-specific (у Proxmox/OpenStack своего `region` нет).
- Секрет **маскируется на любом выходе** (audit / OTel / SSE / error-сообщения) тем же `MaskSecrets`, что чистит bootstrap-токен и vault-ref-ы. Драйверам поэтому **не нужна** capability `vault_access`.

> **Cloud-init bootstrap (B-flat, ADR-017(h) amendment 2026-05-27).** Реализованный MVP: per-VM bootstrap-токен выписывается ПОСЛЕ возврата `VmInfo` (когда SID известен) и кладётся в `register.<step>.hosts[i].bootstrap_token`. **Cloud-init userdata токены НЕ несёт** (cloud-provider API хранит userdata в plaintext metadata, доступной процессам VM) — содержит только: установку `soul`-бинаря (curl с pinned-CA), embedded PEM CA Keeper-а (`/etc/soul/tls/keeper-ca.pem`), минимальный `soul.yml` с `keeper.endpoints`, systemd-unit `soul.service`. **Доставка per-VM-токена на VM — отдельный keeper-side шаг сценария `module: core.bootstrap.delivered`** ([ADR-063](../adr/0063-bootstrap-token-delivery.md); прежняя заглушка `keeper.push.applied` отвергнута — такого keeper-side модуля не существовало, BUG#2): шаг читает `${ register.<step>.hosts }` (`sid`/`primary_ip`/`bootstrap_token`), по SSH кладёт токен на VM (STDIN, не argv) и там же делает redeem — `soul init`. См. [«Cloud-init bootstrap (MVP)»](#cloud-init-bootstrap-mvp) ниже и [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered).

> **Coven-привязка нового хоста — отдельный scenario-шаг.** `core.cloud.provisioned` создаёт VM и регистрирует их в `souls`, но **сам по себе coven-метки не назначает**: запись `souls → coven` — это отдельный шаг сценария через core-модуль `core.soul.registered` ([`docs/keeper/modules.md`](modules.md)). Разделение сознательное: cloud-create и привязка к coven — разные операции реестра, каждый со своими guard-rails, и они компонуются в сценарии независимо.

Дальше следующие шаги сценария идут уже по coven этого incarnation (`on: ["{{ incarnation.name }}"]`) и ставят destiny на свеже-созданные хосты.

## Безопасность destroy

Удаление VM — деструктивная операция, обязательны guard-rails:

- **Tombstone period.** При scale-down или `incarnation.destroy` VM не удаляется немедленно — помечается `marked_for_deletion`, ждёт `tombstone_ttl` (default 24h). Оператор может откатить.
- **Confirm flag.** Reconcile/destroy не делает physical destroy без явного `--allow-destroy` или соответствующего поля.
- **Storage protection.** EBS-volumes / диски не удаляются вместе с VM по умолчанию — отдельно подтверждаются.
- **Audit.** Каждое удаление пишется в журнал с указанием инициатора ([rbac.md](rbac.md)).

Это критично — иначе одна опечатка в `count` стирает прод.

## Cloud-init bootstrap (MVP)

Реализованный B-flat вариант (ADR-017(h) amendment 2026-05-27). Цель: новая VM, созданная `core.cloud.provisioned`, поднимает `soul`-агента и подключается к Keeper-кластеру.

Bootstrap-доставка токена — три режима:

1. **B-flat (default)** — userdata несёт только setup, **без токенов**; токен доставляет отдельный шаг `core.bootstrap.delivered` (token-only). Описан во [Flow](#flow) ниже.
2. **Full-install** — платформы без cloud-init userdata: `core.bootstrap.delivered` с `install: true` (`transport: teleport`) сам ставит весь setup по SSH и доставляет токен ([ADR-063](../adr/0063-bootstrap-token-delivery.md), [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered)).
3. **Self-onboard «Вариант T»** — `self_onboard: true` на `core.cloud.created`: per-VM токены запечены в userdata, VM онбордится **сама в один цикл cloud-init**, шага доставки нет ([ADR-017(h) amendment 2026-07-01](../adr/0017-keeper-side-core.md)). См. [подраздел ниже](#self-onboard-вариант-t).

### Flow

1. **Шаг сценария `module: core.cloud.created`** с параметром `generate_userdata: true`:
   - Keeper резолвит `keeper.yml::cloud_init.tls_ca_ref` в PEM CA через Vault (поле `ca`).
   - Keeper рендерит cloud-config YAML по embed template `keeper/internal/soulinstall/templates/cloud-init.tmpl`. Install-blueprint (пути/права/soul.yml/unit) вынесен в shared-пакет [`keeper/internal/soulinstall`](../../keeper/internal/soulinstall) — единый источник для userdata-пути **и** full-install по SSH ([ADR-063](../adr/0063-bootstrap-token-delivery.md) amendment 2026-06-30); `keeper/internal/cloudinit` остался config-резолвером (Vault) и тонкой обёрткой рендера.
   - Userdata уходит в `CreateRequest.userdata` (ADR-017(e) only-add), провайдер создаёт VM с этой userdata.
   - После Create — Keeper выписывает per-VM bootstrap-токен под FQDN VM, кладёт в `register.<step>.hosts[i].bootstrap_token` (plaintext, маскируется на всех выходах audit/SSE/OTel substring-фильтром `audit.MaskSecrets`).
2. **На VM работает cloud-init:**
   - Устанавливает CA: `/etc/soul/tls/keeper-ca.pem` (PEM, embedded в userdata).
   - Скачивает `soul`-бинарь: `curl --cacert /etc/soul/tls/keeper-ca.pem $SOUL_BINARY_URL` → `/usr/local/bin/soul` (pinned-CA, TOFU-mitigation). При `soul_binary_ca: system` curl идёт без `--cacert` (системный trust-store, для публичных CA artifact-хостов); см. поле в разделе «Конфиг».
   - Пишет минимальный `/etc/soul/soul.yml` с `keeper.endpoints[0] = {host, bootstrap_port, event_stream_port}` (`event_stream_port` — из `cloud_init.event_stream_port`; не задан → fallback на порт `bootstrap_endpoint`, single-port LB).
   - `systemctl daemon-reload + enable + start soul.service`. Без SoulSeed демон **сам не онбордится**: `soul run` завершается ошибкой «SoulSeed not found — run `soul init` first» и рестартится systemd-ом, пока токен не доставлен и не redeem-нут (шаг 3).
3. **Следующий шаг сценария — доставка токена: `module: core.bootstrap.delivered`** ([ADR-063](../adr/0063-bootstrap-token-delivery.md), спецификация — [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered)):
   - Читает `${ register.<step1>.hosts }` (`sid` / `primary_ip` / `bootstrap_token`) через CEL.
   - По SSH (direct-режим через SshProvider-плагин + CA-signed host-cert, либо `transport: teleport` by-name) кладёт токен в `token_path` (default `/etc/soul/token`); **токен идёт в STDIN, не в argv**.
   - Там же **redeem токена**: `test -e <seed-cert> || SOUL_BOOTSTRAP_TOKEN="$(cat <token_path>)" soul init --config /etc/soul/soul.yml` — идемпотентно (guard по seed-cert; токен single-use). При `start_soul: true` (default) — `systemctl daemon-reload && enable && start soul`.
4. **`soul init` → Bootstrap-RPC** (ADR-012(b)): CSR с токеном → подпись Vault PKI → mTLS-cert (SoulSeed). Дальше демон держит EventStream; онбординга набора VM дожидается барьер `await_online` шага `core.soul.registered` ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)).

### Конфиг

Блок `keeper.yml::cloud_init` (опциональный):

```yaml
cloud_init:
  bootstrap_endpoint: lb.keeper.example:9442      # LB host:port (Bootstrap-RPC listener)
  event_stream_port:  9443                         # опц.: TCP-порт EventStream (mTLS); 0/нет → порт bootstrap_endpoint
  tls_ca_ref:         vault:secret/keeper/ca      # PEM CA, поле `ca` в KV
  soul_binary_url:    https://artifacts.example/soul/v1.0.0/soul
  soul_binary_ca:     keeper                       # опц.: keeper (default) | system
  soul_version:       v1.0.0                       # опц. метка для диагностики
```

Тот же блок — единый источник install-параметров и для **full-install-режима** `core.bootstrap.delivered` (платформы без cloud-init userdata): имя `cloud_init` сохранено сознательно, второй блок под тем же содержимым был бы drift ([ADR-063](../adr/0063-bootstrap-token-delivery.md) amendment 2026-06-30).

Поля:

- `bootstrap_endpoint` — `host:port` LB (Bootstrap-RPC listener).
- `event_stream_port` — опц. TCP-порт EventStream-фазы (mTLS) того же host-а; попадает в `event_stream_port` генерённого `soul.yml`. `0`/опущено → back-compat fallback на порт `bootstrap_endpoint` (single-port LB). Без него на топологиях с раздельными портами `soul run` дозванивался бы EventStream-ом на Bootstrap-only listener («Unimplemented: method EventStream», 6-я стена [ADR-063](../adr/0063-bootstrap-token-delivery.md)).
- `tls_ca_ref` — vault-ref (`vault:<mount>/<path>`) на PEM-CA Keeper-а (поле `ca` в KV).
- `soul_binary_url` — HTTPS URL для скачивания `soul`-бинаря (plain http отвергается).
- `soul_binary_ca` — какой trust-store использует curl при скачивании бинаря:
  - `keeper` (default, пустое значение) — pin на keeper-CA (`curl --cacert keeper-ca.pem`); для self-hosted artifact-хоста с тем же CA, что у Keeper-а;
  - `system` — системный trust-store (`curl` без `--cacert`); для artifact-хостов с публичным CA (например, бинарь на Nexus за GlobalSign).
  - `soul_binary_ca: system` ослабляет **только** верификацию сертификата artifact-хоста при curl-скачивании бинаря. Bootstrap-канал (souls↔keeper mTLS) пинится на keeper-CA **всегда**, независимо от этого поля; `system` — это всё ещё system-CA-over-TLS, не plain-http.
- `soul_version` — опц. метка для диагностики.

Hot-reload работает: правка `keeper.yml` через `keeper-reload` → следующий cloud-create-шаг рендерит userdata с новым snapshot-ом (без рестарта Keeper-а).

При отсутствии блока — параметр `generate_userdata: true` валит шаг сценария с понятной ошибкой; явный `userdata: "<blob>"` продолжает работать (legacy / gold-image flow).

### Параметр scenario

```yaml
- name: provision
  on: keeper
  module: core.cloud.created           # base core.cloud + state created (НЕ core.cloud.provisioned; state — последний сегмент)
  params:
    provider:          aws-prod
    profile:           redis-medium-eu # ИМЯ записи реестра profiles (НЕ inline-object — ADR-017 amendment 2026-06-29)
    count:             3
    generate_userdata: true            # ← рендер из keeper.yml::cloud_init
  register: vm
```

`profile` — **имя** строки реестра `profiles` (`POST /v1/profiles` до прогона): Keeper резолвит имя в VM-spec params через Profile-реестр, симметрично `provider`→credentials. Inline-object в `params.profile` **не поддерживается** (рудимент раннего дизайна до появления Profile-реестра; [ADR-017](../adr/0017-keeper-side-core.md) amendment 2026-06-29).

`generate_userdata: true` и `userdata: "..."` — **mutually exclusive** (одновременное присутствие → fail). Без обоих провайдер получает пустой userdata. `self_onboard: true` тоже взаимоисключим с явным `userdata:` — keeper обязан сам запечь токены (см. ниже).

### Self-onboard «Вариант T»

Третий режим bootstrap-доставки ([ADR-017(h) amendment 2026-07-01](../adr/0017-keeper-side-core.md)): VM онбордится **сама в один цикл cloud-init**, без шага `core.bootstrap.delivered` и без claim-callback. Chicken-egg «SID известен только ПОСЛЕ create» снят предсказанием FQDN: keeper сам задаёт базовое имя VM-батча (param `name` → `CreateRequest.name`, драйвер именует VM `<name>-<index>`) и знает FQDN-суффикс провайдера (поле реестра `providers.fqdn_suffix`, миграция 094) — полный FQDN `<name>-<index>.<fqdn_suffix>` каждой VM известен ДО create.

```yaml
- name: provision
  on: keeper
  module: core.cloud.created
  params:
    provider:     dev-cloud       # у Provider-а должен быть задан fqdn_suffix
    profile:      redis-medium-eu
    count:        3
    name:         soul-e2e        # base-имя: FQDN = soul-e2e-<i>.<fqdn_suffix>
    self_onboard: true            # generate_userdata подразумевается
  register: vm
```

Как работает:

1. Keeper **ДО create** выписывает per-VM bootstrap-токены на предсказанные SID (записи `souls` со `status: pending`, в `bootstrap_tokens` — hash) и рендерит userdata с map FQDN→token: файл `/etc/soul/self-onboard-tokens` (`0600`, строки `<fqdn> <token>`).
2. Провайдер создаёт VM с этой userdata; keeper сверяет фактические FQDN с предсказанными — расхождение (драйвер не учёл `CreateRequest.name`) → fail-fast, иначе токен не совпал бы с hostname VM. Провал create/сверки **откатывает** вставленные souls/токены (orphan-cleanup — без него rerun упирался бы в PK-конфликт).
3. cloud-init на VM ставит обычный setup (CA, `soul.yml`, unit, `soul`-бинарь), затем **между установкой бинаря и стартом `soul.service`** выбирает свою строку токена по `$(hostname -f)` и делает `soul init` (токен уходит через env `SOUL_BOOTSTRAP_TOKEN`, не argv). После init `soul.service` поднимает уже онбордившийся демон; онбординга набора ждёт штатный барьер `await_online` (`core.soul.registered`).

Контракт params: `self_onboard: true` (bool, opt) **требует `name`**; **взаимоисключим с явным `userdata:`**; `generate_userdata` подразумевается — блок `keeper.yml::cloud_init` обязан быть сконфигурирован. **Plain-токен в register-output НЕ кладётся** — ключа `bootstrap_token` в `register.<step>.hosts[i]` в этом режиме нет (доставки нет); output несёт признак `self_onboard: true`.

> **★ Security — осознанное отступление от B-flat.** B-flat держит userdata «без токенов» (cloud-provider хранит userdata в plaintext-metadata, доступной процессам VM). Self-onboard кладёт токены в userdata сознательно: они **single-use** — redeem происходит немедленно, на первом же boot-цикле, повторное употребление невозможно; альтернатива — обязательная push-доставка, которой на части платформ нет. Режим — **opt-in per-шаг** (`self_onboard`); default остаётся B-flat.

Границы: провайдер без предсказуемого FQDN (пустой `fqdn_suffix`) — явная ошибка шага; на платформах с выключенным userdata (WB `ci_user_data`, [ADR-066](../adr/0066-teleport-onboarding-profile.md)) режим недоступен — там штатный путь full-install через Teleport ([ADR-063](../adr/0063-bootstrap-token-delivery.md)).

### Безопасность

- **Userdata НЕ несёт токены** (cloud-provider plaintext metadata) — в default-режиме B-flat. Исключение — opt-in [self-onboard «Вариант T»](#self-onboard-вариант-t): токены в userdata сознательно, single-use.
- **Pinned-CA для curl** (`soul_binary_ca: keeper`, default) — атакующий не может подменить `soul`-бинарь man-in-the-middle-ом (требует владения приватником CA Keeper-а). При `soul_binary_ca: system` верификация cert artifact-хоста идёт по системному trust-store (для публичных CA); ослабляется **только** этот шаг — Bootstrap-канал (souls↔keeper mTLS) пинится на keeper-CA всегда, plain-http по-прежнему отвергается.
- **TLS CA из Vault** — единый источник правды, ротация без правок keeper.yml.
- **Per-VM-токен доставляется отдельным шагом по SSH** (`core.bootstrap.delivered`: токен в STDIN, не в argv; audit-payload без токенов) — атакующая поверхность ограничена SSH-доступом, а не cloud metadata.

### Пример

См. [`examples/service/example-cloud-bootstrap/`](../../examples/service/example-cloud-bootstrap/) — полный scenario create со связкой `core.cloud.provisioned` + per-VM-token.

## Список cloud-провайдеров для MVP

AWS / GCP / Azure / Yandex Cloud / OpenStack / vSphere / Proxmox — что из них поставляется в первом релизе, а что — extension community: [open Q №13](../architecture.md#открытые-вопросы).

Reconcile-loop «declared count vs actual VM count» (фоновое выравнивание) — [open Q №17](../architecture.md#открытые-вопросы): закладываем сразу или MVP только manual `incarnation.upgrade/scale`.

## См. также

- [plugins.md](plugins.md) — контракт `CloudDriver`.
- [storage.md](storage.md) — где живут Provider, Profile, реестр VM.
- [rbac.md](rbac.md) — RBAC на cloud-операции.
- [config.md](config.md) → `plugins.cloud_drivers` — реестр драйверов.
- [architecture.md → Cloud-интеграция через `keeper.cloud`](../architecture.md#cloud-интеграция-через-keepercloud).
- [architecture.md → Targeting и связь хостов](../architecture.md#targeting-и-связь-хостов) — `on: keeper` vs `on: [coven, …]`.
- [naming-rules.md](../naming-rules.md) — `keeper.cloud`, `CloudDriver`, Provider, Profile.
