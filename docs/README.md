# Soul Stack Documentation

Unified documentation map. Same navigation framework for both human and AI agent: first determine **what you want to do**, then go to the appropriate section below.

**Routing by role:**

- **New user, first acquaintance** → section [Explore](#explore-newbie).
- **Operator, operates the cluster** (deploy / update / backup / monitoring / restore) → section [Make](#do-operator).
- **Author of configs** (destiny / scenario / service / templates / migrations / modules) → section [Find](#find-reference).
- **Architect / contributor** (design, invariants, solutions) → section [Understand](#understand-architectcontributor).
- **AI Agent** → start **with this map**: find the section for the purpose, follow the link. Agent work rules and a summary of decisions are in [../CLAUDE.md](../CLAUDE.md).

Root product review - [../README.md](../README.md). The public beta limits (what is NOT included) are [known-limitations.md](known-limitations.md).

---

## Explore (newbie)

First acquaintance: understand the dictionary and pick up a demo setup without reading ADR.

| Document | What is it / for whom |
|---|---|
| [getting-started.md](getting-started.md) | **Start here.** Quickstart for an external operator: build binaries from sources (`make build`), raise single-keeper + required infrastructure (Postgres / Redis / Vault) locally (`make dev-up` / `dev-provision` / `dev-keeper`), bootstrap the first Archon, onboard one Soul using CSR flow, apply the script `hello-world`. ~30 minutes, with teams. The browser API viewer also shows `GET /docs`. |
| [guides/first-service.md](guides/first-service.md) | **Next step after quickstart.** Step-by-step tutorial: build **your** service from scratch (`service.yml` with state_schema, `scenario/create`, `essence`), offline validation `soul-lint`, registering the service (git-ref as version), creating an incarnation and checking the result on the host. Walk-through on real [`hello-world`](../examples/service/hello-world/) with links to regulatory specifications for depth. Bridge between getting-started and exploitation. |
| [naming-rules.md](naming-rules.md) | **Dictionary of names** Soul Stack (Keeper / Souls / Destiny / Soulprint / Essence + SoulSeed / Coven / SID / Archon-AID / Reaper). Read before you begin to understand any configs; required before entering **any** new name. |

---

## Do (operator)

Production installation operational runbook. Reference, not a tutorial. All files are in [operations/](operations/README.md) (+ [keeper/prod-setup.md](keeper/prod-setup.md) according to Keeper specifics).

**Unfold and prepare:**

| Document | What is it / for whom |
|---|---|
| [operations/deployment.md](operations/deployment.md) | Deployment of the Keeper product cluster: artifacts, config, launch, readiness check. |
| [operations/deb-onboarding.md](operations/deb-onboarding.md) | Onboarding a cluster from deb packages: installation → Vault-provision → TLS → keeper init → soul init → connected. |
| [operations/infra.md](operations/infra.md) | Mandatory infrastructure outline (Postgres + Redis + Vault): requirements, versions, configuration for the future. |
| [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md) | Bootstrap of the first Archon and basic RBAC: `keeper init --archon`, role issuance, protection against self-lockout. |
| [keeper/prod-setup.md](keeper/prod-setup.md) | Transferring Keeper from the dev stack to production: differences from dev, infra-dependencies outside our code (Vault PKI, Postgres). |

**Operate and scale:**

| Document | What is it / purpose |
|---|---|
| [guides/operator-workflow.md](guides/operator-workflow.md) | **Operator work cycle.** Step-by-step practices: drift-check and reconcile, Service and Soul upgrades, souls scaling, incidents and recovery, automation of regular operations. A bridge between initial deployments and day-to-day operations. |
| [operations/monitoring.md](operations/monitoring.md) | Cluster monitoring: Prometheus, OTel metrics, what and how to monitor. |
| [operations/scaling.md](operations/scaling.md) | Horizontal scaling of a stateless-Keeper cluster for a large fleet. |
| [operations/upgrade.md](operations/upgrade.md) | Updating the cluster and services: order, compatibility, migrations. |

**Recover after failure:**

| Document | What is it / purpose |
|---|---|
| [operations/disaster-recovery.md](operations/disaster-recovery.md) | Backup and restoration of the cluster state after a failure. |
| [operations/recovery-reclaim-apply-runs.md](operations/recovery-reclaim-apply-runs.md) | Reclaim frozen apply-runs and restore runs. |
| [operations/faq.md](operations/faq.md) | Frequently operational questions and answers. |

---

## Find (reference)

Reference: exact formats, behavior, parameters. The source of truth is here (and in the code), not in reviews.

### Keeper API

| Document | What is it / for whom |
|---|---|
| [keeper/operator-api.md](keeper/operator-api.md) | **Operator API**: HTTP endpoints `/v1/*`, conventions, JWT-auth, RFC 7807 errors, mapping endpoint ↔ MCP-tool ↔ permission. Detailed sections of large domains are in the [operator-api/](keeper/operator-api/) subfolder (incarnations / souls / voyages / cadences / tidings / heralds / errands / push / oracle / synods / roles / ...). |
| [keeper/openapi.yaml](keeper/openapi.yaml) | **Committed OpenAPI 3.1 spec** (derived from huma Go types, [ADR-054](adr/0054-openapi-code-first.md)). Consumed by the UI vendor and `soulctl`. |
| API in the browser - `GET /docs` | RapiDoc viewer with full-text search by endpoints and "Try It"; served-spec `GET /openapi.json` / `/openapi.yaml` - for JWT. Mechanism and meta-routes - [keeper/operator-api.md → Served-spec](keeper/operator-api.md). Quickstart login in the browser - [getting-started.md](getting-started.md). |
| [keeper/mcp-tools.md](keeper/mcp-tools.md) | Catalog of Keeper MCP tools (+ details in [mcp-tools/](keeper/mcp-tools/)). |

### DSL and config formats (author of configs)

| Document | What is it / for whom |
|---|---|
| [destiny/](destiny/README.md) | Destiny index folder: format `destiny.yml` and `tasks/main.yml`, task fields, `input:`/`output:`, molecule-style tests. |
| [scenario/](scenario/README.md) | Scenario index folder: orchestration layer (`on:` / `where:` / `apply:`, probe-idiom, barrier / state-commit), border with destiny. The task DSL core is in [destiny/tasks.md](destiny/tasks.md). |
| [templating.md](templating.md) | **Regulatory template engine spec** ([ADR-010](adr/0010-templating.md)): CEL for YAML expressions (marker `${ … }`), Go text/template for files `.tmpl`, sprig allowlist, security model, render phases, `core.file.rendered`. |
| [migrations.md](migrations.md) | **Regulatory spec state_schema migration DSL** ([ADR-019](adr/0019-state-migration-dsl.md)): flat `rename`/`set`/`delete`/`move` + CEL + `foreach`, forward-only, sandbox, atomicity with one PG transaction, test layout. |
| [soul/soulprint.md](soul/soulprint.md) | Soulprint typed schema ([ADR-018](adr/0018-soulprint-typed.md)): fields `SoulprintFacts`, canonical CEL form `soulprint.self.<path>`, virtual projection `covens`. |
| [input.md](input.md) | **Format standard `input:`** for destiny / scenario / module manifest: types, validation keys, formats (hostname / email / semver / ...), examples. The source of truth in discrepancies. |
| [service/manifest.md](service/manifest.md) | Service repo layout and format `service.yml` (`name` / `state_schema_version` / `state_schema` / `destiny[]` / `modules[]`), prohibited keys, state_schema migrations, `soul-lint validate-service` validation. |
| [service/manifest.md#essence](service/manifest.md#essence) | **Essence** - hierarchical assembly of incarnation parameters (`essence/_default.yaml` + overlay by Coven / OS-family, opt. `_stack.yaml`-pipeline). Described inside manifest.md; full regulatory pipeline spec - [architecture.md → Essence](architecture.md#essence-assembly-pipeline). |

### Module Reference

| Document | What is it / for whom |
|---|---|
| [module/](module/README.md) | Index folder **per-module directory**: for each implemented core module - canonical name, states, parameters, idempotency, side-effects, example task. Documents behavior by code (not design - design in [ADR-015](adr/0015-core-modules-mvp.md) / [ADR-017](adr/0017-keeper-side-core.md)). |

Implemented modules in [module/core/](module/core/) - **23 directories** (each with its own `README.md`), and not all "modules" in the same sense:

- **18 Soul-side core** (apply on hosts): `pkg`, `file`, `service`, `user`, `group`, `exec`, `cmd`, `cron`, `mount`, `git`, `archive`, `sysctl`, `url`, `line`, `repo`, `firewall`, `http` (17 per [ADR-015](adr/0015-core-modules-mvp.md): 12 original MVP + post-MVP `url`/`line`/`repo`/`firewall`/`http`) + `augur` ([ADR-025](adr/0025-augur.md), read-probe via broker).
- **4 Keeper-side core** (`on: keeper` dispatcher): `soul`, `cloud`, `vault` ([ADR-017](adr/0017-keeper-side-core.md)) + `choir` ([ADR-044](adr/0044-choir.md)).
- **1 `beacon`** - Vigil body ([ADR-030](adr/0030-vigil-oracle.md)), read-only observer, not apply-module.

The exact summary of "what we think" and the source of truth (registry in the code) is [module/README.md → Directory status](module/README.md).

### Binary and RBAC configs

| Document | What is it / for whom |
|---|---|
| [keeper/](keeper/README.md) | Keeper-side index folder: Postgres + Redis, push, Reaper, plugins (Cloud / SSH), cloud integration, `keeper.yml` format. |
| [keeper/rbac.md](keeper/rbac.md) | RBAC: roles and permissions, unified application to OpenAPI / MCP / push, bootstrap of the first Archon. |
| [soul/](soul/README.md) | Soul-side index folder: identity, bootstrap token onboarding, connection algorithm, `soul.yml` format, module cache on the host. |
| [keeper/run-flavors.md](keeper/run-flavors.md) | Summary of entry-points for starting work: scenario via agent, batch via Voyage, single-Errand, push via SSH. Which endpoint API for which task. |
| [observability.md](observability.md) | Regulatory observability spec ([ADR-024](adr/0024-observability.md)): metric prefixes `keeper_*` / `soul_*`, OTel resource-attrs, cardinality control. |
| [soul-lint.md](soul-lint.md) | Offline linter Destiny / Essence: purpose, list of checks, restrictions. |

---

## Understand (architect/contributor)

Design, invariants, rationales and safety boundaries.

| Document | What is it / for whom |
|---|---|
| [architecture.md](architecture.md) | Source of truth on architecture: overview sections + stub links to ADR, topology, Soul life cycle, registries, connection algorithm, push, Reaper, end-to-end scenario, open questions. Before any task that affects the design. |
| [adr/README.md](adr/README.md) | **Index of all ADRs** with statuses (`active` / `amended` / `superseded`). 51 files `NNNN-<slug>.md`, maximum number - **0054**; numbering with gaps (numbers 0034, 0036, 0037 are not used). One ADR - one file. |
| [requirements.md](requirements.md) | Top-level product requirements: modularity, security, metrics, OTel, Vault, RBAC, MCP, OpenAPI, hot-reload, log rotation. |
| [security/threat-model.md](security/threat-model.md) | **Threat-model** of the Keeper cluster + Souls fleet: assets, actors / surfaces / boundaries, residual risks, environmental requirements. Reference; documents already implemented mechanisms, does not introduce new solutions. |
| [module-collections.md](module-collections.md) | Collections of modules as an entity: feature backlog and open Q (name, distribution, RBAC, signing, push cache). |
| [testing/](testing/README.md) | Index of testing levels (L0–L4); regulatory spec L3a - [testing/e2e.md](testing/e2e.md); cloud live-E2E orchestrator runbook - [testing/e2e-cloud.md](testing/e2e-cloud.md). |
| [testing/load-testing.md](testing/load-testing.md) | Load testing plan: Souls/API/run axes, scale 1k–100k, harness soul-legion, phases P0/P1/P2. P0+P1 MEASURED up to 25k (§8); 100k remains calculated. |
| [dev/local-setup.md](dev/local-setup.md) | Local dev stack: docker-compose (PG / Vault / Redis / OTel) + testcontainers-go for integration tests. |
| [guides/plugin-author.md](guides/plugin-author.md) | **How ​​to write your own module (soul-mod-*) - index.** The author's authoritative step-by-step guide is in the companion repo `soul-stack-plugins` ([module-author-guide.md](https://github.com/souls-guild/soul-stack-plugins/blob/main/docs/module-author-guide.md)); this core-side file is a short guide: when to write a plugin vs core/scenario + pointers to the author's core artifacts (SDK `sdk/module`, `proto/plugin/v1`, ADR-011 / ADR-016 / ADR-026). |
| [../CLAUDE.md](../CLAUDE.md) | Guide for AI agents: operating rules, propose-and-wait, documentation ahead of the code, summary of decisions. Every agent session. |
| [../examples/](../examples/) | Sample artifacts (destiny, service Redis HA, keeper / soul configs, incarnation requests, custom module skeleton) - illustration of formats, broken code. |
| [web/README.md](web/README.md) | **Companion UI** (soul-stack-web): front-end repo documentation, backend API contract, OpenAPI changes require web-side review. |

---

## Boundaries and state

| Document | What is this |
|---|---|
| [known-limitations.md](known-limitations.md) | What is NOT included in the public beta: cloud-provisioning without REST/MCP/UI, incomplete MCP coverage, audit-scaling on large fleets, supply-chain signature, JWT-only identity, push / recovery / Redis profile. Each point with a link to the canon - so that the absence of a feature is not mistaken for a bug. |
| [backlog.md](backlog.md) | **Backlog of deferred major epics**: deliberately paused features with a fixed impact, pragmatic workaround and resumption conditions (not open Q, not design). Now: per-service uniqueness of the incarnation name (the name is still a global PK). |
| [prod-readiness.md](prod-readiness.md) | **GA-gap roadmap**: what is not ready for production / GA (based on audit code results). P0-blockers (e2e-live blocking, clean-room onboarding, release-distribution + cosign, Shepherd, recovery-lease live, external pentest, remove `continue-on-error`), P1-hardening, P2 + strong points and proven load. The source of truth for GA-limits is on par with known-limitations.md; **not to be confused with drifting roadmap.md**. |

**Status.** MVP feature-complete: three binaries (`keeper` / `soul` / `soul-lint`) implemented, Keeper HA cluster (Postgres + Redis) proven on a live testbed; **released `v0.1.0-beta.1` (public beta, private repos `souls-guild`)**. Build / lint / tests - targets [`Makefile`](../Makefile) (`make build` / `make test` / `make check`). All architectural decisions go through ADR ([adr/](adr/README.md)); documentation is ahead of the code - a design change is an edit of the corresponding ADR, and not "new code as it happens."

---

## Dictionary of names (briefly)

Full vocabulary and rules - [naming-rules.md](naming-rules.md). Core terms:

| Soul Stack | Meaning |
|---|---|
| **Keeper** | Guardian, central server |
| **Souls** | Managed Agents |
| **Destiny** | What is applied to the host after the run |
| **Soulprint** (Prints) | Host System Facts |
| **Essence** | Parameters/values ​​collected hierarchically on incarnation |

---

## Where to write what (if you are adding a solution)

- **New ADR / change ADR** → file `adr/NNNN-<slug>.md` + line in [adr/README.md](adr/README.md) + stub link in [architecture.md](architecture.md). ADR/architecture - PM + architect zone, not docs-writer.
- **New entity name** → table in [naming-rules.md](naming-rules.md). First propose-and-wait in the chat, then recording.
- **New key/type/format in `input:`** → [input.md](input.md). First propose-and-wait, then writing to the standard.
- **New CEL function, sprig-allow/deny-edit, render phases, marker** → [templating.md](templating.md). First propose-and-wait.
- **New product requirement** → [requirements.md](requirements.md).
- **Metrica, OTel-instrumentation, resource-attr, namespace-prefix** → [observability.md](observability.md) (conventions) + [naming-rules.md](naming-rules.md) (names). Propose-and-wait for a new name.
- **Border "backend renders vs UI hardcoded"** → [ADR-042](adr/0042-backend-driven-ui.md); through point - [requirements.md](requirements.md).
- **Planned check `soul-lint`** → [soul-lint.md](soul-lint.md), section "Planned checks".
- **Concept / structure of destiny** → [destiny/](destiny/README.md).
- **Testing destiny and coverage** → [destiny/testing.md](destiny/testing.md).
- **Concept/scenario spec** → [scenario/](scenario/README.md). The task DSL core is not duplicated - it is in [destiny/tasks.md](destiny/tasks.md).
- **Format `service.yml` / service-repo layout / `soul-lint validate-service`** → [service/manifest.md](service/manifest.md).
- **Behaviour/config/lifecycle Soul** → [soul/](soul/README.md).
- **Keeper subsystem behavior / config / ** → [keeper/](keeper/README.md).
- **Augur is a Soul external access broker** → [keeper/augur.md](keeper/augur.md) ([ADR-025](adr/0025-augur.md)).
- **Sigil - plugin integrity** → [keeper/plugins.md → Integrity-model](keeper/plugins.md#integrity-model) ([ADR-026](adr/0026-sigil.md)).
- **Feature/open Q around module collections** → [module-collections.md](module-collections.md).
- **New testing level / L3a format** → [testing/](testing/README.md), spec - [testing/e2e.md](testing/e2e.md) ([ADR-039](adr/0039-e2e-testing.md)).
- **Security boundary/threat model** → [security/threat-model.md](security/threat-model.md). Documents what has been implemented; does not introduce new solutions.
- **Major epic postponed (deliberate pause, not open Q)** → entry in [backlog.md](backlog.md): what they wanted, why they postponed it, pragmatic workaround, conditions for resuming. ADR/architecture is not affected.
- **The document grows into a separate file** → create `docs/<topic>.md`, add an entry here and a link from [architecture.md](architecture.md).
