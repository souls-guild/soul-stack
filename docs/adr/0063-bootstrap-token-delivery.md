# ADR-063. core.bootstrap.delivered — keeper-side доставка bootstrap-токена по SSH

> **Статус: active.** Дизайн architect-а (A1 «тонкая доставка»), имя `core.bootstrap.delivered` через propose-and-wait (подтверждено пользователем). Канон фиксируется docs-first ДО кода; этот ADR **amends [ADR-017](0017-keeper-side-core.md), [ADR-061](0061-onboarding-await-and-midrun-reresolve.md), [ADR-015](0015-core-modules-mvp.md)**.
>
> **Прогресс имплементации.** Слайс-пилот реализован: модуль + условная регистрация + Deps + scenario-swap (`keeper.push.applied`-заглушка → `core.bootstrap.delivered`) + unit-тесты. **C1 (cloud-init CA-signed host-key) и live-e2e — следующий слайс, НЕ в этом** (см. §Границы MVP). До C1 live-прогон direct-режима оборвётся: `push.Dial` реджектит host-cert свежей VM, у которой cloud-init поставил голый (не CA-signed) host-key.
>
> **Amendment (Teleport by-name transport) — реализован (пилот).** Второй транспортный режим `transport: teleport` (by-name через Teleport Proxy, host-verify через Teleport identity-file, C1 неприменим) + keeper-side Teleport-Dialer + retry-до-join + wire-up daemon (teleport-режим) + guard-тесты. См. §Amendment ниже. direct-режим bootstrap-модуля в daemon-е пока не подключён (BootstrapDial=nil → не регистрируется) — это generic-live-слайс.
>
> **Amendment (full-install режим для платформ без cloud-init userdata) — Слайс 1/3 реализован.** `core.bootstrap.delivered` получает второй режим работы — **full-install** по Teleport SSH (ставит ВЕСЬ setup, а не только токен) для платформ без cloud-init userdata (напр. WB namespace без `ci_user_data`). Install-blueprint вынесен в shared [keeper/internal/soulinstall](../../keeper/internal/soulinstall) — единый источник правды (canonical `Blueprint`), переиспользуемый обоими путями онбординга. Слайс 1 (blueprint-вынос: `Blueprint`/`RenderCloudInitYAML`/`RenderInstallScript`/`InstallStep` + переключение `cloudinit`-пакета на shared + тесты) **готов**; Слайс 2 (install-режим в самом модуле delivered) и Слайс 3 (scenario `generate_userdata:false`+`install:true`+live) — следующие. См. §Amendment (full-install режим) ниже.

**Контекст.** [ADR-061](0061-onboarding-await-and-midrun-reresolve.md) ввёл единый create-прогон provision→онбординг→роль: `core.cloud.created` создаёт N VM, register-output несёт их `sid` + plain bootstrap-токены, затем `core.soul.registered` с `await_online` блокирующе ждёт онбординга. Между «VM создана» и «`soul`-агент online» VM должна получить свой bootstrap-токен — без него CSR-онбординг ([docs/soul/onboarding.md](../soul/onboarding.md)) не стартует.

cloud-init (B-flat, [ADR-017(h)](0017-keeper-side-core.md)) ставит на VM soul-бинарь + CA + systemd-unit, но **намеренно НЕ несёт токен** (userdata логируется cloud-провайдером — секрет туда класть нельзя). Токен выписывается ПОСЛЕ Create и должен быть доставлен отдельным каналом. До этого ADR scenario нёс адрес-заглушку **`keeper.push.applied`**, который keeper-dispatch отвергал как unknown module (нет такого keeper-side core) — созданная VM никогда не получала токен, барьер `await_online` не набирал presence, прогон уходил в `error_locked`. Это **BUG#2 cloud-provision**.

**Решение.** Новый keeper-side core-модуль **`core.bootstrap.delivered`** (диспетчер `on: keeper`) — тонкая доставка per-VM bootstrap-токена по SSH. Заменяет заглушку `keeper.push.applied`.

## Дизайн A1 — «тонкая доставка»

Модуль кладёт на VM **ТОЛЬКО токен** (всё остальное — soul-бинарь, CA, unit — уже поставил cloud-init) и опционально запускает soul-агент. Это не push-прогон Destiny (тот несёт `ApplyRequest`), а одна операция «доставить секрет + дёрнуть start». Переиспользует существующую push-инфраструктуру SSH ([keeper/internal/push](../../keeper/internal/push)), тот же путь, что `SshDispatcher.SendApply`.

**Поток per-host (последовательно):**

1. `SshProvider.Authorize(host, user)` — deny прерывает доставку до connect-а (**fail-closed**).
2. ephemeral ed25519-keypair + `SshProvider.Sign(pubkey)` → `ssh.AuthMethod`-ы (переиспользует `push.NewEphemeralEd25519` + `push.AuthMethodsFromSign`). Приватник **НИКОГДА** не покидает Keeper.
3. `push.Dial(DialConfig{Host: primary_ip, HostAuthorities: <host-CA из Vault>, …})` → `Session` (CA-signed host-cert verify — тот же, что push).
4. `session.Run("install -d -m 0700 /etc/soul && umask 077 && cat > <token_path> && chmod 0400 <token_path>", tokenBytes)` — **★ токен в STDIN, НЕ в argv** (иначе утечёт в `ps`/audit.log/journald на самой VM).
5. если `start_soul` — `session.Run("systemctl start soul", nil)`.

**B1-strict.** Ошибка любого хоста (Authorize-deny / connect-fail / write-fail / start-fail) → шаг `failed` → state не коммитится → `error_locked`. Партиальной доставки нет.

## Адресация и сторона

- Namespace `core`, module `bootstrap`, state `delivered`. Registry-ключ — base `core.bootstrap`; state приходит из суффикса адреса через `config.SplitModuleAddr` (как все keeper-side core).
- Полное имя задачи: `module: core.bootstrap.delivered`. Сторона **Keeper-side**, шаг **обязан** нести `on: keeper`.
- Реализация — [`keeper/internal/coremod/bootstrap/delivered.go`](../../keeper/internal/coremod/bootstrap/delivered.go).

## Параметры (`params:`)

| Параметр | Тип | Обяз. | Default | Семантика |
|---|---|---|---|---|
| `hosts` | array of object `{sid, primary_ip, bootstrap_token}` | required | — | Список VM. На практике приходит CEL-выражением `${ register.<provision>.hosts }` (выход `core.cloud.created`). Пустой список → `failed`. |
| `ssh_provider` | string | required | — | Имя SshProvider-плагина (`keeper.yml::plugins.ssh_providers[].name`) для SSH-аутентификации. **★ В `transport: teleport` НЕ определяет транспорт** (Authorize/Sign не вызываются) — оператор передаёт имя, но оно идёт ТОЛЬКО в audit-payload. Снятие required-статуса по транспорту — пост-MVP опционально. |
| `token_path` | string | — | `/etc/soul/token` | Путь файла токена на VM. |
| `ssh_user` | string | — | `root` | SSH-пользователь. |
| `ssh_port` | int (1..65535) | — | `22` | TCP-порт sshd. |
| `start_soul` | bool | — | `true` | `systemctl start soul` после доставки токена. |

## Выходной контракт (`output:` модуля)

`register.<имя>.*`: `hosts[] = {sid, delivered, started}` + `count` (число обработанных хостов). Плюс стандартные `.changed` (всегда `true` при успехе) / `.failed` DSL-ядра.

**★ БЕЗ токена в output.** Сам plain-токен виден только в register предыдущего шага (`core.cloud.created`, ключ `bootstrap_token`, маскируется `audit.MaskSecrets` на всех выходах) — в output `core.bootstrap.delivered` его нет вовсе.

## Безопасность

- **Токен в STDIN, не в argv** (§A1 шаг 4): argv процесса виден в `ps` и попадает в audit.log/journald на самой VM.
- **Audit-payload без токенов** (event `bootstrap.delivered`, `source: keeper_internal`): `{action, ssh_provider, count, sids}` — параллель cloud.provisioned-маскинга.
- **Текст ошибки маскируется** перед выдачей в `failed`-event (`maskErr` → `audit.MaskSecrets`): vault-ref / токен не утекают в `status_details`.
- **CA-signed host-cert verify обязателен** (fail-closed): пустой host-CA-набор → Apply отдаёт явную ошибку, не коннектится «вслепую» (как `push.Dial`, `InsecureIgnoreHostKey` запрещён).
- **fail-closed Authorize**: deny прерывает доставку до открытия SSH-сессии.

## Зависимости и регистрация

`coremod.Deps` расширен тремя полями (собираются wire-up-ом из той же push-инфраструктуры, что `SshDispatcher`):

- `BootstrapProviders map[string]bootstrap.SshProviderHost` — дискаверенные SshProvider-плагины по `manifest.Name` (тип `SshProviderHost` = `push.SshProvider`, тот же, что у диспетчера; обёртка pluginhost для Sign/Authorize — `*pluginhost.SshProviderPlugin`).
- `BootstrapHostCAs []push.NamedHostKeyAuthority` — host-CA из Vault (`push.LoadHostCAs`).
- `BootstrapDial push.Dialer` — `push.Dial` (мокается в тестах).

Регистрация в `coremod.Default` **условна** (как `core.choir` при `ChoirStore`): модуль подключается только при непустых `BootstrapProviders` И непустых `BootstrapHostCAs` И заданном `BootstrapDial`. Любой пробел — сборка без push-доступа (pull-only / нет host-CA): шаг с этим адресом упадёт «unknown keeper-side module» (понятный отказ «не сконфигурирован»).

## Границы MVP

- **Один key-based SshProvider-режим.** Контракт SignReply покрывает ephemeral-cert (Vault SSH CA) и static-key; multi-provider routing в одном шаге не вводится (`ssh_provider` — одно имя).
- **Только токен.** Модуль не доставляет бинарь/модули/конфиг (это cloud-init B-flat). Не путать с `SshDispatcher`/push-прогоном Destiny.
- **Хосты последовательно.** Параллельная доставка по N VM — возможное расширение без breaking change (per-host операции независимы).
- **★ C1 — cloud-init CA-signed host-key (required-для-live, СЛЕДУЮЩИЙ слайс).** `push.Dial` доверяет только host-cert, подписанному host-CA (`HostAuthorities`), а не голому host-key (отказ от TOFU). Свежая VM после cloud-init имеет свой host-key — он **обязан** быть CA-signed тем же host-CA, иначе handshake реджектится и доставка падает на connect-е. cloud-init (B-flat userdata) должен генерировать host-key и подписывать его host-CA из `keeper.yml::cloud_init` (или класть pre-signed host-cert). Без C1 модуль валиден на render (L0 Trial) и проходит unit-тесты, но live-e2e не пройдёт. C1 + live-валидация WB cloud — отдельный слайс.

## Amendment (Teleport by-name transport)

Модуль получает второй транспортный режим `transport: teleport` (vs default `direct`=generic push.Dial). В teleport-режиме доставка через Teleport proxy by-name (target=SID/FQDN, НЕ primary_ip): keeper-side Teleport-Dialer ([keeper/internal/push/dial_teleport.go](../../keeper/internal/push/dial_teleport.go)) делает транспорт+user-auth+host-verify целиком через Teleport identity-file (`creds.SSHClientConfig()`). Отклонения от A1: (1) Authorize/Sign/ephemeral-keypair НЕ вызываются; (2) Vault host-CA для teleport НЕ требуется — host-verify через Teleport CA (C1 неприменим к teleport-режиму); (3) добавлен retry-with-backoff до Teleport-join (~3-5мин). direct-режим (Vault/static, CA-signed host-cert, C1) без изменений. Teleport-creds — keeper.yml push-блок (`push.transport` + `push.teleport.{proxy_addr,identity_file,cluster}`), плагин soul-ssh-teleport в этом флоу не участвует.

Новый scenario-параметр `join_wait_timeout` (int, секунды; default 360) — потолок ожидания Teleport-join, релевантен только в teleport-режиме; по истечении шаг `failed` (B1-strict, `error_locked`). Регистрация модуля в teleport-режиме требует только dialer (`BootstrapDial`), providers/host-CA не нужны (см. гейт в `coremod.Default`).

## Amendment 2026-06-30 — full-install режим (платформы без cloud-init userdata)

**Проблема.** Дизайн A1 предполагает, что cloud-init (B-flat, [ADR-017(h)](0017-keeper-side-core.md)) уже поставил на VM soul-бинарь + CA + systemd-unit, а `core.bootstrap.delivered` кладёт ТОЛЬКО per-VM токен. Это требует, чтобы провайдер принимал userdata при Create (`generate_userdata: true`). Часть платформ userdata **не принимает** — например WB namespace без `ci_user_data`: VM поднимается «голой», cloud-init на ней не отрабатывает, и доставка одного токена бессмысленна (нет ни бинаря, ни конфига, ни unit-а, которые токен должен дополнить). Для таких платформ `generate_userdata` — **не единственный** путь онбординга ([ADR-017(h) amendment](0017-keeper-side-core.md)): весь setup обязан поставиться другим каналом.

**Решение — два режима `core.bootstrap.delivered`:**

- **token-only** (текущее поведение, A1): cloud-init поставил setup, модуль доставляет только токен. Без изменений.
- **full-install**: модуль ставит **ВЕСЬ** setup по Teleport SSH — те же файлы по тем же путям с теми же правами, что положил бы cloud-init (keeper-ca.pem → soul.yml → soul.service → curl soul-бинарь), затем токен и опц. `systemctl start soul`. Для платформ без userdata.

**Единый источник install-blueprint (DRY).** Чтобы оба пути онбординга (cloud-init userdata и full-install по SSH) ставили **идентичный** результат — те же пути, права, soul.yml и systemd-unit — install-blueprint вынесен в shared-пакет [`keeper/internal/soulinstall`](../../keeper/internal/soulinstall):

- `Blueprint` — canonical резолвленные параметры install-результата (пути/права — константы пакета: `KeeperCAPath`/`SoulConfigPath`/`SoulServicePath`/`SoulBinaryPath` + режимы).
- `RenderCloudInitYAML(Blueprint) (string, error)` — cloud-config YAML для userdata-пути (его теперь вызывает `cloudinit.GenerateUserdata` тонкой обёрткой).
- `RenderInstallScript(Blueprint) ([]InstallStep, error)` — последовательность SSH-шагов для full-install-пути (`InstallStep{Cmd, Stdin}`). ПОКА фундамент: вызовется install-режимом в Слайсе 2.

Drift между двумя рендерерами невозможен конструктивно: тело soul.yml и systemd-unit задают функции `SoulConfigYAML`/`SystemdUnit`, а cloud-init-шаблон рендерит их через `{{ .SoulConfigYAMLIndented }}`/`{{ .SystemdUnitIndented }}` (с YAML-indent под `content: |`) — текстовой копии этого материала в шаблоне нет, оба пути физически берут один источник.

**Единственное намеренное расхождение прав между путями** — `keeper-ca.pem`: `0600` при full-install по SSH (floor построже) vs `0644` в cloud-init userdata (CA публичен). Остальной setup идентичен — те же пути, soul.yml и systemd-unit из одного источника.

**Источник blueprint = `keeper.yml::cloud_init` (config-reuse).** Full-install читает тот же config-блок `keeper.yml::cloud_init`, что и userdata-путь (`bootstrap_endpoint`/`tls_ca_ref`/`soul_binary_url`/`soul_binary_ca`). Имя блока остаётся `cloud_init` несмотря на не-cloud-init-режим: это единый источник install-параметров для обоих путей, дублировать его под вторым именем — drift. Уточнение в bin-doc, не новый ADR.

**Security-инвариант сохранён в обоих режимах.** Secret-write идёт через SSH **stdin, не argv** (§A1 шаг 4 для токена; в full-install так же пишутся CA-PEM и soul.yml — `cat > path` со stdin, не `echo` в argv). `RenderInstallScript` гарантирует это конструктивно (PEM-тело в `InstallStep.Stdin`, `.Cmd` несёт только путь записи); покрыто ARGV-LEAK-GUARD-тестом. Per-VM токен по-прежнему не попадает ни в userdata, ни в blueprint — отдельный шаг (token-only часть).

**Слайсы реализации:**

1. **blueprint-вынос (готов)** — `soulinstall.Blueprint`/`RenderCloudInitYAML`/`RenderInstallScript`/`InstallStep`, переключение `keeper/internal/cloudinit` на shared-рендерер (внешний контракт `Config`/`Resolver`/`GenerateUserdata` сохранён), тесты обоих рендереров + anti-drift + ARGV-LEAK-GUARD.
2. install-режим в `core.bootstrap.delivered` — выполнение `RenderInstallScript`-шагов по Teleport SSH перед token-write, под scenario-флагом.
3. scenario-интеграция — `core.cloud.created` с `generate_userdata: false` + `core.bootstrap.delivered` с `install: true`; live-валидация на платформе без userdata.

**Cross-ref:** [ADR-017(h)](0017-keeper-side-core.md) — `generate_userdata` НЕ единственный путь онбординга (full-install по SSH — альтернатива для платформ без userdata).

## Что закрывает / отвергнуто

- **Закрывает BUG#2 cloud-provision** (заглушка `keeper.push.applied` keeper-side не существовала).
- **Имя `keeper.push.applied` отвергнуто** как адрес keeper-side core: `push.applied` — это audit-event-тип оператор-инициированного push-прогона Destiny (`POST /v1/push/apply`), не keeper-side модуль доставки токена. Совпадение было иллюстративной заглушкой scenario, вводившей в заблуждение.
- **Отдельный bin-doc** — [docs/keeper/modules.md → `core.bootstrap.delivered`](../keeper/modules.md#corebootstrapdelivered).
