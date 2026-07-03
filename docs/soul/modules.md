# Модули и кеш на хосте Soul

Раздел про **хостовую сторону** модулей: где они физически лежат, как попадают на хост, как кешируются и как чистятся. Про саму модель модулей (core vs custom, адресация `<namespace>.<module>.<state>`, протокол gRPC-stdio, манифест) — [architecture.md → Модель модулей](../architecture.md#модель-модулей); здесь это намеренно не дублируется.

## Раскладка на хосте

```
/var/lib/soul-stack/
  bin/
    soul-<sha>                 # текущая версия + 1–2 предыдущих для отката
  modules/
    community-redis/           # каталожный слот custom-модуля: <ns>-<name>/
      manifest.yaml            #   материализован из PluginSigil.manifest_raw
      soul-mod-redis           #   бинарь (single-active, atomic rename)
    wb-haproxy/
      manifest.yaml
      soul-mod-haproxy
```

- **`bin/soul-<sha>`** — сам исполняемый файл агента. Имя содержит SHA-256 бинаря, что позволяет держать рядом несколько версий и откатываться без перекачки. Используется push-режимом (Keeper выкатывает бинарь через SSH); в pull-режиме обновление демона — задача оператора (systemd-unit, package manager).
- **`modules/<ns>-<name>/{manifest.yaml, soul-mod-<name>}`** — каталожный слот custom-модуля ([ADR-065](../adr/0065-core-module-installed.md); имена — [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny)). **Single-active** на пару `(namespace, name)`: одна активная версия, запись через atomic rename; несколько версий рядом не хранятся — authority = активный Sigil-допуск, «откат» = revoke+allow другого допуска на Keeper-е + повторный install-шаг. `manifest.yaml` материализуется из `PluginSigil.manifest_raw` (приезжает `SigilSnapshot`-ом, не через fetch). Бинарь запускается `soul`-бинарём как sub-process по gRPC-stdio.
- **Core-модули на диске не лежат.** Они статически встроены в `soul-<sha>`-бинарь.

Путь `/var/lib/soul-stack/modules/` настраивается через `paths.modules` в [`soul.yml`](config.md#paths). Путь к `bin/` сейчас фиксирован соглашением (под него выкатывается push-бинарь).

## Поведение в pull (агентский режим)

- `soul`-демон при apply Destiny-шага дёргает встроенный core-модуль или sub-process custom-модуля.
- Доставка custom-модулей на хост — через сам Destiny: встроенный core-модуль `core.module.installed` ([ADR-065](../adr/0065-core-module-installed.md)) — allow-check по локальному Sigil-набору **до** fetch (нет активного допуска → `module_not_allowed` без единого сетевого байта) → server-streaming RPC `FetchModule` с Keeper-а → полный verify (sha256 + подпись Sigil + `manifest_sha256`, `shared/pluginhost`) → atomic rename в каталожный слот → hot-register без рестарта демона (модуль доступен задачам того же прогона). Идемпотентность: sha256 установленного бинаря == sha активного Sigil → `changed=false`, fetch не выполняется. Это обычная Destiny-операция, ничего магического.
- Демон не пытается «угадать», какие модули понадобятся вперёд. Если нужный модуль отсутствует в момент apply — шаг падает, оператор должен включить `core.module.installed` в свою Destiny явно (перед первым использованием модуля).

## Поведение в push (`keeper.push`)

- Keeper передаёт хосту **все модули, зарегистрированные в Keeper-е** (без статического анализа Destiny). Сравнение по SHA-256 на каждый модуль; ничего не изменилось — копирование пропускается. Это работает за счёт горячего кеша на хосте (те же каталожные слоты `<ns>-<name>/`, что в pull).
- Сам `soul`-бинарь докатывается тем же механизмом: Keeper сравнивает SHA-256 целевой версии с тем, что лежит в `bin/`, и копирует только при расхождении.
- Первый прогон на новом хосте — медленный (копируется бинарь и все модули). Последующие — мгновенные.
- Для `bin/` имена файлов с SHA в суффиксе позволяют держать несколько версий агента рядом и откатываться без перекачки; слоты модулей — single-active ([ADR-065](../adr/0065-core-module-installed.md)).

Полный алгоритм push-доставки — в [keeper/push.md → Доставка `soul`-бинаря и модулей на хост](../keeper/push.md#доставка-soul-бинаря-и-модулей-на-хост).

## Локальный cleanup кеша

`Reaper` в Keeper-е работает только над Postgres — он **не ходит** на хосты по SSH и не чистит локальные файлы. Это сознательное решение: иначе Reaper-у пришлось бы выдать SSH-права на весь парк, что плохо с точки зрения blast radius. Хостовая чистка устроена иначе:

### В pull-режиме

`soul`-демон периодически (по расписанию из своего конфига) удаляет в `/var/lib/soul-stack/bin/` и `/var/lib/soul-stack/modules/` те версии, которые не использовались N дней.

Параметры — в [`soul.yml` → cleanup](config.md#cleanup):

| Параметр | Смысл |
|---|---|
| `cleanup.modules_ttl_days` | Сколько дней неиспользованная версия живёт до удаления. |
| `cleanup.run_interval` | Как часто демон запускает проход по кешу. |

### В push-режиме

Чистка идёт в рамках самого `keeper.push`: при подключении к хосту Keeper может опционально (по флагу политики) сравнить локальный кеш с реестром модулей и удалить устаревшие версии в той же SSH-сессии. Параметры на стороне Soul-а в этом случае не используются — push-хост ничего не делает между прогонами.

### При revoke / удалении хоста

Оператор может инициировать `keeper.push.cleanup` — отдельную операцию push, которая стирает `/var/lib/soul-stack/` целиком на указанном хосте. Применяется при отзыве (`revoke`) Soul-а или выводе хоста из реестра.

## Manifest `SoulModule`

Каждый custom-модуль декларирует себя в **статическом `manifest.yaml`** в корне репо плагина и рядом с бинарём в кеше хоста ([ADR-020(a)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). `soul-lint` парсит файл **без запуска бинаря** для статической валидации destiny.

Manifest-формат — **единый для всех трёх kind-ов плагинов** (`soul_module` / `cloud_driver` / `ssh_provider`) с `kind:`-дискриминатором. Нормативный источник по полям manifest, handshake, lifecycle, capabilities, side_effects — **[`../keeper/plugins.md`](../keeper/plugins.md)**. Здесь — только специфика `kind: soul_module`.

### `spec:` для `kind: soul_module`

Kind-specific блок `SoulModule` — `spec.states`: map поддерживаемых состояний с input-схемой для каждого.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `spec.states` | `map<state-name, {input, description?}>` | — | Map поддерживаемых состояний модуля. Ключ — имя состояния (`installed` / `running` / `run` / …, см. [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny)). |
| `spec.states.<name>.input` | input-schema ([`docs/input.md`](../input.md)) | `{}` | Контракт параметров для этого состояния. `soul-lint` валидирует `params:` каждой задачи destiny против этой схемы. |
| `spec.states.<name>.description` | `string` (optional) | — | Человекочитаемое описание для документации / UI. |

Полная таблица общих полей manifest (`kind`, `protocol_version`, `namespace`, `name`, `required_capabilities`, `side_effects`), нормативная JSON-схема handshake, диаграмма lifecycle и enum-таблицы — в **[`../keeper/plugins.md`](../keeper/plugins.md)**, здесь намеренно не дублируются.

### Пример: `soul-mod-haproxy`

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

Адресация шага destiny — `<namespace>.<name>.<state>`, в примере выше — `wb.haproxy.running` / `wb.haproxy.stopped` / `wb.haproxy.restarted` / `wb.haproxy.reloaded`.

### Core-модули и manifest

**Core-модули** (статически встроены в `soul`-бинарь, см. [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny)) обходятся без отдельного файла `manifest.yaml` рядом с бинарём: их декларация эмбедится в реестр на этапе компиляции (`go:embed`), таблица states и input-схем доступна `soul-lint`-у через тот же парсер `shared/plugin`, что и для custom-модулей, но без чтения с диска. Формат декларации — тот же `kind: soul_module`-формат из [`../keeper/plugins.md`](../keeper/plugins.md).

Реализация:

- Манифесты лежат как `*.yaml` рядом с реестром в пакете **`shared/coremanifest`** (по одному файлу на core-модуль: `exec.yaml`, `file.yaml`, …). Размещение в `shared/` выбрано из-за изоляции: и `soul`, и `soul-lint` импортируют `shared/`, но не импортируют друг друга и не тянут `keeper` — компилятор гарантирует, что линтер не притянет рантайм-реализации модулей.
- `soul-lint` при валидации destiny/scenario находит для каждой задачи `module: core.<m>.<state>` манифест в реестре, берёт `spec.states.<state>.input` и проверяет `params:`: неизвестный параметр (`command` вместо `cmd`), отсутствие required (`cmd`/`path`), несовпадение типа литерала. Структурная проверка по `plugin.InputParamDef` (type/required/secret/pattern); enum, числовые границы и вложенные object/array-схемы в этом DSL не выражаются — отложено до унификации `config.InputSchema`↔`plugin`.
- Манифест описывает **author-facing** контракт — то, что оператор пишет в `params:`. Для `core.file.rendered` это `template:` (путь к `.tmpl`) + `vars:`, а **не** runtime-форма `template_content`+`render_context`, которую Keeper подставляет после CEL/text-template-фаз ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). Поэтому runtime `Module.Validate` модулей с handoff-преобразованием params (rendered) валидирует свою runtime-форму отдельно; для модулей без handoff (`core.exec`) runtime `Validate` делегирует тому же manifest-реестру — единый источник per-field-проверок.
- Keeper-side core (`core.soul`/`core.cloud`/`core.vault`, [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)) добавляются в реестр тем же механизмом (новый `<module>.yaml`).

## См. также

- [config.md](config.md) — где задаются `paths.modules` и `cleanup.*`.
- [identity.md](identity.md) — отзыв Soul-а как триггер для `keeper.push.cleanup`.
- [architecture.md → Модель модулей](../architecture.md#модель-модулей) — core vs custom, адресация, манифест, протокол gRPC-stdio.
- [architecture.md → Поведение на хосте и cleanup](../architecture.md#поведение-на-хосте-и-cleanup) — короткий обзор и граница «БД vs хост».
- [architecture.md → ADR-020](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle) — нормативное решение по plugin-инфраструктуре.
- [`../keeper/plugins.md`](../keeper/plugins.md) — **нормативный источник** по manifest, handshake, lifecycle, capabilities, side_effects (формат един для всех трёх kind-ов).
- [keeper/push.md](../keeper/push.md) — push-алгоритм и доставка `soul`-бинаря/модулей со стороны Keeper-а.
- [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny) — словарь имён (`soul-mod-<имя>`, core-модули, custom-модули).
