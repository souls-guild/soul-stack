# ADR-014. Identity-модель оператора (Archon)

- **Контекст.** Сейчас в docs/ имя оператора фигурирует как plain-string в `roles[].operators` ([rbac.md](../keeper/rbac.md)) и в FK-полях `created_by_operator_id` / `changed_by_operator_id` в нескольких таблицах ([`soul/identity.md`](../soul/identity.md), `incarnation`, `state_history`) — но **самой таблицы `operators` в Postgres-схеме нет**. FK ссылаются «в никуда». [ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) фиксирует механизм первого Архонта; этот ADR закрывает форму identity, реестр и аудит.
- **Решение.**

  **(a) Реестр `operators` в Postgres — обязательный.** Поля минимум:
  - `aid` — primary key, kebab-case строка (`archon-alice`).
  - `display_name` — человекочитаемое имя.
  - `auth_method` — enum: `jwt` (MVP), `mtls` / `combined` (post-MVP — без breaking change через миграцию).
  - `created_at` — timestamp.
  - `created_by_aid` — FK на `operators(aid)`; для первого Архонта `NULL` (инвариант: ровно одна запись с `created_by_aid IS NULL`, разрешена только при bootstrap).
  - `revoked_at` — timestamp или `NULL` (активный).
  - `metadata` — `jsonb`, для будущих расширений (email, MFA-флаги и т.п.).

  FK-поля `created_by_operator_id` в других таблицах переименовываются в `created_by_aid` / `changed_by_aid` и становятся настоящими FK на `operators(aid)`.

  Хранение реестра в Postgres (а не в `keeper.yml`) даёт hot-add операторов через OpenAPI/MCP, нормальный аудит-трейл, и работающие FK.

  **RBAC-storage перенесён в БД ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** На момент фиксации ADR-014 роли / permissions / привязка «оператор ↔ роль» оставались в `keeper.yml::rbac` — это давало BUG-1 (init создаёт Архонта в БД, но membership пишет в JWT-claim, который enforcer не читает, резолвя его из YAML; см. [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)). С ADR-028 весь RBAC в Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`). **Разведение термина «FK»:** настоящий PG foreign key — `created_by_aid` / `changed_by_aid` (этот пункт) и `rbac_role_operators.aid` / `granted_by_aid` ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)); прежний метафорический «FK» YAML-списка `roles[].operators` — это **membership** (привязка), теперь материализованная строкой `rbac_role_operators`, а не ссылка в файле. JWT-claim `roles` ([ADR-014(b)](#adr-014-identity-модель-оператора-archon)) перестаёт быть источником membership-а — авторитет membership-а — таблица `rbac_role_operators`.

  **Amendment (2026-06-09, Synod — группа архонов [ADR-049](0049-synod.md#adr-049-synod--группа-архонов)).** Membership «архон ↔ роль» больше не исчерпывается прямыми строками `rbac_role_operators`: вводится промежуточный уровень **[Synod](0049-synod.md#adr-049-synod--группа-архонов)** (группа архонов, бандлящая роли) — модель **Архон → Synod → Роли**. **Эффективные роли архона = прямые (`rbac_role_operators`) ∪ роли через все его Synod-ы** (`synod_operators` ⋈ `synod_roles`). Объединение собирается в snapshot-сборке enforcer-а ([ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)); JWT-claim `roles` по-прежнему не источник membership-а. Где в этом ADR/реконсиляции сказано «эффективные права/роли оператора» (audit `archon.aid`, self-lockout-инвариант) — теперь читается как «прямые ∪ через Synod».

  **(b) Форма credential — JWT (MVP).** `keeper init` и последующие API-вызовы выпуска новых Архонтов возвращают JWT-токен:
  - Claims: `iss: <kid-cluster>`, `sub: <aid>`, `iat`, `exp`, `roles: [...]`, `bootstrap_initial: true|false`.
  - Signing key — keeper-side, хранится в **Vault KV** (на MVP) под путём `secret/keeper/jwt-signing-key`. Post-MVP — Vault Transit для подписи без экспорта ключа.
  - **Аутентификация самого Keeper-а в Vault** (для чтения signing key и прочих `*_ref`) — `vault.auth.method`: `token` (dev/local, статический токен) и **`approle` (реализовано)** для прод. AppRole-credentials берутся локально (`role_id` inline + `secret_id_file`/`secret_id_env`), а не из Vault — иначе циклическая зависимость. Полученный client-token продлевается в фоне. Спека полей — [`docs/keeper/config.md → vault`](../keeper/config.md#vault). Vault Transit для подписи и mTLS-auth Keeper↔Vault — post-MVP.
  - TTL первого bootstrap-токена — **30 дней** (длиннее обычного, чтобы оператор успел настроить дальнейшее администрирование). Обычные post-bootstrap токены — короче (рекомендация: 24h с refresh, конкретные TTL — в `keeper.yml → auth:`).
  - `Authorization: Bearer <jwt>` на всех HTTP/MCP-вызовах.
  - На стороне Keeper-а — JWT-middleware на всех listener-ах OpenAPI/MCP; payload `sub` → AID → RBAC-проверка.

  mTLS-cert для machine-identity (CI, MCP-агенты) и комбинированная форма — отдельная задача post-MVP. Расширение реализуется через `auth_method` enum в `operators` без breaking change.

  **(c) Идентификатор — AID (Archon ID).** Симметрично [SID/KID](../naming-rules.md#идентификаторы). Строчная ASCII-строка, человекочитаемая. Свободен от конфликтов со стандартами (см. [ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)(a)). Валидация: `^[a-z0-9][a-z0-9._@-]{1,127}$` — первый символ буква/цифра, далее charset `a-z 0-9 . _ @ -`, общая длина 2..128 (примеры: `archon-alice`, `archon-ops-01`, `archon-ci-deployer`, email-подобный `alice@corp.com`, ldap-uid `uid-4815`).

  **Amendment (2026-05-29): обязательный префикс `archon-` снят, charset расширен.** Прежняя форма `^archon-[a-z0-9-]{1,62}$` требовала жёсткого префикса и не допускала `. _ @`. Под LDAP/Keycloak auto-provision внешних identity (где `sub`/`uid` — это `alice@corp.com` или `uid-4815`) AID должен напрямую вмещать внешнее имя без искусственной обёртки. Новый charset намеренно безопасный: нет `/`/`\` (path-traversal — AID встраивается в имена файлов bootstrap-токена), только ASCII-lowercase (нет unicode-двойников и регистра), нет управляющих/кавычек (нет инъекций); старт с alphanumeric исключает скрытые `..`/`.`-префиксы. Прежние AID вида `archon-<...>` остаются валидными (charset — надмножество). Защита идентичности больше **не** опирается на наличие префикса `archon-`, а на строгий charset + старт с alphanumeric. **Внешний identity-маппинг** (issuer / external_subject → aid) **не вводится** этим amendment-ом: заложен через существующие `metadata` jsonb + `auth_method` enum, материализация — отдельным поздним ADR при первом реальном запросе, новая сущность сейчас не плодится.

  **(d) Жизненный цикл Архонта.**
  - **Создание:** через `keeper init` (только первого Архонта) или через OpenAPI/MCP с RBAC-permission `operator.create` (для остальных). API возвращает JWT-токен; повторный запрос JWT для существующего Архонта — отдельный endpoint `operator.issue-token` с auth от другого Архонта с правом `operator.issue-token`.
  - **Ревокация:** через OpenAPI/MCP с permission `operator.revoke` — устанавливает `revoked_at`. Активные JWT-токены отозванного Архонта продолжают работать до своего `exp` (короткий TTL — естественная защита; принудительный отзыв «всех живых JWT» — отдельная задача post-MVP, требует JWT-blocklist или session-store).
  - **Удаление записи** — не предусмотрено. Архонты только ревокаются (для аудита `created_by_aid` должен оставаться валидным FK).

  **(e) Audit-trail.**
  - Реестр `operators` сам по себе журналит создание/ревокацию.
  - Все мутации Архонтом других сущностей пишут `changed_by_aid` / `created_by_aid` — это FK, инвариант поддерживается БД.
  - OTel: каждый API/MCP-вызов после успешной JWT-аутентификации имеет атрибут `archon.aid=<aid>`.
  - Audit-events жизненного цикла Архонта (`operator.created` / `operator.revoked` / `operator.access_denied` и т.п.) пишутся в общий audit-pipeline — единая нормировка storage, schema, write-path и retention — [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention).

- **Consequences.**
  - Таблица `operators` — обязательный реестр в Postgres ([keeper/storage.md](../keeper/storage.md) дополняется).
  - FK-поля в `souls`, `bootstrap_tokens` (Soul-side), `incarnation`, `state_history`: переименование `created_by_operator_id` → `created_by_aid`, добавление FK на `operators(aid)`. Это требует миграции (отдельная задача — пока кода нет, миграция будет первой).
  - `keeper.yml` приобретает блок `auth:` с настройками JWT (signing key Vault path, TTL по умолчанию, JWT issuer name). Подробности — отдельная задача, не входит в этот ADR.
  - Vault обязательно содержит `secret/keeper/jwt-signing-key` до старта Keeper-а (либо Keeper сам генерирует и кладёт при `keeper init` — реализационная развилка, не блокер).
  - Форма Operator API (управление Архонтами + остальные endpoint-ы под permissions) задаётся **Go-типами handler-ов** (huma v2 full-typed, code-first, [ADR-054](0054-openapi-code-first.md#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2) заменил [ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict)); OpenAPI-спека ([`docs/keeper/openapi.yaml`](../keeper/openapi.yaml)) — производный снимок, oapi-codegen снесён. Транспорт — REST/HTTP, без gRPC-сервиса. Прежний `proto/operator/v1/*.proto` упразднён (аменд [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). markdown-нормирование HTTP-фасада и mapping-а endpoint ↔ MCP-tool ↔ permission — [`docs/keeper/operator-api.md`](../keeper/operator-api.md).
- **Trade-offs.**
  - JWT без revocation-blocklist означает, что отозванный Архонт может работать до конца TTL своего токена. На MVP принимаем — TTL короткий (24h по умолчанию для не-bootstrap токенов), blast radius ограничен.
  - hot-add операторов через API требует, чтобы хотя бы один Архонт с правом `operator.create` существовал — естественное следствие, не противоречит.
  - `created_by_aid IS NULL` только у первого Архонта — это data-инвариант, должен поддерживаться partial unique index в Postgres (`CREATE UNIQUE INDEX ON operators ((created_by_aid IS NULL)) WHERE created_by_aid IS NULL`).
  - JWT-токен в файле `mode 0400` после `keeper init` — оператор должен надёжно сохранить его перед перезапуском Keeper-а; восстановить «потерянный bootstrap-token» можно только через ручную SQL-операцию (либо `keeper init --reissue-token --force` — отдельная задача).

**Amendment 2026-05-27: near-instant revocation через RBAC-снимок.**

ADR-014(d) изначально пометил «принудительный отзыв всех живых JWT» как
post-MVP. На прод-релизном гейте поднят кейс «уволенный сотрудник
сохраняет JWT-доступ до exp». Решение — не вводить новую сущность
JWT-blocklist / refresh-token, а расширить уже существующий механизм
RBAC-снимка (B2, `rbac:invalidate`, [ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)):

  - `operators.revoked_at` читается в `rbac.LoadSnapshot` как четвёртая
    проекция `Revoked map[string]time.Time`.
  - `rbac.Enforcer.Check` в начале метода отвергает запросы от ревокнутого
    AID с новым sentinel-ом `ErrOperatorRevoked`.
  - `operator.service.Revoke` после успешного UPDATE публикует
    `rbac:invalidate` (тот же топик, что роль-мутации) — все Keeper-инстансы
    rebuild-ят снимок в миллисекунды.
  - Fail-soft fallback — TTL-poll `rbac.DefaultRefreshInterval` (10s).
  - Middleware mapping: revoked AID на verify → 401 (parity с expired);
    отдельный `TypeOperatorRevokedToken` (URN `…/errors/operator-revoked-token`),
    чтобы не пересекаться с 409 `TypeOperatorRevoked` на write-side
    (IssueToken/Revoke для уже ревокнутого AID).

Окно реальной деблокировки — единицы миллисекунд при здоровом Redis,
до 10 секунд при потере pub/sub-сообщения.

**JWT TTL остаётся defense-in-depth.** Рекомендация — снизить default
`auth.jwt.ttl_default` до 1h на проде.
