# ADR-013. Bootstrap первого Архонта

- **Контекст.** RBAC Keeper-а — `default_policy: deny` без исключений ([rbac.md](../keeper/rbac.md)). При первой инициализации кластера в реестре `operators` пусто; без специального механизма любые API/MCP-вызовы падают с 403, и нельзя выпустить ни второго оператора, ни первую machine-identity. Open Q №1 в [Открытых вопросах](../architecture.md#открытые-вопросы). Параллельно: имя сущности «первый оператор» не зафиксировано в [naming-rules.md](../naming-rules.md) — нужен propose-and-wait. Имя `Bootstrap` уже занято [ADR-012(f)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) для Soul-онбординга — для оператора нужно другое.
- **Решение.**

  **(a) Имя сущности — Archon (Архонт).** Греч. «верховный правитель, высшее должностное лицо», ложится на мифо-палитру Soul Stack (Keeper / Souls / Destiny / Soulprint / Essence / SoulSeed / Coven / Reaper). Семантически точно: «верховный администратор кластера», не «творящий» (Keeper не создаёт души, он их управляет). Зафиксировано в [naming-rules.md → Сущности предметной области](../naming-rules.md#сущности-предметной-области). Идентификатор — **AID** (Archon ID), строчная ASCII-строка вида `archon-alice` / `archon-ops-01` / `alice@corp.com` (regex и форма — [ADR-014(c)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). AID свободен от конфликтов с известными стандартами (не OID/ASN.1, не DID/W3C, не PID/GID/unix). **Безопасность AID опирается не на префикс `archon-`** (он снят amendment-ом 2026-05-29, см. [ADR-014(c)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), **а на строгий charset** `[a-z0-9._@-]` со стартом с alphanumeric: нет `/`/`\` (AID попадает в имена файлов bootstrap-токена — нет path-traversal), только ASCII-lowercase (нет unicode-двойников и неоднозначности регистра), нет управляющих/кавычек (нет инъекций в логи/JWT/SQL). Префикс снят, чтобы AID напрямую вмещал внешнее identity-имя из LDAP/Keycloak.

  **(b) Механизм выпуска первой credential — отдельная команда `keeper init`.** Одноразовая administrative subcommand самого `keeper`-бинаря:

  ```
  keeper init --archon=<aid> --config=/etc/keeper/keeper.yml [--credential-out=/etc/keeper/archon-credential.json]
  ```

  - Команда проверяет, что реестр `operators` в Postgres пуст (через PG advisory lock, см. (e) ниже).
  - Создаёт первого Архонта с указанным AID, прикладывает к нему роль `cluster-admin` ([ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).
  - Выпускает JWT-credential (форма — см. [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) и кладёт в файл с `mode 0400` (по аналогии с SoulSeed на хосте Soul — см. [`soul/onboarding.md`](../soul/onboarding.md)).
  - При повторном вызове на уже инициализированной БД отказывается с сообщением «cluster already initialized; archon <aid> exists since <ts>».

  Команда — **administrative subcommand of `keeper`-binary for self-initialization**, не «keeper в клиентском режиме». Это явно НЕ противоречит [ADR-004](0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) (запрет на клиентские подкоманды): фактическую работу делает локально установленный Keeper-бинарь над собственным состоянием в Postgres, а не «keeper подключается к удалённому keeper-у как клиент». Других административных subcommand-ов (`keeper migrate`, `keeper bootstrap-something-else`) на текущий момент не вводится — каждое исключение фиксируется отдельным ADR-ом.

  **(c) Привилегии первого Архонта — явная роль `cluster-admin` с `permissions: ["*"]`.** Запись в `operators` + привязка AID к роли `cluster-admin`. **С [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres):** привязка — это membership-строка `(cluster-admin, <aid>)` в PG-таблице `rbac_role_operators` (не запись в YAML-список `roles[].operators`); роль `cluster-admin` и её permission `*` приходят из seed-миграции (E1). `keeper init` в своей advisory-lock-транзакции (e) пишет **только** эту membership-строку — это фикс BUG-1 ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres) Контекст: раньше init клал `roles` в JWT-claim, который enforcer не читал). Регулярная RBAC-модель, без особых case-ов и hardcoded super-role в коде. Инвариант: **нельзя удалить последнего оператора с активным `*`-permission** — попытка через API возвращает 409 «would lock out the cluster». Это защита от случайного / злонамеренного self-lockout-а.

  **(d) Restart-семантика — отказ без `--initialize`.** Если на старте Keeper видит, что `operators` в Postgres пуст:
  - **Без `--initialize` флага** (либо без переменной `KEEPER_INITIALIZE=true`) — Keeper отказывается стартовать с сообщением `«operators registry is empty; run 'keeper init --archon=<aid>' before starting the cluster»`. Exit code != 0.
  - **С `--initialize`** — Keeper стартует в read-only-режиме (listeners поднимаются, но все API/MCP-вызовы возвращают 503 «cluster awaiting first archon»), пока `keeper init` не отработает.

  Это защита от случайного re-bootstrap-а после catastrophic wipe Postgres — без явного флага случайный наблюдатель не получит автоматически выпущенный admin-token из логов.

  **(e) HA race-condition — Postgres advisory lock.** При первом старте N инстансов одновременно: `keeper init` берёт PG advisory lock (`pg_advisory_xact_lock(<keeper_bootstrap_lock_id>)`) и под lock-ом проверяет `SELECT count(*) FROM operators`. Если ноль — выпускает Архонта, если не ноль — отказывается. Остальные одновременные `keeper init` ждут lock, видят непустой реестр, отказываются с указанием уже созданного Архонта.

  При обычном старте (не `keeper init`) — каждый инстанс независимо проверяет, что `operators` непуст, и стартует / отказывается по правилу (d). Race здесь не возникает: проверка только читает.

  **(f) Аудит.** Выпуск первого Архонта пишется в `audit_log` ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — `event_type: operator.created`, `archon_aid: NULL` (первый — без инициатора-Архонта), KID-исполнителем и `payload: { bootstrap_initial: true, ... }`. Не путать с `incarnation.state_history` (per-incarnation snapshots, [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). Связанные OTel-события — обязательны.

- **Consequences.**
  - В `keeper`-бинаре появляется subcommand `keeper init` (отдельная команда, не подключение к удалённому keeper-у).
  - В `keeper.yml` появляется опциональный флаг `bootstrap.initialize: true` (эквивалент CLI `--initialize`) для оркестраторов, которые не управляют процессом флагами.
  - Таблица `operators` в Postgres — обязательный реестр; FK-поля `created_by_aid`, `changed_by_aid` и подобные теперь работают (см. [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).
  - Open Q №1 закрыт.
  - В [`docs/keeper/rbac.md`](../keeper/rbac.md) раздел «Bootstrap первого оператора («Создатель»)» обновляется под имя Archon.
- **Trade-offs.**
  - Operator-experience: при первой установке нужно явно запустить `keeper init` — не «развернул контейнер → попал в UI». Это сознательный trade-off: безопасность > удобство.
  - При catastrophic recovery (truncate `operators`) автоматического re-bootstrap нет — оператору надо снова `keeper init`. Это и есть защита.
  - `keeper init` исполняется локально на хосте Keeper-а — для cloud-deployment (Helm/K8s) понадобится Init Container или Job. Это закрывается операционной практикой, не архитектурой.

**Amendment 2026-06-23: инвариант «ровно один bootstrap-Архонт» выражается через `created_via`.**

Под модель провижининга операторов ([ADR-058](0058-operator-auth-ldap-oidc.md#adr-058-федеративная-аутентификация-операторов-archon--ldap--oauth2oidc), поле `operators.created_via`, [ADR-014 amendment 2026-06-23](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) data-инвариант «ровно один первый Архонт» переформулирован: партиал-unique index `operators_first_archon_idx` перенесён с `WHERE created_by_aid IS NULL` на **`WHERE created_via='bootstrap'`** (миграция 085). Проверка `keeper init` (e) «реестр пуст / уже инициализирован» по смыслу неизменна — `keeper init` создаёт строку с `created_via='bootstrap'` под тем же advisory lock. Следствие для (f): первый Архонт по-прежнему пишется с `created_by_aid: NULL` и `payload.bootstrap_initial: true`, но единственность теперь гарантирует `created_via`, а не NULL у `created_by_aid` — это легализует `created_by_aid=NULL` у federated/system-операторов (`archon-system`, LDAP/OIDC auto-provision), которые раньше конфликтовали с прежним индексом.
