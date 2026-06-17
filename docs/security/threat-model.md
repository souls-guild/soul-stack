# Threat-model Soul Stack

Зафиксированная модель угроз кластера Keeper + флота Souls. Reference для оператора и команды разработки: что считается активом, какие актёры и поверхности рассматриваются, что закрыто механизмом и что остаётся остаточным риском. **Не маркетинг и не туториал** — нормативная фиксация security-границ на путь к бете.

Документ зафиксирован по итогам security-аудита 2026-06-12 (verdict PASS). Он не вводит новых решений — описывает уже-реализованные механизмы; каждый пункт отсылает к коду/ADR-источнику. Расхождение между этим документом и фактическим поведением кода — баг (заводить как security-issue, а не править доку под код).

См. также: [`../keeper/rbac.md`](../keeper/rbac.md) (RBAC/Purview/least-privilege), [`../operations/bootstrap-rbac.md`](../operations/bootstrap-rbac.md) (Bootstrap Архонта), [`../keeper/prod-setup.md`](../keeper/prod-setup.md) (прод-Vault: AppRole/auto-unseal/signing-key), [ADR-026 → Sigil](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), [ADR-047 → Purview](../adr/0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор).

## Активы

Что защищаем и почему компрометация критична.

| Актив | Где живёт | Почему критичен |
|---|---|---|
| **Vault-секреты** (Essence/пароли) | Vault KV, резолвятся в CEL-фазе рендера на Keeper | Параметры конфигов хостов, пароли БД/сервисов. Утечка = компрометация управляемых сервисов. Никогда не должны попадать в наблюдаемые каналы (см. инвариант ниже). |
| **mTLS-CA** (корень доверия SoulSeed) | Vault PKI root | Подписывает каждый SoulSeed. Компрометация CA = возможность выпустить валидный клиентский сертификат и выдать себя за любой Soul или влиться в EventStream. |
| **JWT signing-key** | Vault KV `secret/keeper/jwt-signing-key` ([ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) | Подписывает JWT всех Архонтов. Компрометация = выпуск токена с любыми ролями (полный обход RBAC). |
| **Sigil trust-anchor** (ed25519 plugin-signing) | приватник — Vault KV `secret/keeper/sigil-keys/<key_id>`; публичный набор едет Soul-у в `BootstrapReply` ([ADR-026(d)/(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)) | Подписывает допущенные digest-ы плагинов. Компрометация = возможность подписать произвольный бинарь плагина (RCE на флоте через подделанный `soul-mod-*`). |
| **Флот Souls** | `soul`-демоны на хостах | RCE-поверхность: модули исполняются на хосте. Компрометация Keeper-управления = исполнение произвольного кода на всём парке. |
| **Postgres** | реестры (`souls`/`operators`/`voyages`/…) + audit | Холодное состояние кластера и audit-журнал. Компрометация = подмена реестров, стирание следов. |
| **Redis** | presence/lease/leader | Горячий слой: presence Souls, SID-lease, leader-election Reaper/Conductor. Компрометация = ложная presence, угон lease, split-brain. |

## Актёры, поверхности и границы (что закрыто)

Для каждого актёра: точка входа (поверхность), механизм закрытия и **остаточный риск** (что осознанно не закрыто).

### Внешний оператор (semi-trusted)

- **Поверхность:** Operator API — OpenAPI (HTTP/JSON) / MCP / CLI как тонкая обёртка. Включая served-спеку (`GET /openapi.yaml` + `GET /openapi.json`) и визуальный вьювер `GET /docs` (RapiDoc) — см. ниже про их доступ.
- **Закрыто:**
  - JWT-аутентификация (claims `iss`/`sub`/`exp`/`roles`, подпись JWT signing-key из Vault, [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). Применяется к `/v1/*` **и** к served-спеке `/openapi.yaml`/`/openapi.json` (тот же `RequireJWT`, см. ниже).
  - RBAC **default-deny** — каждый эндпоинт требует явный permission ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), [`../keeper/rbac.md`](../keeper/rbac.md)).
  - **Purview-scope** — видимость узлов ограничена scope роли (coven/regex/soulprint/state-селектор); fail-closed (пустой Purview → пустой список, не весь флот) ([ADR-047](../adr/0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)).
  - **Least-privilege subset** — Архон не может выдать роль с правами шире собственных (`keeper/internal/rbac`, subset-инвариант учитывает роли через Synod, [ADR-049 §f](../adr/0049-synod.md#adr-049-synod--группа-архонов)).
  - Закрыты: **privilege-escalation** (через subset-проверку), **обход через MCP** (MCP-tools идут через тот же enforcer, что и REST), **self-lockout** (нельзя удалить последнего оператора с `*`-permission, [ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)).
- **Раскрытие API-поверхности (served-спека и вьювер, ADR-054 code-first):**
  - **Спека генерится из кода (huma-агрегатор), не из committed-рукописи.** `/openapi.yaml` и `/openapi.json` отдают runtime-дамп одного source-of-truth (huma-spec, OpenAPI 3.1) — «правда в коде». Committed `docs/keeper/openapi.yaml` — производный генерат для UI-vendor (`make gen-openapi`), он НЕ served. Drift код↔спека ловится тестом `TestFullSpec_CoversAllRoutes` (`keeper/internal/api/huma_full_spec_test.go`: множество роутов спеки == множество реальных chi-роутов).
  - **`/openapi.yaml` + `/openapi.json` — ЗА JWT.** Оба требуют Bearer (тот же `RequireJWT`, что и `/v1`), но смонтированы ВНЕ `/v1` и без `/v1`-обвязки (maxBody/metrics/audit/RBAC): спека статична. Раньше served-спека была публичной — раскрывала полную API-поверхность анонимно; теперь анонимный доступ к перечню эндпоинтов закрыт (`keeper/internal/api/router.go`).
  - **`/docs` (RapiDoc) — публичный shell без раскрытия API (механизм A, ADR-054 doc-viewer).** Сам `/docs` и статика `/docs/assets/*` публичны и не несут данных/описания API — это пустая HTML-страница + web-component RapiDoc. Чувствительное (полная спека) приходит только ПОСЛЕ того, как оператор ввёл JWT: страница фетчит `/openapi.json` с `Authorization: Bearer` и рендерит объект инлайн. Токен держится в `sessionStorage` вкладки (XSS-гигиена, не переживает закрытие вкладки). Ассеты RapiDoc вшиты через `go:embed` (`docsassets`) — без CDN, offline-render, нет загрузки стороннего JS.
- **Остаточный риск:** не выделен сверх покрытого выше; основной — инсайдер-оператор (см. ниже).

### Скомпрометированный Soul

- **Поверхность:** долгоживущий bidi `EventStream` поверх mTLS ([ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)).
- **Закрыто:**
  - Аутентификация **fingerprint → SID** по peer-сертификату: SID берётся из mTLS peer cert (`keeper/internal/grpc/peer.go`), **не из payload** — echo SID в сообщениях идёт только для логов, авторитет — сертификат.
  - **Seed-rotation только своего SID** — Soul не может ротировать чужой SoulSeed.
  - **Augur default-deny** — внешний доступ Soul-а (Vault/Prometheus/ELK) разрешён только явным grant-ом (`rites`, [ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)); по умолчанию ничего.
- **Остаточный риск (by-design):** модули исполняются на хосте с правами service-user. Это свойство pull-модели (Soul применяет Destiny локально) — Keeper не доверяет хосту больше, чем правам этого процесса. Blast radius компрометации одного Soul ограничен этим хостом; cross-host эскалации через EventStream нет (SID-auth + Augur default-deny).

### Сетевой атакующий (MITM / SSRF)

- **Поверхность:** транспорт Keeper↔Soul и **исходящие** соединения Keeper-а к недоверенным целям (webhook-доставка Herald, `core.url`/`core.http` через Augur-делегацию).
- **Закрыто:**
  - **TLS 1.3 + `RequireAndVerifyClientCert`** на EventStream (`shared/tlsx`, `keeper/internal/grpc/auth.go`) — нет downgrade и нет skip-verify в проде.
  - **SSRF-guard** на всех исходящих к недоверенным: `shared/netguard` — **resolve-then-dial, rebind-safe** (`ValidateEndpoint` → `GuardedDialContext` коннектит по уже-проверенному IP, между проверкой и dial нет второго резолва; `NewCheckRedirect` гейтит redirect-hop-ы). Закрывает прямой доступ к приватным IP и DNS-rebind.
- **Остаточный риск:** корректность зависит от правильно сконфигурированной Vault PKI role (`enforce_hostnames`/`allowed_domains`) — см. [требования к окружению](#требования-к-окружению-оператора).

### Инсайдер-оператор

- **Поверхность:** легитимный Архон с выданными правами.
- **Закрыто:**
  - Ограничен **RBAC-scope + least-privilege** (видит и трогает только узлы своего Purview, прав не больше выданных).
  - **Audit с masked-payload** — каждое write-действие пишется в `audit_log`; параметры маскируются на выходе (`shared/audit/mask.go`), секреты в журнал не попадают. Полнота audit-покрытия write-роутов держится агрегатным structural-guard-ом (см. инвариант ниже про анти-S6).
- **Остаточный риск (by-design):** `cluster-admin` (`*`-permission) имеет полный доступ — это необходимая роль (bootstrap, recovery). Минимизация риска: держать минимум `*`-операторов, self-lockout защищает от случайного удаления последнего admin-а, короткий JWT-TTL ограничивает окно компрометации украденного токена.

## Остаточные low-риски (backlog)

Осознанно не закрытые низкоприоритетные риски. Не блокеры беты; кандидаты на defense-in-depth.

- **CSR CN/SAN не валидируется до `SignCSR`.** Онбординг подписывает CSR без проверки CN/SAN на соответствие заявленному SID. Не критично: аутентификация якорится на **registry-fingerprint** (а не на CN сертификата), поэтому подделка CN не даёт привилегий — но валидация CN/SAN добавила бы defense-in-depth (раннее отсечение мусорных CSR).
- **Name-based secret-masking хрупок к новым код-путям.** Маскирование секретов в наблюдаемых каналах работает по именам полей (`shared/audit/mask.go`). Новый код-путь, логирующий `params` напрямую (минуя маскер), может протечь секрет. Защита — инвариант ниже + ревью на каждом новом логирующем пути; кандидат на устранение — taint-tracking секретов вместо name-based.

## Статус внешнего аудита / pentest

Внешний независимый pentest на момент закрытой малой беты **не проводился**. Решением от 2026-06-15 для беты признан достаточным **внутренний security-gate**, в который входит:

- **Deep ИБ-аудит 2026-06-12** — verdict PASS, **0 critical / 0 high** (этот документ — его фиксация).
- **Threat-model** — настоящий документ: активы, актёры, закрытые поверхности и остаточные риски.
- **`govulncheck` чист по всем модулям** — supply-chain-скан зашит в `make check` (см. [требования к окружению](#требования-к-окружению-оператора), таргет `make check-vuln` по всем go.work-модулям).
- **Security-ревалидация OpenAPI-пивота — PASS** — 0 блокеров; аудит-полнота write-роутов (анти-S6), RBAC default-deny + Purview-scope и JWT-enforcement доказаны (см. [Инсайдер-оператор](#инсайдер-оператор) и [требования к окружению](#требования-к-окружению-оператора)).

Внешний независимый pentest запланирован **пост-бета / перед GA** — закрытая малая бета (единицы операторов, флот до сотен хостов) даёт ограниченную поверхность, при которой внутренний gate соразмерен риску; на пути к GA с ростом аудитории внешняя проверка обязательна. Граница гарантий для бета-тестера зафиксирована в [`../known-limitations.md`](../known-limitations.md).

## Требования к окружению оператора

Инварианты, которые **не обеспечиваются кодом автоматически** и должны быть выдержаны при развёртывании. Нарушение любого из них ослабляет модель.

- **Vault PKI role с `enforce_hostnames` + `allowed_domains`.** SSRF-guard и mTLS-валидация полагаются на то, что PKI role не выпускает сертификаты на произвольные домены. Конфигурация role — на операторе ([`../keeper/prod-setup.md`](../keeper/prod-setup.md), [`../operations/infra.md`](../operations/infra.md)).
- **Инвариант: `RenderedTask.Params` никогда не попадает в наблюдаемые каналы.** Отрендеренные параметры задачи (потенциально содержат секреты после CEL-резолва Vault) не должны утекать в audit / OTel / SSE / логи в plaintext. Маскирование — на выходе (`shared/audit/mask.go`); каждый новый код-путь, касающийся `Params`, обязан проходить маскер. Это нормативное требование к коду (ловится ревью), не опция конфига.
- **Инвариант: каждый мутирующий `/v1`-роут пишет audit-event (анти-S6).** Множество write-роутов ⊆ множество audit-покрытых роутов. Держится двухуровневым гейтом в коде, не ревью: агрегатный structural-guard (`keeper/internal/api/audit_completeness_guard_test.go`) — декларативный реестр write-роутов из топологии `buildRouter`, новый write-роут без записи в реестр краснит тест; per-domain `*_RecordsOnSuccess`-тесты — доказывают, что event реально пишется на 2xx (урок S6: «middleware навешан» ≠ «audit пишет» — bridge перехватывал ResponseWriter ДО рекордера, запись молча терялась). Это нормативное требование к коду.
- **Регулярный `govulncheck`.** Supply-chain-скан зависимостей зашит в `make check` через таргет `make check-vuln` (прогон `govulncheck ./...` по всем 5 go.work-модулям; offline-graceful — `SKIP_VULNCHECK=1` пропускает без сети). На момент фиксации govulncheck чист по всем модулям. Запуск перед релизом — часть `make check`; периодический повтор для свежей vuln-DB — операционная гигиена.
