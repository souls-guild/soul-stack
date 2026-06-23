# ADR-058. Федеративная аутентификация операторов (Archon) — LDAP + OAuth2/OIDC

> **Статус: draft / proposed.** Это ЧЕРНОВИК дизайна для propose-and-wait. Имплементация — только после явного одобрения пользователя. Зафиксированы модель и развилки; код в репозитории — пока только скелет (интерфейсы / конфиг-типы / TODO-заглушки), без реальной LDAP/OIDC-логики и без изменения прод-auth-flow.

- **Контекст.** Identity-модель оператора зафиксирована в [ADR-013](0013-bootstrap-archon.md) (Bootstrap первого Архонта через `keeper init --archon`) и [ADR-014](0014-operator-identity.md) (реестр `operators` в Postgres: `aid` PK, `auth_method` enum, JWT-credential MVP с claims `iss`/`sub`/`iat`/`exp`/`roles`/`bootstrap_initial`, signing key из Vault KV `secret/keeper/jwt-signing-key`). `auth_method` уже несёт расширяемый enum (`jwt` реализован, `mtls`/`combined` заявлены post-MVP как only-add). RBAC ([ADR-028](0028-rbac-storage.md)/[ADR-047](0047-purview.md)) оперирует ролями по AID и не зависит от способа аутентификации.

  Сейчас единственный способ завести оператора — `keeper init` (первый) либо `POST /v1/operators` существующим админом с permission `operator.create` (выпускает JWT файлом/в ответе). Для интеграции с корпоративными identity-провайдерами (Active Directory / LDAP, Keycloak / Okta / Google / Azure AD через OIDC) нужен federated-login: человек-Архонт аутентифицируется у внешнего IdP, а Keeper доверяет результату и выдаёт свой внутренний JWT.

- **Решение (рекомендуемое — федеративная модель).** Внешняя аутентификация **валидируется на Keeper-е** и **маппится** на реестр `operators` (AID) + RBAC-роли, после чего Keeper выпускает **внутренний JWT** существующим `jwt.Issuer` (ADR-014). Вся остальная система (auth-middleware, RBAC, MCP, OpenAPI) остаётся JWT-based и **не меняется**.

  ```
  ┌──────────┐   LDAP bind / OIDC code-flow    ┌──────────────┐
  │  Архонт  │ ──────────────────────────────▶ │  внешний IdP  │
  └──────────┘                                  └──────────────┘
       │                                                │
       │  (1) login / (3) callback с id_token|creds     │ (2) аутентификация
       ▼                                                ▼
  ┌──────────────────────────────────────────────────────────┐
  │  Keeper  /auth/*  (НОВОЕ, публичные эндпоинты вне /v1)     │
  │  ┌────────────┐  validate   ┌──────────┐  map AID+roles    │
  │  │ ldap.Authn │ ──────────▶ │ identity │ ────────────────▶ │
  │  │ oidc.Authn │             │  mapper  │                    │
  │  └────────────┘             └──────────┘                    │
  │        │ (4) выпуск ВНУТРЕННЕГО JWT через jwt.Issuer (ADR-014)│
  └────────┼──────────────────────────────────────────────────┘
           ▼
   Bearer JWT → /v1/* (RequireJWT + RBAC БЕЗ изменений)
  ```

  ### Почему федеративная, а не альтернативы

  - **Отвергнуто: проброс внешнего токена напрямую в `/v1/*`** (Keeper валидирует чужой id_token на каждый запрос). Минусы: (a) auth-middleware и MCP пришлось бы учить два формата токена; (b) каждый `/v1`-запрос — внешний JWKS-lookup или повторная LDAP-связь (latency, доступность IdP на critical path); (c) RBAC-роли пришлось бы извлекать из чужих claims на каждый запрос. Федеративная модель локализует внешнюю зависимость в момент login, а не в каждый запрос.
  - **Отвергнуто: внешний IdP как единственный issuer (Keeper без своего JWT).** Ломает [ADR-013](0013-bootstrap-archon.md) (bootstrap первого Архонта без IdP) и offline/air-gapped-сценарии. Внутренний JWT — единая точка для revocation ([ADR-014](0014-operator-identity.md) amend) и короткого TTL.
  - **Принято: гибрид.** Bootstrap-Архонт (ADR-013) и `POST /v1/operators` остаются (нижний слой доверия, не зависит от внешнего IdP). LDAP/OIDC — дополнительный способ логина последующих операторов. Внешняя identity всегда сводится к строке реестра `operators(aid)` — единый authorization-субъект.

- **(a) Расширение `auth_method` enum (only-add, без breaking changes).** К существующим `jwt`/`mtls`/`combined` (ADR-014) добавляются:

  | значение | смысл |
  |---|---|
  | `ldap` | оператор аутентифицируется LDAP-bind, Keeper выпускает внутренний JWT |
  | `oidc` | оператор аутентифицируется OIDC-code-flow, Keeper выпускает внутренний JWT |

  Это additive-расширение в духе ADR-014 (mTLS/combined уже заявлены post-MVP через тот же enum). Меняются: Go-const `operator.AuthMethod`, huma-enum `OperatorAuthMethod` (`huma_enums.go`), SQL `CHECK auth_method IN (...)` (новая миграция, only-add значения). `auth_method` в строке `operators` фиксирует, каким способом оператор пришёл (для аудита и UI); сам внутренний JWT после выпуска одинаков для всех методов.

  > **Развилка имени:** `oidc` против `oauth2`. Рекомендуется **`oidc`** — Keeper полагается на OIDC id_token (identity layer над OAuth2), а не на чистый OAuth2 access-token. «oauth2» точнее для авторизации-без-identity, что нам не подходит. Финальное имя — propose-and-wait (новая сущность в словаре enum, [naming-rules.md](../naming-rules.md)).

- **(b) OAuth2/OIDC — authorization-code flow (логин человека-Архонта).**

  1. `GET /auth/oidc/login` (публичный) → Keeper генерирует `state` + `nonce` + PKCE `code_verifier`, кладёт их в короткоживущий server-side store (Redis, TTL ~5 мин, ADR-006 как координационный слой), редиректит на `authorization_endpoint` IdP с `code_challenge`.
  2. Архонт аутентифицируется у IdP.
  3. `GET /auth/oidc/callback?code=...&state=...` (публичный) → Keeper: проверяет `state` (CSRF), обменивает `code` на токены (`code_verifier`), **валидирует `id_token`**: подпись через JWKS (`jwks_uri` из discovery), `iss` == сконфигурированный issuer, `aud` == client_id, `exp`/`iat`, `nonce` == сохранённый. Извлекает `sub` + сконфигурированные claim-поля (email / preferred_username / groups).
  4. **Маппинг** на `operators(aid)` + роли (см. (e)). Выпускает **внутренний JWT** через `jwt.Issuer`. Возврат: либо JSON `{jwt: ...}` (для API/UI fetch), либо set-cookie + redirect на UI (форма доставки — развилка (g)).

  **Библиотеки:** `github.com/coreos/go-oidc/v3` (discovery + JWKS + id_token verify, Apache-2.0) + `golang.org/x/oauth2` (code-exchange, BSD-3 — совместимо с Apache-2.0). Discovery (`/.well-known/openid-configuration`) кешируется; JWKS — с TTL + key-rotation refetch (даёт go-oidc).

- **(c) LDAP (bind + group→role).**

  1. `POST /auth/ldap/login` (публичный) с body `{username, password}` поверх **HTTPS** (Keeper-listener TLS — обязателен).
  2. Keeper подключается к LDAP по **LDAPS** или **StartTLS** (plaintext-LDAP запрещён). Режим bind:
     - **search-bind** (рекомендуется): service-account (`bind_dn` + `bind_password_ref` из Vault) делает search по `user_filter` (`(uid=%s)` / `(sAMAccountName=%s)`), находит user-DN, затем re-bind этим DN + введённым паролем. Поддерживает гибкие схемы каталога.
     - **direct-bind** (проще): DN строится по шаблону (`uid=%s,ou=people,dc=...`), bind сразу. Не требует service-account, но ломается на нетривиальных DN.
  3. После успешного user-bind — group-search (`group_filter`, например `(member=%s)`) → список групп.
  4. **Маппинг** групп на роли + `operators(aid)` (см. (e)). Выпуск внутреннего JWT.

  **Библиотека:** `github.com/go-ldap/ldap/v3` (MIT — совместимо с Apache-2.0).

- **(d) Маппинг внешней identity → `operators(aid)` + roles.**

  - **AID-derivation:** внешний идентификатор → AID по сконфигурированному источнику (`sub` / `email` / `uid` / `preferred_username`). AID-charset (ADR-014 amend: `^[a-z0-9][a-z0-9._@-]{1,127}$`) уже допускает `@`/`.` именно для email-подобных внешних имён. Нормализация (lowercase, опц. префикс) — конфигурируема.
  - **Role-mapping:** OIDC-`groups`/claims или LDAP-группы → RBAC-роли через конфиг-маппинг (`group_role_map: {ldap-group-or-oidc-group: [role,...]}`). Источник ролей — внешние группы (а не реестр), либо комбинация (см. развилку (g): «роли из IdP» vs «роли только из реестра, IdP даёт только identity»).
  - **Provisioning:** при первом federated-логине либо (i) **auto-provision** — вставка строки `operators` с `auth_method=ldap|oidc`, `created_by_aid` = служебный маркер federated-источника (аудит, ADR-014); либо (ii) **pre-register** — оператор должен быть заранее заведён (`POST /v1/operators`), federated-login лишь аутентифицирует существующего. Развилка (g).
  - **Revoked-проверка:** federated-login для оператора с `revoked_at != nil` отклоняется (нельзя обойти revocation внешним IdP).

- **(e) Config-схема (новые блоки `auth.ldap` / `auth.oidc`, секреты — Vault KV).** Симметрично существующим блокам ([keeper.go](../../shared/config/keeper.go): `KeeperRedis.password_ref`, `KeeperVault`, `KeeperAuthJWT.signing_key_ref`). Все секреты — через `*_ref` (`vault:<mount>/<path>[#field]`, резолв load-time как у `password_ref`/`dsn_ref`). TLS-required.

  ```yaml
  auth:
    jwt:                                  # уже есть (ADR-014) — issuer внутренних токенов
      signing_key_ref: vault:secret/keeper/jwt-signing-key
      issuer: keeper.example.com
      ttl_default: 8h

    ldap:                                 # НОВОЕ (опц. блок)
      url: ldaps://ldap.example.com:636   # ldaps:// или ldap:// (последний только со start_tls)
      start_tls: false                    # StartTLS поверх ldap:// (взаимоисключимо с ldaps://)
      tls:
        ca_ref: vault:secret/keeper/ldap-ca   # CA-bundle для LDAPS (опц., если не системный)
        insecure_skip_verify: false       # dev-only, semantic-WARN
      bind_mode: search                   # search | direct
      bind_dn: cn=svc-keeper,ou=svc,dc=example,dc=com   # service-account (search-режим)
      bind_password_ref: vault:secret/keeper/ldap-bind  # пароль service-account
      base_dn: dc=example,dc=com
      user_filter: (uid=%s)               # %s = введённый username
      user_dn_template: uid=%s,ou=people,dc=example,dc=com  # direct-режим
      group_filter: (member=%s)           # %s = найденный user-DN
      group_attr: cn                      # атрибут группы → имя для role-map
      aid_attr: uid                        # атрибут → AID (или email)
      timeout: 10s
      group_role_map:                      # внешняя группа → RBAC-роли
        keeper-admins: [cluster-admin]
        keeper-ops: [operator]

    oidc:                                 # НОВОЕ (опц. блок)
      issuer: https://keycloak.example.com/realms/soulstack   # discovery base
      client_id: soul-stack-keeper
      client_secret_ref: vault:secret/keeper/oidc-client-secret
      redirect_url: https://keeper.example.com/auth/oidc/callback
      scopes: [openid, email, profile, groups]
      tls:
        ca_ref: vault:secret/keeper/oidc-idp-ca   # опц. кастомный CA для IdP
      aid_claim: email                    # claim → AID (sub | email | preferred_username)
      groups_claim: groups                # claim → список групп для role-map
      use_pkce: true
      group_role_map:
        /keeper-admins: [cluster-admin]
        /keeper-ops: [operator]
  ```

  Оба блока **опциональны** ([ADR-053](0053-dependency-tiers.md) tier: OPTIONAL-with-degradation — не задан → способ логина просто недоступен, Keeper стартует). Валидация: `*_ref` через `checkVaultRef` (semantic-фаза), взаимоисключимость `ldaps://`+`start_tls`, `bind_mode=search` требует `bind_dn`+`bind_password_ref`, `insecure_skip_verify=true` → WARN. Hot-reload — через тот же `Store[T]` (ADR-021).

- **(f) API-поверхность (новые публичные `/auth/*` эндпоинты вне `/v1`).** Регистрируются на root-уровне chi-роутера (parity `/healthz`/`/docs`/`/ui` — БЕЗ `RequireJWT`/RBAC, см. [router.go](../../keeper/internal/api/router.go)):

  | метод + путь | auth | назначение |
  |---|---|---|
  | `GET /auth/methods` | публичный | backend-driven каталог доступных способов логина ([ADR-042](0042-backend-driven-ui.md)): какие из `password`/`ldap`/`oidc` сконфигурированы; для UI login-формы |
  | `GET /auth/oidc/login` | публичный | старт OIDC code-flow (redirect на IdP) |
  | `GET /auth/oidc/callback` | публичный | OIDC-callback, валидация id_token, выпуск внутреннего JWT |
  | `POST /auth/ldap/login` | публичный (HTTPS) | LDAP bind, выпуск внутреннего JWT |

  Защита публичных эндпоинтов — не JWT, а: `state`/`nonce`/PKCE (OIDC), TLS-required, rate-limit (Tempo-like / lockout по username — anti-bruteforce), таймауты, тело-лимит. Успешный federated-login пишет audit-событие (новый тип `operator.login`, источник `api`); неуспех login — опц. audit для security-мониторинга (но с anti-DoS-throttle). MCP-поверхность **не трогаем** — MCP принимает уже выпущенный внутренний JWT. OpenAPI — `/auth/*` регистрируются huma-операциями (новый файл `huma_auth.go`), попадают в `/openapi.yaml` автоматически (ADR-054).

- **(g) Модель безопасности (security-critical, «безопасность на первом месте»).**

  - **TLS везде:** LDAPS/StartTLS обязателен (plaintext-LDAP запрещён конфиг-валидацией); OIDC IdP — только HTTPS issuer; `/auth/ldap/login` — только поверх HTTPS-listener Keeper-а.
  - **OIDC id_token validation:** подпись через JWKS, pin `iss`, pin `aud`==client_id, `exp`/`iat`, `nonce`-match (anti-replay), `state`-match (anti-CSRF), PKCE (anti code-interception).
  - **Секреты не в логах:** `bind_password`/`client_secret`/введённые пароли/выпущенный JWT — масккаются (как `OperatorCreateReply.JWT`, ADR-014); resolve через Vault, plaintext в конфиге нет.
  - **Anti-bruteforce:** rate-limit + lockout по username на `/auth/ldap/login`; throttle на OIDC-callback.
  - **Таймауты:** на LDAP-connect/bind, OIDC token-exchange, JWKS-fetch — чтобы недоступность IdP не вешала Keeper.
  - **Revocation-инвариант:** revoked-оператор не логинится federated-путём (проверка `revoked_at` до выпуска JWT).
  - **Внутренний JWT неизменен:** federated-методы переиспользуют `jwt.Issuer` (ADR-014) — единый формат, единая revocation, короткий TTL. Внешний токен НЕ покидает момент login.

- **(h) Библиотеки и лицензии (проект Apache-2.0, [ADR-016](0016-parity-license.md)).**

  | библиотека | назначение | лицензия | совместимость с Apache-2.0 |
  |---|---|---|---|
  | `github.com/coreos/go-oidc/v3` | OIDC discovery / JWKS / id_token verify | Apache-2.0 | ✅ |
  | `golang.org/x/oauth2` | OAuth2 code-exchange | BSD-3-Clause | ✅ |
  | `github.com/go-ldap/ldap/v3` | LDAP bind / search | MIT | ✅ |
  | `github.com/golang-jwt/jwt/v5` | внутренний JWT (уже в проекте, ADR-014) | MIT | ✅ |

  Все совместимы с Apache-2.0; CLA пока не требуется (ADR-016). На момент написания черновика библиотеки `go-oidc`/`oauth2`/`go-ldap` **ещё не в `go.mod`** — добавляются на этапе имплементации (скелет компилируется без них).

- **Что НЕ входит (границы).** Не входит в этот ADR: SAML, WebAuthn/passkeys, mTLS-аутентификация оператора (ADR-014 уже резервирует `mtls`/`combined` отдельно), SCIM-провижининг/деактивация, refresh-token-ротация внешних токенов (внутренний JWT короткий — re-login), per-IdP multi-tenant (один LDAP + один OIDC в MVP-этого-ADR). Эти — отдельными ADR при реальном запросе.

- **Развилки (propose-and-wait, требуют решения пользователя ДО имплементации).**

  1. **Auto-provision vs pre-register** (LDAP и OIDC независимо): создавать `operators`-строку при первом federated-логине ИЛИ требовать заранее заведённого оператора?
  2. **Источник ролей:** роли из внешних групп/claims (`group_role_map`) ИЛИ только из реестра ролей Keeper (IdP даёт только identity, роли назначает админ через `/v1/roles`)?
  3. **Имя enum-значения:** `oidc` vs `oauth2`.
  4. **Форма доставки внутреннего JWT после login:** JSON-ответ (UI fetch + хранит сам) ИЛИ HttpOnly-cookie + redirect на `/ui`?
  5. **Нужен ли refresh:** короткий internal-JWT + re-login через IdP (проще, рекомендуется) ИЛИ refresh-механизм?
  6. **PKCE для OIDC:** обязателен (рекомендуется) или опц.?
  7. **LDAP bind-режим:** search-bind (service-account, гибче, рекомендуется) vs direct-bind (проще)?
  8. **Подтверждение библиотек:** `go-oidc/v3` + `x/oauth2` + `go-ldap/v3`.
  9. **Config-форма:** одобрить YAML-схему блоков `auth.ldap`/`auth.oidc` из (e).

---

> **Связь с другими ADR.** Amends (после одобрения): [ADR-014](0014-operator-identity.md) (расширение `auth_method` enum + provisioning-аудит). Переиспользует: [ADR-013](0013-bootstrap-archon.md) (bootstrap остаётся нижним слоем), [ADR-006](0006-cache-redis.md) (Redis для state/nonce store), [ADR-042](0042-backend-driven-ui.md) (`/auth/methods` каталог), [ADR-053](0053-dependency-tiers.md) (LDAP/OIDC — OPTIONAL tier), [ADR-054](0054-openapi-code-first.md) (huma-регистрация `/auth/*`). RBAC ([ADR-028](0028-rbac-storage.md)/[ADR-047](0047-purview.md)) — БЕЗ изменений.
