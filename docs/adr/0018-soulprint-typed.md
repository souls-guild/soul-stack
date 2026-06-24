# ADR-018. Soulprint typed-схема MVP

- **Контекст.** [ADR-012(g)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) зафиксировал stub: `SoulprintReport.facts` = `google.protobuf.Struct` до закрытия [open Q №6](../architecture.md#открытые-вопросы). Сейчас Soulprint используется в существующих docs/examples минимум в шести местах (essence pipeline `os/<soulprint.self.os.family>.yaml` — [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация); CEL-аксессоры `soulprint.self.<path>` / `soulprint.hosts.where(<predicate>)` / `soulprint.where(<predicate>)` — [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов); inline в `_stack.yaml` `int(soulprint.self.memory.total_mb * 0.6)`; text/template-контекст `.tmpl` — `self.network.primary_ip` / `self.os.*` / `self.sid`; probe `where:` в scenario; core-модули `core.pkg`/`core.service` для абстракции через native pkg-mgr/init-system) — но typed-схемы у Soulprint нет. Этот ADR закрывает №6: typed `SoulprintFacts` message + sub-messages, минимальный набор полей под первый E2E-сервис, и фиксация канонической CEL-формы. №22 (`soulprint.collectors` в `soul.yml`, user-collectors) **остаётся открытым** — требует отдельного решения по формату коллектора / sandbox / правам запуска / валидации output (не только schema-вопрос).
- **Решение.**

  **(a) Typed-схема в `proto/keeper/v1/soulprint.proto`.** Добавляется новое поле `typed_facts` (field 3); старое `facts: google.protobuf.Struct` (field 2) помечается `deprecated`, но физически остаётся для wire-compat ([ADR-012(c)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) forward-compat only-add). Soul-side новых версий заполняет только `typed_facts`; Keeper толерантен к обоим.

  ```protobuf
  message SoulprintReport {
    google.protobuf.Timestamp collected_at = 1;
    google.protobuf.Struct    facts        = 2 [deprecated = true]; // stub, see ADR-012(g); удалить нельзя — only-add
    SoulprintFacts            typed_facts  = 3;
  }

  message SoulprintFacts {
    string         sid              = 1;  // echo для логов; authority — mTLS peer cert
    string         hostname         = 2;  // короткое имя (без домена)
    OsFacts        os               = 3;
    KernelFacts    kernel           = 4;
    CpuFacts       cpu              = 5;
    MemoryFacts    memory           = 6;
    NetworkFacts   network          = 7;
    // field-номера 8..14 reserved под пост-MVP (uptime/timezone/virtualization/cloud_provider/disks/bios)
    // 15 reserved под опциональный `extra: google.protobuf.Struct` для user-collectors (ADR-кандидат, см. (h))
  }

  message OsFacts {
    string family      = 1;  // debian / rhel / alpine / windows / darwin
    string distro      = 2;  // ubuntu / rocky / alpine
    string version     = 3;  // "22.04" / "9.3" / "3.19"
    string codename    = 4;  // "jammy" / ""
    string arch        = 5;  // amd64 / arm64
    string pkg_mgr     = 6;  // apt / dnf / apk — для core.pkg.installed
    string init_system = 7;  // systemd / openrc / sysv — для core.service.*
  }

  message KernelFacts { string version = 1; string release = 2; }
  message CpuFacts    { int32  count = 1; string model = 2; string vendor = 3; }
  message MemoryFacts { int64  total_mb = 1; int64 available_mb = 2; int64 swap_mb = 3; }

  message NetworkFacts {
    string   primary_ip = 1;  // основной IPv4, тот, что bind по умолчанию (90% use-cases)
    string   fqdn       = 2;  // FQDN (== SID, но факт о системе)
    repeated NetworkInterface interfaces = 3;
  }

  message NetworkInterface {
    string   name = 1;
    repeated string ipv4 = 2;
    repeated string ipv6 = 3;
    string   mac  = 4;
    int32    mtu  = 5;
  }
  ```

  Точные семантические описания, валидация, примеры use-cases — в [`docs/soul/soulprint.md`](../soul/soulprint.md).

  **(b) `os.pkg_mgr` и `os.init_system` собираются Soul-агентом.** Один раз в коде агента (таблица маппинга `family+distro → pkg_mgr/init_system`), не в каждом core-модуле. `core.pkg.installed` и `core.service.*` читают эти поля напрямую из `SoulprintFacts.os` — не дублируют detection. Преимущество: при появлении новой OS-family таблица правится в одном месте.
  - **Amendment (2026-05-25, гибрид + механизм доставки).** Реализовано как **гибрид**: soulprint-факт (`os.pkg_mgr`/`os.init_system`) — **primary** источник правды для выбора backend-а в `core.pkg`/`core.service`; рантайм-детект (`util.ResolvePkgMgr`/`ResolveInitSystem` → `DetectPkgMgr`/`DetectInitSystem`) — **fallback** при пустом/`unknown` факте (не дубль-источник, а аварийный путь). Убирает падение «no supported init system» на хостах/контейнерах, где tools не на месте, и гарантирует, что модуль и CEL `soulprint.self.os.*` (в `where:`/шаблонах) видят ОДИН источник — иначе тихий класс багов (найден приёмочным nginx-прогоном, BUG-B 2026-05-25). **Доставка факта в модуль — Вариант A (in-process):** Soul инжектит локальный snapshot (`util.HostFacts`) в core-модули через опциональный internal-интерфейс `util.SoulprintAware` (НЕ публичный `sdk/module`-контракт) перед `Apply`. Out-of-process custom-плагины факт пока НЕ получают — Вариант B (soulprint в `ApplyRequest` proto, only-add) зарезервирован до первого custom-плагина, которому нужен `pkg_mgr`/`init_system`.

  **(c) `network.primary_ip` + `interfaces[]`.** Convenience-string на корне `NetworkFacts` (используется 90% случаев, в т.ч. `self.network.primary_ip` в `redis.conf.tmpl`); `interfaces[]` — для multi-homed/VLAN-aware случаев. Алгоритм определения `primary_ip` — Soul-side, MVP-эвристика: интерфейс с default-route → его primary IPv4.

  **(d) Каноническая CEL-форма — `soulprint.self.<path>`.** Голая форма `soulprint.<path>` (без `.self`) — **ошибка валидации** в `soul-lint`. Симметрия с `register.self.*` ([destiny/tasks.md §10](../destiny/tasks.md#10-шаблонный-контекст)). Существующие примеры (`examples/destiny/redis/tasks/` и т.п.) переписываются батч-задачей под канон.

  **(e) `covens` НЕ в `SoulprintFacts`.** Это **Keeper-registry-данные** (`souls.coven[]` в Postgres, назначает оператор через API или `core.soul.registered` — см. [`docs/keeper/modules.md`](../keeper/modules.md)), а не факты, собираемые Soul-агентом. `soulprint.self.covens` и `soulprint.hosts[].covens` в CEL — **проекция** Keeper-side данных в Soulprint-namespace: Keeper при резолве CEL-выражения склеивает `SoulprintFacts` (от Soul) + `souls.coven[]` (из Postgres) в логическую view `HostFacts`. Soul ничего про covens не знает.

  **(f) `collected_at` — Soul-side, без жёсткой валидации.** Soul заполняет timestamp момента сбора фактов. Keeper при unmarshal дополнительно ставит `received_at` в Postgres-storage (не часть wire-format, не часть `SoulprintReport` proto). При расхождении `received_at - collected_at > 10 min` — warn в OTel-trace. Жёсткой валидации (drop stale, отказ при future) в MVP нет — Soul в private network без NTP не должен ломаться.

  **(g) Минимальный набор для первого E2E.** Все поля выше — нужны для существующих use-cases (см. inventory в [`docs/soul/soulprint.md → раздел «Inventory использования»`](../soul/soulprint.md)). Если первая попытка реального сервиса упрётся в недостающее поле — добавим только-add (новое field-номер 8+ в `SoulprintFacts`).

  **(h) `extra: google.protobuf.Struct` отложен.** field-номер 15 в `SoulprintFacts` **зарезервирован** под опциональный `extra` для user-collectors, но в MVP НЕ объявлен. Открытие — отдельный ADR при закрытии №22 (требует решений по: формат коллектора `/etc/soul/soulprint.d/*` — бинарь/скрипт; права запуска — Soul под root исполняет чужой код; sandbox; collect-time vs lazy; валидация/санитайз вывода). Закрытие №22 одной строкой `extra: Struct` — недо-решение, не закрытие.

  **(i) Сопровождающий документ.** Детальная спека всех полей с примерами, описание алгоритмов сбора, тейбл маппинга `family→pkg_mgr/init_system` — в [`docs/soul/soulprint.md`](../soul/soulprint.md) (как `docs/templating.md` для ADR-010, как `docs/destiny/output.md` для ADR-009).

- **Consequences.**
  - `proto/keeper/v1/soulprint.proto` дополняется новыми messages; `make gen` пересобирает `proto/gen/go/keeper/v1/soulprint.pb.go`.
  - `docs/soul/soulprint.md` — новый файл (детальная спека).
  - `docs/naming-rules.md` дополняется разделом про Soulprint-поля.
  - Существующие примеры переписываются под `soulprint.self.<path>` отдельной батч-задачей (`examples/destiny/redis/`, `essence/_stack.yaml` и т.п.).
  - `soul-lint` получает static-checkable Soulprint-схему (`docs/templating.md:97` — теперь `soulprint.self.*` имеет конкретные типы из proto, а не `dyn`).
  - **Open Q №6 закрыт.** Open Q №22 (user-collectors) — остаётся открытым.
  - ADR-012(g) обновляется: stub `facts: Struct` помечен `deprecated`, ссылается на `typed_facts`.

- **Trade-offs.**
  - `facts: google.protobuf.Struct deprecated` остаётся в proto навсегда (forward-compat only-add). Cruft, но плата за wire-compat.
  - Soul-агент должен поддерживать таблицу маппинга `family+distro → pkg_mgr/init_system` для самых популярных дистрибутивов. Новая ОС → правка таблицы в Soul, релиз новой версии Soul. Альтернатива (derived в каждом модуле) — хуже (дублирование).
  - `primary_ip` как эвристика default-route может быть неточной в редких сценариях (multi-homed с равными метриками, IPv6-only). Принимаем: 90% случаев — типовой сервер, остальные пусть итерируют `interfaces[]`.
  - `covens` через CEL-проекцию означает, что Keeper при резолве должен join-ить `SoulprintFacts` + `souls.coven[]`. Незначительный compute-overhead per-resolve; кэш в Redis ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)) покрывает.

- **Amendment (2026-05-29, `choirs` как стабильный soulprint-факт — Keeper-проекция).** [ADR-044](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации) (Choir) добавляет стабильный факт `choirs[]` — список имён Choir-ов хоста в текущей инкарнации, доступный в CEL как `soulprint.self.choirs` (и `soulprint.hosts[].choirs`). Симметрично `covens` (пункт (e) выше): это **Keeper-registry-данные** (таблицы `incarnation_choirs` / `incarnation_choir_voices`, [ADR-044](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации) пункт 4), **НЕ** факты, собираемые Soul-агентом — `SoulprintFacts` proto **не дополняется** под Choir. `soulprint.self.choirs` — **виртуальная проекция** при резолве CEL: Keeper join-ит per-host Voice-записи в Soulprint-namespace. Choir — стабильный (declared, не волатильный) факт, поэтому доступен в `where:` без probe (граница «стабильное в Soulprint, волатильное — probe» из [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) соблюдена: declared-топология стабильна, actual-роль после failover — нет). Соответствующая правка `docs/soul/soulprint.md` — слайсом S-T4.

- **Amendment (2026-06-09, `typed_facts` на REST — byte-passthrough категории D).** `GET /v1/souls/{sid}/soulprint` отдаёт `typed_facts` как **byte-passthrough JSONB** (категория D, симметрично Augur `allow` — [ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict)): Keeper читает сырые байты колонки `souls.soulprint_facts` (записанные eventstream-ом через `protojson.Marshal(SoulprintFacts)` с `UseProtoNames`) и отдаёт их на wire **AS-IS**, без `unmarshal→map→re-marshal`. Прежний путь (`map[string]any` с re-marshal) сортировал ключи лексикографически на каждом уровне; byte-passthrough отдаёт **PG-jsonb-нормализованный** порядок ключей — это намеренный одноразовый wire-change порядка ключей (значения идентичны; UI парсит typed_facts по proto-схеме `SoulprintFacts`, порядок ключей для него нерелевантен). **Forward-compat ГАРАНТИРОВАН by design:** новые proto-поля `SoulprintFacts`, добавленные Soul-агентом, доезжают на wire **без рекомпиляции Keeper-а** — Keeper не парсит и не фильтрует содержимое (раньше это было «обещание» через untyped `map`; теперь — прямое следствие byte-passthrough, не зависящее от Go-типа на Keeper-стороне). OpenAPI: `SoulprintReadReply.typed_facts` — `x-go-type: json.RawMessage` (`type: object` в схеме для документации формы). Storage-инвариант (jsonb-колонка отвергает невалидный JSON на записи) делает прежнюю handler-side `unmarshal`-валидацию (и её ветку HTTP 500 на «битый JSONB») избыточной — она снята. Guard-тесты: byte-exact passthrough (неалфавитный порядок ключей сохраняется) + forward-compat (extension-ключ вне `SoulprintFacts`-proto присутствует на wire).
