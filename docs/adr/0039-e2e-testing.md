# ADR-039. E2E-тестирование — три уровня без новой сущности словаря

**Контекст.** Существующие уровни тестов покрывают L0 (unit), L1 (testcontainers per-package integration, `-tags=integration`), L2 ([Trial](0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage) — hermetic render+migration через `soul-trial` на fixtures). Дыра — L3: воспроизводимый прогон полного feature-flow на живом Keeper-инстансе с реальным bidi-stream к Soul-агентам. Без L3 каждая фича оттестирована по частям, но не как единое целое; регрессии на стыке слоёв (apply_runs lifecycle ↔ RBAC ↔ audit ↔ scenario-runner) пропускаются.

**Решение.**

1. **L3 не вводит новой сущности словаря.** Harness живёт как обычные Go-tests с build-tag-ами:
   - `tests/e2e/` + `go test -tags=e2e` — **L3a fast-loop:** soul-stub helper-пакет, testcontainers (PG+Redis+Vault), реальный Keeper-процесс. Покрывает контракт-тесты (apply_runs lifecycle, RBAC, audit, MCP). Прогон — каждый PR.
   - `tests/e2e-live/` + `go test -tags=e2e_live` — **L3b smoke-loop:** real `soul`-бинарь в Linux-контейнере (testcontainers), Keeper отдельным контейнером, реальный mTLS-handshake, реальный apply мутирует filesystem контейнера. Покрывает flagship-сценарии (install-package, multi-host). Прогон — nightly / on-demand.
   - `tests/e2e-k8s/` + `go test -tags=e2e_k8s` — **L3c k8s-loop:** kind-cluster ([sigs.k8s.io/kind](https://sigs.k8s.io/kind)), real K8s-deployment Keeper + Soul + Redis-Cluster + PG. Покрывает HA-кейсы (Watchman, Toll, leader-failover). Прогон — weekly / pre-release.

2. **Soul-stub — helper-пакет** `tests/e2e/internal/soulstub/`, не отдельный бинарь ([ADR-004](0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper): бинари = операторские инструменты с lifecycle; у тестового stub-а его нет). Минимальный fake-Soul: открывает gRPC-стрим, отвечает на `ApplyRequest` предзаписанным `RunResult` из fixture-файлов.

3. **Логика теста — Go, fixtures + expectations — YAML.** Гранулярность: Go-test определяет flow (setup → run → assert), YAML — параметры setup (souls/soulprint/incarnation/vault-secrets) и форму expectations (apply_runs row counts, incarnation.state shape, audit-events presence, soulprint.facts). **НЕ полный DSL** — никаких `steps[]` / `inject_fault` / cross-step `$state.X`. Логика, если нужна — обычный Go-код.

4. **Source-of-truth для assertion-значений — code enums.** YAML expectations ссылаются на existing values из [`applyrun.Status`](../../keeper/internal/applyrun/applyrun.go) / `audit.EventType` / `incarnation.Status` etc. Harness валидирует expectations против Go-типов на старте теста (fail-early, не fail-late на assert).

5. **Git для service-loader** — bare repo per-example, генерируется harness-ом в `TestMain` setup: `git init --bare $TMP/<name>.git` + push снимка `examples/<name>/`. Keeper-конфиг подхватывает `file://$TMP/<name>.git`. Никакого git-daemon-контейнера в дефолте.

6. **Изоляция от dev-стенда.** Каждый E2E-тест строит свой ephemeral stack через testcontainers (своя PG, своя Redis, свой Vault). НЕ использует общий `dev/docker-compose.yml`.

7. **CI/GitHub Actions — отдельной задачей.** Сначала локальный `make e2e` (Makefile-таргет, спавнит `go test -tags=e2e ./tests/e2e/...`). Workflow yaml — после стабилизации pilot.

**Инварианты.**

- Никаких новых бинарей ([ADR-004](0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)).
- Никаких новых сущностей словаря (без propose-and-wait по `Pilgrimage`/`Crucible`/`soul-stub` — это test-only-вспомогательное, не часть архитектуры).
- DSL-значения в YAML expectations = code enums (no string drift).
- L3a Go-test тестирует контракт keeper↔soul, **не** реальное apply (для этого L3b).
- `examples/` остаются доменно-чистыми; тестовая обвязка живёт в `tests/e2e/<name>/`, а не в `examples/<name>/`.

**Отвергнутые альтернативы.**

- (а) **Новый бинарь `cmd/soul-stub/`** — избыточно vs helper-пакет ([ADR-004](0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) трактует бинарь как операторский инструмент, не test-fixture).
- (б) **Новое имя L3-сущности (`Pilgrimage`/`Crucible`)** — propose-and-wait для одной фичи теста = bloat словаря; build-tag не требует имени.
- (в) **Расширение Trial до L3** — Trial = hermetic by definition ([ADR-023](0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)), L3 живой Keeper-процесс → размывает определение Trial.
- (г) **Полный YAML-DSL со `steps[]`/`inject_fault`/cross-step refs** — путь к самописному языку, у нас уже есть Go-тесты для логики.
- (д) **gitea / git-daemon testcontainer как default** — heavy, bare repo в `$TMP` проще и достаточен для file://-загрузчика Keeper-а.
- (е) **GitHub Actions yaml внутри этого ADR** — противоречит инварианту «CI локально в Makefile» (репо пока локальный/приватный), workflow добавляется отдельной задачей.

**Что отложено.**

- **L3b real-soul-in-container** — отдельный slice после стабилизации L3a pilot.
- **L3c kind-cluster** — отдельный slice; kind-config пока не пишется.
- **Chaos-engineering (litmus / chaosmesh)** — отдельная подсистема.
- **Manual-soak (L4)** — пост первой проды.
- **Cross-keeper fault-injection** — после Toll-impl.

**Amendment 2026-05-26 (L3a-impl particulars).** При переходе pilot → L3a-implementation slice зафиксированы конкретные решения, не противоречащие исходному ADR, но необходимые для воспроизводимости harness-а:

1. **Spawn инфры — testcontainers-go (PG/Redis/Vault),** Keeper — sub-process реального бинаря `keeper` (не in-process импорт). harness НЕ импортирует `keeper/internal/*` (Go-internal rules); все DB/Vault-операции — direct SQL / direct Vault HTTP API. Drift между harness-овским CRUD-каркасом и реальным schema-set — известный риск, синхронизация ручная при schema-change.
2. **Keeper-server TLS cert** выпускается harness-ом из Vault PKI (`harness.IssueKeeperServerCert`), симметрично `dev/provision.sh` шаг 7. SAN: `127.0.0.1` + `localhost`, TTL 24h.
3. **Vault test-secrets через direct HTTP API.** `harness.InitVaultTestSecrets` поднимает PKI mount + root CA + role `soul-seed` + JWT signing-key. Формат signing-key — **HS256: 32B base64-encoded string** в поле `signing_key` (см. `keeper/internal/jwt/issuer.go::minSigningKeyBytes`, `dev/provision.sh:88-92`). НЕ Ed25519 PEM. `sigil-signing-key` НЕ pre-seed-ится — `KeyService` introduces ключ runtime через свой API (см. `keeper/internal/sigil/keyservice.go`); pilot smoke-кейс использует только core-модули без plugin-Sigil-подписи.
4. **`keeper init --credential-out=<path>`** — точная форма CLI (см. `keeper/cmd/keeper/main.go::runInit`). Флаги `--jwt-out`/`--non-interactive` НЕ существуют; init non-interactive by default. harness читает выдачу credential-файла после init для последующих Operator-API-вызовов.
5. **`SOUL_STACK_ALLOW_FILE_REPOS=1`** в env subprocess-а keeper — обязательно для service-loader-а с `file://`-URL в test-режиме.
6. **Pre-auth регистрация soul-stub в БД** — Vault PKI leaf-cert на SID + direct SQL INSERT в `souls`/`soul_seeds`. Минует `bootstrap.Bootstrap` (это L3b territory); audit-событие `soul.bootstrapped` НЕ пишется harness-ом.
