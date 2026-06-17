# core-beacon

Встроенные **core-beacon** — тело [Vigil](../../../naming-rules.md#сущности-предметной-области)
(Soul-side event-driven мониторинг, [ADR-030](../../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)).
Beacon наблюдает состояние хоста и при его смене (**edge-triggered**) поднимает
[Portent](../../../naming-rules.md#сущности-предметной-области) Soul → Keeper.

**Read-only по конструкции** — beacon наблюдает, но **НЕ мутирует** хост (инвариант
[ADR-030](../../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)).
Это отличает beacon от core-модулей (`core.<module>.<state>` приводят хост к
состоянию). Beacon адресуется как `core.beacon.<name>` в поле `VigilDef.check`.

Реализация — [`soul/internal/beacon/`](../../../../soul/internal/beacon/); реестр
встроенных beacon собирает `beacon.Default()`
([`beacon.go`](../../../../soul/internal/beacon/beacon.go)). Plugin-beacon
(kind `soul_beacon`, ADR-030 V5-2) — см. раздел [Custom soul_beacon plugins](#custom-soul_beacon-plugins)
ниже; их реестр поверх pluginhost собирает `beacon.NewPluginRegistry`,
соединение с core — `beacon.NewCompositeRegistry`.

## Typed PortentPayload (V5-1)

С V5-1 ([ADR-030 amendment 2026-05-26](../../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor))
встроенные core-beacon выставляют **типизированный payload** в `PortentEvent.payload`
(oneof) параллельно с legacy `PortentEvent.data` (Struct). Каждому встроенному
beacon-у соответствует typed-message:

| Beacon | Typed-message | Где-CEL access |
|---|---|---|
| `core.beacon.file_changed` | `FileChangedPortent` | `event.file_changed.<field>` |
| `core.beacon.service_down` | `ServiceDownPortent` | `event.service_down.<field>` |
| `core.beacon.port_closed` | `PortClosedPortent` | `event.port_closed.<field>` |
| `core.beacon.disk_full` | `DiskFullPortent` | `event.disk_full.<field>` |
| `core.beacon.process_absent` | `ProcessAbsentPortent` | `event.process_absent.<field>` |
| `core.beacon.http_unhealthy` | `HttpUnhealthyPortent` | `event.http_unhealthy.<field>` |
| `core.beacon.inotify` | `InotifyPortent` | `event.inotify.<field>` |
| plugin-beacon (V5-2, `soul_beacon.*`) | `Struct` в `event.custom` | `event.custom.<field>` |

Точные shape-ы — таблицы «**Typed-payload**» в разделах ниже.

### Deprecation `PortentEvent.data` (Struct)

Поле `data` помечено `[deprecated = true]` в proto. План перехода
(**1-release WARN → hard-cut**, parity с push S7-decision):

1. **V5-1 (сейчас) — hand-off-период.** Soul-side эмит-mapper заполняет
   **ОБЕ** ветки: typed `payload` + legacy `data`. Один WARN-лог на процесс
   при первой эмиссии. Where-CEL работает в обоих стилях:
   `event.data.<field>` и `event.<typed_branch>.<field>` равноценны.
2. **V5-2…V5-3.** Same hand-off; новые where-CEL — только typed-форма.
3. **S5-final (один production-релиз спустя).** Hard-cut: `data` удаляется
   из proto-схемы, Soul-side эмит только typed `payload`, where-CEL
   `event.data.*` перестаёт компилироваться (compile-ошибка в Decree).

Type-mismatch (where-CEL ожидает `event.file_changed`, прилетел `service_down`)
→ fail-safe **no-match** (default-deny): отсутствующая ветка даёт
no-such-key, cel-go возвращает runtime-error, Oracle трактует как «не сматчило».

Контракт реализации — [`soul/internal/beacon/typed_payload.go`](../../../../soul/internal/beacon/typed_payload.go)
(`fillTypedPayload`); CEL-активация на Keeper-стороне —
[`keeper/internal/oracle/where.go`](../../../../keeper/internal/oracle/where.go)
(`buildEventActivation`).

## Контракт `State`

`Check` возвращает `State` — **смысловую строку состояния хоста**. Scheduler
сравнивает её с предыдущим значением (edge-triggered): смена `State` → один
`Portent`. Семантика строки — на усмотрение конкретного beacon-а. Beacon **не**
эмитит событие сам и **не** хранит baseline — это делает scheduler.

`data` — `Struct` с деталями для `PortentEvent.data` (без секретов/тел/заголовков —
beacon не светит payload в Portent/логи/OTel). Невалидные params → **ошибка**
`Check` (scheduler пропускает тик, baseline не трогается, Portent не эмитится:
ошибка проверки ≠ смена состояния хоста). «Недоступно с точки зрения наблюдателя»
(refused/timeout/нет init-системы) — это **валидное состояние**, а не ошибка.

## Встроенные beacon

| Beacon | State | Назначение |
|---|---|---|
| `core.beacon.service_down` | `up` / `down` | Активность сервиса (опрос is-active, без start/stop). Pilot — см. [`service_down.go`](../../../../soul/internal/beacon/service_down.go). |
| `core.beacon.file_changed` | хеш SHA-256 / `missing` | Изменение содержимого файла (правка/ротация/удаление). Pilot — см. [`file_changed.go`](../../../../soul/internal/beacon/file_changed.go). |
| `core.beacon.port_closed` | `open` / `closed` | Доступность TCP-порта (один dial, без отправки данных). |
| `core.beacon.disk_full` | `ok` / `full` | Заполнение файловой системы по порогу (statfs). |
| `core.beacon.process_absent` | `present` / `absent` | Наличие процесса по паттерну (`pgrep`). |
| `core.beacon.http_unhealthy` | `healthy` / `unhealthy` | Здоровье HTTP-эндпоинта по статус-коду (один GET). |
| `core.beacon.inotify` | `quiet` / `events` | Kernel-уровневые FS-события через inotify (Linux-only). Pilot — см. [`inotify_linux.go`](../../../../soul/internal/beacon/inotify_linux.go). |

---

## core.beacon.service_down

Наблюдает активность сервиса. Только опрос статуса (`is-active` / эквивалент),
без `start`/`stop` (read-only). Логику определения активности и backend-detection
переиспользует у [`core.service`](../service/README.md) через общий `util.Runner`
/ `util.DetectInitSystem` (systemd / OpenRC / SysV). Реализация —
[`service_down.go`](../../../../soul/internal/beacon/service_down.go).

**State:** `up` — сервис активен; `down` — сервис остановлен **или** init-систему
определить нельзя (с точки зрения наблюдателя сервис недоступен — это и есть
событие интереса, а не ошибка `Check`).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `service` | string | required | Имя юнита (как в `core.service`). |

**data:** `{ service, active, init_system }`. `active` — bool результата опроса;
`init_system` — определённая init-система (`systemd` / `openrc` / `sysv` /
`unknown`). При неопределённой init-системе `active=false`, `init_system=unknown`.

**Typed-payload:** `ServiceDownPortent { service, active, init_system }` (V5-1,
ADR-030 amendment 2026-05-26). Где-CEL — `event.service_down.service` и т.д.

---

## core.beacon.file_changed

Наблюдает изменение содержимого файла. Считает SHA-256 содержимого потоково
(`io.Copy`, без загрузки целиком в память — наблюдаемый файл может быть крупным),
без записи (read-only). Реализация —
[`file_changed.go`](../../../../soul/internal/beacon/file_changed.go).

**State:** hex-хеш SHA-256 содержимого файла; `missing` — файла нет. Смена State
(правка / ротация / появление / удаление) edge-triggered → `Portent`. Появление
и исчезновение файла так же edge-triggered, как смена содержимого (переход
hash↔`missing`).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Абсолютный путь к наблюдаемому файлу. |

**data:** `{ path, sha256 }` для существующего файла; `{ path, state: "missing" }`
для отсутствующего (поле `sha256` тогда не выставляется).

**Typed-payload:** `FileChangedPortent { path, sha256 }` (V5-1, ADR-030 amendment
2026-05-26; `path` строка, `sha256` пусто, если файла нет). Где-CEL читает
`event.file_changed.path` / `event.file_changed.sha256`.

---

## core.beacon.port_closed

Наблюдает доступность TCP-порта. Один `dial` без отправки данных в сокет
(read-only). Реализация — [`port_closed.go`](../../../../soul/internal/beacon/port_closed.go).

**State:** `open` — соединение установилось; `closed` — порт не принял
(connection refused / timeout / host недоступен). С точки зрения наблюдателя
недоступный порт = `closed` (событие интереса, а не ошибка `Check`).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `port` | int | required | TCP-порт `1..65535`. Принимается как число или строка (на случай `${…}`-интерполяции). |
| `host` | string | optional (default `127.0.0.1`) | Целевой хост/IP. Дефолт — локальный демон на своём порту. |
| `timeout` | string (duration) | optional (default `3s`) | Таймаут dial (convention `duration`: `time.ParseDuration` + суффикс `<N>d`). Висящий dial — это уже наблюдаемое «недоступно». |

**data:** `{ host, port }`.

**Typed-payload:** `PortClosedPortent { host, port }` (V5-1, ADR-030 amendment
2026-05-26). Где-CEL — `event.port_closed.host` / `event.port_closed.port`.

---

## core.beacon.disk_full

Наблюдает заполнение файловой системы. Один `statfs`-вызов (read-only syscall),
без парсинга вывода `df` — точнее и без зависимости от локали/формата утилиты.
Реализация — [`disk_full.go`](../../../../soul/internal/beacon/disk_full.go).

**State:** `full` — использование ФС `≥ threshold_percent` (граница
**включающая**); иначе `ok`.

`used_percent` считается как `(Blocks - Bavail) / Blocks`, где `Bavail` — блоки,
доступные **непривилегированному** процессу: root-reserved (`~5%` по умолчанию у
ext-семейства) учитывается как занятый, ровно как в обычном `df`. Расчёт через
`Bfree` завышал бы used против `df` и взводил `full` ложно-рано.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Точка монтирования либо любой путь внутри наблюдаемой ФС. |
| `threshold_percent` | int | optional (default `90`) | Порог `full`, `1..100`. `full` при использовании `≥` порога. |

**data:** `{ path, used_percent, threshold }`.

**Typed-payload:** `DiskFullPortent { path, used_percent, threshold }` (V5-1,
ADR-030 amendment 2026-05-26). Где-CEL — `event.disk_full.used_percent` и т.д.

---

## core.beacon.process_absent

Наблюдает наличие процесса. Опрос через `pgrep` (нет kill/signal). `pgrep` выбран
вместо скана `/proc`: OS-агностичен (Linux/BSD) и мок-абелен в unit-тестах через
`util.Runner` (как `core.service` / `core.beacon.service_down`). Реализация —
[`process_absent.go`](../../../../soul/internal/beacon/process_absent.go).

**State:** `present` — `pgrep` нашёл совпадение (exit 0); `absent` — совпадений
нет (exit 1). Ошибка самого `pgrep` (битый паттерн / нет бинаря, exit ≥2) →
ошибка `Check`.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `pattern` | string | required | Имя / ERE-паттерн процесса (matches против имени процесса, как `pgrep <pattern>`). |

**data:** `{ pattern }`.

**Typed-payload:** `ProcessAbsentPortent { pattern }` (V5-1, ADR-030 amendment
2026-05-26). Где-CEL — `event.process_absent.pattern`.

---

## core.beacon.http_unhealthy

Наблюдает здоровье HTTP-эндпоинта. Один `GET`, **без чтения тела** (read-only).
Безопасность переиспользуется у [`core.http`](../http/README.md) (тот же паттерн
opt-out security-vs-flexibility): `util.ValidateFetchURL` + `util.NewHTTPClient`
(SSRF-guard на dial-фазе, downgrade-защита редиректов, системный TLS trust store).
Реализация — [`http_unhealthy.go`](../../../../soul/internal/beacon/http_unhealthy.go).

**State:** `healthy` — статус-код входит в `status_codes`; `unhealthy` — код вне
набора **или** транспортная ошибка (DNS/TLS/timeout/недоступен/заблокированный
SSRF-guard-ом dial, `status` = 0). Недоступный эндпоинт = `unhealthy` (событие
интереса, а не ошибка `Check`).

**Дефолт максимально безопасный** (https + SSRF-guard + TLS-верификация). Для
внутреннего health-check (`https://127.0.0.1:8443/health`, RFC1918) — где
secure-дефолт даёт ложный `unhealthy` (dial заблокирован netguard-ом) — оператор
явно поднимает opt-out-флаги в `VigilDef.params`. warn при снятии guard здесь
**не** эмитится (в отличие от apply-модулей): beacon — read-probe по расписанию
без output-warnings-канала, явный флаг в `Vigil.params` и есть согласие оператора.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `url` | string | required | Целевой эндпоинт. **`https://`** по умолчанию; `http://` — только с `allow_http`; прочие схемы (`file://` …) отвергаются на `Check` всегда. |
| `status_codes` | list of int | optional (default `[200]`) | «Здоровые» статус-коды. Код вне набора → `unhealthy`. |
| `timeout` | string (duration) | optional (default `30s`) | Таймаут запроса (convention `duration`). Должен быть положительным. |
| `allow_http` | bool | optional (default `false`) | Принять `http://` (снимает https-only и downgrade-защиту редиректов). **Не** открывает SSRF — dial-guard живёт отдельно. |
| `insecure_skip_verify` | bool | optional (default `false`) | Не верифицировать TLS-сертификат (self-signed / internal CA). MITM-риск. |
| `allow_private` | bool | optional (default `false`) | Снять SSRF dial-guard — разрешить dial в loopback / RFC1918 (internal-эндпоинт). |

Три opt-out-контура ортогональны (`allow_http` не открывает SSRF, и т.д.); каждый
снимается только явным флагом.

**data:** `{ url, status }` — **только** URL и статус-код. Тело и заголовки ответа
сюда **не** попадают: sensitive-by-construction
([ADR-010 §7.4](../../../templating.md)) — beacon не светит payload в Portent.
`status` = 0 означает транспортную ошибку (эндпоинт недоступен либо dial
заблокирован SSRF-guard-ом при `allow_private:false`).

**Typed-payload:** `HttpUnhealthyPortent { url, status }` (V5-1, ADR-030
amendment 2026-05-26). Где-CEL — `event.http_unhealthy.url` /
`event.http_unhealthy.status`.

---

## core.beacon.inotify

Наблюдает FS-события через **kernel inotify** syscall — без поллинга и хеша,
beacon будит scheduler только при реальной активности FS. **Linux-only**: на
non-Linux платформах сам beacon отдаёт ошибку `platform not supported`, реестр
этим не падает (адрес-константа доступна везде ради общего keeper-enum /
soul-registry). Реализация — [`inotify_linux.go`](../../../../soul/internal/beacon/inotify_linux.go);
stub — [`inotify_other.go`](../../../../soul/internal/beacon/inotify_other.go).

**Fold-adapter** (V5-3, ADR-030 amendment 2026-05-26): background-goroutine
читает inotify-fd между тиками scheduler-а и накапливает события в буфер;
Check на каждом тике возвращает «окно» событий за интервал. State `events`
взводится, если в окне ≥ 1 события, иначе `quiet`. Сравнение state edge-triggered
(quiet → events / events → quiet) — один Portent на каждое «появление активности»
и один на «затихание», а не лавину Portent-ов по одному на event.

В отличие от `core.beacon.file_changed` (поллинг + SHA-256 содержимого) —
`inotify` не считает хеш и не открывает файлы: kernel шлёт событие на любое
изменение метаданных/содержимого, beacon только проецирует его в Portent. Это
дешевле по CPU/IO на больших каталогах, но не отлавливает «фантомные» правки
без kernel-события (NFS / снапшоты без inotify-форварда).

**State:** `events` — в окне есть ≥ 1 события (`InotifyPortent.count > 0`);
`quiet` — окно пусто (был тик scheduler-а, но kernel не прислал ничего).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Абсолютный путь к файлу или каталогу. Watch на каталог ловит события внутри (одного уровня — без рекурсии); watch на файл — события самого файла. |
| `events` | list of string | optional (default — все 5) | Фильтр типов событий: `created` / `modified` / `deleted` / `moved` / `attrib`. Преобразуется в kernel-маску (IN_CREATE / IN_MODIFY / IN_DELETE / IN_MOVED_* / IN_ATTRIB); kernel сам фильтрует — не из фильтра не доходят до beacon-а. Неизвестный элемент игнорируется (forward-compat). |
| `recursive` | bool | optional (default `false`) | **MVP принимает только `false`.** `true` → beacon отвергает Vigil ошибкой (потенциальный источник багов с walk-mount-point / symlink — отложен до явного запроса). |
| `throttle` | string (duration) | optional | Принимается грамматикой для forward-compat, в MVP **игнорируется**; все события эмитятся как есть. Throttle планируется отдельным slice-ом. |

**data:** `{ path, count, events: [{type, file, at}, …] }` для `events`-state;
`{ path, count: 0 }` для `quiet`. `file` — имя в каталоге (для directory-watch);
пусто, если watch на отдельный файл (kernel не выставляет name). `at` —
Soul-side unix-seconds регистрации (НЕ kernel-time — inotify не даёт времени
события).

**Typed-payload:** `InotifyPortent { path, events: [{type, file, at}], count }`
(V5-3, ADR-030 amendment 2026-05-26). Где-CEL —
`event.inotify.path == "/etc/audit"` или
`event.inotify.events.exists(e, e.type == "created")` (CEL `exists` по
repeated-полю проектируется как list-of-maps на activation).

### Edge cases

- **`max_user_watches` исчерпан** (`fs.inotify.max_user_watches` sysctl). Kernel
  возвращает `ENOSPC` на `inotify_add_watch`. beacon конвертирует в понятную
  ошибку Vigil оператору; scheduler логирует и пропускает тик (baseline не
  установится, Portent не эмитится). Решение — поднять sysctl
  `fs.inotify.max_user_watches`.
- **Отсутствующий path.** `inotify_add_watch` вернёт `ENOENT`; beacon отдаёт
  ошибку (scheduler пропускает тик). После создания path при следующем тике
  watch установится.
- **Permission denied.** Если у Soul-агента нет read-доступа к watch-target —
  `EACCES`; та же семантика (ошибка → пропуск тика, scheduler логирует).
- **Watch на отдельный файл vs каталог.** В первом случае `events[].file` пуст
  (kernel не шлёт name), во втором — содержит относительное имя.
- **Удаление наблюдаемой цели.** Kernel шлёт `IN_DELETE_SELF` → beacon
  проецирует в `type=deleted`, и **watch автоматически прекращается** (kernel
  снимает wd). Поведение re-add после re-create отложено до явного запроса
  оператора.

### Пример Vigil + Decree

```yaml
# vigils-реестр (managed OpenAPI/MCP, ADR-030):
- name: audit-log-tamper
  check: core.beacon.inotify
  interval: "5s"
  params:
    path: /var/log/audit
    events: [modified, deleted, moved]
  coven: [prod]

# decree-реестр:
- name: alert-on-audit-tamper
  on_vigil: audit-log-tamper
  where_cel: "event.inotify.count > 0"
  action:
    scenario: notify-soc
    args:
      reason: "audit log tamper"
```

### Lifecycle и known trade-offs MVP

- **Singleton-семантика.** Один экземпляр `InotifyBeacon` обслуживает все
  Vigil-ы процесса; per-path watches хранятся в map внутри beacon-а. Несколько
  Vigil-ов с разными `path` — независимые kernel-fd и независимые буферы.
- **Fd-leak при удалении Vigil.** Scheduler не сигнализирует beacon-у об
  удалении Vigil-а (ReplaceAll), поэтому kernel-fd для исчезнувших path
  остаются открытыми до завершения процесса (kernel сам освободит).
  Ограниченный leak (множество уникальных path конечно); explicit lifecycle
  hook на интерфейсе `Beacon` — отложен.

## Custom `soul_beacon` plugins

Помимо встроенных `core.beacon.*` оператор может добавить собственные beacon-плагины
([ADR-030 V5-2](../../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)).
4-й kind в plugin-инфре (parity с `soul_module` / `cloud_driver` / `ssh_provider`):

- бинарь `soul-beacon-<name>`, manifest [`kind: soul_beacon`](../../../keeper/plugins.md#spec-для-kind-soul_beacon);
- SDK — [`sdk/beacon`](../../../../sdk/beacon/beacon.go), интерфейс `Beacon` с двумя RPC:
  - `Validate(params) → ok+errors[]` — runtime-проверки `params` Vigil (то, что не выражается JSON Schema manifest-а);
  - `Check(params, state_cookie) → state + payload + state_cookie + error` — один тик опроса.
- адресация Vigil — `<namespace>.<name>` (например `community.zfs-degraded`); диспетчер Soul-side различает встроенные `core.beacon.*` от plugin-beacon по namespace.
- lifecycle — **one-shot per Spawn** ([ADR-020(d)](../../../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)): scheduler делает Spawn → Check → Close на каждом тике; для частых тиков плагин может сохранять in-memory state через `state_cookie` (passback).
- security — fail-closed Sigil-verify перед Spawn ([ADR-026](../../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)): без активного допуска (`keeper.plugin.allow ns=<ns> name=<name> ref=<ref>`) плагин НЕ запускается.

Payload plugin-beacon — `PortentEvent.payload.custom` (Struct, oneof-ветка). Where-CEL Decree читает `event.custom.<field>`. Точная shape `payload` — на усмотрение автора (plugin-specific).

Пример минимального плагина — см. [`docs/keeper/plugins.md#kind-soul_beacon-zfs-pool-health-adr-030-v5-2`](../../../keeper/plugins.md#kind-soul_beacon-zfs-pool-health-adr-030-v5-2).

## См. также

- [ADR-030](../../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) — Vigil / Oracle / event-driven мониторинг (beacons + reactor).
- [naming-rules.md → Сущности предметной области](../../../naming-rules.md#сущности-предметной-области) — Vigil / Portent / Oracle / Decree.
- [core/http/README.md](../http/README.md) — read-probe HTTP (откуда переиспользуется https-only + SSRF-guard).
- [core/service/README.md](../service/README.md) — управление сервисами (откуда `core.beacon.service_down` берёт detection активности).
- [keeper/plugins.md → `kind: soul_beacon`](../../../keeper/plugins.md#spec-для-kind-soul_beacon) — manifest-схема plugin-beacon.
