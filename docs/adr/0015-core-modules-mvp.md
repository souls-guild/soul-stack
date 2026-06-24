# ADR-015. Core-модули MVP: точный список

- **Контекст.** Open Q №5 содержал под-вопрос «точный набор core-модулей в MVP». В [Модели модулей](../architecture.md#модель-модулей) и [Адресации](../architecture.md#адресация-модулей) уже зафиксирована трёхуровневая форма `<namespace>.<module>.<state>`, core-namespace, прецеденты `core.pkg.installed`, `core.file.present`/`core.file.rendered`. Этот ADR закрепляет минимальный список Soul-side и Keeper-side core-модулей, без которых первый сервис E2E не работает. Критерий «MVP» — достаточно для типовой тройки проверки: Redis HA / PostgreSQL standalone / простой web-app.
- **Решение.**

  **Soul-side core MVP (17 модулей):**

  | Модуль | State-формы | Назначение |
  |---|---|---|
  | `core.pkg` | `installed` / `absent` / `latest` | Пакеты OS, абстракция через native pkg-mgr (apt/yum/dnf/pacman/apk), detection через Soulprint. |
  | `core.file` | `present` / `absent` / `rendered` / `directory` | Файл существует с literal-content (`present`) / отсутствует (`absent`) / отрендерен из `.tmpl` (`rendered`, см. [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) / каталог существует с owner/group/mode (`directory`, см. Amendment 2026-06-18 ниже). |
  | `core.service` | `running` / `stopped` / `restarted` / `enabled` | Сервис, абстракция через systemd/openrc/sysv. |
  | `core.user` | `present` / `absent` | Локальные пользователи OS. |
  | `core.group` | `present` / `absent` | Локальные группы. |
  | `core.exec` | `run` (verb) | Произвольная команда, exec(). Probe-идиома [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) завязана на него. |
  | `core.cmd` | `shell` (verb) | shell-команда (pipes, redirects). Отличие от `exec.run` — shell-интерпретация. |
  | `core.cron` | `present` / `absent` | Cron-задачи в crontab-формате. |
  | `core.mount` | `present` / `absent` / `mounted` / `unmounted` | Точки монтирования, /etc/fstab. |
  | `core.git` | `cloned` / `pulled` | Клонирование/обновление git-репозитория на хосте. |
  | `core.archive` | `extracted` | Распаковка архивов (tar/zip/gz/bz2). |
  | `core.sysctl` | `present` | Kernel-параметры (`vm.overcommit_memory`, `kernel.shmmax`, и т.п.). |
  | `core.url` | `fetched` | Загрузка файла по URL (аналог Ansible `get_url`). `https` по умолчанию; `http` / insecure-TLS / приватные IP — явный per-call opt-out (`allow_http`/`insecure_skip_verify`/`allow_private`, каждый default `false`, снятие логируется warn в output `warnings`); идемпотентность через `checksum` (`sha256`/`sha1`) или сравнение SHA-256; atomic verify-then-rename; `headers` sensitive-by-construction ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) §7.4). |
  | `core.line` | `present` / `absent` | In-place построчная правка существующего файла (lineinfile-эквивалент). Первый core-модуль, не перезаписывающий файл целиком. **Урезанный безопасный MVP** (см. ниже): `present`+`regexp` заменяет ПЕРВУЮ матчащую строку (+warning при >1), `present` без `regexp` добавляет точную строку по `insertafter`/`insertbefore` (литерал/EOF/BOF), `absent` удаляет все совпадения. Запись атомарна (temp+rename). |
  | `core.repo` | `present` / `absent` | Пакетный репозиторий (apt/dnf/yum/apk; идея ansible `apt_repository`/`yum_repository`). Backend по `util.DetectPkgMgr`: apt → `/etc/apt/sources.list.d/<name>.list` + ключ в `/etc/apt/keyrings/<name>.gpg` со ссылкой `signed-by=` (современный формат, НЕ `apt-key`); dnf/yum → `/etc/yum.repos.d/<name>.repo` (ini); apk → строка в `/etc/apk/repositories`. Идемпотентность: файл + содержимое + ключ совпадают → `changed=false`. Запись через `util.AtomicWritePreserving`. **Безопасность:** `gpg_key` критичен (supply-chain) — задан → ключ реально материализуется/проверяется; `gpg_check=false` РАЗРЕШЁН (opt-out) с обязательным warning; `http://` ДОПУСТИМ (внутреннее зеркало, в отличие от https-only `core.url`) с обязательным warning. |
  | `core.firewall` | `present` / `absent` | ОДНО правило файрвола (идея ansible `ufw`/`firewalld`). Backend по новому `util.DetectFirewall` (по установленному управляющему бинарю, НЕ по Soulprint): MVP — ufw и firewalld, iptables отложен. Идемпотентность: парсинг `ufw status` / `firewall-cmd --list-...` (хрупок между версиями — покрыт строгими unit-тестами на зафиксированных образцах). **КРИТИЧЕСКИЙ ИНВАРИАНТ БЕЗОПАСНОСТИ:** модуль НИКОГДА не трогает default policy и НИКОГДА не включает файрвол целиком (`ufw enable` / `systemctl start firewalld`) — иначе на удалённом хосте отрежет SSH и потеряем управление. Только add/delete конкретного правила (зафиксировано комментарием в коде и unit-тестом). |
  | `core.http` | `probe` (verb) | **Read-probe HTTP** (health-check / API-readiness / чтение версии; идея ansible `uri`, сознательно сужена до чтения). Объект `http`, verb `probe` — **read-only**. `method` enum `{GET, HEAD}`, default `GET` (НЕ «любой метод» — сужение мутности ansible). `https` по умолчанию; `http` / insecure-TLS / приватные IP — явный per-call opt-out (`allow_http`/`insecure_skip_verify`/`allow_private`, каждый default `false`, снятие логируется warn в output `warnings`); reuse `util.ValidateFetchURL` + `util.CheckRedirect` downgrade-блок (как `core.url`). `status_codes` (default `[200]`; mismatch → `failed` с приложенным output для диагностики). **`changed=false` ВСЕГДА** — конструктивно и ненастраиваемо: probe не меняет состояние хоста; интерпретация результата — `changed_when:` на уровне scenario (прецедент `core.exec.run`). Output/register: `status` / `body` (cap 64 KiB по байтам с **rune-aware**-откатом до последней полной руны + `truncated`-флаг; тело санитизируется в валидный UTF-8 — probe read-only и тело может быть бинарным, не должно ронять Apply; маскируются только **vault-ref-подстроки** `vault:…` внутри тела, НЕ sensitive-целиком — ради health-ответов вида `{"status":"ok"}`. **Ограничение:** произвольные plaintext-секреты (`password: hunter2`) в теле НЕ маскируются — тело semi-trusted, оператор не должен класть в probe-эндпоинт то, что не должно светиться) / `elapsed_ms` / `headers_keys` (только ключи) / `changed=false`. `headers` — sensitive-by-construction ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) §7.4). Мутирующие HTTP (POST/PUT/PATCH/DELETE, вероятно `core.http.request`) — **отложены post-MVP** отдельным ADR-расширением (тогда же — changed-контракт мутаций; НЕ копировать ansible «changed=true если 2xx»). |

  **`core.template` НЕ выделяется** отдельным модулем — рендер файлов делает `core.file.rendered` ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)). Старое упоминание `template` в [Модели модулей](../architecture.md#модель-модулей) и [Адресации](../architecture.md#адресация-модулей) — исторический drift, исправляется этим ADR. **`core.copy` НЕ выделяется** — покрывается `core.file.present` с inline-content.

  **`core.line` (lineinfile-эквивалент) — принят (2026-05)** в урезанном безопасном виде: запрос поступил, выбран предсказуемый MVP вместо ansible-style вседозволенности (ровно та причина, по которой откладывали: «regex matches not what you think»). Сознательные ограничения MVP, расширяемые позже без breaking change: **backrefs НЕ поддержаны** (подстановка групп regexp в `line`); **insertafter/insertbefore — только литерал или EOF/BOF, НЕ regexp**; `present`+`regexp` заменяет только ПЕРВОЕ совпадение (остальные не трогаются + warning). Это **пилот нового паттерна** «in-place построчная правка» — на нём зафиксирован образец для in-place core; по нему реализованы `core.repo` (запись repo-файлов через `util.AtomicWritePreserving`) и `core.firewall`.

  **`core.http` (read-probe HTTP) — принят (2026-05)** по реальному запросу, отдельным слайсом после `core.repo`/`core.firewall`. Scope сознательно сужен до **read-only** (verb `probe`, методы `GET`/`HEAD`): 90% кейсов «HTTP в destiny» — это health-check / API-readiness / чтение версии, они чисты и `changed=false` по природе. Это уход от ansible-style вседозволенности `uri` (любой метод + мутный «changed=true если 2xx»). Объект `http` заведён **отдельно от `core.url`** намеренно: граница — «`url` кладёт байты на диск, `http` возвращает ответ в register»; это оставляет чистое место для будущего мутирующего `core.http.request`. HTTP-инфраструктура (https-only валидация, downgrade-блок редиректов, конструктор клиента) вынесена в `util` (`util.ValidateURL`/`util.CheckRedirect`/`util.NewHTTPClient`/`util.HTTPDoer`) и переиспользуется обоими модулями — единый паттерн supply-chain-защиты. Мутирующие HTTP отложены post-MVP отдельным ADR.

  **Preserve-by-default для in-place core-модулей (нормативно).** In-place core-модули, правящие *существующий* файл (`core.line` present/absent и будущие in-place core), **сохраняют mode/owner/group существующего файла по умолчанию**: если `mode`/`owner`/`group` не заданы в params — текущие права и владелец восстанавливаются после atomic rename (rename создаёт temp с правами процесса, поэтому preserve явный). Явно заданные `mode`/`owner`/`group` **переопределяют** (override). Создание *нового* файла (например `core.line` `create:true`) использует дефолты (`mode` → 0644, владелец — текущий процесс). Реализуется общим кирпичом `util.AtomicWritePreserving` — наследуемая точка для всех in-place core.

  **`core.hostname` — опционален**, чаще решается cloud-init-ом. Добавим, если появится сценарий без cloud-init.

  **Keeper-side core (диспетчер `on: keeper`, см. [`docs/keeper/modules.md`](../keeper/modules.md)):**

  | Модуль | Статус | Назначение |
  |---|---|---|
  | `core.soul.registered` | уже зафиксирован | Привязка SID к coven-меткам реестра souls. |
  | `core.cloud.provisioned` | **вводится** | CloudDriver-вызов из scenario (cloud-create). Заменяет более ранний паттерн «destiny `cloud-provision` с `on: keeper`» — см. [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read). |
  | `core.vault.kv-read` | **вводится** | Чтение секрета из Vault на keeper-стороне в момент рендера. Формализует «vault-resolve фазу» [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) как явный модульный шаг. См. [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read). |

  **Не входит в MVP:**
  - `core.essence.read` — implicit-доступ к `essence.*` в template-контексте покрывает; явный модуль не нужен.
  - `core.incarnation.commit-state` — commit делается keeper-ом неявно при успехе apply ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).
- **Consequences.**
  - В `docs/naming-rules.md` раздел «Конкретные core-модули» дополняется полным списком.
  - В [Модели модулей](../architecture.md#модель-модулей) и [Адресации модулей](../architecture.md#адресация-модулей) `template` убирается из примеров core (заменяется на `core.file.rendered`).
  - Open Q №5 в части «точный набор core-модулей» закрыт; остальные под-вопросы (реестр модулей в Keeper, формат манифеста, версия handshake-протокола, политика версионирования модулей, опциональное `required_modules`) — остаются открытыми.
  - Эти 17 Soul-side + 3 Keeper-side модулей — обязательная имплементация фазы 0; план реализации — отдельная задача (вероятно, pilot одного модуля → батч после ревью pattern-а, см. [CLAUDE.md → Массовые операции и батчинг](../../CLAUDE.md)).
  - `core.url.fetched` добавлен пост-факто (после первых 12) как безопасная декларативная замена обхода через `core.cmd.shell creates:` для скачивания релизов/бинарей. В exporter-примерах destiny паттерн `core.cmd.shell` (curl/wget с `creates:`) заменяется на `core.url.fetched` — отдельным слайсом после ревью самого модуля (стоп-правило батчинга).
  - Помимо 17 MVP core-модулей выше, `soul`-бинарь предоставляет **инфраструктурный** core-модуль `core.module.installed` для доставки custom-модулей на хост (Keeper → artifact-store → `/var/lib/soul-stack/modules/`, SHA-256 verify). Это **не входит в счёт 17**: `core.module.installed` — функция самого Soul-демона, реализованная как core-модуль для единообразия с Destiny DSL (оператор пишет `module: core.module.installed name=wb.haproxy ref=v2.0.0` в своей Destiny явно, если хочет custom-модуль). Спецификация поведения и input-схема — отдельная задача при имплементации Soul-демона.
- **Trade-offs.**
  - 17 Soul-side модулей — больше, чем минимум-минимум (можно ужать до 6, заменив остальное через `core.exec.run`), но плохой DX и невозможно проверить idempotency. Базовые 12 — необходимый минимум для production-ready first service; `core.url` добавлен сверху как безопасная замена `core.cmd.shell`-обхода для download, `core.line` — пост-факто по реальному запросу (урезанный безопасный MVP), `core.repo`/`core.firewall` — следующим слайсом по реальному запросу, `core.http` (read-probe) — отдельным слайсом по реальному запросу.
  - `core.line` **принят** (урезанный безопасный MVP, без backrefs, replace первого совпадения) — пилот паттерна in-place построчной правки. `core.repo`/`core.firewall` **приняты** (2026-05) одним слайсом по образцу пилота: оба переиспользуют `util.AtomicWritePreserving`/`util.DetectPkgMgr`/новый `util.DetectFirewall`, оба `present`/`absent`. `core.http` (read-probe, verb `probe`, `changed=false`) **принят** (2026-05) отдельным слайсом: переиспользует вынесенную в `util` HTTP-инфраструктуру (`util.ValidateURL`/`util.CheckRedirect`/`util.NewHTTPClient`/`util.HTTPDoer`) совместно с `core.url`; мутирующий HTTP отложен post-MVP. Ранний кандидат-имя `core.uri` отвергнут (uri/url-путаница) — выбран объект `http`.
  - `core.cloud.provisioned` и `core.vault.kv-read` — переоформление существующих неявных вещей; миграция примеров в `service.yml` — отдельная задача.
- **Amendment (2026-06-18, новый state `directory` в `core.file`).** В `core.file`
  добавлен state `directory` (`core.file.directory`) — декларативное создание
  каталога вместо императивного `core.exec.run install -d`. Params: `path`
  (required), `owner`, `group`, `mode` (как у `present`), `parents` (bool, default
  `false` — семантика `mkdir -p`: создавать промежуточные каталоги). `recurse`
  (рекурсивное выставление прав на содержимое) сознательно НЕ реализован в MVP —
  управляется только сам каталог; добавляется позже без breaking change при
  реальном запросе. Идемпотентность (паритет с `present`): каталог есть и
  `owner`/`group`/`mode` совпадают → `changed=false`; каталога нет → создать →
  `changed=true`; атрибуты дрейфят → починить (`chmod`/`chown`) → `changed=true`;
  путь занят файлом (не каталогом) → ошибка без перезаписи. Поддержан Plan/Scry
  ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)):
  `planDirectory` — pure-read drift (тот же `changed`, что выполнил бы Apply, без
  мутации хоста). Реализация — [`soul/internal/coremod/file/directory.go`](../../soul/internal/coremod/file/directory.go)
  (`applyDirectory`/`planDirectory` + ветки `case "directory"` в Apply/Plan/Validate),
  переиспользует `util.ParseMode`/`util.ApplyOwnership`/`util.OwnershipDrift`.
  Author-манифест — блок `states.directory` в
  [`shared/coremanifest/file.yaml`](../../shared/coremanifest/file.yaml) (additive,
  only-add; `soul-lint` валидирует автоматически). Additive и обратно совместимо:
  существующие задачи `core.file` не затронуты. Документация —
  [`docs/module/core/file/README.md`](../module/core/file/README.md).
- **Amendment (2026-06-24, новый state `applied` в `core.sysctl`).** В `core.sysctl`
  добавлен state `applied` (`core.sysctl.applied`) — bulk-применение НАБОРА
  kernel-параметров одним drop-in, в дополнение к per-key `present`. Мотивация:
  host-tuning-наборы (Redis/ES/Kafka и т.п.) несут ~десяток параметров; отдельный
  `core.sysctl.present` на каждый раздувает план и сдвигает индексы. Params:
  `settings` (map `имя→значение`, required), `filename` (string, required — имя
  drop-in в `/etc/sysctl.d/`, напр. `30-redis`; суффикс `.conf` добавляется),
  `reload` (enum `auto|always|never`, default `auto` — **реюз словаря enum из
  `core.service` `daemon_reload`**, util.DaemonReloadMode), `ignore_failures` (bool,
  default `false` → `sysctl -e -p`, глушит read-only/несуществующие ключи в
  контейнерах). Идемпотентность: контент drop-in **детерминирован** (ключи
  СОРТИРУЮТСЯ, формат `key = value`) → сравнение с существующим файлом, `changed=true`
  только при diff (атомарная запись через `util.AtomicWritePreserving`); reload
  (`sysctl -p <file>` ТОЧЕЧНО по drop-in, НЕ весь `--system`) гейтится: `never` →
  никогда (opt-out), `always` → безусловно, `auto` → только при file-change (как
  `daemon_reload:auto`); сам reload `changed` НЕ помечает. Поддержан Plan/Scry
  ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)):
  `planApplied` — pure-read drift (сравнение контента без записи/reload).
  **Осознанное исключение из границы с `core.file`.** В отличие от per-key `present`
  (где persist-файл — побочный продукт) и общего правила «файлы рендерит
  `core.file.rendered`», state `applied` **сам владеет drop-in**: модуль строит
  контент из map, пишет его и управляет reload + idempotency единым шагом. Это
  сознательная Ansible-модель (`sysctl`-модуль владеет файлом+reload), а не drift:
  контент drop-in тривиален (`key = value`, sorted) и не требует text/template, а
  связка «файл↔reload↔idempotency» атомарна на уровне модуля. Реализация —
  [`soul/internal/coremod/sysctl/applied.go`](../../soul/internal/coremod/sysctl/applied.go)
  (`applyApplied`/`planApplied` + ветки `case "applied"` в Apply/Plan/Validate),
  переиспользует `util.AtomicWritePreserving`/`util.DaemonReloadMode`. Author-манифест
  — блок `states.applied` в
  [`shared/coremanifest/sysctl.yaml`](../../shared/coremanifest/sysctl.yaml) (additive,
  only-add). Additive и обратно совместимо: задачи `core.sysctl.present` не затронуты.
  Документация — [`docs/module/core/sysctl/README.md`](../module/core/sysctl/README.md).
- **Amendment (2026-06-18, централизованный daemon-reload в `core.service`).** `core.service` (systemd-backend) перед мутирующими actions (`running` / `restarted` / `enabled`) проверяет systemd-флаг `NeedDaemonReload` и при рассинхроне unit-файла с загруженным определением делает `systemctl daemon-reload` ДО start/restart/enable. Закрывает баг: после правки unit-файла без reload `systemctl restart` тихо рестартует со СТАРЫМ определением (exit 0, лишь warning). Поведение управляется опциональным параметром `daemon_reload` (string enum `auto` | `always` | `never`, **default `auto`**, объявлен в `shared/coremanifest/service.yaml` на states `running`/`restarted`/`enabled`; на `stopped` НЕ объявляется — там reload не нужен): `auto` — reload только при `NeedDaemonReload=yes` (gated, идемпотентно); `always` — reload безусловно; `never` — явный opt-out. Механизм проверки — `systemctl show <unit> --property=NeedDaemonReload --value` (`yes`/`no`); на первом install нового unit флаг = `no` (systemd подхватит определение на start), reload не нужен. **reload НЕ помечает шаг `changed`** (changed остаётся функцией только start/restart/enable) — при реально выполненном reload в `output` добавляется диагностическое `reloaded: true`. `openrc`/`sysv` — **no-op** (у них нет daemon-reload). Реализация — хелпер `util.EnsureDaemonReloaded` рядом с `util.ServiceActive` (тот же mock-абельный Runner, без D-Bus/go-systemd); enum валидируется в `core.service.Validate` (неизвестное значение → ошибка валидации, не молча). Additive и обратно совместимо: существующие задачи без `daemon_reload` получают `auto`.
