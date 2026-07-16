# ADR-0068. Upgrading incarnations to a new service version — `upgrade/` directory + upgrade-paths API

> **Status: accepted (implemented — NIM-34, merged to canon 2026-07-05).** Design approved by the user (2026-07-03). Extends the existing `POST /v1/incarnations/{name}/upgrade` (today — pin change + state migrations [ADR-019](0019-state-migration-dsl.md) → `drift`) with optional orchestration of the transition on hosts. **Amends [ADR-019](0019-state-migration-dsl.md)** (upgrade gains a second phase — a host-side upgrade scenario) **/ [ADR-009](0009-scenario-dsl.md)** (a second auto-discovery channel for scenarios: the `upgrade/` directory). Related: [ADR-007](0007-versioning-git-ref.md) (version = git-ref), [ADR-057](0057-state-changes-crud-verbs.md) (day-2 truth = `incarnation.state`), [ADR-065](0065-core-module-installed.md) (symmetry `create: true` / self-describing manifest), [ADR-043](0043-voyage.md) (bulk upgrade — future work). Impl — ticket NIM-34.

## Context

Soul Stack versioning is already fully ref-based ([ADR-007](0007-versioning-git-ref.md)): the service version is a git-tag, and the `version:` field in manifests is forbidden. The pin of a concrete incarnation — `incarnation.service_version` — is **captured on create** from the resolved `service_registry.ref` ([`keeper/internal/api/handlers/incarnation_typed.go:141`](../../keeper/internal/api/handlers/incarnation_typed.go), [`keeper/internal/mcp/incarnation_create.go:179`](../../keeper/internal/mcp/incarnation_create.go)) and is **forced instead of the registry HEAD in all run/render paths** ([`scenario/run.go:297`](../../keeper/internal/scenario/run.go), [`scenario/checkdrift.go:288`](../../keeper/internal/scenario/checkdrift.go), [`scenario/render_host.go:128`](../../keeper/internal/scenario/render_host.go), [`render/cel_render.go:161`](../../keeper/internal/render/cel_render.go), [`render/dispatch.go:152`](../../keeper/internal/render/dispatch.go), teardown — [`incarnation/destroy_prepare.go:92`](../../keeper/internal/incarnation/destroy_prepare.go)). The artifact cache is sha1-addressable → versions coexist. Choosing a version on create/day-2 forms is forbidden (**anti-version-craft** invariant: forms always use `inc.ServiceVersion`, [`api/huma_incarnation_formprefill.go:23`](../../keeper/internal/api/huma_incarnation_formprefill.go), [`api/handlers/incarnation_formprefill.go:53`](../../keeper/internal/api/handlers/incarnation_formprefill.go)).

**Upgrade today.** `POST /v1/incarnations/{name}/upgrade` with `to_version` ([`api/handlers/incarnation_typed.go:449`](../../keeper/internal/api/handlers/incarnation_typed.go) `UpgradeTyped`, route [`api/huma_incarnation.go:118`](../../keeper/internal/api/huma_incarnation.go)): `PrepareUpgrade` resolves the target snapshot, catches no-op/downgrade, assembles the chain of state migrations ([`incarnation/upgrade_prepare.go:74`](../../keeper/internal/incarnation/upgrade_prepare.go)) → `UpgradeStateSchema`/`upgradeTx` applies the migrations + changes `service_version` + sets `status=drift` in a single PG tx ([`incarnation/crud.go:1505`](../../keeper/internal/incarnation/crud.go), final UPDATE — `crud.go:1635`). **Hosts are not touched in the process**: the upgrade ends in an informational `drift` (ADR-031, amendment 2026-06-27, `crud.go:1443` `upgrade-pending-apply`), and the operator rolls out the new version **with an ordinary apply** manually. The returned `apply_id` is the ULID of the upgrade operation for migration history, and **not** an actual Runner run.

Gap: for nontrivial transitions between versions (change of deployment scheme, data relocation, layout change) "the operator will roll out the apply themselves" is not enough. What's needed is a **specialized, version-to-version, orchestrated transition**, triggered by the upgrade, yet without alarming the operator in ordinary day-2 lists and convenient to retry. This extension is what the ADR fixes.

## Decision

### 1. What we do NOT do (boundaries, preserved invariants)

- **Semver parsing of tags — no.** The version resolver remains `git checkout <ref>` ([ADR-007](0007-versioning-git-ref.md)); no ranges/`>=`.
- **Double-pin commit-sha — no.** The pin = git-ref, as is.
- **Choosing a version on create — no.** The anti-version-craft invariant is preserved: create/day-2 forms always use `inc.ServiceVersion`. The only legal place to change the version is the **upgrade action** (and its discovery — §6), not creation.

### 2. The upgrade scenario is optional

Upgrade works even without a single upgrade scenario. If no upgrade scenario is found for the `from→to` transition — the behavior is **exactly today's** (§5, legacy branch): pin change + state migrations [ADR-019](0019-state-migration-dsl.md) + `drift`. An upgrade scenario is an **additional** capability of the service author, not an obligation.

### 3. The upgrade scenario lives in a separate directory `upgrade/<slug>/`, self-describing `from:`

Upgrade scenario files live in a **separate top-level directory of the service repo** `upgrade/<slug>/main.yml` in the tree of the **NEW** version, NOT in the common `scenario/`. Reasons: (a) in day-2 scenario lists they would alarm the operator ("what is this operation, can it be run by hand?"); (b) a separate directory gives natural separation and convenient retry.

The `upgrade/<slug>/main.yml` manifest is **self-describing**: the top-level field `from:` is a list of source version-tags this scenario knows how to upgrade from:

```yaml
# upgrade/v2/main.yml  (in the tree of version v2.x)
from: ["v1.0.0", "v1.2.0"]
description: Transition redis sentinel→cluster on upgrade to v2
# ... ordinary scenario tasks: module: / apply: / state_changes / on: / where: ...
```

- **Symmetry with `create: true`** ([ADR-065](0065-core-module-installed.md)-style self-describing manifest): just as `create: true` declares "this scenario is suitable as a starter", so `from: [...]` declares "this scenario is suitable as a transition from these versions". The discriminator lives **in the scenario file itself**, not in the registry.
- **There is NO `upgrade:` block in `service.yml`.** The auto-discovery canon [ADR-009](0009-scenario-dsl.md) applies: scenarios are not listed in the manifest, keeper finds them by scanning the directory. upgrade/ scenarios are a second such directory alongside `scenario/`.
- **upgrade/ is not shown in ordinary day-2 lists** (`GET /v1/services/{name}/scenarios`, [`api/handlers/service.go:481`](../../keeper/internal/api/handlers/service.go)) — they are visible only through the upgrade contour (§6).

### 4. Direction "`from` in the new version", not "`to` in the old one"

`from:` is declared in the **new** version (the one we upgrade to), listing the old source versions. Rationale:

- **Tags are immutable** ([ADR-007](0007-versioning-git-ref.md)): at the moment `v1.0.0` is released, future versions are unknown — declaring "`to: v2`" in the old tag is physically impossible, the tag is already frozen. The new version, on the other hand, knows all its valid sources.
- **Symmetry with forward-only** [ADR-019](0019-state-migration-dsl.md): migrations are written forward (`<NNN>_to_<MMM>`, from<to), and upgrade is likewise forward. `upgrade/` inherits the same "new knows about old" axis.

### 5. Upgrade — 2 branches based on whether an upgrade scenario exists for `from→to`

Resolution: in the `PrepareUpgrade` phase (before the pin change, while `inc.ServiceVersion` = the old version) scan `upgrade/*/main.yml` of the **target** snapshot and find the scenario whose `from:` contains the current `inc.ServiceVersion`.

- **found** → change the pin + run the state migrations ([ADR-019](0019-state-migration-dsl.md), existing `upgradeTx`) + **auto-launch** the found upgrade scenario via `Runner.Start` ([`scenario/runner.go:25`](../../keeper/internal/scenario/runner.go), `RunSpec` — [`scenario/scenario.go:211`](../../keeper/internal/scenario/scenario.go)) with `ServiceRef` pinned to the **new** `to_version`. The upgrade scenario rolls out the transition on the hosts and drives `incarnation.state` with its own `state_changes`. Success → `ready`. **Failure → `error_locked` → unblock via `rerun-last`** (like an ordinary scenario: `UnlockForRerun` → `RunSpec.FromLocked`, [`scenario/scenario.go:225`](../../keeper/internal/scenario/scenario.go)); the retry runs the same upgrade scenario against the already-raised pin.
- **not-found** → **legacy**: only pin change + state migrations [ADR-019](0019-state-migration-dsl.md) + **WARN** in the log/response + `drift` (exactly today's behavior). The operator finishes the rollout with an ordinary apply.

**★ Fail-closed (422 on an undeclared transition) — DELIBERATELY DISCARDED.** The `upgrade/` directory is **inherited down the git patch tree**: the tag `v2.0.1` carries the same `upgrade/v2/` (with `from: [v1.*]`) as `v2.0.0`. Forbidding undeclared transitions would break innocent patch upgrades `v2.0.0→v2.0.1`, for which no one will write `from: [v2.0.0]`. Therefore "no upgrade scenario → fail" is wrong; the correct rule is "no upgrade scenario → legacy path". The cost: an undeclared "big" transition will silently go to legacy without host orchestration — this is caught by WARN + upgrade-paths (§6), not by 422.

Transactional boundary: the state migration ([ADR-019](0019-state-migration-dsl.md), one PG tx) stays atomic and is committed FIRST; the upgrade scenario is a separate subsequent Runner run. If the scenario fails — the pin is already raised and the state is already migrated; this is consistent with forward-only + `error_locked` + `rerun-last` (the retry does not roll back the pin, it catches up the rollout). The found branch is therefore obliged to **reserve `applying`** and hand control to the Runner instead of the final `drift` (impl fork §Scope: `upgradeTx` in found mode sets `applying`, not `drift`).

### 6. Discoverability — keeper enumerates, not the operator: `GET /v1/incarnations/{name}/upgrade-paths`

The objection to §4 "`from` in the new version does not give visibility into where to transition" is removed by the fact that **the versions are enumerated by keeper**, not by the operator from memory.

**Chosen path — incarnation-scoped: `GET /v1/incarnations/{name}/upgrade-paths`** (justification for the choice — below).

- **Without `?to=` — cheap**: enumeration of the service registry tags (reusing `ls-remote`, the same source as `GET /v1/services/{name}/refs` → `ListRefsTyped`, [`api/handlers/service.go:430`](../../keeper/internal/api/handlers/service.go)) with an `is_current` mark (tag == the incarnation's current pin). **Direction (forward/downgrade) is NOT computed in the cheap mode**: [ADR-007](0007-versioning-git-ref.md) forbids semver parsing of tag names — by names the direction is unreliable, so the exact direction / found-legacy / migrations are moved into `?to=`.
- **With `?to=<ref>` — on-demand analysis of a concrete target**: `direction` — **four values** (`no-op` / `downgrade` / `forward` / `same-schema` — a ref-bump without a schema change); `mode` (found vs legacy §5) is computed only for forward/same-schema; the applicable state migrations [ADR-019](0019-state-migration-dsl.md) (reusing `LoadMigrationChain` from `PrepareUpgrade`). The expensive per-target analysis is not run over the whole list — only on request. **A broken migration chain is not an HTTP error (422), but `200` with `reachable: false` + `unreachable_reason`**: the preview endpoint returns the unreachable target as DATA (the UI renders it gray), reachable targets — `reachable: true`.
- The UI renders a dropdown "what and how I can upgrade to" (the found/legacy label of a target is analyzed on-demand via `?to=`).

**Why incarnation-scoped and not `/v1/services/{name}/upgrade-paths`:** the answer "where and HOW I can transition" inevitably relies on **`from` = the current pin of a concrete incarnation** (`inc.ServiceVersion`) — a service has no single "current version", each incarnation has its own pin. Both expensive parts of the analysis — matching the upgrade scenario's `from:` and computing the applicable state migrations — require `inc.ServiceVersion` + `inc.state_schema_version`, known to the server only per-incarnation. Plus symmetry with the action that upgrade-paths precedes: `POST /v1/incarnations/{name}/upgrade` is also incarnation-scoped, and the UI Upgrade modal is opened from an incarnation. A service-scoped variant would force the client to send `from` as a query parameter (redundant — the server knows it — and fragile). The existing `GET /v1/services/{name}/refs` (service-scoped raw enumeration of tags, already marked "paired /refs for the Upgrade modal", [`api/handlers/service.go:483`](../../keeper/internal/api/handlers/service.go)) remains the low-level building block on top of which upgrade-paths adds incarnation context.

Permission — reuse `incarnation.upgrade` (the same operation, read facet) or `incarnation.get`; the choice is left to impl (§Scope). READ, without audit.

> *Amendment 2026-07-04 (NIM-34 impl): §6 reconciled with the implementation.* The cheap mode is reduced to an `is_current` mark — direction by tag names is NOT computed ([ADR-007](0007-versioning-git-ref.md)). `?to=` gives `direction` ∈ {`no-op`, `downgrade`, `forward`, `same-schema`}, and a target unreachable due to a broken migration chain is signaled by `reachable: false` + `unreachable_reason` (200, not 422). The wire contract — [operator-api/incarnations.md](../keeper/operator-api/incarnations.md); the enum dictionary — [naming-rules.md](../naming-rules.md#upgrade-v2-каталог-upgrade-ключ-from-upgrade-paths).

### 7. The input canon — a boundary: input is NOT migrated

`spec.input` = the **write-once creation recipe** (used by `rerun-last` on the create branch + audit), the truth of the desired state = `incarnation.state` (amendment [ADR-057](0057-state-changes-crud-verbs.md), day-2 reads `state`, not `input`/`essence`). **On upgrade `input` is NOT migrated** and is NOT a source of day-2: it is the pre-state seed of the create moment, not the desired state. The upgrade scenario works with `incarnation.state` (reads the deployed fact, writes via `state_changes`) and the `essence` of the target version — like any day-2 scenario. An explicit boundary: an upgrade may change the shape of `state` (via [ADR-019](0019-state-migration-dsl.md) migrations) and its content (via the upgrade scenario's `state_changes`), but `spec.input` remains a historical snapshot of create and is not rewritten.

### 8. MVP non-goals

- **Auto-chaining `v1→v3`** (composing a chain of upgrade scenarios through intermediate versions) — no; the MVP resolves a single direct `from→to`.
- **glob/semver in `from:`** — no; `from:` is an exact list of immutable tags.
- **Bulk upgrade of a scope** — a separate ticket NIM-35 via Voyage `kind=upgrade` ([ADR-043](0043-voyage.md)); mentioned here only as future work.

## What already exists vs what remains to be built (scope NIM-34)

**Exists and is reused:**
- `POST /v1/incarnations/{name}/upgrade` + `UpgradeTyped`/`PrepareUpgrade`/`UpgradeStateSchema`/`upgradeTx` (pin change + [ADR-019](0019-state-migration-dsl.md) migrations + `drift`).
- `Runner.Start` + `RunSpec` (launching a scenario), `error_locked` + `rerun-last` (`FromLocked`) — all the machinery of the found branch already exists.
- `ls-remote` enumeration of tags (`ListRefsTyped`) and `LoadMigrationChain` — building blocks of upgrade-paths.
- Scenario auto-discovery + self-describing flag (`create: true` → `Create *bool`, [`artifact/scenarios.go:150`](../../keeper/internal/artifact/scenarios.go)) — a template for `from:`.

**Remains to be built:**
1. **Scanner for `upgrade/*/main.yml`** — a new directory alongside `scenario/`. Currently `ListScenarios` hard-scans `scenario/*` (`scenarioDir="scenario"`, [`artifact/scenarios.go:22`](../../keeper/internal/artifact/scenarios.go)), and the Runner loads `scenario/%s/main.yml` (`scenarioMainFile`, [`scenario/scenario.go:48`](../../keeper/internal/scenario/scenario.go)). A second discovery path is needed + parsing the top-level `from: []string` + launching the upgrade scenario by the Runner via the `upgrade/` prefix (not "add a field", but a new load path).
2. **Resolving the upgrade scenario for `from→to`** in `PrepareUpgrade`: scan `upgrade/` of the target snapshot, match `from:` ⊇ `inc.ServiceVersion`.
3. **The found branch in the upgrade flow**: `upgradeTx` in found mode reserves `applying` (not `drift`) → `Runner.Start(upgrade scenario, ref=to)`; not-found → today's `drift` + WARN.
4. **Endpoint `GET /v1/incarnations/{name}/upgrade-paths`** (+ `?to=`): a cheap list of tags + on-demand per-target analysis (found/legacy + state migrations).
5. **UI**: the upgrade-paths dropdown (companion repo, outside the NIM-34 core scope).

## ⚠️ Discovered inconsistencies / decisions made because of them

1. **The directory is named `upgrade/<slug>/`, not `migrate/` — deliberately, to avoid the triple overloading of the word "migrate".** In the codebase it is already taken twice: (a) `scenario/migrate_cluster/` — an **existing create scenario** for migrating **DATA** from an external redis cluster via native replication (`create: true`, visible in day-2, [`examples/service/redis/scenario/migrate_cluster/main.yml`](../../examples/service/redis/scenario/migrate_cluster/main.yml)); (b) `migrations/<NNN>_to_<MMM>` — structural state_schema migrations [ADR-019](0019-state-migration-dsl.md). A third axis "version upgrade" under the "migrate" root would be confused with both. **Decision (2026-07-03): the directory `upgrade/<slug>/`** — symmetry with the action `POST /v1/incarnations/{name}/upgrade` and the upgrade-paths endpoint, zero collisions. The three terms (`upgrade/` version, `scenario/migrate_cluster/` data, `migrations/` schema) are to be disambiguated in [naming-rules.md](../naming-rules.md).
2. **`from` is taken in another sense**: `artifact.StateSchemaMigration.From int` ([`artifact/state_schema.go:41`](../../keeper/internal/artifact/state_schema.go)) — the from-version of the [ADR-019](0019-state-migration-dsl.md) chain (an integer). The new top-level `from: []string` of the upgrade manifest is a list of git-tags. A collision by name, not by structure; to be noted in the doc / disambiguated by field names in the parser.
3. **The current upgrade does NOT launch a Runner run** — it ends in `drift` (`crud.go:1640`), and the returned `apply_id` is the ULID of the upgrade **operation** for migration history, NOT a scenario run. The found branch introduces a real Runner run. It must be decided: the same `apply_id` for both phases (migration-tx + upgrade-run) or two different ones — otherwise `GET .../runs/{apply_id}` and history triage will be ambiguous. (Symmetry: today `writeUpgradeDriftHistory` writes a zero-diff record `upgrade-pending-apply` under the same `apply_id`.)
4. **The anti-version-craft invariant** ([`api/huma_incarnation_formprefill.go:23`](../../keeper/internal/api/huma_incarnation_formprefill.go)) is NOT violated by upgrade-paths (the invariant is about create/day-2 forms, always on `inc.ServiceVersion`), but the ADR must explicitly fix: upgrade-paths + the upgrade action are the **only** legal contour for changing the version; choosing a version on create is still forbidden.

## Consequences

- Upgrade turns from "pin change + drift" into two-phase: structural migration ([ADR-019](0019-state-migration-dsl.md)) + optional host orchestration (the upgrade scenario). Full backward compatibility: a service without `upgrade/` works as it does today (the legacy branch).
- A second auto-discovery channel for scenarios (`upgrade/`) — an amendment to [ADR-009](0009-scenario-dsl.md); `from:` is added to [naming-rules.md](../naming-rules.md).
- The new route `GET /v1/incarnations/{name}/upgrade-paths` — an OpenAPI/MCP surface (docs-writer, huma-native).
- The `upgrade/` directory requires a test convention (symmetric to `scenario/<name>/tests/`, `migrations/<NNN>_to_<MMM>/tests/`).
- Disambiguating the overloaded "migrate" in the doc/naming is a mandatory side effect (see inconsistency 1).

## Rejected alternatives

- **`to:` in the old version** — impossible due to immutable tags (§4).
- **Fail-closed 422 on an undeclared transition** — breaks patch upgrades (§5, ★).
- **The directory `migrate/`** — the word is overloaded three times (see inconsistency 1); `upgrade/` was chosen.
- **Service-scoped `/v1/services/{name}/upgrade-paths`** — the incarnation's `from` is known to the server, sending it as a query parameter is redundant and fragile (§6).
- **Upgrade scenarios in the common `scenario/`** with a discriminator — they alarm the operator in day-2 lists, break the convenience of retry/separation (§3).
- **Bulk/chained upgrade in the MVP** — moved to future work (§8, NIM-35/[ADR-043](0043-voyage.md)).

## Future work

- **NIM-35**: bulk upgrade of a scope via Voyage `kind=upgrade` ([ADR-043](0043-voyage.md)).
- Auto-chaining `v1→v2→v3`, glob/semver in `from:` — upon a real request, without a breaking change.
