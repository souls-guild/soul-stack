# ADR-065. core.module.installed — доставка SoulModule-плагинов на Soul-хост (FetchModule + каталог plugins.soul_modules)

> **Статус: active (принят, docs-first ДО кода; реализация — слайсы S1–S6).** Дизайн architect-а, все решения утверждены пользователем (2026-07-02). Закрывает open Q №5 «где живёт реестр модулей в Keeper-е» ([architecture.md → Открытые вопросы](../architecture.md#текущие)) и дизайн-долг [ADR-015 Consequences](0015-core-modules-mvp.md) («спецификация `core.module.installed` — отдельная задача»). **Amends [ADR-012](0012-keeper-soul-grpc.md) (третий RPC `FetchModule`, only-add) / [ADR-020](0020-plugin-infrastructure.md) (каталог `plugins.soul_modules[]`) / [ADR-015](0015-core-modules-mvp.md) (спецификация зафиксирована).** [ADR-026](0026-sigil.md) (Sigil) — **НЕ меняется**: допуски, подпись, snapshot-раздача и host-side verify реюзаются как есть, новых trust-механизмов нет.

**Контекст.** `core.module.installed` заявлен с [ADR-015](0015-core-modules-mvp.md) как инфраструктурный core-модуль доставки custom-модулей (`soul-mod-*`) на управляемый хост, но ни транспорта байтов Keeper→Soul, ни реестра источников SoulModule на Keeper-е не существовало — спецификация была отложена «до имплементации Soul-демона». Тем временем вся смежная инфраструктура уже live: git-резолв каталога плагинов в ФС-кеш ([`keeper/internal/plugingit`](../../keeper/internal/plugingit), [ADR-026(g)](0026-sigil.md) — но только для keeper-side kinds `cloud_drivers`/`ssh_providers`), Sigil-допуски в PG (`plugin_sigils` + persisted `manifest_raw`) и их раздача Soul-у (`SigilSnapshot`/`SigilTrustAnchors`, ReplaceAll), host-side verify ([`shared/pluginhost`](../../shared/pluginhost)). Первый реальный потребитель — `community.redis.*` в cloud-provision-прогонах: свежесозданная VM должна получить плагин **в том же прогоне**, который ставит redis-роль ([ADR-061](0061-onboarding-await-and-midrun-reresolve.md)/[ADR-063](0063-bootstrap-token-delivery.md)). Не хватало ровно двух кусков: (1) канал передачи байтов бинаря Keeper→Soul; (2) откуда Keeper эти байты берёт (open Q №5).

## Решение

### (a) Транспорт fetch — server-streaming RPC `FetchModule`

Новый **третий** RPC в `service Keeper` ([ADR-012(a)](0012-keeper-soul-grpc.md) расширяется):

```
service Keeper {
  rpc Bootstrap(BootstrapRequest) returns (BootstrapReply);            // unary, server-only TLS (как было)
  rpc EventStream(stream FromSoul) returns (stream FromKeeper);        // bidi, mTLS (как было)
  rpc FetchModule(PluginFetchRequest) returns (stream PluginChunk);    // server-streaming, mTLS — НОВЫЙ
}
```

- **Тот же mTLS-listener, что EventStream** (Bootstrap остаётся на своём server-only TLS listener-е, [ADR-012(f)](0012-keeper-soul-grpc.md)); нового порта/листенера нет. Байты артефакта едут **отдельным HTTP/2-стримом**, НЕ через EventStream — мегабайты бинаря не душат control-plane (очередь apply/presence-сообщений не блокируется).
- **Content-addressed.** Keeper отдаёт **только** байты, чей `sha256` есть в **активном** допуске `plugin_sigils` с `kind: soul_module` (kind читается из persisted manifest допуска, новых PG-колонок нет). Запрос неведомого/отозванного digest-а → отказ.
- **Авторизация — mTLS peer-cert (SoulSeed)**, как EventStream; SID — из SAN ([ADR-012(i)](0012-keeper-soul-grpc.md)). Операторского RBAC нет — Soul не оператор.
- **Guard-rails:** потолок размера — существующий `plugins.max_artifact_size_mb` ([ADR-026(g)](0026-sigil.md)); rate-limit параллельных fetch per-SID (защита от штормов; config-имя поля — при имплементации S1).
- **Forward-compat — only-add** ([ADR-012(c)](0012-keeper-soul-grpc.md)): новый метод + новые message, существующие не тронуты. Старый Soul метод не вызывает; новый Soul против старого Keeper-а получает `Unimplemented` → install-шаг падает `module_fetch_failed` (explicit-reject, не зависание). Точный состав полей `PluginFetchRequest` (content-адрес `binary_sha256` + `namespace`/`name` для лукапа слота и диагностики) и размещение в тематической раскладке `.proto`-файлов ([ADR-012(b)](0012-keeper-soul-grpc.md)) — слайс S1.

### (b) Реестр байтов — каталог `plugins.soul_modules[]`, НОВОГО хранилища НЕТ (закрывает open Q №5)

`keeper.yml::plugins` расширяется третьим родом записей — **`soul_modules[]`** (`{name, source, ref}`), симметрично `cloud_drivers`/`ssh_providers` ([keeper/plugins.md → Каталог плагинов](../keeper/plugins.md#каталог-плагинов-в-keeperyml)):

```yaml
plugins:
  soul_modules:
    - { name: redis, source: "git@github.com:souls-guild/soul-mod-community-redis.git", ref: v1.2.0 }
```

- **Резолв — существующий `plugingit`** (go-git F-fetch → R-nested ФС-кеш `cache_root`, [ADR-026(g)](0026-sigil.md)) с реюзом всего hardening (scheme-allowlist, size-limits, fail-closed per-entry).
- **Допуск — существующий Sigil-флоу**: Архонт `plugin.allow` → запись в `plugin_sigils`.
- **Authority на wire — sha256 из PG `plugin_sigils`** (подпись Keeper-а); ФС-кеш несёт только байты.
- **Нового хранилища НЕТ**: PG = допуски (уже есть), ФС = байты (уже есть), git = происхождение (уже есть, [ADR-007](0007-versioning-git-ref.md)).
- **HA:** ФС-кеш — per-instance. Fetch, попавший на Keeper-инстанс без материализованного слота → on-demand резолв каталога либо отказ с retry (Soul переспрашивает; политика — S1). Расхождения между инстансами безопасны конструктивно: отдаваемые байты в любом случае сверяются с sha256 допуска.
- **S3-совместимый artifact-store — post-GA расширение ЗА fetch-абстракцией**: контракт `FetchModule` не меняется, меняется только бекенд чтения байтов на Keeper-е. Отмечено, в этом ADR НЕ реализуется.

### (c) Семантика `core.module.installed` (Soul-side, state `installed`)

Адресация: namespace `core`, module `module`, state `installed`; шаг — Soul-side (`on:` опущен или coven-метки).

| Параметр | Тип | Обяз. | Семантика |
|---|---|---|---|
| `name` | string | **да** | Полное имя плагина `<namespace>.<name>` (например `community.redis`). |
| `ref` | string | — | **Pin-сверка, НЕ выбор версии**: активный Sigil-допуск обязан быть на этом ref, иначе шаг `failed` (`module_not_allowed`). Authority = sha256 активного допуска; `ref` — страховка оператора «я ожидаю именно этот ref». |

**Идемпотентность:** sha256 уже установленного бинаря == `binary_sha256` активного Sigil → `changed=false`, fetch не выполняется. **Скоп «все allowed скопом» — НЕ в MVP** (отдельная опция позже, при реальном запросе).

### (d) hot-register — ОБЯЗАТЕЛЕН в MVP

После успешной установки Soul **re-discover-ит каталог модулей без рестарта демона** (thread-safe `Rescan` реестра custom-модулей). Без этого канонический сценарий не работает: задачи `community.redis.*` **в том же прогоне** после install-шага не нашли бы модуль, а рестарт демона = обрыв EventStream = обрыв прогона.

**Известное ограничение MVP:** Beacon-реестр (`soul_beacon`-плагины, [ADR-030](0030-vigil-oracle.md)) при rescan **НЕ пересобирается** — hot-reload beacon-ов — отдельный слайс post-MVP.

### (e) Scenario-интеграция — явный шаг, без auto-inject

Оператор пишет install-шаг **явно** перед первым использованием модуля (симметрия `core.soul.registered` — канон «оператор пишет явно»):

```yaml
- module: core.module.installed
  params: { name: community.redis }
```

`service.yml::modules[]` — **validation-hint post-MVP**: render/soul-lint гейт «модуль используется в задачах → обязан быть в `modules[]` и иметь активный Sigil-допуск». Это подсказка-проверка, **НЕ auto-inject** install-шага.

### (f) Sigil-верификация install-time — реюз, новых trust-механизмов НЕТ

1. **allow-check ДО fetch:** нет активного допуска `(namespace, name)` с `kind: soul_module` в локальном Sigil-наборе Soul-а → шаг `failed` `module_not_allowed` — **до единого сетевого байта**.
2. fetch по content-адресу (`FetchModule`).
3. **полный verify перед atomic rename:** sha256(скачанные байты) == `binary_sha256` допуска + подпись Sigil валидна trust-anchor-набором + `manifest_sha256` совпадает. Реюз [`shared/pluginhost`](../../shared/pluginhost). Провал → `module_verify_failed`, бинарь не материализуется.
4. **Manifest материализуется из `PluginSigil.manifest_raw`** (уже возится `SigilSnapshot`-ом) — через `FetchModule` manifest НЕ едет.

### (g) Раскладка кеша Soul — каталожная

`<paths.modules>/<ns>-<name>/{manifest.yaml, soul-mod-<name>}` — single-active слот на пару `(namespace, name)`, запись через atomic rename. Заменяет раннюю плоскую схему `soul-mod-<name>-<sha>` (doc-fix [soul/modules.md](../soul/modules.md)). Оси `commit_sha` (как в keeper-side R-nested) на Soul-е нет намеренно: несколько версий рядом не нужны — authority = активный Sigil, «откат» = revoke+allow другого допуска на Keeper-е + повторный install-шаг.

## Границы MVP

- **без `absent`-state** — cleanup кеша через существующий TTL (`cleanup.modules_ttl_days`, [soul/modules.md](../soul/modules.md));
- **без auto-inject** install-шагов (см. (e));
- **без beacon-hot-reload** при rescan (см. (d));
- **без скопа «все allowed скопом»** (см. (c));
- **S3-artifact-store — post-GA** за fetch-абстракцией (см. (b)).

## Contract-impact

- **proto** — only-add: RPC `FetchModule` + message `PluginFetchRequest`/`PluginChunk` ([ADR-012(c)](0012-keeper-soul-grpc.md) forward-compat; поля и файл — S1). `proto/plugin/v1/` не тронут.
- **config** — additive: `plugins.soul_modules[]` (+ config-поле rate-limit fetch, S1); существующие поля `plugins.*`/`plugin_runtime` не меняются.
- **PG-схема — НЕ тронута**: `plugin_sigils` как есть; `kind: soul_module` читается из manifest допуска (persisted `manifest_raw`, миграция 030).
- **UI / soulctl / MCP / plugin-SDK — не задеты**: allow/revoke/list-поверхность Sigil уже существует ([ADR-026](0026-sigil.md)); авторам плагинов ничего делать не нужно.
- **TaskError-reasons** (открытый каталог, [naming-rules.md → Error codes](../naming-rules.md#error-codes)): `module_not_allowed` / `module_fetch_failed` / `module_verify_failed`.

## Отвергнутые альтернативы

- **Байты через EventStream** (chunk-message в `oneof payload`). Отвергнуто: мегабайты артефакта в control-plane-стриме блокируют очередь apply/presence-сообщений; отдельный HTTP/2-стрим на том же соединении даёт изоляцию бесплатно.
- **Новое хранилище байтов** (PG `bytea` / обязательный artifact-store). Отвергнуто: байты уже лежат в ФС-кеше git-резолвера, допуски — уже в PG; третье хранилище — дубль без выигрыша, обязательный artifact-store ломал бы обязательный контур [ADR-053](0053-dependency-tiers.md). S3 — post-GA опция за fetch-абстракцией.
- **Рестарт демона вместо hot-register.** Отвергнуто: рестарт = обрыв EventStream = обрыв текущего прогона — install-шаг и потребитель модуля не могут жить в одном прогоне.
- **Auto-inject install-шага** (по анализу используемых модулей). Отвергнуто: скрытая магия против канона «оператор пишет явно»; вместо этого post-MVP validation-hint через `service.yml::modules[]`.
- **`ref` как выбор версии.** Отвергнуто: источник истины «какой бинарь допущен» — sha256 активного Sigil-допуска, а не параметр задачи; `ref` в params — только pin-сверка.

## Слайсы

- **S0** — этот документ (ADR + amendments + naming + doc-fix).
- **S1** — proto `FetchModule`/`PluginFetchRequest`/`PluginChunk` (`make gen`) + keeper-side handler (content-addressed отдача по `plugin_sigils`, mTLS-auth, rate-limit per-SID, size-cap).
- **S2** — config-каталог `plugins.soul_modules[]` + резолв SoulModule-записей существующим `plugingit` (реюз).
- **S3** — Soul-side `core.module.installed`: allow-check → fetch → verify → atomic rename в каталожный слот; идемпотентность по sha256.
- **S4** — hot-register: thread-safe `Rescan` реестра custom-модулей Soul-демона.
- **S5** — e2e-guard: install-шаг + `community.redis.*` в одном прогоне (регресс-тест канонического сценария).
- **S6** — live-валидация на cloud-provision флоте (redis) + закрытие DoD.

## Amends

- **[ADR-012](0012-keeper-soul-grpc.md)** — `service Keeper` расширен третьим RPC `FetchModule` (only-add).
- **[ADR-020](0020-plugin-infrastructure.md)** — каталог `keeper.yml::plugins` расширен `soul_modules[]`; доставка SoulModule-плагинов на Soul-хосты формализована (до этого резолв каталога — только keeper-side kinds).
- **[ADR-015](0015-core-modules-mvp.md)** — «спецификация `core.module.installed` — отдельная задача» закрыта этим ADR.
- **[ADR-026](0026-sigil.md)** — БЕЗ изменений (cross-ref: `FetchModule` реюзает допуски `plugin_sigils`, подпись и `shared/pluginhost`-verify как есть).
