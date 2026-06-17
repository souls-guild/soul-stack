# Soulprint — typed-схема MVP

Soulprint — наш аналог Salt grains: факты о хосте, которые Soul-агент собирает и периодически push-ит Keeper-у. Используются в taргетинге сценариев, в essence pipeline, в core-модулях для абстракции через native pkg-mgr/init-system, и в template-рендере конфигов.

**Источник правды по схеме — [ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp).** Этот документ — детальная спека полей, семантики, алгоритмов сбора и use-cases.

## Контракт доставки

- Soul-агент периодически собирает факты (`refresh_interval` в `soul.yml`, дефолт `5m` — см. [`soul/config.md`](config.md)) и шлёт `SoulprintReport` через EventStream ([ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)).
- `SoulprintReport.collected_at` — Soul-side timestamp момента сбора. Keeper при unmarshal дополнительно ставит `received_at` в Postgres (не часть wire-format). При расхождении > 10 минут — warn в OTel-trace.
- `SoulprintReport.facts` (`google.protobuf.Struct`) — **deprecated** stub из эпохи [ADR-012(g)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add). Сохранён для wire-compat (forward-compat only-add); Soul-агенты новых версий не заполняют, Keeper толерантен.
- `SoulprintReport.typed_facts` (`SoulprintFacts`) — основной канал, см. ниже.

## Полная схема `SoulprintFacts`

### Корневое сообщение

| Поле | Тип | Семантика |
|---|---|---|
| `sid` | string | Echo SID для логов. **Authority — mTLS peer cert** (см. [ADR-012(i)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)), не identity-claim. |
| `hostname` | string | Короткое имя (без домена), результат `uname -n` / `gethostname()`. Отличается от `network.fqdn` (полное FQDN). |
| `os` | [OsFacts](#osfacts) | Операционная система. |
| `kernel` | [KernelFacts](#kernelfacts) | Ядро. |
| `cpu` | [CpuFacts](#cpufacts) | Процессоры. |
| `memory` | [MemoryFacts](#memoryfacts) | Память. |
| `network` | [NetworkFacts](#networkfacts) | Сеть. |

**Зарезервированы field-номера 8..14** под пост-MVP-расширения (`uptime`, `timezone`, `virtualization`, `cloud_provider`, `disks`, `bios`). Добавляются отдельными ADR с propose-and-wait по имени поля. **Field 15** зарезервирован под опциональный `extra: google.protobuf.Struct` для user-collectors (open Q №22).

### OsFacts

| Поле | Тип | Пример | Семантика |
|---|---|---|---|
| `family` | string | `debian` / `rhel` / `alpine` / `windows` / `darwin` | Используется в essence pipeline (ступень `os/<family>.yaml`, см. [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). |
| `distro` | string | `ubuntu` / `rocky` / `alpine` | Конкретный дистрибутив. |
| `version` | string | `22.04` / `9.3` / `3.19` | Версия дистрибутива как строка (не SemVer). |
| `codename` | string | `jammy` / `bookworm` / `""` | Опционально (есть не у всех distros). |
| `arch` | string | `amd64` / `arm64` | Архитектура target ОС. |
| `pkg_mgr` | string | `apt` / `dnf` / `apk` / `pacman` | **Собирается Soul-агентом** через таблицу маппинга `family+distro → pkg_mgr`. Читается напрямую `core.pkg.installed` для абстракции через native pkg-mgr. |
| `init_system` | string | `systemd` / `openrc` / `sysv` / `launchd` | Аналогично, для `core.service.*`. |

**Таблица маппинга `pkg_mgr` / `init_system`** — внутри Soul-агента (Go-код). MVP-покрытие:

| family | distro | pkg_mgr | init_system |
|---|---|---|---|
| debian | ubuntu | apt | systemd |
| debian | debian | apt | systemd |
| rhel | rocky | dnf | systemd |
| rhel | centos | dnf | systemd |
| rhel | fedora | dnf | systemd |
| alpine | alpine | apk | openrc |
| darwin | macos | brew | launchd |
| windows | windows | (n/a) | (n/a) |

Расширения — отдельные изменения Soul-бинаря, новая версия. Это сознательная цена централизации таблицы в одном месте.

### KernelFacts

| Поле | Тип | Пример |
|---|---|---|
| `version` | string | `5.15.0-101-generic` (полный, с suffix-ом дистрибутива) |
| `release` | string | `5.15.0` (только версия ядра, без suffix-а) |

### CpuFacts

| Поле | Тип | Семантика |
|---|---|---|
| `count` | int32 | Количество logical CPUs (с учётом HT/SMT). |
| `model` | string | Маркетинговое имя (`Intel Xeon E5-2670`, `Apple M2`). |
| `vendor` | string | `GenuineIntel` / `AuthenticAMD` / `ARM` / `Apple`. |

**Не включено в MVP:** `cores` (физические ядра, без HT), `freq_mhz`, `cache_kb`. Добавляются позже only-add.

### MemoryFacts

| Поле | Тип | Семантика |
|---|---|---|
| `total_mb` | int64 | Полный объём RAM в МБ (не байты!). Используется в essence pipeline: `int(soulprint.self.memory.total_mb * 0.6)`. |
| `available_mb` | int64 | Свободно сейчас (значение из `/proc/meminfo` или эквивалент). |
| `swap_mb` | int64 | Объём swap. |

### NetworkFacts

| Поле | Тип | Семантика |
|---|---|---|
| `primary_ip` | string | Основной IPv4 хоста — тот, что используется как bind-адрес по умолчанию. **Эвристика Soul-агента:** интерфейс с default-route → его primary IPv4. Используется в 90% случаев (например, `redis.conf.tmpl: bind {{ .self.network.primary_ip }}`). |
| `fqdn` | string | Полный FQDN, обычно совпадает с SID. Отличается от `hostname` (короткий). |
| `interfaces` | [NetworkInterface[]](#networkinterface) | Полный список сетевых интерфейсов для multi-homed/VLAN-aware случаев. |

#### NetworkInterface

| Поле | Тип | Семантика |
|---|---|---|
| `name` | string | `eth0` / `ens3` / `wlan0` / `lo` |
| `ipv4` | string[] | IPv4-адреса интерфейса (CIDR-нотация: `10.0.0.1/24`). |
| `ipv6` | string[] | IPv6-адреса. |
| `mac` | string | MAC-адрес. |
| `mtu` | int32 | MTU интерфейса. |

## CEL-доступ — каноническая форма

### Стабильный слой (текущий хост)

Из destiny и scenario:

| Путь | Тип | Контекст |
|---|---|---|
| `soulprint.self.sid` | string | везде |
| `soulprint.self.hostname` | string | везде |
| `soulprint.self.os.family` | string | везде |
| `soulprint.self.os.pkg_mgr` | string | везде (используется core.pkg.*) |
| `soulprint.self.os.init_system` | string | везде (используется core.service.*) |
| `soulprint.self.kernel.version` | string | везде |
| `soulprint.self.cpu.count` | int | везде |
| `soulprint.self.memory.total_mb` | int | везде (essence pipeline) |
| `soulprint.self.network.primary_ip` | string | везде |
| `soulprint.self.network.fqdn` | string | везде |
| `soulprint.self.network.interfaces[i].ipv4` | list<string> | везде |
| `soulprint.self.covens` | list<string> | **Keeper-registry-проекция** (не из `SoulprintFacts`, см. ниже) |
| `soulprint.self.choirs` | list<string> | **Keeper-registry-проекция** Choir-членства хоста (ADR-044, зеркало `covens`; не из `SoulprintFacts`, см. ниже) |

**Голая форма `soulprint.<path>` без `.self`** — **ошибка валидации `soul-lint`**. Каноническая форма обязательна.

**`soulprint.self.sid` / `.covens` / `.choirs` / `.role` — registry-проекция, доступны ВСЕГДА**, независимо от того, прислал ли Soul `SoulprintReport`. Источник — roster Keeper-а (`souls.sid` / `souls.coven[]` / `incarnation_choir_voices` / роль из Voice или `incarnation.spec.hosts[].role`), а не collected-факты: `sid` авторитетен через mTLS peer cert. Keeper подмешивает их в `soulprint.self` при резолве CEL даже при NULL reported facts (свежеподключённый хост / коллектор ещё не реализован). Registry-поля **перезаписывают** одноимённые reported-ключи, если те окажутся в push-е (registry — источник истины, [ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)). Остальные ветки (`os` / `network` / `kernel` / `cpu` / `memory`) доступны только когда Soul их прислал — иначе обращение даёт штатный `no such key`.

### Scenario-only аксессоры

Эти доступны **только из scenario**, не из destiny (см. [destiny/tasks.md §10](../destiny/tasks.md#10-шаблонный-контекст)):

| Путь | Тип | Семантика |
|---|---|---|
| `soulprint.hosts` | list<HostFacts> | Все хосты текущего прогона со стабильными фактами (см. [scenario/orchestration.md §4.1](../scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор)). |
| `soulprint.hosts.where(<predicate>)` | list<HostFacts> | Фильтр по **CEL-предикату-строке** (`"'db' in covens"`, `"'replicas' in choirs"`, `"os.family == 'debian'"`, `"sid == soulprint.self.sid"`). Атрибуты — covens / choirs / sid / network.* / os.* / role. |
| `soulprint.where(<predicate>)` | list<HostFacts> | Список хостов **текущего прогона**, удовлетворяющих CEL-предикату-строке по стабильным фактам. Совпадает с `soulprint.hosts.where(<predicate>)` по семантике и источнику данных — это синоним для частого случая, когда полный список `soulprint.hosts` промежуточно не нужен. **Scenario-only**, как и `soulprint.hosts` ([orchestration.md §4.1](../scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор)). Keyword-args (`coven=...`) не поддерживаются (CEL не имеет keyword-args). |

Структура элемента `HostFacts` совпадает с `SoulprintFacts` + добавляются `covens`, `choirs` (обе — Keeper-registry-проекции) и `role` (Keeper-registry-проекция, доступна на любом прогоне). `role` заполняет topology-резолвер для каждого хоста roster-а с precedence **Choir Voice > spec**: источник — роль Voice-а из `incarnation_choir_voices` (ADR-044, S-T6), а `incarnation.spec.hosts[].role` — fallback для хостов без Voice (в т.ч. bootstrap-create, где Choir-членств ещё нет). Если ни Voice, ни spec роли не дают — `role` пустая.

### Text/template-контекст (для `.tmpl`)

Внутри файлов `.tmpl` (рендер через `core.file.rendered`, [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) есть фиксированный набор системных полей:

- `.self.sid` — string
- `.self.hostname` — string
- `.self.os.*` — поля OsFacts
- `.self.kernel.*` — поля KernelFacts
- `.self.cpu.*` — поля CpuFacts
- `.self.memory.*` — поля MemoryFacts
- `.self.network.*` — поля NetworkFacts
- `.self.covens` — list<string>
- `.self.choirs` — list<string> (registry-проекция Choir-членства, ADR-044)

Эти поля доступны без явной передачи в `vars:` — это контракт `core.file.rendered`.

## Граница `Soulprint` ↔ `souls`-registry

| Где живёт | Что | Кто заполняет |
|---|---|---|
| `SoulprintFacts` (Soul → Keeper, push) | os/kernel/cpu/memory/network/hostname/sid | Soul-агент |
| `souls.coven[]` (Postgres, Keeper-side) | covens | Оператор через API / `core.soul.registered` |
| `incarnation_choir_voices` (Postgres, Keeper-side) | choirs (Choir-членство хоста) | `core.choir.present`/`absent` (ADR-044) |
| `souls.status` (Postgres) | pending/connected/disconnected/revoked | Keeper-управляемое |
| `incarnation_choir_voices.role` (Postgres) | role хоста (Choir Voice — приоритетный источник) | `core.choir.present`/`absent` (ADR-044, S-T6) |
| `incarnation.spec.hosts[].role` (Postgres) | declared role (fallback при отсутствии Voice; в т.ч. bootstrap-create) | Оператор в spec |

**`soulprint.self.covens` в CEL** — это виртуальная проекция: Keeper при резолве склеивает `SoulprintFacts` (от Soul) + `souls.coven[]` (из Postgres) в логическую view. Soul ничего не знает про covens. То же для `soulprint.hosts[].covens` и `soulprint.hosts[].role`.

**`soulprint.self.choirs` в CEL** — симметричная виртуальная проекция: Keeper подмешивает имена Choir-ов хоста из `incarnation_choir_voices` (ADR-044), это **не** collected-факт `SoulprintFacts`. Доступна всегда (как `covens`), список — registry-данные roster-а, не push Soul-а. То же для `soulprint.hosts[].choirs`.

## Use-cases (inventory из текущих примеров)

| Use-case | Поле | Файл |
|---|---|---|
| Essence pipeline — ступень по OS | `soulprint.self.os.family` | `examples/service/service-redis-cluster/essence/_stack.yaml:14` |
| Essence pipeline — расчёт maxmemory | `soulprint.self.memory.total_mb` | `examples/service/service-redis-cluster/essence/_stack.yaml:28` |
| Probe self-check в destiny | `soulprint.self.network.primary_ip != input.master_addr` | `examples/destiny/destiny-redis-replication-config/tasks/main.yml` |
| Render config через `.tmpl` | `.self.network.primary_ip` | `examples/destiny/destiny-redis/templates/redis.conf.tmpl` |
| Scenario `where:` по SID | `soulprint.self.sid == input.target_sid` | `examples/service/service-redis-cluster/scenario/add_replica/main.yml` |
| Scenario probe master | `soulprint.hosts.where("role == 'primary'")[0].network.primary_ip` | `scenario/create/main.yml`, `scenario/create/replication.yml` |
| Smoke-test «ровно один primary» | `size(soulprint.hosts.where("role == 'primary'")) == 1` | `tests/smoke.yml:45` |
| core.pkg.installed → native pkg-mgr | `soulprint.self.os.pkg_mgr` | внутри core-модуля |
| core.service.* → init system | `soulprint.self.os.init_system` | внутри core-модуля |

## Что НЕ в MVP

- **User-collectors** (`/etc/soul/soulprint.d/*` коллекторы — open Q №22). Требует отдельных решений по формату коллектора, sandbox, правам запуска, валидации output. Закрывается отдельным ADR, когда появится конкретный сценарий.
- **`uptime` / `timezone`** — добавятся пост-MVP only-add (field 8/9 в `SoulprintFacts`).
- **`virtualization`** (KVM / Hyper-V / WSL / container) — пост-MVP.
- **`cloud_provider`** (aws / gcp / azure detection через metadata) — пост-MVP, пересекается с CloudDriver-плагинами.
- **`disks`** (mount points / FS / size) — пост-MVP.
- **`bios`** (vendor / version / virtualization-extensions) — пост-MVP.
- **`cpu.cores`** (физические, без HT), **`cpu.freq_mhz`**, **`cpu.cache_kb`** — пост-MVP only-add.

## Связанные документы

- [ADR-018 в `docs/architecture.md`](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) — фиксация решения.
- [ADR-012(g) в `docs/architecture.md`](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) — stub `facts: Struct` (теперь deprecated).
- [`docs/soul/config.md`](config.md) — блок `soulprint:` (`refresh_interval`).
- [`docs/keeper/storage.md`](../keeper/storage.md) — где Soulprint лежит в Postgres.
- [`docs/keeper/modules.md`](../keeper/modules.md) — `core.soul.registered` (Keeper-registry covens).
- [`docs/templating.md`](../templating.md) — CEL и text/template-контексты.
- [`docs/destiny/tasks.md`](../destiny/tasks.md) — destiny-контекст для Soulprint.
- [`docs/scenario/orchestration.md`](../scenario/orchestration.md) — scenario-only аксессоры (`soulprint.hosts`, `soulprint.where`).
- [`proto/keeper/v1/soulprint.proto`](../../proto/keeper/v1/soulprint.proto) — фактический proto-файл.
