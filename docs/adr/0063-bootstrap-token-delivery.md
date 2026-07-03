# ADR-063. core.bootstrap.delivered — keeper-side доставка bootstrap-токена по SSH

> **Статус: active.** Дизайн architect-а (A1 «тонкая доставка»), имя `core.bootstrap.delivered` через propose-and-wait (подтверждено пользователем). Канон фиксируется docs-first ДО кода; этот ADR **amends [ADR-017](0017-keeper-side-core.md), [ADR-061](0061-onboarding-await-and-midrun-reresolve.md), [ADR-015](0015-core-modules-mvp.md)**.
>
> **Прогресс имплементации.** Слайс-пилот реализован: модуль + условная регистрация + Deps + scenario-swap (`keeper.push.applied`-заглушка → `core.bootstrap.delivered`) + unit-тесты. **C1 (cloud-init CA-signed host-key) и live-e2e — следующий слайс, НЕ в этом** (см. §Границы MVP). До C1 live-прогон direct-режима оборвётся: `push.Dial` реджектит host-cert свежей VM, у которой cloud-init поставил голый (не CA-signed) host-key.
>
> **Amendment (Teleport by-name transport) — реализован (пилот).** Второй транспортный режим `transport: teleport` (by-name через Teleport Proxy, host-verify через Teleport identity-file, C1 неприменим) + keeper-side Teleport-Dialer + retry-до-join + wire-up daemon (teleport-режим) + guard-тесты. См. §Amendment ниже. direct-режим bootstrap-модуля в daemon-е пока не подключён (BootstrapDial=nil → не регистрируется) — это generic-live-слайс.
>
> **Amendment (full-install режим для платформ без cloud-init userdata) — Слайс 1/3 реализован.** `core.bootstrap.delivered` получает второй режим работы — **full-install** по Teleport SSH (ставит ВЕСЬ setup, а не только токен) для платформ без cloud-init userdata (напр. WB namespace без `ci_user_data`). Install-blueprint вынесен в shared [keeper/internal/soulinstall](../../keeper/internal/soulinstall) — единый источник правды (canonical `Blueprint`), переиспользуемый обоими путями онбординга. Слайс 1 (blueprint-вынос: `Blueprint`/`RenderCloudInitYAML`/`RenderInstallScript`/`InstallStep` + переключение `cloudinit`-пакета на shared + тесты) **готов**; Слайс 2 (install-режим в самом модуле delivered) и Слайс 3 (scenario `generate_userdata:false`+`install:true`+live) — следующие. См. §Amendment (full-install режим) ниже.
>
> **Amendment (init-фаза + активация unit-а + `event_stream_port`) — реализован, live-обходами доказан.** Live-прогон push-install-flow упёрся в две стены: (5) токен доставлялся, но никем не redeem-ился — soul-side «подхвата» token-файла не существует, seed создаёт ТОЛЬКО `soul init`, и soul run падал рестарт-циклом «SoulSeed not found»; (6) blueprint выводил ОБА порта soul.yml из одного `bootstrap_endpoint` — soul dial-ил EventStream на Bootstrap-порт («Unimplemented: method EventStream»). Плюс дыра: push-install делал только `systemctl start` без `daemon-reload`/`enable` — после ребута VM unit не поднимался. См. §Amendment (init-фаза) ниже.

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
5. `session.Run("test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN=\"$(cat <token_path>)\" /usr/local/bin/soul init --config /etc/soul/soul.yml", nil)` — **redeem токена** (CSR→Bootstrap-RPC→SoulSeed; §Amendment init-фаза). Guard по seed-cert = идемпотентность (токен single-use); литеральная `$(cat …)` раскрывается subshell-ом на VM — токен не в argv keeper-а.
6. если `start_soul` — `session.Run("systemctl daemon-reload && systemctl enable soul && systemctl start soul", nil)` (parity с cloud-init runcmd; enable переживает ребут VM).

**B1-strict.** Ошибка любого хоста (Authorize-deny / connect-fail / write-fail / init-fail / start-fail) → шаг `failed` → state не коммитится → `error_locked`. Партиальной доставки нет.

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
| `start_soul` | bool | — | `true` | Активация unit-а после init: `systemctl daemon-reload && systemctl enable soul && systemctl start soul`. `soul init` (шаг 5) выполняется независимо от этого флага. |

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

### Amendment 2026-06-30 — Teleport-proxy за L7-TLS-балансировщиком (`use_system_trust` + `alpn_upgrade`)

**Проблема.** Teleport-Dialer ([dial_teleport.go](../../keeper/internal/push/dial_teleport.go)) верифицирует proxy-server-cert через identity-CA-pool (`creds.TLSConfig()`) + форсированный sentinel-ServerName `teleport.cluster.local` (Teleport API client). Это валидно ТОЛЬКО когда proxy презентует Teleport-issued cert. Если Teleport-proxy стоит ЗА публичным L7-TLS-балансировщиком (WB: wildcard `*.tp.rwb.ru`, SAN `*.tp.rwb.ru, tp.rwb.ru, www.tp.rwb.ru` — **нет** `teleport.cluster.local`), gRPC-handshake (`DialHost`-путь через `credentials.NewTLS`) падает на x509 DNSName mismatch: `certificate is valid for *.tp.rwb.ru, not teleport.cluster.local`. `proxy.ClientConfig.InsecureSkipVerify` не помогает — он влияет лишь на ALPN-conn-upgrade-обёртку, а gRPC-handshake идёт через наш `TLSConfigFunc` с форсированным ServerName.

**Решение — опциональное поле `push.teleport.use_system_trust` (bool, default false).** Когда `true`, `TLSConfigFunc` после `creds.TLSConfig()` правит возвращаемый `*tls.Config`: `RootCAs = nil` (Go берёт системный trust store → верифицирует публичный балансировщик-cert) + `ServerName = host(proxy_addr)` (снимает sentinel `teleport.cluster.local`). `Certificates`/`GetClientCertificate` (mTLS client-cert для auth на proxy) сохраняются. Когда `false` (дефолт) — поведение бит-в-бит прежнее (identity-CA-pool + sentinel-ServerName); существующие инсталляции с Teleport-issued proxy-cert не затронуты.

**Security-обоснование.** Это **не** `InsecureSkipVerify` (доверие любому серту — открыло бы MITM): `RootCAs=nil` даёт ту же разблокировку, но СОХРАНЯЕТ верификацию публичного cert по системному trust. Server-cert proxy **не граница доверия Soul Stack**: аутентификация target-узлов идёт через client-mTLS-cert (auth на proxy) + SSH host-CA из identity-file (host-verify через Teleport CA). Системный trust верифицирует только публичный балансировщик-cert; доверие к узлам не ослаблено. host из `proxy_addr` режется `net.SplitHostPort` на старте dialer-а (битый `proxy_addr` без `:port` → конструктор-ошибка, fail-closed, не поздний Dial).

**Вторая половина того же кейса — `push.teleport.alpn_upgrade` (bool, default false).** `use_system_trust` чинит ВНУТРЕННИЙ gRPC-mTLS-handshake (cert mismatch), но за L7-TLS-балансировщиком остаётся вторая преграда: LB терминирует TLS и **не проксирует raw gRPC/SSH-stream** (`DialHost` падает уже после TLS — на `403 Forbidden; transport: received unexpected content-type "text/plain"`; 403 отдаёт web-слой LB, это **не** Teleport-RBAC). Когда `alpn_upgrade: true`, в `proxy.ClientConfig` выставляется `ALPNConnUpgradeRequired: true` — Teleport оборачивает stream в ALPN-conn-upgrade (WebSocket-туннель на `/webapi/connectionupgrade`), который L7-LB пропускает как обычный HTTP. `WithALPNConnUpgradePing` Teleport включает сам внутри `newDialerForGRPCClient`. Других полей не трогаем: `TLSRoutingEnabled` влияет лишь на путь к Auth (не на `DialHost`), `InsecureSkipVerify` прокинулся бы во ВНЕШНИЙ TLS к LB и отключил верификацию публичного cert (MITM-дыра) — категорически нет.

**Оба флага — пара для proxy-за-L7-LB.** `use_system_trust` чинит внутренний gRPC-TLS поверх туннеля, `alpn_upgrade` пробивает сам туннель через L7-LB; слои ортогональны, но для этой топологии включаются вместе. За транспортом аутентификация target-узлов на identity не меняется: доступ роли к нодам (`ssh`-логины) остаётся Teleport-RBAC на identity-file — это конфигурируется на Teleport-стороне (`tctl`-роль bot-а), не нашим кодом.

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

## Amendment 2026-07-02 — init-фаза в потоке A1, активация unit-а, `event_stream_port` в cloud_init

Три дефекта push-install-flow, каждый доказан live-обходом (после ручного обхода soul выходил в CONNECTED):

**(5-я стена, blocker) Нет init-фазы — доставленный токен никем не redeem-ился.** A1 заканчивался на «токен в `token_path` + `systemctl start soul`», молча предполагая, что soul-агент сам подхватит token-файл. Такого механизма **не существует**: `/etc/soul/token` знали только delivered-модуль и доки, потребителя в `soul/` нет. SoulSeed создаёт ТОЛЬКО `soul init` (токен из `--token` > env `SOUL_BOOTSTRAP_TOKEN`, STDIN не читается; CSR→Bootstrap-RPC→seed в `<paths.seed>/current/`), и `soul run` падал рестарт-циклом «SoulSeed not found — run soul init --token first».

Фикс — новый шаг 5 потока A1 между token-write и активацией (оба режима, token-only и install):

```
test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN="$(cat <token_path>)" /usr/local/bin/soul init --config /etc/soul/soul.yml
```

- **Идемпотентность обязательна:** guard по seed-cert — токен single-use, retry шага после успешного redeem без guard завалил бы хост. Путь guard-а закреплён константой `soulinstall.SeedCertPath` + sync-guard-тест с layout-константами `soul/internal/seed` (`currentLink`/`CertFile`) и `paths.seed` генерённого soul.yml (`TestSeedCertPath_SyncWithSoulSeedLayout`).
- **Secret-floor сохранён:** команда несёт литеральную нераскрытую `$(cat <token_path>)`, раскрывается subshell-ом на VM — токен НЕ в argv keeper-а; STDIN пуст. Бит-в-бит симметрия self-onboard-фазы cloud-init.tmpl.
- **token-write остаётся:** init читает токен из файла — второй передачи секрета нет, контракт `token_path` additive.
- `soul init` выполняется независимо от `start_soul` (redeem — суть доставки, start — отдельная опция).

**(Дыра, major) Нет `daemon-reload` + `enable`.** push-install делал только `systemctl start soul` — cloud-init.tmpl runcmd делает `daemon-reload` (подхват свежезаписанного unit-а) + `enable` (unit переживает ребут) + `start`. Без enable после ребута VM soul.service не поднимался. Фикс: `start_soul` теперь выполняет цепочку `systemctl daemon-reload && systemctl enable soul && systemctl start soul` (все три идемпотентны, безопасно в обоих режимах).

**(6-я стена, blocker) `event_stream_port` в soul.yml = bootstrap-порту.** `keeper.yml::cloud_init` нёс только `bootstrap_endpoint` (`host:port`), и blueprint выводил ОБА порта soul.yml из него — soul run dial-ил EventStream на Bootstrap-only listener (`Unimplemented: method EventStream not implemented`; EventStream живёт на отдельном mTLS-порту, ADR-012(b)). Допущение «за LB-ом порт один» на live-топологиях не выполняется. Фикс: новое опциональное поле **`cloud_init.event_stream_port`** (int) → `cloudinit.Config.EventStreamPort` → `soulinstall.Blueprint.EventStreamPort` → `event_stream_port` soul.yml в обоих рендерерах (cloud-init userdata и install-script); `bootstrap_port` по-прежнему из `bootstrap_endpoint`. `0`/опущено → back-compat fallback на bootstrap-порт (single-port LB, прежнее поведение бит-в-бит).

- **Закрывает BUG#2 cloud-provision** (заглушка `keeper.push.applied` keeper-side не существовала).
- **Имя `keeper.push.applied` отвергнуто** как адрес keeper-side core: `push.applied` — это audit-event-тип оператор-инициированного push-прогона Destiny (`POST /v1/push/apply`), не keeper-side модуль доставки токена. Совпадение было иллюстративной заглушкой scenario, вводившей в заблуждение.
- **Отдельный bin-doc** — [docs/keeper/modules.md → `core.bootstrap.delivered`](../keeper/modules.md#corebootstrapdelivered).
