# ADR-016. Parity strategy with SaltStack/Ansible and the Soul Stack license

- **Context.** In May 2026 the user formulated a requirement: "implement all the current modules from SaltStack and Ansible, so that the ecosystem is at least no worse." This is a **direction of work spanning months**, requiring decomposition and fixing the strategy before the start. In parallel a blocker was uncovered: the Soul Stack license had not been explicitly set, and without it decisions on the wrapper option cannot be made (GPLv3 Ansible — copyleft risk) and the SDK for plugin authors cannot be published.
- **Decision.**

  > ⚠️ **License part (a) REVISED 2026-07-09** — see the "fair-code / BSL" Amendment at the end of the file: core (this repo) and frontend (`soul-stack-web`) moved to **BSL 1.1** (Change License Apache 2.0, Change Date 2 years), SDK/examples/plugins remain **Apache 2.0**. The text below is the original Apache rationale, kept for context.

  **(a) Soul Stack license — Apache 2.0.** OSI-approved, permissive, patent grant. The standard for modern Go infrastructure (Kubernetes, Vault, Terraform, Prometheus, etcd). Does not impede corporate adoptions. File [`LICENSE`](../../LICENSE) at the root of the repository. Copyright header — `Copyright 2026 Soul Stack Authors` in every code file (via the linter once the first source appears). **Open core / freemium** monetization: additional paid products (enterprise SSO, audit-exports, managed HA, premium support) — **separate repositories** under a separate commercial license, pulling the Apache 2.0 core as a dependency. This is not part of this repository and not part of the Apache 2.0 codebase.

  CLA (Contributor License Agreement) — set up when the **first external contributor** appears, not now. Until then the copyright holder is single, and the license changes freely (should it ever be needed).

  **(b) Parity strategy — a hybrid without a wrapper.** No embedding of Ansible/Salt code into Soul Stack in any form. Parity is achieved through:
  - **Core MVP — our rewrite in Go** (see [ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)). Statically built into the `soul` binary.
  - **Exotics — community plugins** `soul-mod-*` / `soul-cloud-*` / `soul-ssh-*` in separate repositories via our Go SDK ([ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules), `sdk/`). Plugin authors decide for themselves: write from scratch, port from Salt (Apache 2.0 — license-wise ok), port from Ansible (GPLv3 — not allowed in a plugin for our Apache system, a rewrite is needed).
  - **Wrapping Ansible modules is forbidden** — GPLv3 copyleft contamination risk, Python-runtime +attack surface contradicts "security first," the templating engines do not match (Jinja2 vs CEL+Go text/template).
  - **Wrapping Salt modules is not recommended** — license-wise ok (Apache 2.0), but the Python-runtime is the same risk, and Salt modules are tied to Salt grains/pillars/loader — the wrapper becomes a half-rewrite.

  The "security first" principle here means **a safe default + an auditable opt-out under the operator's responsibility**, not a permanent ban on capabilities: the safeguard (for example https-only / SSRF-guard / TLS-verify in `core.url`/`core.http`, [ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)) is armed by default and is lifted by an explicit per-call flag with a warning in the `warnings` output — the operator deliberately weakens the protection and sees it in the `RunResult`.

  **(c) Phased parity roadmap:**

  1. **Phase 0 (now).** Core MVP per [ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list). The first service E2E.
  2. **Phase 1.** SDK for plugin authors (`sdk/module/`, `sdk/clouddriver/`, `sdk/sshprovider/` — already in [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules)). Template repo `soul-mod-template`. Documentation "how to write a module in an hour."
  3. **Phase 2.** The first ~10 official `soul-mod-*` for hot cases: `postgresql_user`, `redis_acl`, `nginx_vhost`, `docker_container`, `k8s_namespace`, `certbot`, `haproxy`, `mysql_user`, `rabbitmq_user`, `vault_kv`.
  4. **Phase 3.** Community onboarding. The name "module collection" (open Q in [module-collections.md](../module-collections.md)) — a separate task.
  5. **Phase 4 (cloud parity).** 3 CloudDriver in MVP (AWS / GCP / Azure — priority open Q #13), the rest — community.

  **(d) Not covered by this direction:**
  - ~~Event-driven contour (a Salt beacons/engines equivalent) — not in Soul Stack, a separate new ADR candidate for the future. Backlog.~~ **Closed by [ADR-030](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor):** the beacons contour is introduced (Vigil/Portent/Oracle/Decree); community checks — via the 4th plugin kind `soul_beacon`. The engines equivalent (long-running on a host/Keeper) ADR-030 does **not** introduce — it remains backlog.
  - Network-OS / proxy-minions — a separate fork when a scenario appears.
  - Windows support — a separate fork when a scenario appears.

  **(e) Strategy document.** The detailed breakdown (inventory, categories, mapping onto the 3 plugin contracts, the phased roadmap) was assumed to be a separate document `docs/ecosystem-parity.md` — it was not created; the parity strategy is fixed here, in [ADR-016](#adr-016-parity-strategy-with-saltstackansible-and-the-soul-stack-license) (the Decision/Consequences section). This ADR is the single source of truth for the decision.

- **Consequences.**
  - At the repo root the file [`LICENSE`](../../LICENSE) (Apache 2.0). *[→ BSL 1.1 per Amendment 2026-07-09.]*
  - In [`docs/architecture.md`](../architecture.md) a new "License" section appears (or a link to LICENSE) — a separate task.
  - Any plugin in a `soul-mod-*` / `soul-cloud-*` / `soul-ssh-*` repository that uses code under GPLv3 **cannot be included in the official Soul Stack list** without a rewrite. Community plugins under any compatible license are accepted as community.
  - Wrapping Ansible modules is not considered as an option at any stage.
  - Open Q #5 in the strategy part — closed. The open Q "Soul Stack license" — does not arise (closed immediately).
- **Trade-offs.**
  - Parity is achieved more slowly than with the wrapper variant (years of community work vs instant coverage). Accepted — security + license cleanliness are more critical.
  - Apache 2.0 does not block AWS-Soul-as-a-service competitors. If in the future this becomes a problem — a migration to BSL/SUL is possible for **new versions**, but it will require a CLA from all contributors (therefore it is better to set up the CLA before the contributor pool grows — a separate operational task). **[Implemented by Amendment 2026-07-09 — core+web moved to BSL 1.1; timing correct: no external contributors yet, fork risk zero, CLA set up before the first external PR.]**
  - The open core split requires discipline: enterprise features must live in a separate repository under a different license, not in this one. An accidental contribution of an enterprise feature into the Apache core risks "accidentally" making it open source — an explicit gate (review) is needed.

- **Amendment (2026-05-27, Plugin SDK Phase 2 — closure).** Phase 2 ((c).2 of the phased roadmap) is fixed as ready to start. The PM decisions closing the forks of list/namespace/template-mechanism/repos/coverage:

  - **(f) Ten first official `soul-mod-*` (final list, namespace `official`):**

    | # | Binary | Purpose |
    |---|---|---|
    | 1 | `soul-mod-official-postgres-user` | PostgreSQL roles (parity pair to `postgres-db`) |
    | 2 | `soul-mod-official-postgres-db` | PostgreSQL databases |
    | 3 | `soul-mod-official-mysql-user` | MySQL/MariaDB users |
    | 4 | `soul-mod-official-mysql-db` | MySQL/MariaDB databases |
    | 5 | `soul-mod-official-nginx-vhost` | nginx virtual hosts |
    | 6 | `soul-mod-official-haproxy-backend` | HAProxy backend / server |
    | 7 | `soul-mod-official-docker-container` | Docker container |
    | 8 | `soul-mod-official-letsencrypt-cert` | Let's Encrypt / certbot certificate |
    | 9 | `soul-mod-official-redis-acl` | Redis ACL |
    | 10 | `soul-mod-official-rabbitmq-user` | RabbitMQ users |

    Supersedes the draft list from ADR-016 (c).Phase 2 (`k8s_namespace` rejected — it is a CloudDriver parallel, not a SoulModule; `vault_kv` is covered by keeper-side `core.vault.kv-read` in [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read); the parity pair `postgres-user`+`postgres-db` and `letsencrypt-cert` are added).

  - **(g) Reserved namespace `official`.** Plugin manifest name: `<namespace>.<name>` = `official.postgres-user` (validated by the existing `shared/plugin` regexes — `namespace` and `name` are both `^[a-z][a-z0-9-]{0,62}$`, see [naming-rules.md → Plugin manifest: regex of names](../naming-rules.md#plugin-manifest-regex-of-names)). Binary-name pattern — `soul-mod-official-<name>`. Sigil-trust under the namespace `official` is signed by the keeper-cluster-issuing-key.

  - **(h) Template mechanism — a hybrid.** Template repo `soul-stack-plugins/soul-mod-template/` (clone-and-modify) **AND** the CLI `soul-lint plugin-init <namespace>/<name>` (embed-uses a static template via `go:embed`). Both paths produce an identical tree. The primary source of truth is the embed in `soul-lint/internal/plugininit/template/`; the copy in the companion repo is synchronized manually (sync-job — backlog). `soul-lint` is extended with a new subcommand — the formal extension of the offline linter's scope to init-tooling is fixed here (propose-and-wait passed by the user's decision).

  - **(i) Repos structure.** A pilot monorepo `soul-stack-plugins/` for SDK-1 (this amend) + SDK-2 (3 pilot modules) + SDK-3 (rollout of 7). After the SDK-2 review (a stop-gate) — extract per-module via `git filter-repo`. Goal by the end of SDK-3: each plugin = its own git repo for a clean release cycle.

  - **(j) L3b coverage strategy.** 5 flagship modules go under a full L3b live-test: `postgres-user` / `nginx-vhost` / `docker-container` / `letsencrypt-cert` / `haproxy-backend`. The other 5 — L0 + L1 (testcontainers). The imbalance is deliberate: the cost-of-L3b runtime is high (a privileged Debian-12 + bootstrap of dependent services), and the flagship set covers 3 categories (relational DB + reverse proxy + container-runtime).

  - **(k) state_schema for plugins.** Plugins do NOT own `state_schema` (this is a service-level attribute, [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). A plugin may **read** `incarnation.state` through the `register` mechanism of service.yml, but does not introduce its own version chains.

  - **(l) Slice map.** SDK-1 (template + CLI + amend, this commit) → SDK-2 (3 pilot modules postgres-user/nginx-vhost/docker-container — full pipeline) → SDK-3 (rollout of 7 in 2-3 parallel batches) → SDK-4 (community onboarding: Sigil-flow, listing).

  - **(m) Companion repo `soul-stack-plugins/`.** In parity with the `soul-stack-web` extraction — a separate git repo, not part of the core go.work. Apache 2.0 [LICENSE](https://github.com/co-cy/soul-stack-plugins/blob/main/LICENSE) (parity with core). Contents: `soul-mod-template/` + `docs/module-author-guide.md` + (from SDK-2) plugin directories.

- **Amendment (2026-05-27, SDK-2 pilot — closure).** SDK-2 completed: the first 3 official `soul-mod-*` modules are ready as a pattern-fixture for the SDK-3 rollout of 7 modules.
  - **What is ready.** In `soul-stack-plugins/` (a separate git repo, not in the core go.work):
    - `soul-mod-official-postgres-user/` — idempotent CREATE/ALTER/DROP ROLE via `pgx/v5` + a `pg_roles` probe.
    - `soul-mod-official-nginx-vhost/` — render vhost-config (Go `text/template`) + `nginx -t` validate BEFORE write + symlink `sites-enabled/` + `nginx -s reload`. Plan + `PlanReadSafe` ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) implemented (drift-detect via comparison of content + symlink target).
    - `soul-mod-official-docker-container/` — a three-state `running`/`stopped`/`absent` via docker-CLI + drift-detect (image/env/ports/volumes/networks/restart_policy → recreate stop+rm+create+start). Plan + `PlanReadSafe`.
  - **Pattern fixture (fixed for the SDK-3 rollout).**
    - Scaffold via `soul-lint plugin-init official/<name>` ([Amendment 2026-05-27, SDK-1](#adr-016-parity-strategy-with-saltstackansible-and-the-soul-stack-license)). After scaffold — handler.go in `internal/<name_snake>/`, not in `internal/` (a scaffold-drift fix, see the observations of the SDK-2 review).
    - State semantics: `present`/`absent` (persistent resources) or `running`/`stopped`/`absent` (containers/processes). The strategy is the natural semantics of the resource, not the dogma "always present/absent."
    - Test levels: **L0** (in-memory fake-runner + a fake stream via `grpc.ServerStreamingServer[ApplyEvent]`, coverage of create/alter/idempotent/drop/unknown-state/error-paths) + **L1** (testcontainers or a real-daemon with the build-tag `integration`, full-lifecycle) + **L3b** (skeleton with `t.Skip`, build-tag `live`, awaits the Vigil-extension L3b-harness in the core repo).
    - DI via optional module fields (`connect func(...)` / `runner dockerRunner` / `fs vfs`+`runner cmdRunner`) — nil → real-impl, L0 substitutes a fake.
    - Secret-input ([shared/plugin.input_secret_without_vault_pattern](https://github.com/co-cy/soul-stack/blob/main/shared/plugin/manifest.go)) — `secret: true` + `pattern: "^vault:.*"`. Keeper-side vault-resolve ([ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)) resolves the `vault:` ref into the real value BEFORE Apply — the plugin sees the resolved one.
    - The manifest.yaml format `side_effects[]` — `[{<resource-type>: <name>}]` (`user`/`file`/`service`/...), a closed enum.
  - **L3b harness Vigil-extension in the core repo** — a separate slice post-SDK-2 (the `t.Skip` skeleton tests await it).
  - **Sigil signature of plugins** ([ADR-026](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)) — not required yet (a pilot dev-cycle). The production signing flow — SDK-4.
  - **Per-module repo extraction** — a separate slice post-SDK-2-review via `git filter-repo`.
  - Closes SDK-2 ((l) Slice map).

- **Amendment (2026-06-22, closed-beta distribution phase).** The release of a closed beta under a proprietary license was considered (all rights reserved + a beta-EULA "for evaluation only") with a subsequent manual switch to Apache 2.0 at GA.

  - **(n) A proprietary beta license — rejected.** ⚠️ *The "Apache 2.0 throughout the entire lifecycle" conclusion was REVISED by Amendment 2026-07-09 (fair-code / BSL). The rejection specifically of a proprietary beta-EULA still stands; the change is to fair-code BSL, not to a proprietary beta.* The decision favors the simpler and less risky variant: the license **remains Apache 2.0 throughout the entire lifecycle** — decision (a) stands unchanged. The "closedness" of the beta is provided **operationally** — by private repositories (org `souls-guild`) and access by invitation, not by the license. The "transition to the public phase" at GA means **opening the repositories** (public), not a change of license. Consequences: editing the `LICENSE` files, the license labels in the nfpm manifests (`license: Apache-2.0` is correct) and the license statements in the public docs is **not required**. The SDK and community plugins also remain Apache 2.0 (as in (a)). A separate beta-EULA as an artifact is **not introduced**.

- **Amendment (2026-07-09, разворот в fair-code / BSL — монетизация).** Перед публичным релизом принято решение по монетизации: полностью открытый Apache-навсегда (решение (a), Amendment (n)) заменяется моделью **fair-code**. Это пересматривает лицензионную часть (a) и вывод (n) «Apache на всём ЖЦ»; стратегия parity (b–m: гибрид без wrapper-а, SDK, official-плагины) не меняется.

  - **(o) Лицензионная карта (3 корзины).**
    - **Ядро** (этот репозиторий: `keeper`/`soul`/`soulctl`/`soul-lint` + встроенные `core.*`-модули) **и frontend** (`soul-stack-web`) → **Business Source License 1.1 (BSL 1.1)**. Change License — **Apache License 2.0**; Change Date — **2 года** от первого публичного релиза каждой версии (скользящее окно: каждая версия становится Apache 2.0 через 2 года после своего релиза). Файл [`LICENSE`](../../LICENSE) в корне — BSL 1.1 с заполненными параметрами.
    - **SDK** (`sdk/*`), **`examples/`** и **official/community-плагины** (`soul-stack-plugins`, `soul-mod-*`) → **Apache 2.0** (без изменений). Split юридически чист: плагины — отдельные процессы через gRPC-stdio, не derivative work ядра. Сторонние авторы вольны в своей лицензии, включая проприетарные платные плагины.
    - **Премиум-паки и enterprise-модули** (позже) → отдельная **коммерческая** лицензия (open-core поверх BSL-ядра).

  - **(p) Additional Use Grant (стартовый).** Production-использование разрешено, **кроме** предоставления Licensed Work третьим лицам как **Hosted/Managed Service** и **white-label**. Разрешено: (a) внутреннее использование (вкл. коммерческую эксплуатацию своей/корпоративной инфраструктуры); (b) dev/test/eval/демо; (c) professional/managed-услуги клиентам, где клиент получает **результат**, а не доступ к Soul Stack как продукту. **Embedding/OEM** в стартовый грант не входит (требует коммерческой лицензии; расширить позже). Стратегия — стартовать с чётким грантом и **расширять** со временем (ослабление безопасно; ужесточение = репутационный налог + форк-риск). Точный юридический текст гранта и определение «Hosted/Managed Service» — в файле `LICENSE`; **финал сверяется с юристом**.

  - **(q) Trademark вместо лицензии кода.** Managed-предложение, бренд «Soul Stack», статусы «official»/«certified» защищаются **торговой маркой**, а не лицензией. Отдельная trademark-policy — TODO (разрешено: self-host/обучение/плагины; запрещено: называть форк «Soul Stack», продавать «official managed»/«сертификацию» от нашего имени).

  - **(r) CLA — обязателен до первого внешнего contributor-а.** При fair-code CLA нужен не «на всякий случай», а чтобы держать право на Additional Use Grant, Change License и будущие правки лицензии. Завести до первого внешнего PR (сейчас внешних contributors нет — окно открыто). См. [CONTRIBUTING.md](../../CONTRIBUTING.md).

  - **(s) Обоснование fair-code (кратко).** Полное закрытие душит доверие (агент + секреты на серверах клиента) и воронку против бесплатных Ansible/Terraform; чистый Apache не защищает от перепродажи ядра как чужого сервиса. Fair-code (модель MariaDB/Sentry/CockroachDB/n8n) сохраняет открытость и аудируемость + возвращает каждую версию в Apache через Change Date. Тайминг оптимален: fair-code с рождения (закрытая бета, внешних contributors = 0) → нулевой форк-риск (в отличие от ухода из опенсорса после adoption: Terraform→OpenTofu, Redis→Valkey). BSL выбран над FSL за параметрическую гибкость Use Grant. Модель дохода: услуги → solution-packs → managed (L3). Полный контекст — документ-стратегия монетизации.

  - **Следствия Amendment.** `LICENSE` (core) = BSL 1.1; nfpm-метки `license` и OCI-label `org.opencontainers.image.licenses` = `BUSL-1.1` (SPDX-идентификатор BSL 1.1). `soul-stack-web` LICENSE + футер сайта — переводятся на BSL отдельной волной. Публичный [раздел «Лицензирование»](https://github.com/co-cy/soul-stack) в доке + питч-дек выровнены с fair-code. **Финальные тексты лицензий — с юристом** (черновики по эталону BSL 1.1 вынесены на приёмку).
