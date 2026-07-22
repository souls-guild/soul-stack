# ADR-016. Ecosystem parity strategy and the Soul Stack license

- **Context.** In May 2026 the user formulated a requirement: "implement a comprehensive set of configuration-management modules, so that the ecosystem is at least on par with what operators expect." This is a **direction of work spanning months**, requiring decomposition and fixing the strategy before the start. In parallel a blocker was uncovered: the Soul Stack license had not been explicitly set, and without it decisions on the wrapper option cannot be made (a GPLv3-licensed wrapper — copyleft risk) and the SDK for plugin authors cannot be published.
- **Decision.**

  > ⚠️ **License part (a) REVISED 2026-07-09** — see the "fair-code / BSL" Amendment at the end of the file: core (this repo) and frontend (`soul-stack-web`) moved to **BSL 1.1** (Change License Apache 2.0, Change Date 2 years), SDK/examples/plugins remain **Apache 2.0**. The text below is the original Apache rationale, kept for context.

  **(a) Soul Stack license — Apache 2.0.** OSI-approved, permissive, patent grant. The standard for modern Go infrastructure (Kubernetes, Vault, Terraform, Prometheus, etcd). Does not impede corporate adoptions. File [`LICENSE`](../../LICENSE) at the root of the repository. Copyright header — `Copyright 2026 Soul Stack Authors` in every code file (via the linter once the first source appears). **Open core / freemium** monetization: additional paid products (enterprise SSO, audit-exports, managed HA, premium support) — **separate repositories** under a separate commercial license, pulling the Apache 2.0 core as a dependency. This is not part of this repository and not part of the Apache 2.0 codebase.

  CLA (Contributor License Agreement) — set up when the **first external contributor** appears, not now. Until then the copyright holder is single, and the license changes freely (should it ever be needed).

  **(b) Parity strategy — a hybrid without a wrapper.** No embedding of third-party copyleft module code into Soul Stack in any form. Parity is achieved through:
  - **Core MVP — our rewrite in Go** (see [ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)). Statically built into the `soul` binary.
  - **Exotics — community plugins** `soul-mod-*` / `soul-cloud-*` / `soul-ssh-*` in separate repositories via our Go SDK ([ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules), `sdk/`). Plugin authors decide for themselves: write from scratch, port from a permissively-licensed source (license-wise ok), port from a GPLv3 source (not allowed in a plugin for our system, a rewrite is needed).
  - **Wrapping GPLv3 modules is forbidden** — copyleft contamination risk, a foreign language runtime + attack surface contradicts "security first," and the templating engines do not match (external template engines vs CEL+Go text/template).
  - **Wrapping permissively-licensed external modules is not recommended** — license-wise ok, but the foreign runtime is the same risk, and such modules are tied to their own facts/parameters/loader — the wrapper becomes a half-rewrite.

  The "security first" principle here means **a safe default + an auditable opt-out under the operator's responsibility**, not a permanent ban on capabilities: the safeguard (for example https-only / SSRF-guard / TLS-verify in `core.url`/`core.http`, [ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)) is armed by default and is lifted by an explicit per-call flag with a warning in the `warnings` output — the operator deliberately weakens the protection and sees it in the `RunResult`.

  **(c) Phased parity roadmap:**

  1. **Phase 0 (now).** Core MVP per [ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list). The first service E2E.
  2. **Phase 1.** SDK for plugin authors (`sdk/module/`, `sdk/clouddriver/`, `sdk/sshprovider/` — already in [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules)). Template repo `soul-mod-template`. Documentation "how to write a module in an hour."
  3. **Phase 2.** The first ~10 official `soul-mod-*` for hot cases: `postgresql_user`, `redis_acl`, `nginx_vhost`, `docker_container`, `k8s_namespace`, `certbot`, `haproxy`, `mysql_user`, `rabbitmq_user`, `vault_kv`.
  4. **Phase 3.** Community onboarding. The name "module collection" (open Q in [module-collections.md](../module-collections.md)) — a separate task.
  5. **Phase 4 (cloud parity).** 3 CloudDriver in MVP (AWS / GCP / Azure — priority open Q #13), the rest — community.

  **(d) Not covered by this direction:**
  - ~~Event-driven monitoring contour — not in Soul Stack, a separate new ADR candidate for the future. Backlog.~~ **Closed by [ADR-030](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor):** the beacons contour is introduced (Vigil/Portent/Oracle/Decree); community checks — via the 4th plugin kind `soul_beacon`. The long-running-agent equivalent (on a host/Keeper) ADR-030 does **not** introduce — it remains backlog.
  - Network-OS / proxy-minions — a separate fork when a scenario appears.
  - Windows support — a separate fork when a scenario appears.

  **(e) Strategy document.** The detailed breakdown (inventory, categories, mapping onto the 3 plugin contracts, the phased roadmap) was assumed to be a separate document `docs/ecosystem-parity.md` — it was not created; the parity strategy is fixed here, in [ADR-016](#adr-016-ecosystem-parity-strategy-and-the-soul-stack-license) (the Decision/Consequences section). This ADR is the single source of truth for the decision.

- **Consequences.**
  - At the repo root the file [`LICENSE`](../../LICENSE) (Apache 2.0). *[→ BSL 1.1 per Amendment 2026-07-09.]*
  - In [`docs/architecture.md`](../architecture.md) a new "License" section appears (or a link to LICENSE) — a separate task.
  - Any plugin in a `soul-mod-*` / `soul-cloud-*` / `soul-ssh-*` repository that uses code under GPLv3 **cannot be included in the official Soul Stack list** without a rewrite. Community plugins under any compatible license are accepted as community.
  - Wrapping GPLv3 modules is not considered as an option at any stage.
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

  - **(m) Companion repo `soul-stack-plugins/`.** In parity with the `soul-stack-web` extraction — a separate git repo, not part of the core go.work. Apache 2.0 [LICENSE](https://github.com/souls-guild/soul-stack-plugins/blob/main/LICENSE) (parity with core). Contents: `soul-mod-template/` + `docs/module-author-guide.md` + (from SDK-2) plugin directories.

- **Amendment (2026-05-27, SDK-2 pilot — closure).** SDK-2 completed: the first 3 official `soul-mod-*` modules are ready as a pattern-fixture for the SDK-3 rollout of 7 modules.
  - **What is ready.** In `soul-stack-plugins/` (a separate git repo, not in the core go.work):
    - `soul-mod-official-postgres-user/` — idempotent CREATE/ALTER/DROP ROLE via `pgx/v5` + a `pg_roles` probe.
    - `soul-mod-official-nginx-vhost/` — render vhost-config (Go `text/template`) + `nginx -t` validate BEFORE write + symlink `sites-enabled/` + `nginx -s reload`. Plan + `PlanReadSafe` ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) implemented (drift-detect via comparison of content + symlink target).
    - `soul-mod-official-docker-container/` — a three-state `running`/`stopped`/`absent` via docker-CLI + drift-detect (image/env/ports/volumes/networks/restart_policy → recreate stop+rm+create+start). Plan + `PlanReadSafe`.
  - **Pattern fixture (fixed for the SDK-3 rollout).**
    - Scaffold via `soul-lint plugin-init official/<name>` ([Amendment 2026-05-27, SDK-1](#adr-016-ecosystem-parity-strategy-and-the-soul-stack-license)). After scaffold — handler.go in `internal/<name_snake>/`, not in `internal/` (a scaffold-drift fix, see the observations of the SDK-2 review).
    - State semantics: `present`/`absent` (persistent resources) or `running`/`stopped`/`absent` (containers/processes). The strategy is the natural semantics of the resource, not the dogma "always present/absent."
    - Test levels: **L0** (in-memory fake-runner + a fake stream via `grpc.ServerStreamingServer[ApplyEvent]`, coverage of create/alter/idempotent/drop/unknown-state/error-paths) + **L1** (testcontainers or a real-daemon with the build-tag `integration`, full-lifecycle) + **L3b** (skeleton with `t.Skip`, build-tag `live`, awaits the Vigil-extension L3b-harness in the core repo).
    - DI via optional module fields (`connect func(...)` / `runner dockerRunner` / `fs vfs`+`runner cmdRunner`) — nil → real-impl, L0 substitutes a fake.
    - Secret-input ([shared/plugin.input_secret_without_vault_pattern](https://github.com/souls-guild/soul-stack/blob/main/shared/plugin/manifest.go)) — `secret: true` + `pattern: "^vault:.*"`. Keeper-side vault-resolve ([ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)) resolves the `vault:` ref into the real value BEFORE Apply — the plugin sees the resolved one.
    - The manifest.yaml format `side_effects[]` — `[{<resource-type>: <name>}]` (`user`/`file`/`service`/...), a closed enum.
  - **L3b harness Vigil-extension in the core repo** — a separate slice post-SDK-2 (the `t.Skip` skeleton tests await it).
  - **Sigil signature of plugins** ([ADR-026](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)) — not required yet (a pilot dev-cycle). The production signing flow — SDK-4.
  - **Per-module repo extraction** — a separate slice post-SDK-2-review via `git filter-repo`.
  - Closes SDK-2 ((l) Slice map).

- **Amendment (2026-06-22, public-beta distribution phase).** The release of a public beta under a proprietary license was considered (all rights reserved + a beta-EULA "for evaluation only") with a subsequent manual switch to Apache 2.0 at GA.

  - **(n) A proprietary beta license — rejected.** ⚠️ *The "Apache 2.0 throughout the entire lifecycle" conclusion was REVISED by Amendment 2026-07-09 (fair-code / BSL). The rejection specifically of a proprietary beta-EULA still stands; the change is to fair-code BSL, not to a proprietary beta.* The decision favors the simpler and less risky variant: the license **remains Apache 2.0 throughout the entire lifecycle** — decision (a) stands unchanged. Originally the beta's protection was framed as **operational** rather than licensed (Apache 2.0 throughout, distribution controlled outside the license). Amendment 2026-07-09 supersedes that: protection rests on the **fair-code license (BSL)** itself — not on a proprietary beta-EULA and not on repository access — so reaching the public phase requires no license change. Consequences: editing the `LICENSE` files, the license labels in the nfpm manifests (`license: Apache-2.0` is correct) and the license statements in the public docs is **not required**. The SDK and community plugins also remain Apache 2.0 (as in (a)). A separate beta-EULA as an artifact is **not introduced**.

- **Amendment (2026-07-09, pivot to fair-code / BSL — monetization).** Ahead of the public release, a monetization decision was made: the fully open Apache-forever model (decision (a), Amendment (n)) is replaced by a **fair-code** model. This revises the licensing part (a) and the (n) conclusion of "Apache across the whole lifecycle"; the parity strategy (b-m: hybrid without a wrapper, SDK, official plugins) is unchanged.

  - **(o) License map (2 buckets).**
    - **Core** (this repository: `keeper`/`soul`/`soulctl`/`soul-lint` + built-in `core.*` modules) **and frontend** (`soul-stack-web`) → **Business Source License 1.1 (BSL 1.1)**. Change License — **Apache License 2.0**; Change Date — **2 years** from each version's first public release (a sliding window: every version becomes Apache 2.0 two years after its own release). The [`LICENSE`](../../LICENSE) file at the root — BSL 1.1 with the parameters filled in.
    - **SDK** (`sdk/*`), **plugin protocol** (`proto/plugin/*` — the `pluginv1` gRPC contract), **`examples/`**, and **official/community plugins** (`soul-stack-plugins`, `soul-mod-*`) → **Apache 2.0** (unchanged). The split is legally clean: plugins are separate processes over gRPC-stdio, not a derivative work of the core. Third-party authors are free to choose their own license, including proprietary paid plugins.
      - **Why `proto/plugin/*` must be Apache, not BSL.** Every plugin **statically links** the generated `pluginv1` stubs (`proto/plugin/gen/go/v1`) to speak the handshake / SoulModule / CloudDriver / SshProvider contract. If that package inherited the core's BSL, the BSL terms would leak into every third-party plugin binary — breaking the "plugins choose their own license" guarantee. Apache 2.0 on `proto/plugin/*` keeps the linked contract permissive, so a plugin's license stays unconstrained by the core.

  - **(p) Additional Use Grant (starting) — internal-use-only.** Production use is granted **solely for Internal Use**: operating the Licensed Work to manage your own or your organization's infrastructure, including commercial internal operations. **Any other production use requires a separate commercial license** — including making the Licensed Work available to third parties (whether for a fee or free of charge, and whether under its own name, your name, or a white-label brand) as a **Hosted/Managed Service** or a **product**, and **embedding it into a third-party product (OEM)**. Client-facing managed or professional services built on the Licensed Work are **not** part of the starting grant (an earlier "result, not access" carve-out was dropped to keep the boundary unambiguous). Non-production use (development, testing, evaluation, demonstration) remains permitted under the base BSL terms. The grant deliberately starts **tight** and may be **expanded** later (loosening is safe; tightening carries a reputational cost and fork risk). The authoritative grant text and the definition of "Hosted/Managed Service" live in the [`LICENSE`](../../LICENSE) file; the final wording is to be confirmed by counsel.

  - **(q) Trademark instead of code license.** The managed offering, the "Soul Stack" brand, and "official"/"certified" statuses are protected by **trademark**, not by license. A separate trademark policy — TODO (allowed: self-host/training/plugins; prohibited: calling a fork "Soul Stack", selling "official managed"/"certification" under our name).

  - **(r) CLA — mandatory before the first external contributor.** Under fair-code a CLA is needed not "just in case" but to hold the right to the Additional Use Grant, Change License, and future license amendments. Set it up before the first external PR (currently there are no external contributors — the window is open). See [CONTRIBUTING.md](../../CONTRIBUTING.md).

  - **(s) Fair-code rationale (brief).** Full closedness kills trust (an agent + secrets on the client's servers) and the funnel against free Terraform; pure Apache doesn't protect against reselling the core as someone else's service. Fair-code (the MariaDB/Sentry/CockroachDB/n8n model) keeps openness and auditability + returns every version to Apache via the Change Date. The timing is optimal: fair-code from birth (public beta, zero external contributors) → zero fork risk (unlike leaving open source after adoption: Terraform→OpenTofu, Redis→Valkey). BSL was chosen over FSL for the Use Grant's parametric flexibility. Revenue model: services → solution packs → managed (L3). Full context — the monetization strategy document.

  - **Amendment consequences.** `LICENSE` (core) = BSL 1.1; the nfpm `license` labels and the OCI label `org.opencontainers.image.licenses` = `BUSL-1.1` (the SPDX identifier for BSL 1.1). `soul-stack-web` LICENSE + the site footer — will be moved to BSL in a separate wave. The public ["Licensing" section](https://github.com/souls-guild/soul-stack) in the docs + the pitch deck are aligned with fair-code. **Final license texts — with counsel** (drafts based on the BSL 1.1 template are submitted for review).
