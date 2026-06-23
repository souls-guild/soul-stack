# ADR-016. Стратегия parity с SaltStack/Ansible и лицензия Soul Stack

- **Контекст.** В мае 2026 пользователь сформулировал требование: «реализовать все актуальные модули из SaltStack и Ansible, чтобы экосистема была как минимум не хуже». Это **направление работ на месяцы**, требующее декомпозиции и фиксации стратегии до старта. Параллельно вскрыт блокер: лицензия Soul Stack не была явно установлена, без неё нельзя принимать решения по wrapper-возможности (GPLv3 Ansible — copyleft-риск) и нельзя выкладывать SDK для авторов плагинов.
- **Решение.**

  **(a) Лицензия Soul Stack — Apache 2.0.** OSI-approved, permissive, patent grant. Стандарт для современной Go-инфраструктуры (Kubernetes, Vault, Terraform, Prometheus, etcd). Не препятствует корпоративным усыновлениям. Файл [`LICENSE`](../../LICENSE) в корне репозитория. Copyright header — `Copyright 2026 Soul Stack Authors` в каждом файле кода (через линтер при появлении первого исходника). **Open core / freemium**-монетизация: дополнительные платные продукты (enterprise SSO, audit-exports, managed HA, premium support) — **отдельные репозитории** под отдельной коммерческой лицензией, тянут Apache 2.0 ядро как зависимость. Это не часть данного репозитория и не часть Apache 2.0 кодовой базы.

  CLA (Contributor License Agreement) — заводится при появлении **первого внешнего contributor-а**, не сейчас. До тех пор copyright holder — единый, лицензия меняется свободно (если когда-нибудь понадобится).

  **(b) Стратегия parity — гибрид без wrapper-а.** Никакого встраивания Ansible/Salt-кода в Soul Stack ни в каком виде. Парности добиваемся через:
  - **Core MVP — наш рерайт на Go** (см. [ADR-015](0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)). Статически встроен в `soul`-бинарь.
  - **Экзотика — community-плагины** `soul-mod-*` / `soul-cloud-*` / `soul-ssh-*` в отдельных репозиториях через наш Go SDK ([ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам), `sdk/`). Авторы плагинов сами решают: написать с нуля, портировать из Salt (Apache 2.0 — лицензионно ok), портировать из Ansible (GPLv3 — нельзя в плагин для нашей Apache-системы, нужен рерайт).
  - **Wrapper Ansible-модулей запрещён** — GPLv3 copyleft-риск контаминации, Python-runtime +attack surface противоречит «безопасность на первом месте», шаблонизаторы не совпадают (Jinja2 vs CEL+Go text/template).
  - **Wrapper Salt-модулей не рекомендуется** — лицензионно ok (Apache 2.0), но Python-runtime тот же риск, и Salt-модули завязаны на Salt grains/pillars/loader — wrapper становится half-rewrite.

  Принцип «безопасность на первом месте» здесь означает **безопасный default + аудируемый opt-out под ответственность оператора**, а не вечный запрет возможностей: контур (например https-only / SSRF-guard / TLS-verify в `core.url`/`core.http`, [ADR-015](0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)) взведён по умолчанию и снимается явным per-call-флагом с warning в output `warnings` — оператор осознанно ослабляет защиту и видит это в `RunResult`.

  **(c) Поэтапная карта parity:**

  1. **Фаза 0 (сейчас).** Core MVP по [ADR-015](0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список). Первый сервис E2E.
  2. **Фаза 1.** SDK для авторов плагинов (`sdk/module/`, `sdk/clouddriver/`, `sdk/sshprovider/` — уже в [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). Шаблон-репо `soul-mod-template`. Документация «как написать модуль за час».
  3. **Фаза 2.** Первые ~10 official `soul-mod-*` для горячих кейсов: `postgresql_user`, `redis_acl`, `nginx_vhost`, `docker_container`, `k8s_namespace`, `certbot`, `haproxy`, `mysql_user`, `rabbitmq_user`, `vault_kv`.
  4. **Фаза 3.** Community-onboarding. Имя «коллекция модулей» (open Q в [module-collections.md](../module-collections.md)) — отдельная задача.
  5. **Фаза 4 (cloud parity).** 3 CloudDriver в MVP (AWS / GCP / Azure — приоритет open Q №13), остальное — community.

  **(d) Не покрывается этим направлением:**
  - ~~Event-driven контур (Salt beacons/engines-эквивалент) — нет в Soul Stack, отдельный новый ADR-кандидат на будущее. Backlog.~~ **Закрыт [ADR-030](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor):** beacons-контур введён (Vigil/Portent/Oracle/Decree); community-проверки — через 4-й plugin-kind `soul_beacon`. Engines-эквивалент (long-running на хосте/Keeper-е) ADR-030 **не** вводит — остаётся backlog.
  - Network-OS / proxy-minions — отдельная развилка, когда появится сценарий.
  - Windows-поддержка — отдельная развилка, когда появится сценарий.

  **(e) Документ-стратегия.** Детальная раскладка (инвентарь, категории, маппинг на 3 плагин-контракта, поэтапную карту) предполагалась отдельным документом `docs/ecosystem-parity.md` — он не создавался; стратегия parity зафиксирована здесь, в [ADR-016](#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) (раздел Decision/Consequences). Этот ADR — единый источник правды решения.

- **Consequences.**
  - В корне репо файл [`LICENSE`](../../LICENSE) (Apache 2.0).
  - В [`docs/architecture.md`](../architecture.md) появляется новый раздел «Лицензия» (или ссылка на LICENSE) — отдельная задача.
  - Любой плагин в `soul-mod-*` / `soul-cloud-*` / `soul-ssh-*` репозитории, использующий код под GPLv3, **не может быть включён в official-список Soul Stack** без переписки. Community-плагины под любой совместимой лицензией принимаются как community.
  - Wrapper Ansible-модулей не рассматривается как опция ни на каком этапе.
  - Open Q №5 в части стратегии — закрыт. Open Q «Лицензия Soul Stack» — не появляется (закрыт сразу).
- **Trade-offs.**
  - Parity достигается медленнее, чем при wrapper-варианте (годы community-работы vs мгновенный охват). Принимаем — безопасность + лицензионная чистота критичнее.
  - Apache 2.0 не блокирует AWS-Soul-as-a-service конкурентов. Если в будущем это станет проблемой — миграция на BSL/SUL возможна для **новых версий**, но потребует CLA от всех contributors (поэтому CLA лучше завести до того, как pool contributors вырастет — отдельная operational задача).
  - Open core разделение требует дисциплины: enterprise-фичи должны жить в отдельном репозитории под другой лицензией, не в этом. Случайный contribution enterprise-фичи в Apache-ядро рискует «случайно» сделать её open source — нужен явный gate (review).

- **Amendment (2026-05-27, Plugin SDK Фаза 2 — closure).** Фаза 2 ((c).2 поэтапной карты) фиксируется готовой к старту. PM-decisions, закрывающие развилки списка/namespace/template-механизма/repos/coverage:

  - **(f) Десять первых official `soul-mod-*` (final list, namespace `official`):**

    | # | Binary | Назначение |
    |---|---|---|
    | 1 | `soul-mod-official-postgres-user` | PostgreSQL роли (parity-pair к `postgres-db`) |
    | 2 | `soul-mod-official-postgres-db` | PostgreSQL базы |
    | 3 | `soul-mod-official-mysql-user` | MySQL/MariaDB пользователи |
    | 4 | `soul-mod-official-mysql-db` | MySQL/MariaDB базы |
    | 5 | `soul-mod-official-nginx-vhost` | nginx virtual hosts |
    | 6 | `soul-mod-official-haproxy-backend` | HAProxy backend / server |
    | 7 | `soul-mod-official-docker-container` | Docker контейнер |
    | 8 | `soul-mod-official-letsencrypt-cert` | Let's Encrypt / certbot сертификат |
    | 9 | `soul-mod-official-redis-acl` | Redis ACL |
    | 10 | `soul-mod-official-rabbitmq-user` | RabbitMQ пользователи |

    Замещает черновой список из ADR-016 (c).Фаза 2 (`k8s_namespace` отвергнут — это CloudDriver-параллель, не SoulModule; `vault_kv` покрыт keeper-side `core.vault.kv-read` в [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read); добавлены parity-pair `postgres-user`+`postgres-db` и `letsencrypt-cert`).

  - **(g) Reserved namespace `official`.** Manifest-имя плагина: `<namespace>.<name>` = `official.postgres-user` (валидируется существующими regex-ами `shared/plugin` — `namespace` и `name` оба `^[a-z][a-z0-9-]{0,62}$`, см. [naming-rules.md → Plugin manifest: regex имён](../naming-rules.md#plugin-manifest-regex-имён)). Binary-name pattern — `soul-mod-official-<name>`. Sigil-trust под namespace `official` подписывается keeper-cluster-issuing-key.

  - **(h) Template-механизм — гибрид.** Шаблон-репо `soul-stack-plugins/soul-mod-template/` (clone-and-modify) **И** CLI `soul-lint plugin-init <namespace>/<name>` (embed-uses static template через `go:embed`). Оба пути выдают идентичное дерево. Первичный источник правды — embed в `soul-lint/internal/plugininit/template/`; копия в companion-репо синхронизируется вручную (sync-job — backlog). `soul-lint` расширяется новым subcommand-ом — формальное расширение scope офлайн-линтера на init-tooling зафиксировано здесь (propose-and-wait пройден решением пользователя).

  - **(i) Repos structure.** Pilot monorepo `soul-stack-plugins/` для SDK-1 (этот amend) + SDK-2 (3 pilot модуля) + SDK-3 (тираж 7). После SDK-2 review (стоп-гейт) — extract per-module через `git filter-repo`. Цель к концу SDK-3: каждый плагин = свой git-репо для clean release-cycle.

  - **(j) L3b coverage strategy.** 5 flagship модулей идут под full L3b live-test: `postgres-user` / `nginx-vhost` / `docker-container` / `letsencrypt-cert` / `haproxy-backend`. Остальные 5 — L0 + L1 (testcontainers). Дисбаланс осознанный: cost-of-L3b runtime высок (privileged Debian-12 + bootstrap depend-сервисов), flagship-набор покрывает 3 категории (relational DB + reverse proxy + container-runtime).

  - **(k) state_schema у плагинов.** Плагины НЕ владеют `state_schema` (это атрибут service-уровня, [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). Плагин может **читать** `incarnation.state` через `register`-механизм service.yml, но не вводит свои version-цепочки.

  - **(l) Slice-карта.** SDK-1 (template + CLI + amend, этот commit) → SDK-2 (3 pilot модуля postgres-user/nginx-vhost/docker-container — full pipeline) → SDK-3 (тираж 7 в 2-3 параллельных батчах) → SDK-4 (community-onboarding: Sigil-flow, listing).

  - **(m) Companion-репо `soul-stack-plugins/`.** Parity с `soul-stack-web` extraction — отдельный git-репо, не входит в core go.work. Apache 2.0 [LICENSE](https://github.com/co-cy/soul-stack-plugins/blob/main/LICENSE) (parity с core). Содержимое: `soul-mod-template/` + `docs/module-author-guide.md` + (от SDK-2) каталоги плагинов.

- **Amendment (2026-05-27, SDK-2 pilot — closure).** SDK-2 завершён: первые 3 official `soul-mod-*` модуля готовы как pattern-fixture для SDK-3-тиража 7 модулей.
  - **Что готово.** В `soul-stack-plugins/` (отдельный git-репо, не в core go.work):
    - `soul-mod-official-postgres-user/` — idempotent CREATE/ALTER/DROP ROLE через `pgx/v5` + probe `pg_roles`.
    - `soul-mod-official-nginx-vhost/` — render vhost-config (Go `text/template`) + `nginx -t` validate ДО write + symlink `sites-enabled/` + `nginx -s reload`. Plan + `PlanReadSafe` ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) реализованы (drift-detect через сравнение content + symlink target).
    - `soul-mod-official-docker-container/` — трёхсостояная `running`/`stopped`/`absent` через docker-CLI + drift-detect (image/env/ports/volumes/networks/restart_policy → recreate stop+rm+create+start). Plan + `PlanReadSafe`.
  - **Pattern fixture (фиксируется для SDK-3 тиража).**
    - Scaffold через `soul-lint plugin-init official/<name>` ([Amendment 2026-05-27, SDK-1](#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)). После scaffold — handler.go в `internal/<name_snake>/`, не в `internal/` (исправление scaffold-drift, см. observations SDK-2 review).
    - State-semantics: `present`/`absent` (постоянные ресурсы) или `running`/`stopped`/`absent` (контейнеры/процессы). Стратегия — естественная семантика ресурса, не догма «всегда present/absent».
    - Уровни тестов: **L0** (in-memory fake-runner + fake stream через `grpc.ServerStreamingServer[ApplyEvent]`, покрытие create/alter/idempotent/drop/unknown-state/error-paths) + **L1** (testcontainers или real-daemon с build-tag `integration`, full-lifecycle) + **L3b** (skeleton с `t.Skip`, build-tag `live`, ждёт Vigil-extension L3b-harness в core-repo).
    - DI через optional-поля модуля (`connect func(...)` / `runner dockerRunner` / `fs vfs`+`runner cmdRunner`) — nil → real-impl, L0 подсовывает fake.
    - Secret-input ([shared/plugin.input_secret_without_vault_pattern](https://github.com/co-cy/soul-stack/blob/main/shared/plugin/manifest.go)) — `secret: true` + `pattern: "^vault:.*"`. Keeper-side vault-resolve ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) резолвит `vault:`-ref в реальное значение ДО Apply — плагин видит резолвлённое.
    - manifest.yaml формат `side_effects[]` — `[{<resource-type>: <name>}]` (`user`/`file`/`service`/...), closed enum.
  - **L3b harness Vigil-extension в core-repo** — отдельный slice пост-SDK-2 (skeleton-тесты `t.Skip` ждут).
  - **Sigil-подпись плагинов** ([ADR-026](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)) — пока НЕ требуется (pilot dev-cycle). Production signing flow — SDK-4.
  - **Per-module repo extraction** — отдельный slice post-SDK-2-review через `git filter-repo`.
  - Закрывает SDK-2 ((l) Slice-карта).

- **Amendment (2026-06-22, фаза дистрибуции закрытой беты).** Рассматривался выпуск закрытой беты под проприетарной лицензией (all rights reserved + beta-EULA «только для оценки») с последующим ручным переключением на Apache 2.0 на GA.

  - **(n) Проприетарная бета-лицензия — отвергнута.** Решение в пользу более простого и менее рискованного варианта: лицензия **остаётся Apache 2.0 на всём протяжении жизненного цикла** — решение (a) в силе без изменений. «Закрытость» беты обеспечивается **операционно** — приватными репозиториями (org `souls-guild`) и доступом по приглашению, а не лицензией. «Переход в публичную фазу» на GA означает **открытие репозиториев** (public), а не смену лицензии. Следствия: править `LICENSE`-файлы, лицензионные метки в nfpm-манифестах (`license: Apache-2.0` корректна) и лицензионные заявления в публичной доке **не требуется**. SDK и community-плагины также остаются Apache 2.0 (как в (a)). Отдельный beta-EULA как артефакт **не вводится**.
