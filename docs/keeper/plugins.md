# Plugin-инфраструктура Soul Stack

Нормативная спецификация формата `manifest.yaml`, handshake-строки, lifecycle плагина, версионирования, capabilities и side_effects. Источник правды по решениям — [ADR-020](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle). Этот документ — таблицы полей, JSON-схема handshake, диаграмма lifecycle, enum-таблицы, полные примеры manifest для всех трёх kind-ов плагинов.

Документ покрывает **все три kind-а плагинов** (manifest формат един, [ADR-020(e)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)):

| Kind | Host | Бинарь | Назначение |
|---|---|---|---|
| `soul_module` | `soul` (агент или push) | `soul-mod-<name>` | Реализует шаги Destiny: [`SoulModule`](#service-контракт-soulmodule). Также см. [`../soul/modules.md`](../soul/modules.md). |
| `cloud_driver` | `keeper` (модуль `keeper.cloud`) | `soul-cloud-<provider>` | Создание/удаление VM в облаке: [`CloudDriver`](#service-контракт-clouddriver). |
| `ssh_provider` | `keeper` (модуль `keeper.push`) | `soul-ssh-<provider>` | SSH-credentials для push-прогона: [`SshProvider`](#service-контракт-sshprovider). |

## Конвенции типов

Используется единый словарь типов, как в [`config.md`](config.md):

| Запись | Смысл |
|---|---|
| `string` | UTF-8 строка. |
| `int` | знаковое 64-битное целое. |
| `bool` | `true` / `false`. |
| `path` | абсолютный путь в локальной ФС host-а. |
| `enum{a,b,c}` | строка из явно перечисленного множества (lowercase ASCII, без пробелов). |
| `base64-pem` | base64-кодированный PEM-блок; пустая строка `""` = поле не используется. |
| `list<T>` / `map<K,V>` | как обычно. |
| `JSON Schema` | JSON Schema draft-2020-12, embedded YAML-объектом. |

`default: —` — обязательное поле. Опциональные поля помечены `optional`. Closed enum означает: расширение значений — через PR в `proto/plugin/vN/`, не через freeform.

## Manifest

Статический файл `manifest.yaml` в **корне репозитория плагина** и **рядом с бинарём** в кеше host-а ([ADR-020(a)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Парсится `soul-lint`-ом **без запуска бинаря** (требование [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).

Команда оператора: `soul-lint validate-manifest <path> [--json]`. Делает: parse YAML, проверка closed-enum `kind`/`required_capabilities`/`side_effects`, regex namespace/name/state-name, `protocol_version ∈ SupportedProtocolVersions`, kind-specific `spec` (`states` для `soul_module` / `profile_schema` для `cloud_driver` / `provider_kind` для `ssh_provider`) и input-DSL первого уровня (тип/required/secret/pattern). Exit-code: `0` = ok, `1` = есть errors, `2` = I/O fatal. Парсер и валидатор живут в `shared/plugin/manifest.go` — общий источник правды с runtime-discovery в `soul/internal/pluginhost/`.

### Общие поля (для всех kind-ов)

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `kind` | `enum{soul_module,cloud_driver,ssh_provider,soul_beacon}` | — | Дискриминатор типа плагина. Закрытый enum; расширение — через PR в `proto/plugin/vN/manifest.proto`, не breaking ([ADR-020(e)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). `soul_beacon` — Soul-side плагин event-driven мониторинга (ADR-030 V5-2). |
| `protocol_version` | `int32` | — | Версия `proto/plugin/vN/`. Дублируется в handshake-строке; cross-check внутри плагина и vs `SupportedProtocolVersions` host-а ([ADR-020(c)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). **Не версия артефакта** — это compat-флаг API, исключение из [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте). `int32` (а не `int`) — намеренное исключение из словаря типов: версия протокола не вырастет за 2³¹, на wire-уровне тип фиксируется в `proto/plugin/v1/manifest.proto`. |
| `namespace` | `string` (kebab-case) | — | Коллекция плагина. Для core: `core`. Для third-party: `wb` / `community` / имя организации. |
| `name` | `string` (kebab-case) | — | Имя плагина внутри коллекции. Адресация — `<namespace>.<name>.<state>` для модулей. |
| `required_capabilities` | `list<enum>` | `[]` | Closed enum, см. [таблицу capabilities](#required_capabilities-таблица). `soul-lint` сверяет с `plugin_runtime.allowed_capabilities` host-а. |
| `side_effects` | `list<map<enum,value>>` | `[]` | Strict-контракт touched ресурсов, см. [таблицу side_effects](#side_effects-таблица). Runtime-нарушение (плагин трогает ресурс, не объявленный в `side_effects`) → шаг помечается `failed`, причина `policy_violation` отражается в диагностическом канале `TaskEvent` / `RunResult` (точная форма поля — отдельная задача нормирования audit-pipeline для `side_effects`, см. backlog). |
| `spec` | kind-specific блок | — | Kind-specific поля; форма зависит от `kind:` (см. ниже). |
| `binary_sha256` | `string` (hex64) | `""` (optional) | SHA-256-отпечаток бинаря плагина (hex lowercase, ровно 64 символа). Optional — пусто до подписи **Sigil** ([ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)); используется для verify-against-Sigil перед `exec` (см. [Integrity-model](#integrity-model)). Тип `string` (hex), не `bytes` — консистентно с `plugin_sigils.sha256` (TEXT CHECK hex64). |

Формат input-схемы внутри `spec:` зависит от `kind:`. Для `soul_module` — Soul Stack input-DSL ([`docs/input.md`](../input.md)) в `spec.states.<name>.input`. Для `cloud_driver` и `ssh_provider` — JSON Schema draft 2020-12 в `spec.profile_schema` / `spec.params_schema` соответственно.

### `spec` для `kind: soul_module`

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `spec.states` | `map<state-name, {input, description?}>` | — | Map поддерживаемых состояний (или verb-форм). Ключ — имя состояния (`installed` / `running` / `run` / …). |
| `spec.states.<name>.input` | input-schema (см. [`docs/input.md`](../input.md)) | `{}` | Контракт параметров для этого состояния. `soul-lint` валидирует `params:` каждой задачи destiny против этой схемы. |
| `spec.states.<name>.description` | `string` (optional) | — | Человекочитаемое описание для документации / UI. |

### `spec` для `kind: cloud_driver`

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `spec.profile_schema` | `JSON Schema` | — | Схема параметров VM-профиля. Используется при создании Profile через OpenAPI/MCP для валидации (см. [`cloud.md`](cloud.md)). |
| `spec.provider_kind` | `string` (optional) | — | Семейство cloud-провайдера (`aws` / `gcp` / `yandex-cloud` / `openstack`). Информативное поле. |

### `spec` для `kind: ssh_provider`

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `spec.provider_kind` | `string` (convention: `vault_ssh_ca` / `static_key` / `teleport`; расширение через PR без правки proto) | — | Тип SSH-провайдера; влияет на UI/документацию, но **не** на контракт `Sign`/`Authorize`. На proto-уровне — открытая строка для forward-compat (без правки `proto/plugin/vN/`). |
| `spec.params_schema` | `JSON Schema` (optional) | `{}` | Схема параметров провайдера, задаваемых в `keeper.yml` (например, `vault_mount` для `vault_ssh_ca`). |

### `spec` для `kind: soul_beacon`

Soul-side плагин event-driven мониторинга ([ADR-030 V5-2](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) + [amendment 2026-05-26](../adr/0030-vigil-oracle.md#amendment-2026-05-26-s5-closure), бинарь `soul-beacon-<name>`). Read-only по конструкции: `Check` наблюдает состояние хоста и НЕ мутирует систему. Адресация Vigil — `<namespace>.<name>` в поле `VigilDef.check` (диспетчер Soul-side различает встроенные `core.beacon.*` от plugin-beacon по namespace).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `spec.params_schema` | `JSON Schema` (optional) | `{}` | Схема `params` Vigil, задаваемых оператором через OpenAPI/MCP. Runtime-проверки (то, что не выражается JSON Schema) — через `SoulBeacon.Validate`. |

Не разрешены: `spec.states` (один тип операции — `Check`, нет state-семантики SoulModule), `spec.provider_kind` (только для `ssh_provider`), `spec.profile_schema` (только для `cloud_driver`).

### Расширение manifest

Новые kind-ы (`secrets_provider`, `audit_sink`, …) — добавление варианта в enum `kind` и нового подсообщения `*Spec` в `proto/plugin/vN/manifest.proto`. Forward-compat: host более ранней версии видит неизвестный `kind:` → отвергает плагин с сообщением `unknown kind=X, host supports [...]`.

### Drift manifest ↔ код плагина

Расхождение между декларацией в `manifest.yaml` и реальным поведением кода — реальный риск. Защита:

- **Self-test:** плагин обязан возвращать `INVALID_ARGUMENT` на вызов `Apply` с параметрами вне input-schema. Подразумевается, что SDK генерирует валидатор из схемы автоматически.
- **Cross-check `kind`:** `manifest.kind != handshake.kind` → host отвергает запуск ([ADR-020(c)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)).
- **Generated manifest (post-MVP):** `soul-mod gen-manifest --check` из SDK — сравнивает декларации в коде vs `manifest.yaml`. Не часть MVP.

## Handshake

Плагин при запуске пишет в stdout **ровно одну строку** с JSON-payload. Все строки до первой с магическим полем `"soul_stack":"plugin-v1"` host **игнорирует** (логирует на debug-уровне). После handshake stdout считается закрытым для plugin-протокола — любые последующие записи в stdout host игнорирует. Логи плагина в MVP пишутся в **stderr** (стандартный UNIX-канал для diagnostics); host пересылает stderr плагина в свой log/OTel-pipeline с тегом `plugin=<namespace>.<name>`. Структурированный log-stream через отдельный gRPC-RPC — зарезервирован под `proto/plugin/v2/`, в MVP отсутствует.

### Формат handshake-строки

```json
{"soul_stack":"plugin-v1","protocol_version":1,"kind":"soul_module","network":"unix","address":"/var/run/soul-stack/plugins/wb-haproxy-12345.sock","server_cert":""}
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `soul_stack` | `string` (константа `"plugin-v1"`) | — | Магическое sanity-поле. Host игнорирует все stdout-строки до первой с этим полем. Значение не зависит от `protocol_version` — это маркер «формат handshake-строки v1»; меняется только при breaking-смене самого формата handshake (отдельный ADR). |
| `protocol_version` | `int32` | — | Версия plugin-протокола (см. [Versioning](#versioning)). Должен совпадать с `manifest.protocol_version`. Тип `int32` (не `int`) — то же намеренное исключение, что в общих полях Manifest. |
| `kind` | `enum{soul_module,cloud_driver,ssh_provider,soul_beacon}` | — | Должен совпадать с `manifest.kind`. |
| `network` | `string` (MVP convention: `"unix"`; будущие `"named_pipe"` / `"tcp"`) | — | Тип сокета. MVP — только `unix`. Расширение `named_pipe` (Windows) / `tcp` (loopback) — post-MVP, без правки `proto/plugin/vN/` (на proto-уровне — открытая строка для forward-compat). |
| `address` | `path` | — | Путь к Unix-socket, на котором плагин слушает gRPC. Должен совпадать с переданным в env-var `SOUL_PLUGIN_SOCKET` (см. [Lifecycle](#lifecycle)). |
| `server_cert` | `base64-pem` (optional) | `""` | Зарезервировано под опциональный mTLS post-MVP. В MVP всегда `""` ([ADR-020(h)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). |

Расширение через новые **опциональные** ключи (`features`, `capabilities`, …) — без breaking. Неизвестные опциональные ключи host игнорирует.

### Поведение host-а при handshake

| Ситуация | Поведение host-а |
|---|---|
| stdout-строка не парсится как JSON | Игнорируется, читается следующая строка. |
| stdout-строка валидный JSON, но без `"soul_stack":"plugin-v1"` | Игнорируется. |
| Handshake-строка появилась, но `protocol_version ∉ SupportedProtocolVersions` host-а | Hard fail: `protocol_version=N, host supports [...]`. SIGTERM плагину. |
| `manifest.protocol_version != handshake.protocol_version` | Hard fail: drift внутри плагина. SIGTERM. |
| `manifest.kind != handshake.kind` | Hard fail: drift внутри плагина. SIGTERM. |
| Handshake не появился за `plugin_runtime.startup_timeout` (default `10s`) | Hard fail: startup timeout. SIGTERM, через `shutdown_grace` — SIGKILL. |
| Handshake OK, но connect к `address` не удался | Hard fail: socket unreachable. SIGTERM. |
| Несколько строк с `"soul_stack":"plugin-v1"` | Первая — handshake; все последующие на stdout — игнорируются (после handshake stdout «закрыт» для plugin-протокола). |

### Поведение host-а после handshake (crash плагина)

После успешного handshake плагин — отдельный one-shot процесс, который может упасть до или во время Apply-stream-а. Поведение host-а:

| Ситуация | Поведение host-а |
|---|---|
| Плагин завершился с exit code ≠ 0 **до начала Apply** (после handshake, до первого RPC) | Шаг помечается `failed`, причина `plugin_init_failed`. Stderr-tail (последние 4KB) отражается в диагностическом канале `TaskEvent` / `RunResult` (точная форма поля — отдельная задача нормирования audit-pipeline, см. backlog). |
| Плагин завершился с exit code ≠ 0 **в середине Apply-stream** | Шаг помечается `failed`, причина `plugin_crash`. gRPC-stream закрывается; stderr-tail (4KB) отражается в диагностическом канале `TaskEvent` / `RunResult`. |
| Плагин panic / OOM-killed / SIGSEGV (любой non-graceful exit) | То же поведение, что выше; конкретная причина — best-effort из exit code (например, `exit_code=139` → SIGSEGV), host пишет в диагностический канал. |
| Retry | На уровне plugin-host **retry в MVP нет**. Retry-семантика — на уровне scenario через ключ `retry:` (см. [`../scenario/`](../scenario/README.md)). |

Имена причин (`plugin_init_failed`, `plugin_crash`) — отдельные значения в открытом каталоге `TaskError.reason` (нормирование полного каталога — отдельная backlog-задача вместе с закрытием `proto/plugin/v1/` и audit-pipeline; см. также [naming-rules.md → Поведение host-а после handshake](../naming-rules.md#поведение-host-а-после-handshake-plugin_init_failed--plugin_crash)).

## Lifecycle

Плагин — **one-shot процесс на каждый Apply** ([ADR-020(d)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Long-lived (один процесс на серию вызовов) — отдельным ADR при необходимости.

### Диаграмма

```
host (keeper / soul)                              plugin (soul-mod-* / soul-cloud-* / soul-ssh-*)
─────────────────────                              ─────────────────────────────────────────────
   1. mkdir /var/run/soul-stack/plugins/  (mode 0700, owned by service user)
   2. socket_path := "/var/run/.../plugins/<namespace>-<name>-<pid>.sock"
   3. fork():
      env SOUL_PLUGIN_SOCKET=<socket_path>
      exec <plugin_binary>             ─────────►   init(); read env SOUL_PLUGIN_SOCKET
                                                    listen(unix, $SOUL_PLUGIN_SOCKET, mode 0700)
                                                    register gRPC services
                                                    print one-line JSON handshake to stdout
                                       ◄─────────   (handshake bytes)
   4. read stdout, ignore lines until "soul_stack":"plugin-v1"
   5. validate handshake (protocol_version, kind, address)
   6. dial gRPC at <socket_path>
   7. RPCs (Validate / Plan / Apply / Schema / ...)
                                       ◄────►       (gRPC traffic over Unix-socket)
   8. host done. SIGTERM(plugin)        ─────────►  signal handler: finish in-flight RPCs
                                                    close gRPC server
                                                    unlink(socket_path)
                                                    exit(0)
   9. wait(grace=10s); if alive — SIGKILL.
  10. unlink per-pid socket file if still exists.
```

### Параметры lifecycle (настраиваются через `plugin_runtime:` блок)

Блок `plugin_runtime:` в [`keeper.yml`](config.md) / [`soul.yml`](../soul/config.md) — нормативная спецификация: [`config.md → plugin_runtime`](config.md#plugin_runtime) (Keeper-side) и [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime) (Soul-side). Defaults фиксированы в [ADR-020(d/f/g/h)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle); таблица ниже дублирует их inline для удобства чтения этого документа.

| Параметр (в `plugin_runtime:`) | Default | Смысл |
|---|---|---|
| `startup_timeout` | `10s` | Время от fork до появления handshake-строки. Превышение → SIGTERM. |
| `shutdown_grace` | `10s` | Время от SIGTERM до SIGKILL. |
| `allowed_capabilities` | все 6 capabilities из [таблицы](#required_capabilities-таблица) | Список разрешённых на этом host-е capabilities; `soul-lint` сверяет с `manifest.required_capabilities`. |
| `conflict_policy` | `warn` | Что делать при конфликте `side_effects` двух плагинов: `warn` / `fail`. |
| `enable_tls` | `false` | Post-MVP опция: включение mTLS на plugin-сокете ([ADR-020(h)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). |

Полная типизация полей, валидация значений и per-поле hot-reload-политика — в [`config.md → plugin_runtime`](config.md#plugin_runtime) (Keeper) и [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime) (Soul).

### Расположение сокета

| Host | Директория | Mode | Owner |
|---|---|---|---|
| `soul` | `/var/run/soul-stack/plugins/` | `0700` | service user `soul` |
| `keeper` | `/var/run/soul-stack-keeper/plugins/` | `0700` | service user `keeper` |

Имя файла сокета — `<namespace>-<name>-<pid>.sock` (точка из `<namespace>.<name>`-адресации плагина заменена дефисом для согласованности с файловой грамматикой). Пример: для плагина `wb.haproxy` с pid `12345` → `wb-haproxy-12345.sock`. После выхода плагина host удаляет файл (на случай, если плагин не unlink-нул сам).

## Integrity-model

Бинарь плагина в кеше host-а форкается с правами service-user-а (`keeper`-плагины имеют доступ к Vault / PG / PKI). Подмена бинаря в `/var/lib/soul-stack-keeper/plugins/` (либо в artifact-source / при Keeper-checkout-е git-ref-а **до** первого появления на host-е) → RCE. Защита — **Sigil** ([ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)): Keeper-signed digest-индекс (**Вариант A**). Сверка по SHA-256 (инвариант [CLAUDE.md](../../CLAUDE.md) «кеш по SHA-256») сохраняется как defense-in-depth перед каждым `exec`; «доверяй как есть» при first-load **заменено** на верификацию Sigil.

> **Sigil заменяет прежнюю TOFU-модель** (trust-on-first-use). TOFU описывал first-load как «host сам считает SHA-256 и доверяет бинарю как есть» — это не закрывало подмену бинаря **до** его первого появления на host-е (см. [«Закрытый gap — first-load»](#закрытый-gap--first-load) ниже). С Sigil authority над тем, какой бинарь допущен, принадлежит Keeper-у, а не host-у.

### Корень доверия

| Что | Где |
|---|---|
| **Allow-list** `(namespace, name, ref) → sha256` | PG-таблица `plugin_sigils` (Keeper-state). Запись добавляется **только когда Архонт явно допускает** плагин через OpenAPI (`POST /v1/plugins/sigils`, S4a) / MCP (S4b) — permission `plugin.allow`, [rbac.md → Plugin Sigil](rbac.md#plugin-sigil-3). `ref` — **git-verified** (Keeper резолвит `source`+`ref` в `commit_sha`-слот кеша через go-git, [ADR-026(g)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)); цепочка доверия `ref` → `commit_sha` → `binary_sha256` → подпись Keeper-а. |
| **Подписывающий ключ Keeper-а** (private) | Vault KV — по паттерну `secret/keeper/jwt-signing-key` ([ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). |
| **Публичный ключ Keeper-а** (trust-anchor host-а) | Приезжает Soul-у в **bootstrap** вместе с CA-chain (тем же каналом `BootstrapReply`, что и mTLS CA, [ADR-012(f)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)): одиночный `sigil_pubkey_pem` либо multi-anchor `sigil_pubkey_pem_set` (приоритет set > single). Runtime-набор anchor-ов доставляется `SigilTrustAnchors` и **полностью замещает** bootstrap-anchors (replace, не мёрж; ротация R3) — см. [Active-набор и replace-семантика](#active-набор-и-replace-семантика). |

**Sigil** = `sign_keeper(блок)`, где подписываемый блок несёт `(namespace, name, ref, binary_sha256, manifest)`. Подпись **покрывает manifest** с пришитым `binary_sha256` ([ADR-026(c)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)) → задекларированные `side_effects` / `required_capabilities` / `protocol_version` перестают быть подделываемыми (нельзя подменить manifest, не сломав подпись).

> **`ref` — git-verified** ([ADR-026(g)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), **Вариант A, F-fetch**). Keeper сам резолвит `source`+`ref` из каталога `keeper.yml` через **go-git**: shallow `clone`→`fetch`→`ResolveRevision(<ref>^{commit})` (резолв в 40-hex `commit_sha`)→detached-HEAD `checkout`, затем извлекает **УЖЕ собранный** бинарь `dist/<binary-name>` + `manifest.yaml` (F-fetch — компиляции на Keeper нет). Граница «verified» = «Keeper зачекаутил именно этот `ref` и зафиксировал результат (`commit_sha` + `binary_sha256`)», **НЕ** bit-reproducibility сборки. Кеш — **R-nested**: `<cacheRoot>/<ns>-<name>/<commit_sha>/` (иммутабельный слот) + symlink `current → <commit_sha>` (атомарно переставляемый указатель на активный слот). **Single-active-на-пару**: `current` указывает ровно на один `commit_sha`, но несколько `commit_sha`-слотов под одной `(namespace, name)` сосуществуют. `plugin.allow` читает бинарь+manifest АКТИВНОГО слота через `current` ([`pluginhost.ReadSlot`](../../keeper/internal/pluginhost/slot.go)), считает `sha256`, подписывает и вставляет запись; `ref` в lookup слота не участвует. **Authority целостности = `sha256` + подпись** (инвариант (b) ADR-026 не ослаблен); `ref`/`commit_sha` несут происхождение и audit-читаемость, не доверие. `commit_sha` — audit-метка ВНЕ подписи (подпись — над `(namespace, name, ref, binary_sha256, manifest_sha256)`); добавится колонкой в `plugin_sigils` при S3.

### Формат подписываемого блока (нормативный, S3)

Блок собирается чистой детерминированной функцией (`shared/pluginhost.BuildSigilBlock`) — общий код для подписи на Keeper (S3) и верификации на Soul (S6), **без** proto-marshal (proto-сериализация недетерминирована — её исключили сознательно):

```
блок = DST || LP(namespace) || LP(name) || LP(ref) || LP(binary_sha256) || LP(manifest_sha256)
```

- **`DST`** — domain-separation-тег, ASCII-константа `soul-stack/sigil/v1` (без length-prefix, фиксированный известный префикс). Версия `/v1` обязательна: смена формата блока → `…/v2`, старые подписи перестают проходить против нового кода (явный разрыв совместимости). DST первым → подпись над Sigil нельзя переиспользовать в другом протоколе.
- **`LP(x)`** = 4 байта big-endian uint32 длины `x`, затем сами байты `x`. Применяется к **каждому** переменному полю — защита границ полей: без length-prefix конкатенации `("ab","c")` и `("a","bc")` дали бы один блок, и подпись над одним набором подошла бы к другому.
- Хеши (`binary_sha256`, `manifest_sha256`) кладутся **сырыми байтами** (для SHA-256 — 32 байта), **не** hex-строкой.
- Порядок полей зафиксирован ровно: `namespace`, `name`, `ref`, `binary_sha256`, `manifest_sha256`.

**Ключ подписи — ed25519** (асимметрия обязательна, в отличие от HS256-симметричного JWT signing-key): приватник живёт в Vault KV по `sigil.signing_key_ref` ([config.md → sigil](config.md#sigil)), подпись — сырые 64 байта; публичная часть едет Soul-у в bootstrap как trust-anchor.

**S3↔S6-инвариант (байты manifest).** Байты `manifest.yaml`, которые Keeper хеширует при подписи, обязаны совпадать с байтами, которые Soul re-хеширует при verify. Гарантия: (1) manifest и бинарь доставляются одним artifact-потоком; (2) обе стороны прогоняют сырые байты через `NormalizeManifestBytes` перед SHA-256 — **только байтовая** канонизация (strip BOM, CRLF→LF, ровно один trailing `\n`), **без** re-parse/re-emit YAML (хеш не должен зависеть от версии yaml-эмиттера).

**Канон vs проекция в `plugin_sigils`.** Реестр держит manifest в **двух** колонках:

- **`manifest_raw` (`bytea`, миграция 030) — КАНОН.** Byte-exact те же сырые байты, над которыми поставлена подпись (единый `ReadSlot` при `plugin.allow`). Именно они едут в `PluginSigil.manifest` для broadcast и re-хешируются Soul-ом при verify. Колонка nullable на уровне DDL (forward-only для старых строк), но `allow`-путь требует non-NULL: `Insert` отклоняет пустой `manifest_raw` (пустые подписанные байты = баг вызова, корень доверия; `{}`-fallback тут **неприменим** — `Normalize("{}") != Normalize("")`).
- **`manifest` (`jsonb`, миграция 028) — производная.** Проекция для query/audit (искать по `side_effects` / `required_capabilities`, показывать в UI). **НЕ** канон для verify: JSONB-роундтрип не сохраняет байты. Брать `manifest` (JSONB) для verify/broadcast — ошибка; источник истины всегда `manifest_raw`.

### Механизм

| Шаг | Поведение host-а |
|---|---|
| Discover | Считает SHA-256 бинаря потоково; кладёт в `Discovered.Digest` для логов / OTel-атрибутов. Ошибка чтения бинаря для digest → плагин пропускается с warning. |
| Получение Sigil | **Push** (Keeper передаёт плагины ОТ Keeper-а по mTLS — Keeper уже trust-anchor): Sigil едет вместе с бинарём. **Pull** (Soul-демон): Sigil приходит only-add proto-message-ом в `EventStream` ([ADR-026(e)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). |
| Verify (до seal/exec) | (1) SHA-256 фактического бинаря == `binary_sha256` в Sigil; (2) подпись Sigil валидна публичным ключом Keeper-а (из bootstrap). Обе проверки прошли → seal + exec. Любое расхождение → отказ, **бинарь не запускается**, event `plugin.verify_failed`. |
| Re-exec из кеша | SHA-256-сверка перед каждым последующим `exec` (defense-in-depth для shared-кеша). Расхождение → отказ, бинарь не запускается. |

Integrity-gate срабатывает **до** `mkdir socket-dir` и `exec` — невалидный бинарь не получает управления.

### Active-набор и replace-семантика

Active-набор допусков и набор trust-anchor-ов на Soul-стороне ведутся **replace-семантикой** ([ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). **`SigilSnapshot`** — единственный источник истины active-набора: Soul применяет его как **ReplaceAll** (заменяет весь свой набор, не upsert), отсутствующий в snapshot допуск **забывает** — так срабатывают revoke и retire (Keeper после `plugin.revoke` шлёт новый snapshot без отозванного допуска → near-instant revoke без перезапуска Soul-а). Одиночный broadcast `PluginSigil` — **уведомление о новом допуске, не мутация набора** (Soul по нему upsert не делает; авторитет — только snapshot). Набор trust-anchor-ов (**`SigilTrustAnchors`**) ведётся так же replace: runtime-доставка **полностью замещает** bootstrap-anchors (replace, не мёрж), multi-anchor поддерживает безразрывную ротацию ключа подписи. В bootstrap при непустом `sigil_pubkey_pem_set` одиночное `sigil_pubkey_pem` игнорируется (приоритет set > single); оба пустых = Sigil выключен.

### Ротация ключей подписи (multi-anchor, R3)

Ключи подписи Sigil ротируются **без разрыва** verify на Soul-ах ([ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). Реестр `sigil_signing_keys` (миграция 037) держит набор ключей: ровно один **primary** (им Keeper подписывает новые Sigil-ы) и любое число прочих **active** (ими Soul ещё валидирует ранее подписанное). **Приватник НИКОГДА не в Postgres** — только публичная часть (`pubkey_pem`, SPKI) + ссылка `vault_ref` на приватник в Vault KV.

Operator-facing ротация (R3-S7, REST `/v1/sigil/keys*` + MCP `keeper.sigil.key.*`):

- **introduce** (`POST /v1/sigil/keys`, permission `sigil.key-introduce`): Keeper генерирует ed25519-пару, пишет приватник в Vault KV (`secret/keeper/sigil-keys/<key_id>`, `key_id` = SHA-256(SPKI) hex), вставляет публичную часть в реестр как active. Ответ несёт `key_id` + `pubkey_pem` — **приватник НИКОГДА не возвращается** и не логируется.
- **set-primary** (`POST /v1/sigil/keys/{key_id}/primary`, `sigil.key-set-primary`): новый ключ становится primary, новые Sigil-ы подписываются им после cluster reload.
- **retire** (`DELETE /v1/sigil/keys/{key_id}`, `sigil.key-retire`): ключ выводится из набора.
- **list** (`GET /v1/sigil/keys`, `sigil.key-list`): active-ключи, primary первым.

После каждой мутации мутирующая нода публикует в Redis-канал `sigil:anchors-changed`; **каждая** нода re-build-ит Signer (новый primary + якоря) и re-broadcast-ит `SigilTrustAnchors` своим Soul-ам — набор обновляется near-instant по всему кластеру. Безразрывная ротация: вводим новый ключ active → делаем primary → старый дослуживает active (Soul ещё доверяет его подписям) → выводим retired.

**Retire-инвариант (безопасность).** `Retire` разрешён, только когда: (1) новый набор разошёлся по кластеру (`sigil:anchors-changed` → reload) и (2) bootstrap-reply отдаёт набор из **живого источника** (новый Soul после ротации получает актуальные якоря, а не снимок старта). Дополнительно: нельзя retire **primary** напрямую (сперва set-primary другому) и **последний active** (набор не должен опустеть — verify лишился бы всех якорей).

### Permissions (least-privilege)

| Объект | Mode | Owner | Требование |
|---|---|---|---|
| Кеш-каталог `<cacheRoot>/<ns>-<name>/<commit_sha>/` (R-nested per-commit слот + symlink `current → <commit_sha>`, [ADR-026(g)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)) | `0755` | service-user (`keeper` / `soul`) | Запись — только владельцу. Group/other — read-only, чтобы посторонний процесс не подменил бинарь или sidecar. |
| Бинарь плагина | `0755` | service-user | Исполняемый, запись только владельцу. |
| Sidecar `.sha256` | `0400` | service-user | Read-only после записи. |

Host **обязан** запускаться под выделенным least-privilege service-user-ом, не root (кроме плагинов с `run_as_root`-capability). Защита от подмены sidecar **вместе** с бинарём держится именно на правах каталога: атакующий без write-доступа к каталогу не перезапишет ни бинарь, ни `.sha256`. Sidecar `.sha256` — это кеш digest-а для re-exec-сверки (defense-in-depth shared-кеша), **не** trust-anchor: authoritative-источник «допущен ли бинарь» — Sigil (подпись Keeper-а + allow-list `plugin_sigils`), а не локально посчитанный хеш.

### Закрытый gap — first-load

Прежняя TOFU-модель защищала от подмены бинаря **после** первой загрузки в кеш, но **не** от malicious-плагина при **первой** загрузке (если атакующий подменил бинарь в artifact-source / при Keeper-checkout-е git-ref-а до того, как host его впервые увидел) — он проходил integrity-gate «как есть» и форкался с правами service-user-а (RCE-вектор). **Этот gap закрывается Sigil-ом** ([ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)): first-load больше не «доверяй как есть» — host верифицирует подпись Keeper-а и сверяет digest с явно допущенным Архонтом значением **до** seal/exec. Malicious-бинарь без валидного Sigil не получает управления.

> **Имплементация (host-side verify — LIVE).** Git-резолвер каталога плагинов готов (A1-S1): [`keeper/internal/plugingit`](../../keeper/internal/plugingit) (go-git F-fetch, R-nested-кеш `<ns>-<name>/<commit_sha>/` + `current`, scheme-allowlist, git-egress size-limit (ADR-026(g)), sentinel-ы `ErrRefNotResolved`/`ErrManifestNotFound`/`ErrArtifactNotFound`/`ErrSourceUnavailable`/`ErrCloneTooLarge`/`ErrArtifactTooLarge`), config-поля `plugins.work_root`/`plugins.fetch_timeout`/`plugins.max_artifact_size_mb`/`plugins.max_clone_size_mb`; чтение активного слота при `plugin.allow` — [`pluginhost.ReadSlot`](../../keeper/internal/pluginhost/slot.go) через `current`. Keeper-side подпись готова (S3): общий helper сборки блока + канонизации manifest в [`shared/pluginhost`](../../shared/pluginhost) (`BuildSigilBlock` / `NormalizeManifestBytes`), ed25519-подпись + CRUD реестра `plugin_sigils` в [`keeper/internal/sigil`](../../keeper/internal/sigil), конфиг `sigil.signing_key_ref`. `plugin.allow` персистит подписанные сырые байты в `manifest_raw` (M1-storage, миграция 030) — `ListActive` / `GetActive` отдают их S6-sender-у/S6b-verify byte-exact. **Host-side verify-against-Sigil — LIVE (S6):** TOFU-ветка first-load заменена на verify по Sigil + multi-anchor-набору в [`shared/pluginhost`](../../shared/pluginhost) (SHA-256-сверка перед каждым `exec` остаётся defense-in-depth). **Multi-anchor ротация ключей подписи — LIVE (R3, [ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)):** реестр `sigil_signing_keys` (миграция 037, [`keeper/internal/sigil/keys.go`](../../keeper/internal/sigil/keys.go)), multi-anchor Signer, broadcast `SigilTrustAnchors` + Redis-канал `sigil:anchors-changed` (cluster reload), operator-facing rotation (R3-S7: REST `/v1/sigil/keys*` + MCP `keeper.sigil.key.*`, permissions `sigil.key-introduce|retire|list|set-primary`, audit `sigil.key-introduced|retired|primary-set`), bootstrap-reply из живого источника якорей. Отложено: колонка `commit_sha` в `plugin_sigils` (A1-S3, audit-метка происхождения, ВНЕ подписи).

## Versioning

`protocol_version: int` — версия plugin-протокола. **Одно поле — два места** ([ADR-020(c)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)):

- В `manifest.yaml` — для статического `soul-lint`.
- В handshake-строке — для runtime sanity до открытия gRPC.

### Соответствие `protocol_version` ↔ `proto/plugin/vN/`

| `protocol_version` | proto package | Статус | Состав |
|---|---|---|---|
| `1` | `proto/plugin/v1/` | MVP | `handshake.proto`, `manifest.proto`, `soulmodule.proto`, `clouddriver.proto`, `sshprovider.proto` (закрытие — отдельная задача после ADR-020). |

### `SupportedProtocolVersions`

Каждый host-бинарь (`keeper` / `soul` / `soul-lint`) держит константу — упорядоченный list поддерживаемых версий протокола. В MVP — `[1]`.

Эволюция: добавление `proto/plugin/v2/` → следующая версия host-бинаря держит `[1, 2]` (forward-compat only-add, аналог [ADR-012(g)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) для plugin-протокола). Удаление старых версий — breaking-релиз host-бинаря, отдельным ADR.

### Cross-check матрица

Сводный список cross-check-ов между manifest, handshake-строкой и константой `SupportedProtocolVersions` host-а. Дублирует runtime-строки из таблицы [«Поведение host-а при handshake»](#поведение-host-а-при-handshake) в формальной нотации + добавляет статическую проверку `soul-lint`.

| Условие fail | Где | Поведение |
|---|---|---|
| `manifest.protocol_version != handshake.protocol_version` | host, после handshake | Hard fail: drift внутри плагина. |
| `manifest.protocol_version ∉ SupportedProtocolVersions` | `soul-lint` при валидации destiny | Ошибка валидации destiny **до запуска**. |
| `handshake.protocol_version ∉ SupportedProtocolVersions` | host, после handshake | Hard fail: `protocol_version=N, host supports [...]`. |
| `manifest.kind != handshake.kind` | host, после handshake | Hard fail: drift внутри плагина. |

## `required_capabilities`-таблица

Closed enum capabilities. Плагин декларирует, что ему нужно от системы host-а; `soul-lint` сверяет с `plugin_runtime.allowed_capabilities` host-а (mismatch → ошибка валидации destiny **до запуска**).

| Capability | Смысл |
|---|---|
| `run_as_root` | Host-процесс (`soul` / `keeper`) должен иметь UID 0 при запуске плагина. |
| `network_outbound` | Плагин делает исходящие сетевые вызовы (cloud API, vault, package mirror). |
| `network_inbound` | Плагин слушает порт (редкий случай, для тестовых helper-плагинов). |
| `vault_access` | Плагин обращается к Vault через клиентский helper SDK. |
| `fs_write_root` | Плагин пишет за пределы `/var/lib/soul-stack/`. |
| `exec_subprocess` | Плагин запускает внешние команды через `os/exec`. |

Расширение enum-а — через PR в `proto/plugin/vN/manifest.proto`, не breaking. Freeform-extensions с префиксом `x-` (open-ended capabilities) **в MVP отвергнуты** — добавится при первом реальном запросе.

**Декларация, не runtime-enforcement.** `required_capabilities` — это *статическая декларация* того, что плагину нужно от host-а, для сверки `soul-lint`-ом с `plugin_runtime.allowed_capabilities` host-а: при mismatch — ошибка валидации destiny **до запуска**. Host повышения прав по этому полю **не делает** — шаг исполняется ровно с привилегиями процесса (`soul` / `keeper`), а само поле прав не выдаёт. Так, `run_as_root` означает «модуль работает корректно только при UID 0 host-процесса» (требование к среде), а не «модуль повышается до root сам». Встроенные core-модули (`soul`-side, статически вкомпилированные) объявляют `required_capabilities` в своих манифестах [`shared/coremanifest/<name>.yaml`](../../shared/coremanifest) и проходят **ту же** статическую сверку — для них поле не несёт никакой иной (runtime) семантики, чем для внешних плагинов.

## `side_effects`-таблица

Closed enum типов ресурсов, которые плагин трогает (touched resources). Грамматика записи — `{<resource-type>: <value>}`.

| Resource type | Значение (тип) | Пример |
|---|---|---|
| `service` | `string` (имя сервиса) | `{ service: haproxy }` |
| `file` | `path` (абсолютный путь) | `{ file: /etc/haproxy/haproxy.cfg }` |
| `package` | `string` (имя пакета OS) | `{ package: haproxy }` |
| `port` | `int` (tcp/udp порт) | `{ port: 80 }` |
| `user` | `string` (имя OS-пользователя) | `{ user: postgres }` |
| `group` | `string` (имя OS-группы) | `{ group: postgres }` |
| `directory` | `path` (абсолютный путь к директории) | `{ directory: /var/lib/postgresql }` |
| `cron` | `string` (имя cron-задачи) | `{ cron: backup-nightly }` |
| `mount` | `path` (mountpoint) | `{ mount: /var/lib/data }` |

Расширение enum-а — через PR в `proto/plugin/vN/`, не breaking. Wildcard-значения (`file: /etc/haproxy/**`) и условные `side_effects` (`when: …`) **в MVP отвергнуты** — добавится при первом реальном запросе.

### Грамматика записей `side_effects`

Каждая запись в `side_effects` — объект **ровно с одной** парой `<resource_type>: <resource_value>`. Если плагин трогает несколько ресурсов разных типов — это **отдельные записи** в списке:

```yaml
side_effects:
  - { service: haproxy }
  - { file: /etc/haproxy/haproxy.cfg }
  - { port: 80 }
```

Парсер manifest валидирует: запись с более чем одной парой → ошибка `multiple_resource_types_in_side_effect_entry`. Несколько ресурсов **одного** типа также пишутся отдельными записями (например, два разных файла — две `{ file: … }`-записи).

### Поведение host-а на side_effects

| Ситуация | Поведение |
|---|---|
| Каждый touched ресурс при `Apply` | Запись в audit-event с указанием плагина (`namespace.name`) и `apply_id`. |
| Два плагина в одном прогоне на тот же ресурс | По политике `plugin_runtime.conflict_policy`: `warn` (default) или `fail`. |
| Плагин трогает ресурс не из `side_effects` | Шаг помечается `failed`, причина `policy_violation` отражается в диагностическом канале `TaskEvent` / `RunResult` (точная форма поля — отдельная задача нормирования audit-pipeline для `side_effects`, см. backlog). |

## Service-контракт `SoulModule`

Host — `soul`-бинарь. Бинари — `soul-mod-<name>` (например, `soul-mod-haproxy`).

| Метод | Назначение |
|---|---|
| `Validate(ValidateRequest) → ValidateReply` | Runtime-проверки параметров (полная схема в `manifest.spec.states.<state>.input` уже проверена `soul-lint`-ом; здесь — дополнительные семантические проверки, требующие доступа к системе host-а). |
| `Plan(PlanRequest) → stream PlanEvent` | Dry-run: модуль рассчитывает изменения без их применения. Возвращает stream-событий прогресса. |
| `Apply(ApplyRequest) → stream ApplyEvent` | Применяет изменения. Stream-события для long-running-операций (см. оговорку про MVP в [ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) — прогресс агрегируется на Soul, наружу через `TaskEvent` идёт только финальный результат). |

`Manifest()` RPC в MVP **не вводится** — манифест читается host-ом из статического `manifest.yaml` ([ADR-020(a)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Возможный future-метод (для self-test «сам себя сравни с задекларированным») — в `proto/plugin/v2/`.

Адресация шага destiny — `<namespace>.<name>.<state>` (см. [`../soul/modules.md`](../soul/modules.md), [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny)).

## Service-контракт `CloudDriver`

Host — `keeper` (модуль `keeper.cloud`, см. [`cloud.md`](cloud.md)). Бинари — `soul-cloud-<provider>` (например, `soul-cloud-aws`, `soul-cloud-yc`).

| Метод | Назначение |
|---|---|
| `Schema(SchemaRequest) → SchemaReply` | Публикует `profile_schema` (JSON Schema VM-профиля; должен совпадать с `manifest.spec.profile_schema`). Используется при создании Profile через OpenAPI/MCP для валидации. `SchemaRequest` — пустое сообщение (вместо `google.protobuf.Empty`) для forward-compat возможности добавлять поля без breaking change. |
| `Validate(ValidateProfileRequest) → ValidateProfileReply` | Runtime-проверки параметров профиля (квоты, доступность образа, валидность subnet-а — то, что не выражается JSON Schema). Имя request/reply отличается от SoulModule `ValidateRequest/Reply` — единый proto-пакет `soulstack.plugin.v1`, имена messages должны быть уникальны. |
| `Create(CreateRequest) → stream CreateEvent` | Создаёт VM (одну или N), стримит прогресс. |
| `Destroy(DestroyRequest) → stream DestroyEvent` | Удаляет VM. Под guard-rails — см. [cloud.md → Безопасность destroy](cloud.md#безопасность-destroy). |
| `Status(StatusRequest) → StatusReply` | Опрос состояния конкретной VM. `StatusRequest.credentials` несёт plain-секрет провайдера (A-flow, симметрично `CreateRequest`/`DestroyRequest`) — без credentials драйвер не сможет обратиться к provider API. |
| `List(ListRequest) → stream VmInfo` | Перечисление VM, известных провайдеру. `ListRequest.credentials` — A-flow (симметрично `Create`/`Destroy`/`Status`); `ListRequest.filter` — provider-specific фильтр (теги/регион), credentials в filter класть НЕЛЬЗЯ. |

Использование — в [`cloud.md`](cloud.md). Cloud-create встроен в сценарии как шаг с `on: keeper` (модуль `core.cloud.provisioned`, [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)).

## Service-контракт `SshProvider`

Host — `keeper` (модуль `keeper.push`, см. [`push.md`](push.md)). Бинари — `soul-ssh-<provider>` (например, `soul-ssh-vault`, `soul-ssh-static`, `soul-ssh-teleport`).

| Метод | Назначение |
|---|---|
| `Sign(SignRequest) → SignReply` | Выдать SSH-сертификат / ключ для текущей сессии (например, Vault SSH CA выпускает короткоживущий сертификат). |
| `Authorize(AuthorizeRequest) → AuthorizeReply` | Подтвердить право Keeper-а ходить на конкретный хост (политика провайдера, если она есть). |

Под этот контракт укладываются Vault SSH CA, static-key, Teleport — три кандидата на MVP, конкретный набор обязательных реализаций — [open Q SSH-2 / №3](../architecture.md#открытые-вопросы). Использование — в [`push.md → Аутентификация SSH`](push.md#аутентификация-ssh--pluggable-provider).

## Полные примеры manifest

### `kind: soul_module` (HAProxy)

```yaml
# soul-mod-haproxy/manifest.yaml
kind: soul_module
protocol_version: 1
namespace: wb
name: haproxy

required_capabilities:
  - run_as_root
  - exec_subprocess

side_effects:
  - { service: haproxy }
  - { file: /etc/haproxy/haproxy.cfg }
  - { package: haproxy }

spec:
  states:
    running:
      description: HAProxy запущен и включён в systemd.
      input:
        name:        { type: string, required: true }
        enabled:     { type: boolean, default: true }
        config_path: { type: string, default: /etc/haproxy/haproxy.cfg }
    stopped:
      description: HAProxy остановлен.
      input:
        name: { type: string, required: true }
    restarted:
      description: HAProxy перезапущен (force-restart).
      input:
        name:        { type: string, required: true }
        config_path: { type: string, default: /etc/haproxy/haproxy.cfg }
    reloaded:
      description: HAProxy reload (SIGHUP) без downtime.
      input:
        name: { type: string, required: true }
```

### `kind: cloud_driver` (AWS)

```yaml
# soul-cloud-aws/manifest.yaml
kind: cloud_driver
protocol_version: 1
namespace: soulstack
name: aws

required_capabilities:
  - network_outbound
  - vault_access

side_effects: []   # cloud_driver не трогает локальные ресурсы host-а

spec:
  provider_kind: aws
  profile_schema:
    type: object
    required: [image_id, instance_type, subnet_id]
    properties:
      image_id:      { type: string, pattern: "^ami-[0-9a-f]+$" }
      instance_type: { type: string }
      subnet_id:     { type: string, pattern: "^subnet-[0-9a-f]+$" }
      security_group_ids:
        type: array
        items: { type: string, pattern: "^sg-[0-9a-f]+$" }
      tags:
        type: object
        additionalProperties: { type: string }
```

### `kind: ssh_provider` (Vault SSH CA)

Реальная реализация — [`examples/module/soul-ssh-vault/`](../../examples/module/soul-ssh-vault). Vault SSH CA: плагин ходит в Vault сам (variant B, см. ниже), вызывает `ssh/sign/<role>` для подписи Keeper-ephemeral pubkey, возвращает только `certificate` (`private_key=""`).

**Канонический режим — Keeper-ephemeral** (security-first, PM-decision):

1. `keeper.push` генерит ephemeral ed25519 keypair per-session и передаёт публичную часть в `SignRequest.public_key`.
2. Плагин аутентифицируется в Vault (`auth_method: token | approle`), вызывает `<vault_mount>/sign/<role>` с этой pubkey и `valid_principals=<req.user>`.
3. Возвращает `SignReply{certificate=<signed_key>, private_key=""}` — приватник НИКОГДА не покидает Keeper.
4. `keeper.push` собирает [`ssh.NewCertSigner`](https://pkg.go.dev/golang.org/x/crypto/ssh#NewCertSigner) из ephemeral signer + cert и открывает SSH-сессию. После закрытия сессии приватник уходит в GC.

**Creds-flow — variant B** (плагин сам ходит в Vault, [`vault_access` capability](#required_capabilities)): для `ssh/sign` это естественнее A-flow (Keeper не выступает прокси к Vault-движку, не парсит response). Симметрично для `auth_method: approle` плагин делает `auth/<mount>/login` сам.

**Params** приезжают через env `SOUL_SSH_VAULT_PARAMS` (JSON по `schema.json`, симметрично `SOUL_SSH_STATIC_PARAMS`). SshProvider-контракт не несёт per-request параметров провайдера, поэтому конфиг едет на старте процесса, как и путь к сокету (`SOUL_PLUGIN_SOCKET`).

```yaml
# soul-ssh-vault/manifest.yaml
kind: ssh_provider
protocol_version: 1
namespace: ssh
name: vault

required_capabilities:
  - network_outbound          # HTTP-вызовы Vault API
  - vault_access              # плагин САМ ходит в Vault (variant B)

side_effects: []              # ssh_provider не трогает локальные ресурсы host-а

spec:
  provider_kind: vault_ssh_ca
  params_schema:
    type: object
    required: [vault_addr, role]
    properties:
      vault_addr:  { type: string, pattern: "^https?://" }
      vault_mount: { type: string, default: "ssh" }
      role:        { type: string }
      auth_method: { type: string, enum: [token, approle], default: token }
      token:       { type: string }                    # SENSITIVE; для auth_method=token
      approle:
        type: object
        properties:
          role_id:   { type: string }
          secret_id: { type: string }                  # SENSITIVE
          mount:     { type: string, default: "approle" }
      valid_principals:                                # локальный allowlist поверх Vault role
        type: array
        items: { type: string }
      deny:                                            # deny-list пар (host, user); пустой = allow-all
        type: array
        items:
          type: object
          properties:
            host: { type: string }
            user: { type: string }
```

Пример `SOUL_SSH_VAULT_PARAMS` (JSON, передаётся в env плагина при fork-е):

```json
{
  "vault_addr": "https://vault.internal:8200",
  "vault_mount": "ssh",
  "role": "keeper-push",
  "auth_method": "approle",
  "approle": { "role_id": "...", "secret_id": "...", "mount": "approle" },
  "valid_principals": ["soul", "deploy"],
  "deny": [{ "user": "root" }]
}
```

### `kind: ssh_provider` (static-key)

Reference-реализация SshProvider (пилот тиража) — [`examples/module/soul-ssh-static/`](../../examples/module/soul-ssh-static). Static-key: долгоживущий приватный ключ на keeper-host-е, его публичная часть — в `authorized_keys` целевых хостов ([push.md → static key](push.md#аутентификация-ssh--pluggable-provider)). `Sign` отдаёт готовую пару (`certificate=""`), `Authorize` — deny-list (по умолчанию allow-all, для dev/test). Params провайдера (`key_path` / deny-list) приезжают на старте через env (SshProvider-контракт не несёт per-request параметров провайдера; `vault_ref` резолвится `keeper.push` в `key_path` до запуска плагина — A-flow, параллель с cloud credentials).

```yaml
# soul-ssh-static/manifest.yaml
kind: ssh_provider
protocol_version: 1
namespace: ssh
name: static

required_capabilities:
  - vault_access            # опциональный резолв ключа из Vault KV

side_effects: []            # ssh_provider не трогает локальные ресурсы host-а

spec:
  provider_kind: static_key
  params_schema:
    type: object
    oneOf:                  # ровно один источник ключа
      - required: [key_path]
      - required: [vault_ref]
    properties:
      key_path:  { type: string }
      vault_ref: { type: string }
      deny:                 # deny-list пар (host, user); пустой = allow-all
        type: array
        items:
          type: object
          properties:
            host: { type: string }
            user: { type: string }
```

### `kind: ssh_provider` (Teleport)

Реальная реализация — [`examples/module/soul-ssh-teleport/`](../../examples/module/soul-ssh-teleport). Teleport-провайдер: плагин ходит в Teleport Auth сам (creds-flow B, симметрично Vault SSH CA), вызывает `GenerateUserCerts(SSHPublicKey)` для подписи Keeper-ephemeral pubkey, возвращает только `certificate` (`private_key=""`) и заполняет only-add поле `SignReply.proxy_jump` endpoint-ом Teleport-proxy.

**Канонический режим — Keeper-ephemeral** (security-first, PM-decision): Teleport API (`api/client.GenerateUserCerts`) принимает `SSHPublicKey []byte` и возвращает signed SSH-cert на этот pubkey — variant A (Vault-style) укладывается на Teleport без отклонения от решения key-ownership.

**Creds-flow — variant B** (плагин сам аутентифицируется в Teleport): identity-file (`tctl auth sign`) либо tbot-сокет, путь приезжает в `SOUL_SSH_TELEPORT_PARAMS`. Capability `vault_access` НЕ требуется — Teleport не Vault; credentials живут в файле/сокете.

**Dispatcher proxy_jump support — LIVE (S3).** `keeper.push` уважает `SignReply.proxy_jump`: при непустом значении dispatcher открывает SSH-client к proxy-хопу теми же `cfg.Auth` (signed user-cert на ephemeral keypair), на этом client-е запрашивает `direct-tcpip`-канал до `host:port` целевого хоста и поверх канала проводит второй SSH-handshake (эквивалент `ssh -J <proxy> <host>`). Cert от Teleport/Vault SSH CA авторизует пользователя на обоих хопах — это canonical Teleport-flow. Host-cert verification (`ssh.CertChecker`, fail-closed без CA) работает на ОБА хопа: по умолчанию используется один `HostAuthority.CAPublicKey` (типовой случай — один host-CA подписывает host-cert-ы и proxy, и target); отдельный proxy-CA — расширение через `DialConfig.ProxyHostAuthority` (поле без UI пока, активируется при операторском запросе). При пустом `proxy_jump` — прямой `net.Dial(host:port)` (S0-flow без регрессий). Реализация — [`keeper/internal/push/session.go`](../../keeper/internal/push/session.go) (функция `Dial`, ветка `dialViaProxy`).

**Params** приезжают через env `SOUL_SSH_TELEPORT_PARAMS` (JSON по `schema.json`, симметрично `SOUL_SSH_VAULT_PARAMS` / `SOUL_SSH_STATIC_PARAMS`).

```yaml
# soul-ssh-teleport/manifest.yaml
kind: ssh_provider
protocol_version: 1
namespace: ssh
name: teleport

required_capabilities:
  - network_outbound          # gRPC-вызовы Teleport Auth API через Teleport-proxy

side_effects: []              # ssh_provider не трогает локальные ресурсы host-а

spec:
  provider_kind: teleport
  params_schema:
    type: object
    required: [proxy_addr]
    properties:
      proxy_addr:    { type: string }            # Teleport proxy host:port (едет в SignReply.proxy_jump)
      cluster_name:  { type: string }            # multi-cluster trust (опц.)
      identity_file: { type: string }            # путь к Teleport identity-file (creds-flow B)
      tbot_socket:   { type: string }            # либо tbot-сокет (взаимоисключающе с identity_file)
      roles:                                     # запрашиваемые Teleport-роли (опц.)
        type: array
        items: { type: string }
      valid_principals:                          # локальный allowlist поверх Teleport-роли
        type: array
        items: { type: string }
      deny:                                      # deny-list пар (host, user); пустой = allow-all
        type: array
        items:
          type: object
          properties:
            host: { type: string }
            user: { type: string }
```

Пример `SOUL_SSH_TELEPORT_PARAMS` (JSON, передаётся в env плагина при fork-е):

```json
{
  "proxy_addr": "teleport.example.com:3023",
  "cluster_name": "root",
  "identity_file": "/etc/teleport/keeper-push.identity",
  "roles": ["node-admin"],
  "valid_principals": ["soul", "deploy"],
  "deny": [{ "user": "root" }]
}
```

### `kind: soul_beacon` (ZFS pool health, ADR-030 V5-2)

```yaml
kind: soul_beacon
protocol_version: 1

namespace: community
name: zfs-degraded

required_capabilities:
  - exec_subprocess           # запуск `zpool status`

side_effects: []              # read-only, beacon не мутирует хост

spec:
  params_schema:
    type: object
    required: [pool]
    properties:
      pool: { type: string }  # имя ZFS-пула для опроса
```

SDK — [`sdk/beacon`](../../sdk/beacon/beacon.go). Минимальный код плагина:

```go
package main

import (
    "context"

    pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
    "github.com/souls-guild/soul-stack/sdk/beacon"
    "google.golang.org/protobuf/types/known/structpb"
)

type ZFSDegraded struct { beacon.BaseBeacon }

func (z *ZFSDegraded) Check(_ context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
    pool := req.GetParams().GetFields()["pool"].GetStringValue()
    state := "ok"
    if poolIsDegraded(pool) {
        state = "degraded"
    }
    payload, _ := structpb.NewStruct(map[string]any{"pool": pool})
    return &pluginv1.CheckReply{State: state, Payload: payload}, nil
}

func main() { beacon.Serve(&ZFSDegraded{}) }
```

`Check` вызывается Soul-scheduler-ом per-tick (interval из `VigilDef.interval`); смена `state` → `PortentEvent.payload.custom` ([V5-1 typed payload](../module/core/beacon/README.md#typed-portentpayload-v5-1)).

## Каталог плагинов в `keeper.yml`

Объявляется в блоке `plugins:` ([config.md](config.md)):

```yaml
plugins:
  cloud_drivers:
    - { name: aws, source: "git@github.com:soul-stack-ecosystem/soul-cloud-aws.git", ref: v2.0.0 }
    - { name: yc,  source: "git@github.com:our-company/soul-cloud-yc.git",          ref: v0.3.1 }

  ssh_providers:
    - { name: vault-ssh, source: "git@github.com:soul-stack-ecosystem/soul-ssh-vault.git", ref: v1.0.0 }
    - { name: static,    source: "git@github.com:soul-stack-ecosystem/soul-ssh-static.git", ref: main }

  soul_modules:                     # SoulModule-плагины (ADR-065): резолв тем же резолвером, допуск тем же Sigil-флоу
    - { name: redis, source: "git@github.com:souls-guild/soul-mod-community-redis.git", ref: v1.2.0 }
```

Версия плагина — **git ref** (tag или branch) согласно [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте). Никаких semver-range.

### Что Keeper делает резолвером (git-verified, F-fetch)

Keeper при старте резолвит каталог сам — через [`keeper/internal/plugingit`](../../keeper/internal/plugingit) ([ADR-026(g)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), A1-S1). Для каждой записи:

1. `validateGitScheme(source)` — scheme-allowlist: prod `https://` / `ssh://` / scp-форма `user@host:path`; `file://` — только под env-флагом `SOUL_STACK_ALLOW_FILE_REPOS=1` (dev/test). Иная схема → `ErrSourceUnavailable`.
2. shallow `clone` (`Depth=1`) рабочего клона в `<work_root>/<name>/` (СТРОГО вне `cache_root`), либо `fetch`, если клон уже есть. Транспорт — **go-git** (pure-Go, без форка системного `git`); auth — SSH-agent для ssh/scp-форм.
3. `ResolveRevision(<ref>^{commit})` → 40-hex `commit_sha` (кандидаты: tag → remote-tracking-ветка → полный hash; нерезолвящийся → `ErrRefNotResolved`).
4. detached-HEAD `checkout` на `commit_sha` (go-git hooks не исполняет).
5. parse `manifest.yaml` checkout-а (нет → `ErrManifestNotFound`) → по `kind` → конвенция `dist/<binary-name>` (бинарь уже собран, **F-fetch — Keeper не компилирует**; нет / не обычный файл → `ErrArtifactNotFound`).
6. atomic-извлечение manifest+бинаря в иммутабельный слот `<cache_root>/<ns>-<name>/<commit_sha>/` (staging на том же fs → fsync → `rename`); `commit_sha`-слот иммутабелен (повторный резолв того же коммита — skip).
7. atomic-переключение symlink `<cache_root>/<ns>-<name>/current → <commit_sha>`.
8. `binary_sha256 := sha256(<слот>/<binary-name>)`.

Per-entry резолв **fail-closed**: сломанная запись (любой sentinel выше / недоступный remote / таймаут) → per-entry warning, Keeper не падает. При apply-операциях запускается плагин из активного слота (`current`).

git-стек — go-git by-design: hooks не исполняются, submodules не рекурсятся, `ext::` отсутствует, `file://` заперт scheme-allowlist-ом; **нет runtime-зависимости от бинаря `git`** на keeper-host. Hardening: `Depth=1` + context-timeout (`plugins.fetch_timeout`, дефолт 120s), `plugins.work_root` СТРОГО вне `cache_root`, **size-limit по объёму** (`plugins.max_clone_size_mb` на рабочее дерево клона + `plugins.max_artifact_size_mb` на бинарь, fail-closed — см. ниже). git-egress — **HIGH security-риск**: остаток обязательного security-pass-а перед прод (`noexec` на слот / sandbox git-операций) — отложен. **GC старых `commit_sha`-слотов** (несколько слотов на пару после ротаций `ref`/коммитов) — отложен, кандидат на Reaper-правило (имя — отдельный propose-and-wait).

**Size-limit (ADR-026(g), fail-closed).** `source` operator-asserted, но репозиторий недоверенный, а `fetch_timeout` ограничивает egress лишь по времени. Два cap-а по объёму защищают диск keeper-host-а от DoS враждебным/огромным репо:

- `plugins.max_clone_size_mb` (дефолт 1024 MiB) — суммарный размер рабочего дерева клона (du-подобный walk checkout + `.git`), проверяется **после checkout, до извлечения артефакта**. Превышение → `ErrCloneTooLarge` + cleanup `work_root/<name>`.
- `plugins.max_artifact_size_mb` (дефолт 256 MiB) — размер бинаря `dist/<binary-name>`, проверяется по `os.Stat` перед копированием и `io.LimitReader`-ом во время copy (defense-in-depth). Превышение → `ErrArtifactTooLarge`, слот не материализуется.

Оба sentinel-а — per-entry fail-closed (warning, как `ErrArtifactNotFound`/`ErrSourceUnavailable`): сломанная запись скипается, слот не создаётся → плагину **нечего допускать** через Sigil.

Config-поля резолвера в [`config.md → plugins`](config.md):

| Поле | Дефолт | Смысл |
|---|---|---|
| `plugins.cache_root` | `pluginhost.DefaultCacheRoot` | Корень R-nested-кеша слотов (`<ns>-<name>/<commit_sha>/`). Абсолютный путь. |
| `plugins.work_root` | `/var/lib/soul-stack-keeper/plugin-src` | Корень рабочих git-клонов резолвера. **СТРОГО вне `cache_root`** (`.git`/checkout не попадают в читаемый кеш-каталог). Абсолютный путь. |
| `plugins.fetch_timeout` | `120s` | Потолок одной цепочки go-git-операций резолва (clone→fetch→checkout). git-egress — внешний вызов, таймаут обязателен. |
| `plugins.max_artifact_size_mb` | `256` | Потолок размера бинаря `dist/<binary-name>` (size-limit hardening). Превышение → `ErrArtifactTooLarge`, fail-closed. |
| `plugins.max_clone_size_mb` | `1024` | Потолок размера рабочего дерева клона (checkout + `.git`). Превышение → `ErrCloneTooLarge` + cleanup, fail-closed. |

Каталог `SoulModule`-плагинов — **`plugins.soul_modules[]`** тем же форматом (`{name, source, ref}`; [ADR-065](../adr/0065-core-module-installed.md), amendment [ADR-020](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)): резолвится тем же резолвером в `cache_root`, допускается тем же Sigil-флоу. Раздача на Soul-хосты — server-streaming RPC `FetchModule` (content-addressed: Keeper отдаёт только байты, чей sha256 есть в активном допуске `kind: soul_module`) + core-модуль `core.module.installed` (см. [`../soul/modules.md`](../soul/modules.md)).

## Преимущества единой инфраструктуры

- Один SDK на язык (Go / Rust / Python) покрывает все три kind-а плагинов через общий `sdk/handshake/` helper.
- Один способ распространения и кеширования: git-резолв каталога `plugins.*` в кеш Keeper-а + кеш host-а по SHA-256 (на Soul — через `FetchModule`, [ADR-065](../adr/0065-core-module-installed.md)) + Sigil-верификация перед запуском (см. [Integrity-model](#integrity-model)).
- Один способ конфигурирования (manifest + JSON Schema параметров).
- Третьи стороны могут выпускать собственные плагины (cloud-провайдер для нишевого облака, SSH-провайдер для нестандартной CA, custom-модуль конкретной компании) без модификации ядра Soul Stack.
- Один формат audit-trail (`side_effects` → audit-event).

## См. также

- [architecture.md → ADR-020](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle) — нормативное решение по всему этому документу.
- [architecture.md → Plugin-инфраструктура](../architecture.md#plugin-инфраструктура) — высокоуровневый обзор.
- [`../soul/modules.md`](../soul/modules.md) — `SoulModule`-специфика, раскладка на хосте Soul, кеш.
- [cloud.md](cloud.md) — использование `CloudDriver`.
- [push.md](push.md) — использование `SshProvider`.
- [config.md](config.md) → `plugins:` — формат каталога.
- [architecture.md → ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — `ref:` как версия плагина, исключение для `protocol_version`.
- [architecture.md → ADR-011](../adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) — `proto/plugin/` подмодуль и `sdk/handshake/`.
- [architecture.md → ADR-016](../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — почему не зависим от `hashicorp/go-plugin`.
- [naming-rules.md](../naming-rules.md) — `Manifest`, `Handshake`, `kind`, `SoulModule`, `CloudDriver`, `SshProvider`, capabilities-enum, resource-types-enum.
