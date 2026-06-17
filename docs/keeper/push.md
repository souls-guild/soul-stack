# Push-режим (`keeper.push`)

Модуль внутри `keeper`-бинаря для управления хостами **без установки Soul-агента**. Keeper по SSH ходит на хост, выполняет шаги Destiny, забирает результаты; на хосте между прогонами ничего не работает.

Не отдельный бинарь ([ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)) — это серверная функция (RBAC, аудит, выпуск SSH-credentials через Vault), её место в Keeper-е, а не в клиентском бинаре.

## Назначение

Используется для:

- одноразовых задач, где постоянный агент избыточен;
- хостов, где политика безопасности запрещает долгоживущий демон с привилегиями;
- начальной автоматизации до того, как принято решение ставить Soul-агента;
- bootstrap-сценариев: выкатить `soul`-бинарь и SoulSeed-токен push-режимом, дальше работать в pull.

## Модель

- **Единый реестр.** Push-хост — запись в той же таблице `souls` с `transport: ssh`. Колонки `last_seen_at`, `last_seen_by_kid`, `coven`, `registered_at` имеют смысл и здесь (`last_seen_at` обновляется по факту последнего push-прогона, не стрима). См. [storage.md](storage.md) и [`../soul/identity.md`](../soul/identity.md).
- **`soul_seeds` для push не используется.** У push-хостов нет mTLS-идентичности — нет сертификата, нет приватного ключа, нечего ротировать.
- **Нет демона.** Между прогонами хост ничего не делает; никакой стрим не висит.
- **Аудит.** Каждый push-прогон — событие в журнале Keeper-а (что, куда, кем, результат) с RBAC-фильтром ([rbac.md](rbac.md)).
- **Миграция push ↔ agent.** Хост может быть в `transport: ssh` и затем смигрирован в `transport: agent` (поставили Soul) — запись та же, поле меняется, история не теряется.

## Аутентификация SSH — pluggable provider

Конкретная реализация **не закреплена** ([open Q SSH-2 / №3](../architecture.md#открытые-вопросы)). Принцип: SSH-аутентификация идёт через подключаемый интерфейс провайдера; в первом релизе поставляется минимум одна эталонная реализация, остальные подключаются через тот же интерфейс. Кандидаты:

- **Vault SSH CA.** Keeper при каждом push запрашивает у Vault короткоживущий SSH-сертификат, ходит им. Хосты доверяют CA Vault. Без долгоживущих ключей. Лучший вариант по безопасности, согласован с уже обязательным Vault.
- **Static key.** Долгоживущий SSH-ключ на keeper-хосте, его публичная часть в `authorized_keys` целевых хостов. Для dev/test и инсталляций без Vault.
- **Teleport.** Интеграция с Teleport bastion: Keeper ходит через Teleport-proxy, использует Teleport-issued SSH-сертификаты.

Все три вписываются под единый контракт **`SshProvider`** (gRPC-stdio плагин: `Sign` / `Authorize`). Подробности контракта — в [plugins.md](plugins.md). Каталог провайдеров в конфиге — [config.md](config.md) → `plugins.ssh_providers`.

## Интерфейс оператора

Push, как и весь Keeper, выставляется через **OpenAPI и MCP** ([ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)). CLI-обёртка допустима как тонкая утилита поверх API, не первичный интерфейс. Типичный поток:

1. Оператор готовит inventory (список хостов и/или Coven-метки) и Destiny.
2. Дёргает `POST /v1/push/apply` (или MCP-tool `keeper.push.apply`) с inventory, ссылкой на Destiny, и опциями. Нормативная спецификация request/response — [operator-api/push.md → `POST /v1/push/apply`](operator-api/push.md#post-v1pushapply--push-прогон-destiny-по-ssh).
3. Keeper для каждого хоста: получает SSH-credentials у выбранного provider-а, открывает SSH-сессию, выполняет шаги Destiny, собирает результат.
4. Журнал прогона доступен через тот же API/MCP.

## Доставка `soul`-бинаря и модулей на хост

Push-режим переиспользует тот же `soul`-бинарь и те же модули, что и pull ([architecture.md → Модель модулей](../architecture.md#модель-модулей), [`../soul/modules.md`](../soul/modules.md)). Бинарь работает в одноразовом режиме `soul apply`: получает отрендеренный `ApplyRequest` (protojson) через stdin, применяет, отдаёт NDJSON-поток `TaskEvent` + финальный `RunResult` (protojson) в stdout, завершается с exit 0 при `RunResult.status==success` и 1 иначе. Контракт proto — общий с pull (ADR-012), вводить push-only схему не требуется.

### Раскладка на хосте

```
/var/lib/soul-stack/
  bin/
    soul-<sha>          # текущая версия + 1–2 предыдущих для отката
  modules/
    soul-mod-<name>-<sha>
    ...
```

### Алгоритм каждого push-прогона

1. Keeper подключается к хосту через выбранный SSH-провайдер.
2. Сравнивает по SHA-256 целевую версию `soul`-бинаря с тем, что лежит в `/var/lib/soul-stack/bin/`. Совпало — копирование пропускается, иначе бинарь докатывается.
3. Передаются **все модули, зарегистрированные в Keeper-е** (без статического анализа Destiny). Сравнение по SHA-256 на каждый модуль; ничего не изменилось — копирование пропускается. Работает за счёт горячего кеша.
4. Запускается `soul apply` — отрендеренный план (`ApplyRequest`: `apply_id` + `RenderedTask[]` после Keeper-side фаз `vault-resolve → input-validation → CEL-render → text/template-render`, ADR-012(d)) передаётся в stdin как protojson и не пишется на диск. Сырой Destiny/Essence на push-хост не попадает — Keeper резолвит Vault у себя, Soul только исполняет план. Stdout читается как NDJSON-поток `TaskEvent` + финальный `RunResult`.
5. После — артефакты остаются в кеше; хостовый cleanup устаревших версий — отдельная операция, см. [`../soul/modules.md`](../soul/modules.md).

### Ключевые свойства

- **Передаём всё, не угадываем.** Статический анализ «какие модули нужны конкретно этому Destiny» обманчив (модули вызываются динамически из шаблонов и условий). Передавать все + кеш по хешу = просто и без сюрпризов. Будущая оптимизация — обязательное декларирование `required_modules` в Destiny — описана в [open Q №5](../architecture.md#открытые-вопросы).
- **Кеш по хешу.** Первый прогон на новом хосте — медленный (копируется бинарь и все модули). Последующие — мгновенные.
- **Версии хранятся явно.** Имя файла содержит SHA, что позволяет иметь несколько версий рядом и откатываться без перекачки.
- **Реестр модулей в Keeper-е** — где он физически (Postgres `bytea` / отдельный artifact store / ФС) — [open Q №5](../architecture.md#открытые-вопросы). Влияет на эксплуатацию и backup-стратегию, но не на сам протокол доставки.

## Cleanup на хосте

Reaper Keeper-а на хосты **не ходит** — он работает только над Postgres ([reaper.md](reaper.md)). Удаление устаревших файлов в `/var/lib/soul-stack/` устроено иначе:

- В push-режиме чистка может идти в рамках самого `keeper.push` (опционально, по флагу политики): сравнение локального кеша с реестром модулей и удаление устаревших версий в той же SSH-сессии.
- При отзыве (`revoke`) или удалении хоста из реестра оператор может инициировать `keeper.push.cleanup` — отдельную операцию push, которая стирает `/var/lib/soul-stack/` целиком на указанном хосте.

Подробности и pull-вариант чистки — в [`../soul/modules.md`](../soul/modules.md).

## Wire-up SshDispatcher в daemon (S6 pilot, 2026-05-26)

Pilot-фаза включения `keeper.push.apply` в проде (single-CA, config-backed targets/providers, single-provider routing). Long-term canon (S7) — миграция в `souls.ssh_target jsonb` + PG-table `push_providers` + `push.host_ca_refs[]` — отдельный slice; pilot будет deprecated сразу после S7.

`setupPushDispatchers` (`keeper/cmd/keeper/daemon.go`) поднимается сразу после `setupPushOrchestrator` и до `setupGRPCEventStream`:

1. **Gate-проверки.** Пустой `plugins.ssh_providers[]` ИЛИ нет дискаверенных SshProvider-плагинов в кеше ИЛИ отсутствует `push.host_ca_ref` → push выключен (WARN в лог, `api.Deps.PushRun=nil`, `/v1/push/*` и `keeper.push.apply` возвращают «не сконфигурировано»). Это нормальный режим pull-only-инсталляции — без ошибки старта.

2. **Резолв host-CA.** `push.host_ca_refs[]` (S7-3 multi-CA) → `push.LoadHostCAs` для каждого ref-а читает Vault KV, парсит PEM `public_key` в `ssh.PublicKey`, собирает `[]NamedHostKeyAuthority` (имя из `name` оператора). Backward-compat: при пустом `host_ca_refs[]` и заполненном singular `host_ca_ref` daemon auto-adapt-ит singular в singleton-набор с auto-name `default` + одноразовый WARN. Любая ошибка резолва (Vault недоступен / поле отсутствует / битый PEM) — **fail-fast**: keeper отказывается стартовать (`errSetupFailed`) с именем сбойного CA в сообщении. Оператор явно объявил push в конфиге, молча отключать без host-CA нельзя.

3. **Spawn SshProvider-плагина (single-provider pilot).** Берётся **первый** дискаверенный SshProvider-плагин (по порядку `pluginhost.Discover`/`FilterByCatalog`). Multi-provider routing (`push_runs.ssh_provider` → выбор адаптера) отложен на S7.

4. **Env-payload params.** Для плагина с именем `<name>` ищется запись `push.providers[].name == <name>`. Если есть и `params` непуст — сериализуется в JSON и кладётся в env-переменную `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` плагина ([ADR-020 amendment l](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Пример: `vault-bastion` → `SOUL_SSH_VAULT_BASTION_PARAMS`. Запись отсутствует / params пуст → плагин стартует без env-payload.

5. **Сборка SshDispatcher.** `push.NewSshDispatcher` с:
   - `Provider` — обёртка `pluginhost.SshProviderPlugin` поверх spawned-плагина;
   - `Targets` — `push.NewConfigTargetResolver(cfg.Push.Targets)` (per-SID lookup, дефолты port=22/user=root/soul_path=/usr/local/bin/soul);
   - `Souls` — `push.NewPGSoulLookup(pool)` (проверка предусловия `transport=ssh`);
   - `HostAuthorities` — резолвленный multi-CA-набор (S7-3, OR-проверка через `ssh.CertChecker.IsHostAuthority`);
   - `Metrics` — `push.Metrics` (счётчик `keeper_push_host_ca_used_total{ca_name=...}` на каждый матч CA);
   - `Deliverer` / `Cleaner` — `ShaDeliverer` / `ShaCleaner` (S1/S5).

6. **Lifecycle plugin-handle.** Spawned-плагин держится **до shutdown** (long-living handle, в отличие от cloud-плагинов, где Spawn идёт per-RPC). Close регистрируется в `cleanups`-стеке ПОСЛЕ всех ssh-потребителей (LIFO выполнит ДО Redis/Pool — плагин держит unix-socket на keeper-host-стороне, разумно прибрать первым).

7. **`finalizePushOrchestrator` собирает `*pushorch.PushRun`.** Если `d.pushDispatcher != nil` после `setupPushDispatchers`, после `setupGRPCEventStream` (там создаётся `topologyResolver`) собирается `pushorch.PushRun` со всеми deps; иначе остаётся nil (`api.Deps.PushRun=nil`).

### Когда push **включается**

- `plugins.ssh_providers[]` непустой И хотя бы один плагин дискаверен в кеше;
- `push.host_ca_refs[]` (S7-3 canonical) непустой ИЛИ singular `push.host_ca_ref` (deprecated, auto-adapt) задан; в любом случае резолвится из Vault, поле `public_key` — валидный PEM SSH public key;
- хотя бы одна запись `souls.ssh_target jsonb` (S7-1, canonical) либо непустой `push.targets[]` + `push.allow_legacy_push_targets: true` (legacy fallback под deprecation-окном).

## S7-1 migration to souls.ssh_target (2026-05-26)

ADR-032 amendment 2026-05-26 (S7-1) переносит per-host SSH-реквизиты push-flow из `keeper.yml::push.targets[]` (pilot S6) в реестр `souls`: новая колонка `souls.ssh_target jsonb` со shape-CHECK `{ssh_port:int, ssh_user:text, soul_path:text}` становится canonical-источником.

**Write-path:** `PUT /v1/souls/{sid}/ssh-target` (permission `soul.ssh-target-update`, audit `soul.ssh-target.updated`) либо MCP-tool `keeper.soul.ssh-target.update`. soulctl: `soulctl souls ssh-target set <sid> --port … --user … --soul-path …`.

**Read-path (резолвер):** `PGFallbackTargetResolver` (`keeper/internal/push/target_pg.go`) на каждом `SshDispatcher.SendApply` делает SELECT по PK `souls.sid` и:

- PG-row.ssh_target проставлен → отдаёт SSHTarget с подстановкой дефолтов (port 22 / user root / soul-path `/usr/local/bin/soul`) для опущенных полей.
- PG-row.ssh_target IS NULL и `push.allow_legacy_push_targets: false` (default) → `ErrTargetNotConfigured` (fail-closed, оператор видит чёткое сообщение в `push_runs.summary`).
- PG-row.ssh_target IS NULL и `push.allow_legacy_push_targets: true` → одноразовый WARN deprecation + fallback на `ConfigTargetResolver` поверх `keeper.yml::push.targets[]`.

**Deprecation policy (PM-decision):** 1 release WARN → hard-cut. После закрытия S7 inline-форма `push.targets[]` отвергается на schema-валидации как `unknown_key`.

**Что НЕ в S7-1:**
- Auto-import keeper.yml::push.targets[] в `souls.ssh_target` — отложено в S7-4 (требует explicit consent + idempotency-гарантий, чтобы не перетереть оператор-вручную-выставленные значения).
- `push_providers` PG-table (env-payload SSH-плагинов) — S7-2.

## S7-2 migration to push_providers PG-table (2026-05-26)

ADR-032 amendment 2026-05-26 (S7-2) переносит per-provider env-payload params SSH-плагинов из `keeper.yml::push.providers[]` (pilot S6 / S7-1) в PG-таблицу `push_providers` (миграция 054). Сущность реализована как «SSH Provider» variant of Provider (PM-decision: расширение существующей Provider entity, не новая сущность): тот же концепт provider, но разные таблицы (`providers` для cloud, `push_providers` для ssh), разные схемы params и разные RBAC-permission-области (`provider.*` vs `push-provider.*`).

**Имя/формат.**

- PK: `name TEXT`, регулярка `^[a-z][a-z0-9-]{0,62}$` (env-var-name-safe — name транслируется в `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`, лидирующая цифра/дефис сломает env-имя; на одно ограничение строже cloud Provider).
- `params JSONB NOT NULL DEFAULT '{}'` — opaque-форма самого плагина.
- `created_by_aid TEXT NOT NULL REFERENCES operators(aid)` — изменения только через Архонтов.
- `updated_at` / `updated_by_aid` — для аудита триажа (last-write-wins, no version vector).

**Sensitive params (PM-decision S7-2 #5).** Реальные секреты (`secret_id` / `token` / `password` / `private_key`) ОБЯЗАНЫ быть vault-refs (`vault:<path>`); plaintext-значение по этим ключам отвергается на service-слое (`pushprovider.Service.validateSensitive`) с `ErrSensitiveNotVaultRef` (422 validation-failed). Allow-list ключей — opaque, расширение через PR в `keeper/internal/pushprovider/service.go::sensitiveKeys`. Non-sensitive params (`vault_addr` / `role` / `proxy_addr`) допускаются plain.

**Write-path:** REST `POST/PUT/DELETE /v1/push-providers[/{name}]` (permissions `push-provider.create/update/delete`, audit `push-provider.created/updated/deleted`) либо MCP-tools `keeper.push-provider.{create,update,delete,list,read}`. soulctl: `soulctl push-providers {create,update,delete,list,get}`.

**Read-path (резолвер).** `PGFallbackProviderResolver` (`keeper/internal/push/provider_pg.go`) на старте `setupPushDispatchers` резолвит env-payload params для дискаверенного SSH-плагина:

- PG-row найдена → отдаёт `params` (пустые `{}` допустимы; плагин стартует с дефолтами).
- PG-row отсутствует и `push.allow_legacy_push_providers: false` (default) → `ErrPushProviderNotConfigured`; daemon стартует плагин без env-payload (поведение зависит от плагина: `soul-ssh-static` работает с дефолтами, `soul-ssh-vault` требует params и упадёт сам).
- PG-row отсутствует и `push.allow_legacy_push_providers: true` → одноразовый WARN deprecation + fallback на `keeper.yml::push.providers[]` (legacy).

**Hot-reload (spawn-on-change, PM-decision S7-2 #6).** REST/MCP-мутация публикует в Redis pub/sub topic `push-providers:changed` (`keeper/internal/redis/pushproviderchanged.go`). Каждая нода кластера подписана через `SubscribePushProvidersChanged`. При получении сообщения daemon-listener (`runPushProviderInvalidationListener`) делегирует фактический re-spawn методу `SshDispatcher.RefreshProvider` (S7-2 closure, ADR-032 amendment 2026-05-27): под `d.mu.Lock` зовётся `ProviderRespawner.RespawnProvider`, который закрывает старый plugin-handle и spawn-ит новый с обновлёнными env-payload `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` (резолв через `PGFallbackProviderResolver` — PG-first + legacy fallback). Параллельные SendApply на горячем пути не блокируются: берут snapshot ссылки на provider через RLock и доезжают до конца сессии. Persistence Redis pub/sub нет: потеря сообщения → re-spawn ленивый при следующей мутации либо рестарте keeper-а; мутации редкие, окно устаревания миллисекунды. При неудачном re-spawn (упавший Spawn, отсутствующий discovered-binary) dispatcher переходит в degraded state до следующего успешного refresh — последующий SendApply падает с «SshProvider недоступен», listener продолжает работу (ERROR-лог).

**Audit.** События `push-provider.created` / `.updated` / `.deleted` (kebab single-section имя ресурса; payload `{name, params_keys}` без values — фиксирует факт мутации без раскрытия секретов).

**Deprecation policy (PM-decision S7-2 #4).** 1-release WARN → hard-cut: после следующего релиза inline-форма `keeper.yml::push.providers[]` отвергается на schema-валидации как `unknown_key`. Auto-import legacy `push.providers[]` в PG — НЕ в этом slice, отложен в S7-4 (требует explicit consent + idempotency).

**Что НЕ в S7-2:**
- Auto-import legacy `keeper.yml::push.providers[]` в PG — S7-4.
- Per-host invalidation (текущая публикация — кластеро-wide; per-host-routing — отдельный slice multi-provider routing).
- Capabilities manifest плагина с собственным `sensitive_keys[]` — пост-MVP (текущий allow-list — opaque в pushprovider-пакете).

## S7-3 multi-CA host_ca_refs[] (2026-05-26)

ADR-032 amendment 2026-05-26 (S7-3) расширяет single Vault-ref `push.host_ca_ref` до массива структур `push.host_ca_refs[]` для verify host-keys через SSH. Singular `host_ca_ref` остаётся под 1-release WARN deprecation window.

**Грамматика:**

```yaml
push:
  host_ca_refs:
    - { ref: vault:secret/keeper/ssh-host-ca-prod,  name: trusted-bastion-1 }
    - { ref: vault:secret/keeper/ssh-host-ca-stage, name: trusted-bastion-2 }
```

- `ref` — vault-ref (`vault:<mount>/<path>`), указывает на Vault KV с полем `public_key` (тот же формат, что singular). Plaintext-inline-PEM запрещён (`vault_ref_invalid`).
- `name` — operator-defined kebab-case, обязательно, должно быть уникально в наборе (`duplicate_push_host_ca_name`). Используется как label-значение в `keeper_push_host_ca_used_total{ca_name=...}` и в diag-сообщениях.

**Verify-логика.** На каждом SSH-handshake-е (target и proxy_jump-хоп) `ssh.CertChecker.IsHostAuthority` OR-проверяет marshaled-форму предъявленного host-authority против всех CA из набора. Host-cert, подписанный любым из них → доверенный; иначе reject (отказ от TOFU). При матче callback `OnHostCAMatch(caName)` инкрементирует `keeper_push_host_ca_used_total{ca_name=...}` + debug-лог. Линейный bytes.Equal по marshaled-форме внутри handshake-а — handshake уже делает больше системной работы (crypto/network), индекс по форме излишен (closed-set единиц CA).

**Backward-compat (S7-3 deprecation window).** При заполненном singular `push.host_ca_ref` и пустом `host_ca_refs[]` daemon на старте `setupPushDispatchers` auto-adapt-ит singular в `host_ca_refs[0]` с `name='default'` и пишет одноразовый WARN. Одновременное присутствие singular + plural → schema-фаза отвергает (`mutually_exclusive_keys`). После закрытия окна (1 release) singular будет hard-cut на schema-фазе как `unknown_key`.

**Per-provider CA-override — ОТЛОЖЕНО.** В MVP all-providers-trust-all-CAs: один набор `host_ca_refs[]` действует на все SSH-handshake-и (target + proxy_jump). Отдельный CA на provider/route — пост-MVP open Q после S7.

**PM-decisions S7-3:** (1) формат — массив структур (не плоский массив строк) для извлечения `name` под label-значение; (2) backward-compat singular auto-adapt в singleton + WARN; (3) mutually exclusive singular+plural; (4) verify через `CertChecker.IsHostAuthority` OR-loop; (5) per-provider CA-override отложен (all-trust-all в MVP); (6) deprecation policy — 1 release WARN (как S7-1/S7-2).

## S7-4 auto-import legacy on start (2026-05-26)

[ADR-032 amendment 2026-05-26 (S7-4)](../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario) закрывает migration loop: оператор включает один проход миграции inline-`keeper.yml::push`-блоков в PG-источники флагами в самом `keeper.yml`. Pilot и canon продолжают coexist (резолверы PG-first + fallback под `allow_legacy_push_*`), а auto-import — отдельный opt-in, не вшит в работу резолверов.

**Грамматика.** Два новых поля, оба default `false` (без явного согласия оператора молчаливая миграция данных запрещена):

```yaml
push:
  # … host_ca_refs[], targets[], providers[] (legacy / canon) …

  # S7-4: opt-in one-shot миграция при старте Keeper-а.
  auto_import_legacy_targets:    true   # push.targets[]    → souls.ssh_target jsonb
  auto_import_legacy_providers:  true   # push.providers[]  → push_providers PG-table

  # S7-1/S7-2: legacy-fallback при PG-row отсутствует.
  # При allow_legacy_*=false (default) и auto_import_*=false — yml игнорируется
  # резолверами; включение auto_import_* — рекомендованный путь миграции.
  allow_legacy_push_targets:     false
  allow_legacy_push_providers:   false
```

**One-shot семантика.** Импорт идёт шагом `runLegacyAutoImport` в pipeline `keeper run` после `setupPushProviderSvc` (CRUD-фасад и audit-writer уже подняты) и ДО `setupAPIServer` (импортированные строки сразу видны через REST/MCP). Идемпотентно:

- `targets[]` — для каждого SID читается `souls.ssh_target`; `IS NULL` → INSERT + audit; не NULL → skip (PG canonical, не перезаписываем).
- `providers[]` — для каждого `name` пробуется `SelectByName`; `ErrPushProviderNotFound` → INSERT с `created_by_aid='archon-system'` + audit; запись есть → skip.
- Повторный старт без новых записей в `keeper.yml` — no-op (нет ни одного write).
- Отсутствующая `souls`-row для config-target SID — WARN-skip (не fatal): soul ещё не зарегистрирован, импорт реквизита бессмыслен; оператор позже PUT-нёт через `/v1/souls/{sid}/ssh-target`.
- PG read/write fail (одно из чтений / UPDATE / INSERT) → `errSetupFailed`. Уже импортированные строки остаются; неимпортированные подхватятся при следующем старте.
- Audit write fail — best-effort WARN, импорт следующих записей продолжается (storage уже committed; mismatch audit↔storage оператор разрешает вручную, паттерн `bootstrap.ErrAuditWriteFailed`).

**Audit-events** (новый `source: config_bootstrap`, `archon_aid: NULL`):

| Имя | Когда пишется | Payload |
|---|---|---|
| `soul.ssh-target.imported_from_config` | per-row, после успешного `UpdateSshTarget`. | `{sid, ssh_port, ssh_user, soul_path}` — cleartext (зеркало `soul.ssh-target.updated`). |
| `push-provider.imported_from_config` | per-row, после успешного `pushprovider.Insert`. | `{name, params_keys}` — список ключей params, БЕЗ значений (симметрия с `push-provider.created`: sensitive значения в audit не пишутся, политика единая). |

**System-AID `archon-system`.** Импортированные `push_providers`-строки несут `created_by_aid='archon-system'` (отделить от Архонт-create-ов). FK на `operators(aid)` обязывает row `archon-system` существовать в реестре до первого auto-import-а; pilot S7-4 предполагает, что оператор добавляет её руками (`POST /v1/operators` с `aid=archon-system, auth_method=none`) либо она будет заведена в bootstrap-семантике следующего slice-а (отдельный заход).

**Deprecation timeline.**

- **S7 wave (текущий релиз):** pilot и canon coexist. PG-first резолверы. yml fallback под `allow_legacy_push_*` (default false). Auto-import под `auto_import_legacy_*` (default false). 1-release WARN при использовании любого legacy-fallback и при auto-adapt singular `host_ca_ref`.
- **S8 (после следующего prod-release):** hard-cut. `keeper.yml::push.targets[]` / `push.providers[]` / singular `host_ca_ref` → `unknown_key` на schema-фазе; `allow_legacy_push_*` / `auto_import_legacy_*` флаги удаляются; PG-резолверы упрощаются до PG-only (без `Fallback`/`AllowLegacy` полей).

**Operator-driven migration path** (если auto-import не подходит — например, оператор хочет review импортируемых данных перед записью):

- `soulctl souls ssh-target set <sid> --port … --user … --soul-path …` (S7-1).
- `soulctl push-providers create <name> --params=…` (S7-2).
- После заведения PG-данных — удалить inline-блоки из `keeper.yml`.

**PM-decisions S7-4:** (1) auto-import — opt-in (default false, explicit operator-consent); (2) one-shot + idempotent (PG-row presence check, не ON CONFLICT — нужно различить created/skipped для audit); (3) audit-events `soul.ssh-target.imported_from_config` + `push-provider.imported_from_config`; (4) новая `source` enum `config_bootstrap` (отделить от `keeper_internal` semantically); (5) deprecation 1-release WARN → S8 hard-cut; (6) system-AID `archon-system` для импортированных `push_providers` строк.

**Что НЕ в S7-4:** S8 hard-cut (отдельный slice после prod-release); миграция reserved-AID `archon-system` (отдельный slice). TODO `push-providers:changed` re-spawn SshDispatcher plugin-handle — **закрыт** в ADR-032 amendment 2026-05-27 (S7-2 closure), см. раздел «S7-2 migration» выше.

## P2 Multi-provider routing (2026-05-27)

[ADR-032 amendment 2026-05-27](../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario) переводит push с single-provider pilot (один SshProvider на keeper) на eager spawn ВСЕХ дискаверенных SshProvider-плагинов + per-SID routing их между Souls. Оператор поднимает одновременно, например, `soul-ssh-vault` и `soul-ssh-static` и маршрутизирует SID-ы между ними.

### Selector R1 — 3-tier resolve

Router (`keeper/internal/push/router.go::PGRouter`) разрешает имя SshProvider-плагина per-SID в три уровня:

1. **Level 1: `souls.ssh_target.ssh_provider`** (per-SID explicit). Optional поле в jsonb-shape `souls.ssh_target` (миграция 056). Когда задано — побеждает всегда. `source: soul` в audit.
2. **Level 2: `push.coven_default_providers: { <coven>: <provider_name> }`** (per-coven default). Карта в `keeper.yml`, hot-reload-aware. Tiebreak при множественном coven-match — **алфавитный порядок имён ковенов** (детерминизм). `source: coven`.
3. **Level 3: `push.cluster_default_provider: <name>`** (cluster fallback). Скаляр в `keeper.yml`. `source: cluster`.

Все три уровня пусты → **`ErrProviderNotRouted`** → fail per-host (status="error", error_code="provider_not_routed"). **БЕЗ provider-chain fallback** (security-инвариант: auth-perimeter разных providers разный, silent fallback ломает trust).

### α-compat: per-job preset

REST-body / MCP-args `POST /v1/push/apply` несут optional поле `ssh_provider` — старый pilot-параметр S6. P2 сохраняет совместимость:

- Поле задано → preset применяется КО ВСЕМ SID-ам прогона, **router НЕ вызывается** (per-job override). audit-source трактуется как `soul` (per-SID explicit семантически).
- Поле пусто → router-резолв per-SID по правилам выше.

### Eager spawn

Все SshProvider-плагины, прошедшие `pluginhost.Discover` + `FilterByCatalog`, **spawn-ятся при старте Keeper-а** одной волной в `setupPushDispatchers`. Аргументы:

- **UX-предсказуемость:** оператор видит на старте, что все настроенные провайдеры работают. Lazy-spawn-задержка на первом push прогоне ухудшает first-SLA.
- **Plugin start cost — единовременный:** даже 5+ SshProvider-плагинов поднимаются параллельно за единицы секунд (handshake + Sigil-verify); вторичные RPC (Authorize/Sign) — мгновенные.

При spawn-fail любого плагина — `errSetupFailed` (оператор явно объявил его в `plugins.ssh_providers[]`, молча игнорировать нельзя).

### Аудит и метрики

- **Audit:** routing-decision **НЕ** пишется отдельным event-ом (избыточный шум при N\_SID-ах в прогоне). Реальный SshProvider per-SID сохраняется в **`push_runs.summary.hosts[sid].ssh_provider`** — операторская выборка через `GET /v1/push/{apply_id}`.
- **Metric:** `keeper_push_provider_routed_total{provider, decision_source}` (counter). `decision_source ∈ {soul, coven, cluster}`. Cardinality-safe: ~N\_providers × 3 = единицы серий.

### Грамматика keeper.yml

```yaml
push:
  # ... host_ca_refs[], host_ca_ref (legacy), allow_legacy_push_*, providers[] (legacy), targets[] (legacy) ...

  # P2 Multi-provider routing (Level 2 + Level 3). Hot-reload-aware.
  coven_default_providers:
    prod:    vault-bastion
    stage:   static-stage
    eu-west: static-eu

  cluster_default_provider: static-fallback
```

### Operator surface

- **Per-SID:** `PUT /v1/souls/{sid}/ssh-target` (extended body: `ssh_provider`) → `soulctl souls ssh-target set <sid> ... --ssh-provider=<name>`. MCP-tool `keeper.soul.ssh-target.update` принимает то же поле.
- **Bulk per-coven:** `soulctl souls ssh-target bulk-set --coven=<name> --ssh-provider=<name>` (client-side fan-out поверх list+PUT; server-side bulk-эндпоинта не вводится).
- **Cluster / per-coven default:** редактирование `keeper.yml::push.{coven_default_providers,cluster_default_provider}` + SIGHUP / API-reload (hot-reload-aware).

### Что НЕ в P2

- **Soul-label-selector** `souls.attributes` (новая сущность; propose-and-wait, отложено).
- **Per-job inline `routing_rules`** (γ-variant per-prog override карты; post-MVP).
- **Provider-chain fallback** (silent retry на другом provider при connect-fail — security trade-off, отвергнуто PM-decision).
- **Lazy spawn** (UX trade-off, отвергнуто).

**Metrics:**

- `keeper_push_host_ca_used_total{ca_name="<name>"}` (counter) — инкрементируется при каждом матче CA в `hostCertCallback`. Cardinality-safe: имена закрепляются в `keeper.yml::push.host_ca_refs[].name` (closed-set единиц), kebab-case-формат валидируется schema-фазой.

### Когда push **выключается** (fail-open, без ошибки старта)

- `plugins.ssh_providers[]` пустой/отсутствует → INFO в лог;
- Discover не нашёл SshProvider-плагинов (битый кеш / mismatch имён в FilterByCatalog) → WARN;
- `push:` блок или `push.host_ca_ref` отсутствует → WARN.

В этих случаях `/v1/push/*` возвращает 404 (роуты не подключаются), MCP `keeper.push.apply` возвращает internal-error «не сконфигурировано».

### Когда push **валит старт keeper-а** (fail-closed)

- `push.host_ca_ref` задан, но Vault недоступен / поле отсутствует / битый PEM.
- Spawn первого SshProvider-плагина упал (Sigil-verify не прошёл / handshake-таймаут / capability-mismatch).
- Build `SshDispatcher` упал (программная неконсистентность, не runtime).

См. также [`config.md → push`](config.md#push) для нормативной грамматики блока.

## См. также

- [plugins.md](plugins.md) — контракт `SshProvider`, общий gRPC-stdio механизм.
- [reaper.md](reaper.md) — Жнец работает над Postgres, не над хостами.
- [config.md](config.md) → `plugins.ssh_providers` — реестр SSH-провайдеров.
- [rbac.md](rbac.md) — RBAC применяется к push единообразно.
- [`../soul/modules.md`](../soul/modules.md) — раскладка модулей на хосте и хостовый cleanup.
- [architecture.md → Push-режим](../architecture.md#push-режим-keeperpush).
- [architecture.md → Доставка SoulSeed-токена на хост](../architecture.md#доставка-soulseed-токена-на-хост) — push как целевой путь для bootstrap агента.
