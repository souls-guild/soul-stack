# ADR-058. Федеративная аутентификация операторов (Archon) — LDAP + OAuth2/OIDC

> **Статус: accepted + end-to-end (LDAP, стадия 1 / OIDC, стадия 2).** LDAP- и OIDC-аутентификация **реализованы и доведены end-to-end**. LDAP: search-bind, auto-provision по группам **с посевом строки `operators`**, cookie-доставка JWT — блокер посева системного оператора `archon-system` **закрыт** (`created_via`-релакс bootstrap-индекса, миграция 086; см. ниже в (d)). OIDC (стадия 2): authorization-code flow с **обязательным PKCE (S256)**, discovery (`go-oidc/v3`) + JWKS-валидация `id_token` (подпись/iss/aud/exp/nonce), cluster-shared server-side flow-state store на Redis (state→nonce/PKCE-verifier, single-use GETDEL, TTL 5m), эндпоинты `GET /auth/oidc/{login,callback}`, cookie-доставка JWT (`SameSite=Lax` — переживает cross-site redirect от IdP). Mapper обобщён под оба метода (`auth_method`/`created_via` = `ldap`|`oidc` из `cfg.Method`). Все развилки разрешены: enum-имя — `oidc` (закреплено), PKCE — обязателен (config-флага нет, оператору не оставлен).

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

  Это additive-расширение в духе ADR-014 (mTLS/combined уже заявлены post-MVP через тот же enum). Реализовано: Go-const `operator.AuthMethod` (`AuthMethodLDAP`/`AuthMethodOIDC`), `operator.Insert` принимает оба, huma-enum `OperatorAuthMethod` + query-фильтр list `enum:"jwt,mtls,combined,ldap,oidc"` (`huma_enums.go`/`huma_operator_op.go`), SQL `CHECK auth_method_valid` расширен миграцией **083** (only-add, forward-only). `oidc` заведён сразу (стадия 2), чтобы имплементация OIDC не трогала enum/CHECK повторно. `auth_method` в строке `operators` фиксирует, каким способом оператор пришёл (для аудита и UI); сам внутренний JWT после выпуска одинаков для всех методов.

  > **Имя enum:** `oidc` (закреплено) против `oauth2`. Выбрано **`oidc`** — Keeper полагается на OIDC id_token (identity layer над OAuth2), а не на чистый OAuth2 access-token. «oauth2» точнее для авторизации-без-identity, что нам не подходит.

- **(b) OAuth2/OIDC — authorization-code flow с PKCE (логин человека-Архонта). РЕАЛИЗОВАНО (стадия 2).**

  1. `GET /auth/oidc/login` (публичный) → Keeper генерирует `state` + `nonce` + PKCE `code_verifier` (256-битные crypto/rand), кладёт `{state→(nonce,verifier)}` в server-side flow-state store (Redis `OIDCFlowStore`, TTL 5 мин, ADR-006), редиректит на `authorization_endpoint` IdP с S256 `code_challenge`. `code_verifier`/`nonce` НЕ покидают сервер (в URL только `state` + `code_challenge`).
  2. Архонт аутентифицируется у IdP.
  3. `GET /auth/oidc/callback?code=...&state=...` (публичный) → Keeper: **Consume `state`** из store (`GETDEL` — атомарно читает И удаляет, single-use: anti-CSRF + anti-replay; неизвестный/потреблённый/истёкший → отказ), обменивает `code` на токены с `code_verifier` (**PKCE-enforced**: без verifier IdP отвергает обмен), **валидирует `id_token`** через `go-oidc` verifier: подпись через JWKS (`jwks_uri` из discovery), `iss` == issuer, `aud` == client_id, `exp`/`iat`; затем **сверяет `nonce`** (anti-replay id_token). Извлекает `sub` + сконфигурированные claim-поля (`aid_claim` дефолт `sub` → AID, email/preferred_username/`groups_claim`).
  4. **Маппинг** на `operators(aid)` + роли (общий `auth.DBMapper`, `Method=oidc`; auto-provision по группам, см. (e)/(i)). Выпускает **внутренний JWT** через `jwt.Issuer`, кладёт в HttpOnly+Secure cookie `soul_session` (`SameSite=Lax`, чтобы cookie доехала на cross-site redirect от IdP; `Strict` срезал бы её) и редиректит на `/ui` (302). JSON-токена в теле нет (cookie-only, parity LDAP). PKCE **обязателен** — config-флага нет (развилка №6 разрешена в пользу «обязателен»). OIDC требует живого Redis (flow-store cluster-shared, ADR-053 OPTIONAL-tier): без Redis эндпоинты не монтируются.

  **Библиотеки (добавлены стадией 2):** `github.com/coreos/go-oidc/v3` (discovery + JWKS + id_token verify, Apache-2.0) + `golang.org/x/oauth2` (code-exchange + PKCE-хелперы `GenerateVerifier`/`S256ChallengeOption`, BSD-3 — совместимо с Apache-2.0). Discovery (`/.well-known/openid-configuration`) — сетевой вызов на load-time (`setupOIDCAuth`); JWKS — с TTL + key-rotation refetch (даёт go-oidc). Кастомный CA IdP (`tls.ca_ref`) прокидывается в discovery/JWKS/token-exchange через `oidc.ClientContext`. Раскладка: `keeper/internal/auth/oidc` (authenticator) + `keeper/internal/redis/oidcflow.go` (store) + `keeper/internal/api/huma_oidc.go` (эндпоинты).

- **(c) LDAP (bind + group→role). РЕАЛИЗОВАНО (стадия 1).**

  1. `POST /auth/ldap/login` (публичный) с body `{username, password}` поверх **HTTPS** (Keeper-listener TLS — обязателен).
  2. Keeper подключается к LDAP по **LDAPS** или **StartTLS** (plaintext-LDAP запрещён конфиг-валидацией И конструктором `ldap.New`). Режим bind — **search-bind** (единственный в стадии 1, развилка №7 разрешена): service-account (`bind_dn` + `bind_password_ref` из Vault) делает search по `user_filter` (`(uid=%s)` / `(sAMAccountName=%s)`); username экранируется `ldap.EscapeFilter` (anti-injection); ровно одна запись, иначе отказ; затем re-bind найденным user-DN + введённым паролем (проверка пароля). direct-bind отложен (расширение без breaking change).
  3. После успешного user-bind — group-search (`group_filter`, например `(member=%s)`, user-DN экранируется) → список групп по `group_attr` (дефолт `cn`).
  4. **Маппинг** групп на роли + `operators(aid)` (см. (e)). Выпуск внутреннего JWT в cookie.

  Все причины отказа санитизируются в один `auth.ErrAuthFailed` (anti-oracle: наружу 401 без причины); пароль/bind-creds никогда не попадают в ошибки или логи (только debug-этап без секрета). Реализация — `keeper/internal/auth/ldap/ldap.go` (conn-интерфейс над `*ldap.Conn` для unit-тестов без реального LDAP).

  **Библиотека:** `github.com/go-ldap/ldap/v3` (MIT — совместимо с Apache-2.0), добавлена в `keeper/go.mod`.

- **(d) Маппинг внешней identity → `operators(aid)` + roles. РЕАЛИЗОВАНО (LDAP, `keeper/internal/auth/mapper.go`).**

  - **AID-derivation (РЕШЕНО):** AID выводится из LDAP-атрибута, заданного config `aid_attr` (`uid` | `mail`). **Дефолт — `uid`** (если `aid_attr` не задан): короче и стабильнее `mail` (почтовый адрес может переназначаться), почти всегда присутствует в схеме `person`/`inetOrgPerson`. AID lowercase-нормализуется. AID-charset (ADR-014 amend: `^[a-z0-9][a-z0-9._@-]{1,127}$`) допускает `@`/`.` для email-подобных имён; невалидный derived-AID → `ErrAuthFailed` (без утечки). Authenticator кладёт derived-AID в новое поле `ExternalIdentity.AID` (отделено от `Subject`=user-DN, additive-расширение контракта `auth.go`).
  - **Role-mapping (РЕШЕНО — роли из групп, развилка №2):** LDAP-группы → RBAC-роли через `group_role_map: {ldap-group: [role,...]}`. Источник ролей — внешние группы (а не реестр), и для нового, и для существующего оператора. Membership синхронизируется в `rbac_role_operators` (идемпотентный `rbac.GrantOperator`, `granted_by_aid=NULL`), т.к. авторитет RBAC — таблица membership, а не JWT-claim `roles` (ADR-028(c)). Роли дедуплицируются и стабильно сортируются.
  - **Provisioning (РЕШЕНО — auto-provision по группам, развилка №1):** первый federated-логин **создаёт** строку `operators` (`auth_method=ldap`), **если** группы пользователя пересекают `group_role_map`; вне групп — отказ `ErrNoRoleMapping` (403, оператор НЕ создаётся). Роли берутся из групп. Пишется audit `operator.provisioned`.
  - **`created_by_aid` для federated (bootstrap-инвариант ADR-013 — РЕШЕНО через `created_via`).** Прежняя проблема: partial unique index `operators_first_archon_idx` (миграция 003) держал «ровно одна строка с `created_by_aid IS NULL`» (первый bootstrap-Архонт), и federated-provision **не мог** ставить `NULL` (сломал бы инвариант на втором federated-юзере) и не мог ставить self (CHECK `self_reference_ok`). Прежний костыль — `created_by_aid = 'archon-system'` (`auth.FederatedSourceAID`) — **удалён**. Решение: реестр получил поле **`created_via`** ([ADR-014 amendment 2026-06-23](0014-operator-identity.md#adr-014-identity-модель-оператора-archon), домен `{bootstrap,user,ldap,oidc,system}`, миграция 084), bootstrap-инвариант перенесён на partial-index `WHERE created_via='bootstrap'` (миграция 085). После релакса **`created_by_aid=NULL` легален для не-bootstrap-строк**, и federated-provision пишет `created_by_aid=NULL` + `created_via='ldap'` (источник атрибутируется самим `created_via`, не суррогатным FK на `archon-system`). CHECK `self_reference_ok` остаётся. `archon-system` посеян легально миграцией **086** (`created_via='system'`, `created_by_aid=NULL`) и продолжает быть FK-якорем для push auto-import (`push.AutoImportSystemAID` пишет `push_providers.created_by_aid='archon-system'`) — но federated-mapper его больше НЕ использует.
  - **Revoked-проверка (РЕАЛИЗОВАНО):** federated-login для оператора с `revoked_at != nil` отклоняется (`ErrOperatorRevoked` → 403) ДО выпуска JWT — нельзя обойти revocation внешним IdP.
  - **Audit-разделение:** `operator.provisioned` пишет Mapper (факт создания строки); `operator.login` пишет endpoint после выпуска JWT (одно login-событие на логин, без задвоения). Оба — `source: api`, payload без пароля/bind-creds.

  > **Посев `archon-system` — ЗАКРЫТО (2026-06-23, прежде needs_architect).** Прежний блокер: системный оператор `archon-system` должен иметь `created_by_aid`, а оно не может быть `NULL` (конфликт с прежним bootstrap-индексом `WHERE created_by_aid IS NULL`) и не может быть self (CHECK). Решение архитектора одобрено и реализовано — **релакс bootstrap-индекса через `created_via`**: индекс перенесён на `WHERE created_via='bootstrap'` (миграция 085), после чего `archon-system` посеян миграцией **086** (`created_via='system'`, `created_by_aid=NULL`, `ON CONFLICT DO NOTHING`) **легально**, не нарушая единственности первого Архонта. **End-to-end auto-provision РАБОТАЕТ:** первый federated-логин нового оператора создаёт строку `operators` (`created_by_aid=NULL`, `created_via='ldap'`) — маппинг ролей, cookie, revoked-инвариант доведены и протестированы. Прежний костыль `auth.FederatedSourceAID` (federated писал `created_by_aid='archon-system'`) удалён.

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

    oidc:                                 # НОВОЕ (опц. блок, требует Redis)
      issuer: https://keycloak.example.com/realms/soulstack   # discovery base (HTTPS-only)
      client_id: soul-stack-keeper
      client_secret_ref: vault:secret/keeper/oidc-client-secret  # опц. (public-client может без него)
      redirect_url: https://keeper.example.com/auth/oidc/callback
      scopes: [openid, email, profile, groups]
      tls:
        ca_ref: vault:secret/keeper/oidc-idp-ca   # опц. кастомный CA для IdP
      aid_claim: preferred_username       # claim → AID (sub | email | preferred_username; дефолт sub)
      groups_claim: groups                # claim → список групп для role-map (дефолт groups)
      group_role_map:
        /keeper-admins: [cluster-admin]
        /keeper-ops: [operator]
  ```

  > PKCE (S256) включён реализацией всегда — config-флага `use_pkce` нет (оператору не оставлен на выбор, ADR-058 развилка №6 разрешена в пользу «обязателен»).

  Оба блока **опциональны** ([ADR-053](0053-dependency-tiers.md) tier: OPTIONAL-with-degradation — не задан → способ логина просто недоступен, Keeper стартует; OIDC дополнительно требует Redis для flow-store). Semantic-валидация `auth.ldap` **реализована** (`shared/config/semantic.go::checkAuthLDAP`): TLS-required (`ldaps://` ЛИБО `ldap://`+`start_tls`, иначе ERROR `ldap_plaintext_forbidden`), взаимоисключимость `ldaps://`+`start_tls` (ERROR `ldap_tls_conflict`), `bind_mode=search` (или пусто=дефолт) требует `bind_dn`+`bind_password_ref`, `*_ref` через `checkVaultRef`, `timeout` — duration-формат, `insecure_skip_verify=true` → WARN. Semantic-валидация `auth.oidc` **реализована** (`checkAuthOIDC`): `issuer` HTTPS-only (ERROR `oidc_issuer_not_https`), обязательность `client_id`/`redirect_url` (ERROR `oidc_client_id_required`/`oidc_redirect_url_required`), `client_secret_ref`/`tls.ca_ref` через `checkVaultRef` (secret опционален для public-client). Резолв `*_ref` из Vault + discovery → `auth/{ldap,oidc}.Config` — на load-time в daemon (`setupLDAPAuth`/`setupOIDCAuth`, зеркало `bootstrap.LoadSigningKey`). Hot-reload — через тот же `Store[T]` (ADR-021).

- **(f) API-поверхность (новые публичные `/auth/*` эндпоинты вне `/v1`).** Регистрируются на root-уровне chi-роутера (parity `/healthz`/`/docs`/`/ui` — БЕЗ `RequireJWT`/RBAC, см. [router.go](../../keeper/internal/api/router.go)):

  | метод + путь | auth | назначение |
  |---|---|---|
  | `GET /auth/methods` | публичный | backend-driven каталог доступных способов логина ([ADR-042](0042-backend-driven-ui.md)): какие из `password`/`ldap`/`oidc` сконфигурированы; для UI login-формы — **follow-up** |
  | `GET /auth/oidc/login` | публичный | **РЕАЛИЗОВАНО:** старт OIDC code-flow с PKCE (302 redirect на IdP) |
  | `GET /auth/oidc/callback` | публичный | **РЕАЛИЗОВАНО:** OIDC-callback, валидация id_token, выпуск внутреннего JWT в cookie (302 на /ui) |
  | `POST /auth/ldap/login` | публичный (HTTPS) | **РЕАЛИЗОВАНО:** LDAP search-bind, выпуск внутреннего JWT в cookie |

  `POST /auth/ldap/login` (`huma_auth.go`) и `GET /auth/oidc/{login,callback}` (`huma_oidc.go`) реализованы: ROOT-mount вне `/v1` (parity `/healthz`, без `RequireJWT`/RBAC/metrics; anti-DoS body-limit стоит), монтируются только при non-nil `LDAPAuth`/`OIDCAuth` (opt-in, как `pushH`/`errandH`; OIDC дополнительно требует Redis). FULL-TYPED huma-операции; в committed `docs/keeper/openapi.yaml` (группы `prefix /auth` в `fullSpecGroups`). Защита: TLS-required, body-лимит, OIDC — `state`(single-use)/`nonce`/PKCE. Успешный login пишет audit `operator.login` (`source: api`, payload `{method, aid, provisioned}` — без секретов). `GET /auth/methods` (backend-driven каталог) + rate-limit/lockout по username (anti-bruteforce) — follow-up. MCP-поверхность **не трогаем** — MCP принимает уже выпущенный внутренний JWT. OpenAPI — `/auth/*` регистрируются huma-операциями, попадают в `/openapi.yaml` автоматически (ADR-054).

- **(g) Модель безопасности (security-critical, «безопасность на первом месте»).**

  - **TLS везде:** LDAPS/StartTLS обязателен (plaintext-LDAP запрещён конфиг-валидацией); OIDC IdP — только HTTPS issuer; `/auth/ldap/login` — только поверх HTTPS-listener Keeper-а.
  - **OIDC id_token validation (РЕАЛИЗОВАНО):** подпись через JWKS, pin `iss`, pin `aud`==client_id, `exp`/`iat` (go-oidc verifier), `nonce`-match (anti-replay), `state`-match single-use через GETDEL (anti-CSRF + anti-replay code), PKCE S256 обязателен (anti code-interception). Любая причина отказа санитизируется в `auth.ErrAuthFailed` наружу (anti-oracle); детали — debug-лог без токенов. Guard-тесты: `keeper/internal/auth/oidc/oidc_test.go` (mock-IdP на TLS-httptest + JWKS) покрывают bad-signature/expired/wrong-aud/nonce-mismatch/state-mismatch/single-use/PKCE-enforced.
  - **Секреты не в логах:** `bind_password`/`client_secret`/введённые пароли/выпущенный JWT — масккаются (как `OperatorCreateReply.JWT`, ADR-014); resolve через Vault, plaintext в конфиге нет.
  - **Anti-bruteforce:** rate-limit + lockout по username на `/auth/ldap/login`; throttle на OIDC-callback.
  - **Таймауты:** на LDAP-connect/bind, OIDC token-exchange, JWKS-fetch — чтобы недоступность IdP не вешала Keeper.
  - **Revocation-инвариант:** revoked-оператор не логинится federated-путём (проверка `revoked_at` до выпуска JWT).
  - **Внутренний JWT неизменен:** federated-методы переиспользуют `jwt.Issuer` (ADR-014) — единый формат, единая revocation, короткий TTL. Внешний токен НЕ покидает момент login.
  - **Доставка JWT (РЕШЕНО — cookie-only, развилка №4):** внутренний JWT отдаётся **только** в `Set-Cookie` `soul_session` с `HttpOnly` + `Secure` + `SameSite=Strict` + `Path=/`. JSON-токена в теле ответа НЕТ (минимизация XSS-эксфильтрации). TTL cookie = `auth.jwt.ttl_default`. Программная выписка токена (CI/скрипты) остаётся через существующий `POST /v1/operators/{aid}/issue-token` (ADR-014).
  - **Refresh (РЕШЕНО — нет, развилка №5):** refresh-механизма нет; по истечении короткого internal-JWT — повторный federated-login.

- **(h) Библиотеки и лицензии (проект Apache-2.0, [ADR-016](0016-parity-license.md)).**

  | библиотека | назначение | лицензия | совместимость с Apache-2.0 |
  |---|---|---|---|
  | `github.com/coreos/go-oidc/v3` | OIDC discovery / JWKS / id_token verify | Apache-2.0 | ✅ |
  | `golang.org/x/oauth2` | OAuth2 code-exchange | BSD-3-Clause | ✅ |
  | `github.com/go-ldap/ldap/v3` | LDAP bind / search | MIT | ✅ |
  | `github.com/golang-jwt/jwt/v5` | внутренний JWT (уже в проекте, ADR-014) | MIT | ✅ |

  Все совместимы с Apache-2.0; CLA пока не требуется (ADR-016). `go-ldap/v3` **добавлен** в `keeper/go.mod` (стадия 1). `go-oidc/v3` + `x/oauth2` **добавлены** в `keeper/go.mod` (стадия 2). Тесты используют `github.com/go-jose/go-jose/v4` (транзитивная зависимость go-oidc, Apache-2.0) для подписи id_token mock-IdP.

- **(i) Политика провижининга операторов (РЕАЛИЗОВАНО, Часть B).** Кластер-уровневый гейт «какими методами вообще можно СОЗДАВАТЬ оператора» — отдельно от того, какие способы логина сконфигурированы.

  - **Ключ `provisioning_allowed_methods`** — well-known скаляр `keeper_settings` ([ADR-029](0029-service-registry.md#adr-029-реестр-service-ов--postgres), key-value cluster-settings; парность `default_destiny_source`). Значение — **CSV из домена `{user, ldap, oidc}`** (методы СОЗДАНИЯ оператора). **`bootstrap` и `system` НЕ гейтятся НИКОГДА** (в домен ключа не входят) — `keeper init` и системный посев `archon-system` обязаны работать всегда (anti-lockout первого уровня). Резолв — синхронный снимок `serviceregistry.Holder` (паттерн ADR-029(b), геттер `ProvisioningMethodAllowed(method)` / `ProvisioningPolicy()`), TTL-poll + `service:invalidate`.
    - **ОТСУТСТВИЕ ключа** = все способы разрешены (back-compat — поведение до Части B сохраняется).
    - **Заданный-но-ПУСТОЙ** (csv `""` / только запятые) = **config-error** на load снимка (`ErrEmptyProvisioningMethods`) — anti-lockout второго уровня: нельзя запретить ВСЕ методы создания и тем самым залочить заведение операторов. В runtime недостижимо (PUT валидирует ДО записи).
    - Метод вне `{user,ldap,oidc}` → `ErrInvalidProvisioningMethod`.
  - **Гейтинг ТОЛЬКО на ветке СОЗДАНИЯ оператора:** `POST /v1/operators` (метод `user`) и federated auto-provision (метод `ldap`/`oidc`, mapper). **Existing-оператор логинится даже если его метод впоследствии запрещён** — политика не отзывает доступ, только запрещает заводить новых. Federated-login существующей строки `operators` метод-гейт не проходит.
  - **Runtime-API** — `GET`/`PUT` `/v1/provisioning-policy` (RBAC permission `provisioning.read` / `provisioning.update`; селектор NoSelector — кластер-уровневая, как `operator.*`/`role.*`). `GET` — read без audit (`policy_set=false` → ключ не задан, дефолт «всё разрешено»). `PUT` — replace-семантика, пишет audit `provisioning.policy_changed`; пустой список → 422, метод вне домена → 422 (problem-type `provisioning-method-disabled` для отказа на создании; `validation-failed`/422 для PUT-ошибок). Монтируется только при non-nil reader/gate (opt-in, parity push/errand-роутов).
  - **Отказ на создании** возвращает `application/problem+json` с problem-type **`provisioning-method-disabled`** (`TypeProvisioningMethodDisabled`).

- **Что НЕ входит (границы).** Не входит в этот ADR: SAML, WebAuthn/passkeys, mTLS-аутентификация оператора (ADR-014 уже резервирует `mtls`/`combined` отдельно), SCIM-провижининг/деактивация, refresh-token-ротация внешних токенов (внутренний JWT короткий — re-login), per-IdP multi-tenant (один LDAP + один OIDC в MVP-этого-ADR). Эти — отдельными ADR при реальном запросе.

- **Развилки.**

  Разрешены (стадия 1, LDAP):

  1. **Auto-provision vs pre-register → РЕШЕНО: auto-provision по группам.** Первый логин создаёт оператора, если есть пересечение групп с `group_role_map`; вне групп — отказ (не создаётся).
  2. **Источник ролей → РЕШЕНО: внешние группы.** Роли из `group_role_map`, синхронизируются в `rbac_role_operators`.
  4. **Форма доставки JWT → РЕШЕНО: HttpOnly+Secure+SameSite=Strict cookie `soul_session`.** JSON-токена в теле нет. Программная выписка — через существующий `operator issue-token`.
  5. **Refresh → РЕШЕНО: нет.** Короткий internal-JWT + re-login.
  7. **LDAP bind-режим → РЕШЕНО: search-bind** (единственный в стадии 1; direct-bind отложен без breaking change).
  8. **Библиотека LDAP → РЕШЕНО: `github.com/go-ldap/ldap/v3`.**
  9. **Config-форма `auth.ldap` → РЕШЕНО** (одобрена YAML-схема из (e)).

  Разрешено (стадия 2, OIDC):

  3. **Имя enum-значения → РЕШЕНО: `oidc`** (закреплено; Keeper полагается на OIDC id_token, не на чистый OAuth2 access-token).
  6. **PKCE для OIDC → РЕШЕНО: обязателен** (S256 always-on, config-флага нет — оператору не оставлен на выбор; защита от code-interception на первом месте).
  - **Библиотеки OIDC → РЕШЕНО: `go-oidc/v3` + `x/oauth2`** (добавлены в `keeper/go.mod`).

  Разрешено (стадия 1, посев + провижининг):

  10. **Посев `archon-system` без нарушения bootstrap-индекса → РЕШЕНО** (прежде needs_architect): релакс bootstrap partial-unique-индекса на `WHERE created_via='bootstrap'` (миграции 084/085) легализует `created_by_aid=NULL` у не-bootstrap-строк; `archon-system` посеян миграцией 086. End-to-end auto-provision доведён. См. (d).
  11. **Политика провижининга → РЕШЕНО** (см. (i)): well-known `provisioning_allowed_methods` (CSV `{user,ldap,oidc}`, absent=все, пустой=config-error), runtime-API `/v1/provisioning-policy`, permission `provisioning.read`/`provisioning.update`, audit `provisioning.policy_changed`, гейтинг только на ветке создания оператора.

---

> **Связь с другими ADR.** Amends: [ADR-014](0014-operator-identity.md) (расширение `auth_method` enum + поле `operators.created_via` + provisioning-аудит — amendment 2026-06-23), [ADR-013](0013-bootstrap-archon.md) (bootstrap-инвариант через `created_via='bootstrap'` — amendment 2026-06-23), [ADR-029](0029-service-registry.md) (well-known ключ `keeper_settings.provisioning_allowed_methods`). Переиспользует: [ADR-013](0013-bootstrap-archon.md) (bootstrap остаётся нижним слоем), [ADR-006](0006-cache-redis.md) (Redis для state/nonce store), [ADR-042](0042-backend-driven-ui.md) (`/auth/methods` каталог), [ADR-053](0053-dependency-tiers.md) (LDAP/OIDC — OPTIONAL tier), [ADR-054](0054-openapi-code-first.md) (huma-регистрация `/auth/*`). RBAC ([ADR-028](0028-rbac-storage.md)/[ADR-047](0047-purview.md)) — БЕЗ изменений (новые permission `provisioning.*` регистрируются в каталоге, enforcer-механика не меняется).
