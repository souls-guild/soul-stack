# ADR-020. Plugin-инфраструктура: формат manifest, handshake, lifecycle

- **Контекст.** В Soul Stack три категории плагинов с разными service-контрактами (`SoulModule`, `CloudDriver`, `SshProvider`), но **единая инфраструктура** — handshake, способ запуска, формат манифеста, версионирование (см. раздел [«Plugin-инфраструктура»](../architecture.md#plugin-инфраструктура), [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). На момент фиксации эта инфраструктура описана только эскизно:
  - [§«Манифест модуля»](../architecture.md#манифест-модуля) приведён как пример-черновик, с явной open под-Q «отдельный `manifest.yaml` рядом с бинарём vs первый RPC-метод `Manifest()`».
  - [§«Протокол модулей — gRPC over stdio (HashiCorp-style)»](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style) — общая ссылка на модель HashiCorp `go-plugin`, с open под-Q «имя файла и точная версия протокола».
  - Формат handshake-строки, перечень `required_capabilities`, грамматика `side_effects`, lifecycle плагина, путь к сокету, политика TLS — нормативно нигде не зафиксированы.

  Без нормативной фиксации нельзя ни закрыть `proto/plugin/v1/*.proto` (следующая задача после этого ADR), ни написать единый handshake-helper в `sdk/handshake/`, ни реализовать статическую валидацию destiny в `soul-lint` (последнее — требование [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).

- **Решение.**

  **(a) Manifest — статический `manifest.yaml` в репо плагина.** Файл лежит в корне репозитория плагина и поставляется рядом с бинарём (в кеше Keeper-а / на хосте Soul). `soul-lint` парсит его **без запуска бинаря** — это прямое требование [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (статическая валидация destiny без поднятия плагин-процесса).

  Альтернатива «RPC-only через метод `Manifest()`» отвергнута: ломает offline-валидацию `soul-lint` (нужно сначала закачать и запустить плагин). Альтернатива «гибрид» отвергнута как переусложнение без выгоды.

  Drift «manifest рассинхронизирован с реальным кодом плагина» — реальный риск; снижается через self-test плагина (вызов `Apply` с параметрами вне input-схемы должен возвращать `INVALID_ARGUMENT`) и опциональным generated manifest (`soul-mod gen-manifest --check`) — расширение SDK post-MVP без breaking changes.

  **(b) Handshake — JSON в одну строку с магическим префикс-полем.** Плагин при запуске пишет в stdout **ровно одну строку**:

  ```json
  {"soul_stack":"plugin-v1","protocol_version":1,"kind":"soul_module","network":"unix","address":"/var/run/soul-stack/plugins/wb-haproxy-12345.sock","server_cert":""}
  ```

  - Магическое поле `"soul_stack":"plugin-v1"` — sanity-check. Host игнорирует все строки в stdout до первой строки с этим полем (защита от случайного `fmt.Println` в `init()` плагина или вывода библиотек).
  - Поля: `soul_stack` (constant string, `"plugin-v1"`), `protocol_version` (int), `kind` (enum, см. (e)), `network` (enum `unix`, расширение до `named_pipe` / `tcp` — post-MVP), `address` (path к сокету), `server_cert` (base64-PEM или пустая строка; зарезервировано под опциональный mTLS post-MVP, см. (h)).
  - Расширение через новые **опциональные** ключи (`features`, `capabilities`, …) — без breaking changes.
  - **Не используем `hashicorp/go-plugin` библиотеку как зависимость.** Их формат 6-полевой pipe-строки избыточен и негибок; MPL-2.0 копилефт не нужен (см. [ADR-016](0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)). Заимствуется только модель «one-line handshake → gRPC-over-socket».

  Отвергнуты: (а) HashiCorp 6-полевая pipe-строка (избыточные поля + жёсткий формат), (в) минимальный pipe (расширение = breaking), (г) framed-обмен (нечитаемо, не ложится на модель one-line handshake).

  **(c) Versioning — `protocol_version` дублируется в manifest и handshake.** Один int, два места:
  - В `manifest.yaml` — для статического `soul-lint` (без запуска плагина).
  - В JSON-handshake-строке — для runtime sanity-check **до** открытия gRPC-канала.

  Host (`keeper` / `soul` / `soul-lint`) держит константу `SupportedProtocolVersions = [1]`. Три cross-check-а:
  - `handshake.protocol_version != manifest.protocol_version` → плагин некорректен, отказ запуска (drift внутри плагина).
  - `protocol_version` вне `SupportedProtocolVersions` → hard fail с сообщением `protocol_version=N, host supports [...]`.
  - `manifest.kind != handshake.kind` → отказ запуска (drift).

  Жёсткое соответствие **`protocol_version: N` ↔ `proto/plugin/vN/`** (один go.mod-подмодуль по [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). Эволюция: добавление `proto/plugin/v2/` → host версии N+1 поддерживает `[1, 2]` (forward-compat only-add, аналог [ADR-012(g)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) для plugin-протокола; никогда не удалять/не reuse field-номера в `proto/plugin/v1/`).

  `protocol_version` — **compat-флаг API, не версия артефакта**. Это уже артикулированное исключение `protocol_version` в [ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте); ADR-020 не вводит новое исключение, а только фиксирует его место в plugin-инфраструктуре.

  Отвергнуты: (а) только handshake / (г) gRPC reflection — оба ломают [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (offline-валидация); (б) только manifest — drift не ловится.

  **(d) Socket + lifecycle.**
  - **Тип сокета:** Unix domain socket only в MVP. Поле `network` в handshake-JSON допускает расширение enum-а (`unix | named_pipe | tcp`) — Windows-поддержка post-MVP без breaking changes.
  - **Путь к сокету:** host передаёт через env-var **`SOUL_PLUGIN_SOCKET`**. Директория — `/var/run/soul-stack/plugins/<namespace>-<name>-<pid>.sock` для Soul-host-а, `/var/run/soul-stack-keeper/plugins/<namespace>-<name>-<pid>.sock` для Keeper-host-а; mode `0700`, owned by service user (`soul` или `keeper`). SDK тривиально читает env-var и открывает сокет.
  - **Запуск:** host fork-ает плагин-процесс, передаёт `SOUL_PLUGIN_SOCKET`, читает stdout до первой строки с `"soul_stack":"plugin-v1"`. Все строки до этой — игнорируются (но логируются на debug-уровне).
  - **Shutdown:** host шлёт SIGTERM; SDK предоставляет signal-handler-helper, плагин завершает текущие RPC и выходит. RPC `Shutdown()` в proto-контракт MVP не вводится (расширение в `proto/plugin/v2/` если понадобится). Grace 10s — если плагин не завершился — SIGKILL.
  - **Lifecycle:** **one-shot** — запуск на каждый Apply, выход после. Long-lived (один процесс на серию вызовов) — отдельным ADR при необходимости (профилирование cold-start или появление CloudDriver с батч-операциями к одному cloud-API-токену).
  - **Timeouts (defaults):** startup `10s` (handshake-строка должна появиться), shutdown grace `10s` (после SIGTERM → SIGKILL). Конкретные значения настраиваются через `keeper.yml` / `soul.yml` блок `plugin_runtime:` — нормативная спецификация в [`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) и [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime).

  **(e) Manifest формат — единая схема с `kind:`-дискриминатором.** Один и тот же YAML-формат для всех трёх типов плагинов; различия в `spec:`-секции:

  ```yaml
  # soul-mod-haproxy/manifest.yaml
  kind: soul_module                 # дискриминатор: soul_module | cloud_driver | ssh_provider
  protocol_version: 1
  namespace: wb
  name: haproxy
  required_capabilities: [run_as_root]
  side_effects:
    - { service: haproxy }
    - { file: /etc/haproxy/haproxy.cfg }
  spec:                             # kind-specific блок
    states:
      running:
        input:
          name:    { type: string, required: true }
          enabled: { type: boolean, default: true }
      stopped:
        input:
          name: { type: string, required: true }
  ```

  Общие поля в корне: `kind`, `protocol_version`, `namespace`, `name`, `required_capabilities`, `side_effects`. Kind-specific — в `spec:`:
  - `spec.states` (map<state-name, {input}>) — для `kind: soul_module`.
  - `spec.profile_schema` (JSON Schema) — для `kind: cloud_driver` (схема VM-профиля, см. [`docs/keeper/cloud.md`](../keeper/cloud.md)).
  - `spec.provider_kind` (enum / string) — для `kind: ssh_provider` (Vault SSH CA / static / Teleport / ...).

  В `sdk/handshake/` — один Go-тип `Manifest` с `oneof` подсообщениями `SoulModuleSpec` / `CloudDriverSpec` / `SshProviderSpec` (proto-стиль). Эволюция новых kind-ов (`secrets_provider` и т.п.) — добавление варианта в enum без breaking changes.

  Имя бинаря (`soul-mod-*` / `soul-cloud-*` / `soul-ssh-*`) — **convention, не контракт**. Cross-check `manifest.kind == "soul_module"` && имя бинаря `soul-mod-*` → warn в лог при расхождении, не fail (alias/symlink — допустимы).

  Отвергнуты: три отдельных формата (drift общих полей при эволюции); гибрид (эквивалент (е)); имя бинаря-дискриминатор (слабый дискриминатор).

  **(f) `required_capabilities` — enum с фиксированным стартовым набором.** Плагин декларирует, что ему нужно от системы host-а. `soul-lint` статически проверяет: `required_capabilities` плагина ⊆ `plugin_runtime.allowed_capabilities` host-а ([`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) / [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime)). Mismatch → ошибка валидации destiny **до запуска**.

  Стартовый набор (closed enum, MVP):
  | Capability | Смысл |
  |---|---|
  | `run_as_root` | Host-процесс (`soul` / `keeper`) должен иметь UID 0 при запуске плагина. |
  | `network_outbound` | Плагин делает исходящие сетевые вызовы (cloud API, vault, package mirror). |
  | `network_inbound` | Плагин слушает порт (редкий случай, для тестовых helper-плагинов). |
  | `vault_access` | Плагин обращается к Vault через клиентский helper SDK. |
  | `fs_write_root` | Плагин пишет за пределы `/var/lib/soul-stack/`. |
  | `exec_subprocess` | Плагин запускает внешние команды через `os/exec`. |

  Расширение списка — через PR в `proto/plugin/vN/manifest.proto`, не breaking. Freeform-extensions с префиксом `x-` (open-ended capabilities) **в MVP отвергнуты** — добавится при первом реальном запросе.

  **(g) `side_effects` — strict-контракт.** Плагин обязан перечислить **все ресурсы**, которые он трогает (touched resources). Грамматика: список записей вида `{<resource-type>: <value>}`, где `<resource-type>` — closed enum:

  | Resource type | Значение |
  |---|---|
  | `service` | имя сервиса (`haproxy`, `redis-server`). |
  | `file` | абсолютный путь к файлу (`/etc/haproxy/haproxy.cfg`). |
  | `package` | имя пакета OS (`haproxy`, `nginx`). |
  | `port` | tcp/udp-порт как int (`80`, `443`). |
  | `user` | имя пользователя OS (`postgres`). |
  | `group` | имя группы OS. |
  | `directory` | абсолютный путь к директории. |
  | `cron` | имя cron-задачи. |
  | `mount` | mountpoint (`/var/lib/data`). |

  Расширение enum-а — через PR в `proto/plugin/vN/`, не breaking. Поведение host-а:
  - **Audit-trail:** каждый touched ресурс пишется в audit-event с указанием плагина и `apply_id`.
  - **Conflict detection:** два плагина в одном прогоне, претендующие на тот же ресурс, → warning или fail (политика resolution — `plugin_runtime.conflict_policy`, нормативно: [`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) и [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime)).
  - **Runtime-нарушение:** плагин трогает ресурс, не объявленный в `side_effects` → шаг помечается `failed`, причина `policy_violation` отражается в диагностическом канале `TaskEvent` / `RunResult` (точная форма поля — отдельная задача, см. backlog), и event `task.policy_violation` пишется в общий audit-pipeline ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) — нормирует storage / schema / write-path для всех audit-событий, включая `side_effects`-нарушения). В этом ADR ввод нового поля или нового статуса в [ADR-012](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) **не закрепляется** — это изменение proto-контракта, отдельное propose-and-wait при закрытии `proto/plugin/v1/`.

  Wildcard-значения (`file: /etc/haproxy/**`) и условные `side_effects` (`when: …`) **в MVP отвергнуты** — добавится при первом реальном запросе.

  **(h) TLS на plugin-сокете — нет в MVP.** Безопасность через file-permissions: Unix domain socket в host-managed директории mode `0700`, owned by service user. Другие процессы физически не могут открыть сокет.

  HashiCorp использует mTLS на TCP-loopback — там это оправдано (любой процесс на хосте может connect к loopback-порту). У нас Unix-socket — этой угрозы нет. Цена mTLS на каждый одноразовый плагин-запуск: +50–150 ms на TLS-handshake, без выгоды над file-permissions.

  Расширение к mTLS post-MVP — **без breaking changes**: поле `server_cert` (base64-PEM) уже зарезервировано в JSON-handshake; включение через `plugin_runtime.enable_tls: true` ([`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) / [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime)).

- **Consequences.**
  - **`proto/plugin/v1/*.proto`** заводится с пятью файлами: `handshake.proto` (формат JSON-строки как proto-стиль message, для генерации Go-структуры в `sdk/handshake/`), `manifest.proto` (typed `Manifest` с `oneof spec`), и три service-файла — `soulmodule.proto`, `clouddriver.proto`, `sshprovider.proto`. **В этом ADR — не закрывается**, отдельная задача после ADR-020.
  - **`sdk/handshake/`** — единый Go-helper для всех трёх kind-ов: читает env-var `SOUL_PLUGIN_SOCKET`, пишет JSON-handshake в stdout, открывает Unix-socket, регистрирует gRPC-server, обрабатывает SIGTERM.
  - **`soul-lint`** обязан понимать manifest для статической валидации destiny: неизвестный модуль, неизвестное `state`, неверные параметры (`input`-schema), `required_capabilities` ⊄ host's `allowed_capabilities`, неизвестный `kind`.
  - В **`keeper.yml`** / **`soul.yml`** появляется блок `plugin_runtime:` (`socket_dir`, `startup_timeout`, `shutdown_grace`, `allowed_capabilities`, `conflict_policy`, опц. `enable_tls`) — нормирован в [`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) и [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime), с per-поле hot-reload-политикой.
  - **`docs/keeper/plugins.md`** переписывается нормативно: таблицы полей manifest, JSON-схема handshake, диаграмма lifecycle, enum-таблицы capabilities и side_effects, полные примеры manifest для трёх kind-ов.
  - **`docs/soul/modules.md`** дополняется кратким разделом про SoulModule manifest, cross-link на `docs/keeper/plugins.md` как нормативный источник.
  - **`docs/naming-rules.md`** дополняется именами `kind`, `Manifest`, `Handshake`, capabilities-enum, resource-types-enum, `plugin_runtime`.
  - Закрывает две open под-Q в [§«Модель модулей»](../architecture.md#модель-модулей): «отдельный `manifest.yaml` vs RPC `Manifest()`» (в [§«Манифест модуля»](../architecture.md#манифест-модуля)) и «имя файла и точная версия протокола» (в [§«Протокол модулей — gRPC over stdio (HashiCorp-style)»](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style)).
  - `examples/` — обновляются после закрытия `proto/plugin/v1/*.proto` (не в этом ADR).

- **Trade-offs.**
  - **Drift manifest ↔ код плагина.** Реальный риск: автор плагина забывает обновить `manifest.yaml` после правки `Apply`. Mitigation — self-test (запуск с invalid input → `INVALID_ARGUMENT`) и post-MVP `soul-mod gen-manifest --check` из SDK. Альтернатива «RPC-only Manifest()» убирает drift, но ломает offline-валидацию `soul-lint` — это более дорогая цена.
  - **One-shot lifecycle vs cold-start cost.** Каждый Apply поднимает плагин-процесс с нуля; для long-running scenarios с десятками вызовов одного плагина это overhead 50–200 ms × N. Приемлемо для MVP; long-lived — отдельным ADR при первом профиле производительности, который упрётся в это.
  - **`server_cert` в handshake-JSON всегда пустая в MVP.** Cruft (одно неиспользуемое поле), но обеспечивает forward-compat для будущего mTLS без правки proto/JSON-формата.
  - **Closed enums capabilities / side_effects.** Любой новый capability или resource-type требует PR в proto-контракт. Это сознательная плата за статическую валидацию `soul-lint` (open-ended `x-` ключи делают валидацию бессмысленной). Расширение enum — minor по [ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) (Go-library tag), не breaking.
  - **File-permissions вместо mTLS.** На multi-tenant хостах с непривилегированными процессами от других пользователей — file-perms эквивалентен mTLS (никто не откроет сокет 0700 от чужого user). На root-compromise — оба варианта проиграны одинаково. Соответствие угрозам Soul Stack — корректно.

- **Amendment (2026-05-26, SshProvider — MVP-набор закрыт).** По итогам пилотов `keeper.push` зафиксирован финальный набор SSH-провайдеров и решения по трём общим механикам (credentials-flow, key-ownership, params-delivery). Решения приняты пользователем 2026-05-26.
  - **(i) MVP-набор `SshProvider` — три плагина, закоммичены и работают.** `soul-ssh-static` (commit `4f95ef6`) — reference; `soul-ssh-vault` (commit `3642520`, dispatcher S2 ephemeral keypair); `soul-ssh-teleport` (commit `af27678`, only-add в proto: `SignReply.proxy_jump` field 4). Бинари — `soul-ssh-{static,vault,teleport}`, имена `kind: ssh_provider` в manifest, поле `spec.provider_kind ∈ {static_key, vault_ssh_ca, teleport}` (closed enum для `kind: ssh_provider`, симметрично [ADR-026(c)](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) для cloud-driver-ов). Расширение enum — propose-and-wait + PR в [keeper/plugins.md → Manifest](../keeper/plugins.md#manifest) и [naming-rules.md](../naming-rules.md).
  - **(j) Credentials-flow для Vault SSH CA — Вариант B (плагин сам ходит в Vault через `vault_access` capability).** Расходится с cloud-Вариантом A ([ADR-017 amendment (d)](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)) **сознательно**: cloud-creds — это **static KV-секрет** (Keeper резолвит KV → плагин получает plaintext), для которого Variant A корректен. `ssh/sign` — это **операция Vault** (минт сертификата по pubkey оператором), не чтение значения; Вариант A для операции означал бы, что Keeper становится Vault-SSH-proxy, дублирующим логику Vault SSH-engine. Вариант B оставляет операцию там, где она нативна. Capability `vault_access` остаётся в манифесте `soul-ssh-vault` (в отличие от cloud-плагинов, где она снята).
  - **(k) Key-ownership для Vault SSH CA / Teleport — Keeper-ephemeral (приватник не покидает Keeper).** Keeper-side (`keeper.push`-dispatcher) генерит ephemeral SSH-keypair per-session, шлёт **только public key** в `SignRequest.public_key`. Плагин подписывает pubkey в Vault SSH-CA / Teleport-CA и возвращает только `certificate` (+ опц. `proxy_jump` для Teleport, см. (i)); поле `private_key` в `SignReply` — **всегда пустое** для CA-провайдеров (заполняется только `static_key`, который сам — материал ключа). Обоснование — security-first (CLAUDE.md): чем меньше точек, через которые проходит приватник, тем меньше поверхность утечки; провайдер вообще не видит приватник пользователя.
  - **(l) Params-delivery — env-convention per-plugin.** Параметры провайдера (Vault mount-path, Teleport-proxy URL, …) host передаёт в плагин через env-переменные с зафиксированными именами:
    - `SOUL_SSH_STATIC_PARAMS` — для `soul-ssh-static` (JSON, форма провайдера).
    - `SOUL_SSH_VAULT_PARAMS` — для `soul-ssh-vault` (JSON, `{mount, role, ttl, ...}`).
    - `SOUL_SSH_TELEPORT_PARAMS` — для `soul-ssh-teleport` (JSON, `{proxy_addr, role, ...}`).

    Generic-механизм (handshake-`PluginParams`-поле в JSON-handshake) **отложен post-MVP** — пилоты не показали необходимости (формы параметров расходятся между провайдерами, типовой JSON-blob в env проще, чем доводить общий schema-валидатор). При появлении четвёртого провайдера с пересекающимися параметрами — возврат к решению через propose-and-wait.
  - **(m) Открытое (S3 dispatcher `proxy_jump` support).** Teleport-пилот возвращает в `SignReply.proxy_jump` адрес bastion-а, через который ставится SSH-сессия, но dispatcher (`keeper/internal/push`) поле **ИГНОРИРУЕТ** — `net.Dial(host:port)` идёт **напрямую**. Полный Teleport-через-bastion флоу требует dispatcher proxy_jump support (отдельный слайс в работе параллельно этой канон-фиксации). Пока пилот применим **только к хостам с прямой SSH-доступностью**; Teleport-через-bastion станет рабочим после dispatcher-слайса. Это **не Sshprovider-проблема** — плагин корректно возвращает поле, контракт only-add закрыт; вопрос — в hosts-side флоу `keeper.push`.
