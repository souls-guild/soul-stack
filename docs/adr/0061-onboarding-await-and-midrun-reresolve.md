# ADR-061. Single-run provision→onboarding→role: onboarding-await + mid-run re-resolve roster

> **Status: active.** User decision + architect design (epic "Path-2", slices S0 (this ADR) / S1 (await_online pilot) / S2 (Stratify passage boundary) / S3 (actual re-resolve roster in run.go)). The canon is fixed docs-first BEFORE code; this ADR **amends [ADR-009](0009-scenario-dsl.md), [ADR-056](0056-staged-render-passage.md), [ADR-006](0006-cache-redis.md), [ADR-017](0017-keeper-side-core.md)**. propose-and-wait is closed (the new capabilities are flags on the existing `core.soul.registered`, not a new entity).
>
> **Implementation progress.** S1 (`await_online` pilot) is implemented: blocking poll of the Redis SID-lease + B1-strict fail + list-SID + the `keeper.yml::max_await_timeout` ceiling. S2 (Stratify passage boundary for `refresh_soulprint`) and S3 (actual re-resolve roster in run.go) are **implemented**: `refresh_soulprint: true` makes the step a passage-defining emitter, the scenario-runner re-resolves the roster at the refresh boundary (live snapshot, see §S3), `register.<name>.refreshed` echoes the flag value. Cloud e2e is a separate slice.

## Amendment 2026-06-28 — no_hosts is skipped for provision-from-zero (two classes)

**Problem.** The unconditional no_hosts gate (`run.go` step 3: `if len(hosts)==0 { abort no_hosts }`) cut down cloud-bootstrap create on an empty roster BEFORE keeper-dispatch. The target provision scenario (`example-cloud-bootstrap/scenario/create`, see [ADR-017(h)](0017-keeper-side-core.md), [ADR-063](0063-bootstrap-token-delivery.md)) has NO hosts at the start — it is the one that creates them (`core.cloud.created` → `core.bootstrap.delivered`, both `on: keeper`). Chicken-egg: the run requires connected hosts, yet the run is what provisions them. The gate made provision-from-zero unexecutable.

**Solution (Variant A + a 2nd bypass class).** The gate is skipped on ONE OF the two signs of provision-from-zero; otherwise — `no_hosts` BIT-FOR-BIT:

```
provisionsRoster := config.HasRefreshEmitter(scn.Tasks)
if len(hosts)==0 && !allKeeperTasks(scn.Tasks) && !provisionsRoster { abort no_hosts }
```

- **Class (a) — all-keeper** (`allKeeperTasks` over `scn.Tasks` AFTER `ExpandIncludes`, each `render.IsKeeperTask`). The sign is an all-`on:keeper` scenario composition (VMs are created by keeper tasks, there are no hosts at the start by definition). By COMPOSITION, not by a flag — no extra DSL surface is introduced. An empty scenario (`len(tasks)==0`) → `allKeeperTasks` = `false`: "no tasks" is not a reason to bypass the gate.
- **Class (b) — mixed with a refresh emitter** (`config.HasRefreshEmitter(scn.Tasks)`: the plan carries `core.soul.registered` with `refresh_soulprint: true`, recursively through `block:`). The roster is re-resolved **mid-run** (§S2/§S3): host deploy tasks are stratified into a Passage **AFTER** the refresh boundary and see the already re-resolved live snapshot (the VMs that onboarded), not an empty P0. Therefore an empty starting roster is **legitimate** — this is exactly the staged provision→role in a single run. The detector reuses the existing private predicate `taskIsRefreshEmitter` (the same one that feeds the roster axis `Stratify`/`RefreshBoundaries`) — module-name+param-parse is NOT duplicated.
- **Mixed WITHOUT a refresh emitter** (host task + keeper task, but no `refresh_soulprint`) — `allKeeperTasks`=false AND `HasRefreshEmitter`=false → **keeps no_hosts**. Correct: a host task on an empty P0 without a mid-run re-resolve is no_hosts. Combining provision and role into a single NON-staged Passage (without a refresh emitter) is still not allowed.

**Essence keeper context on an empty roster.** Essence resolution took the OS-family/Covens of the `hosts[0]` representative — on an empty roster it would panic. On an empty roster, essence is resolved in the **keeper context** (`keeperEssenceInput`): default layer + the incarnation Coven overlay (the root Coven label = `inc.Name`, [ADR-008](0008-coven-stable-tags.md): every host of the roster carries it, so it is applicable even without a host) + the `spec.essence` override. The OS-family overlay is **skipped** (there is no per-host soulprint) — symmetrically to `renderKeeperTask`, which renders keeper tasks without a per-host soulprint. After the created VMs onboard, subsequent Passages get per-host essence the usual way (§S3).

**Open.** standalone-recovery of a long `await_online` barrier in a single-keeper run is **not closed** (see §"HA — provision scenarios via Voyage" below): provision scenarios are recommended to be run via Voyage.

**Cross-ref:** [ADR-017(h)](0017-keeper-side-core.md) (cloud-init B-flat + `core.cloud.created` generate_userdata), [ADR-063](0063-bootstrap-token-delivery.md) (`core.bootstrap.delivered`).

## Amendment 2026-06-29 — deploy-branch size-asserts are gated by `when: !provision.enabled`

**Problem.** The Redis service carries keeper-side render-time **size-asserts** (`assert: size(soulprint.hosts) == shards*(1+replicas)` for cluster, `== 1+replicas` for sentinel — a topology guard, [ADR-009 amendment 2026-06-23](0009-scenario-dsl.md)). Pre-flight `EvalAsserts` (non-staged, [pre-flight invariant](0056-staged-render-passage.md)) evaluates them **before** the run starts. On the provision path (`input.provision.enabled`) the souls at that moment are **not yet created** — they will be brought up by the `redis-provision.yml` steps (`core.cloud.created` → VM → `await_online` barrier → `refresh_soulprint`), so `size(soulprint.hosts)` at pre-flight equals 0 → the assert failed with `ErrAssertFailed` (422) BEFORE the cluster even exists. The same chicken-egg that closed the no_hosts-bypass (amendment 2026-06-28), but on a different gate — a pre-flight assert, not run.go no_hosts.

**Solution (Variant 1 — a provision-aware gate in the redis service, locally).** A `when: "!(has(input.provision) && input.provision.enabled)"` (the inversion of the provision body's include-when) was added to the deploy-branch size-assert tasks. The predicate is **STATIC** ([`isStaticWhen`](../../keeper/internal/render/pipeline.go): pure input, without `register.*`/`soulprint.self`) → pre-flight evaluates it itself and on provision placeholder-skips the assert (NOT `ErrAssertFailed`). On an existing roster (`provision` omitted / `enabled=false`) `has(input.provision)` yields false → `when=true` → the size-guard is **active as before** (NOT weakened for NON-provision runs).

- **Where exactly.** `create/cluster.yml`, `create/sentinel.yml`, `migrate_cluster/cluster.yml` (all three include the common provision body `redis-provision.yml`). `migrate_cluster/sentinel.yml` carries NO size-guard of its own (it goes straight to `include redis-deploy-sentinel.yml`) — there is nothing to gate. `create_from_souls/*` do NOT include the provision body (always-existing-roster) → their size-guard is WITHOUT a gate (correct: provision is impossible there).
- **Why the gate is safe.** On provision the roster invariant is guaranteed **not by pre-flight**, but by two other mechanisms: (1) the `count` formula of `redis-provision.yml` (the number of VMs created = the same `shards*(1+replicas)` / `1+replicas` — the single source of truth for the size, there is deliberately no separate `node_count`); (2) the blocking `await_online` barrier (B1-strict: an online shortfall → `failed` → `error_locked`). Symmetrically to the no_hosts-bypass: the provision path is served by the scenario composition + the barrier, not by an up-front check of an empty roster.
- **`pre-flight` remains non-staged (the invariant is preserved).** The fix is at the SERVICE LEVEL (a DSL `when:` gate), keeper `EvalAsserts`/`PreflightAssert` is NOT touched. Static-when placeholder-skip is an already existing pre-flight mechanism (`evalAssertTask` → `isStaticWhen`), not staged pre-flight.
- **Known-edge (degraded UX, NOT silent corruption).** With `cluster_topology` + provision, `count` provision is derived from `shards`/`replicas` (the `redis-provision.yml` formula), NOT from the sum of the topology sizes (which the size-guard uses on an existing roster). If these two diverge (the operator set `cluster_topology` whose sum of sizes ≠ `shards*(1+replicas)`), provision will bring up VMs by the formula, while the plugin will verify the layout against the topology → the divergence is caught by the **`await_online` barrier + plugin fail-fast** (`community.redis.cluster` checks the node count), NOT pre-flight. This is a late but clear failure (degraded UX), not silent corruption — acceptable for the edge case `cluster_topology`+provision.

**Cross-ref:** amendment 2026-06-28 (no_hosts-bypass — a related chicken-egg on run.go).

## Amendment 2026-07-02 — the barrier waits for presence AND the first soulprint when `refresh_soulprint`

**Problem (structural race, the 7th wall of live-create).** The `await_online` barrier waited **only** for the presence-lease (Redis SID-lease = the transport is alive), NOT for the first typed soulprint in PG; `refresh_soulprint` was a passive echo flag (re-resolves the roster from the registry, does not wait for or request facts). Immediately after the barrier, the render of the next Passage (redis `vars.yml`: `soulprint.self.os.arch`) reads the soulprint, but `souls.soulprint_facts` is still `NULL`: the Soul sends the initial SoulprintReport on connect ([ADR-018](0018-soulprint-typed.md), best-effort), its processing is asynchronous, and on provision-from-zero the barrier and the render fit within one second → `render_failed` "no such key: os". The `create_from_souls` Souls did not hit the race — onboarding happened hours before create, facts were long in PG.

**Solution (Variant 1 — facts-wait in the barrier).** With `refresh_soulprint: true` **and** `await_online: true` the barrier additionally requires from each SID a written typed soulprint (`souls.soulprint_facts IS NOT NULL`, batch check `soul.SelectSoulsWithSoulprint` through the module's extended `Store`). A SID is **"ready" = online (lease) AND facts written**; `await_min_count` is counted over the ready ones. The same `await_poll_interval`, the same `await_timeout`.

- **Timeout diagnostics (B1-strict)** distinguishes shortfall classes: `not online: [sids]` (no lease — onboarding did not happen) and `online but factless: [sids]` (there is a lease, but the first report is not yet written — a race / loss of the initial-send). Without this separation an infra race is indistinguishable from a failed onboarding.
- **Idempotency.** `create_from_souls` / rerun: facts are already in PG → the barrier passes on the very first poll (zero wait). Only provision-from-zero waits for the first soulprint — normally milliseconds-to-seconds (the initial-send on connect is present).
- **Back-compat.** `refresh_soulprint: false` / omitted → the previous presence-only barrier, facts are not polled. `refresh_soulprint` without `await_online` — there is no barrier at all (the Stratify emitter + re-resolve, §S2/§S3, do not change).
- **For reference [ADR-018](0018-soulprint-typed.md).** The initial report on connect remains best-effort — it is not guaranteed by the moment of the lease; the race is compensated by the keeper-side facts-wait, ADR-018 does not change.

**Rejected/deferred (Variant 2, backlog).** An active soulprint request (`SoulprintRequest` in the `FromKeeper` oneof) — a proto-contract change for a case that is closed by passively waiting for the write; return to it if the initial-send shows systematic losses.

**Context.** The target scenario — **a single create-scenario** deploys an N-shard cluster from "nothing":

1. `core.cloud.provisioned` (`on: keeper`, [ADR-017](0017-keeper-side-core.md)) creates N VMs, the register-output carries their `sid` / bootstrap tokens / cloud metadata;
2. the created VMs onboard (cloud-init + CSR-bootstrap, [ADR-012](0012-keeper-soul-grpc.md)), their `soul` agents bring up an EventStream to the Keeper;
3. subsequent scenario tasks apply the redis role to the **already onboarded** hosts — `on: [incarnation.name]`, `where: …`, reading `soulprint.hosts`.

This scenario **does not work** on the current engine for three related reasons:

- **`soulprint.hosts` is a snapshot at the start of the run.** The incarnation roster is resolved once before the first Passage and does not grow within the run. The VMs created by step (1) do NOT make it into the roster of step (3) — `soulprint.hosts` does not see them, `on: [incarnation.name]` does not target them.
- **`refresh_soulprint` is a stub.** The flag on `core.soul.registered` is accepted but ignored (`register.<name>.refreshed` is always `false`, [ADR-017](0017-keeper-side-core.md), `keeper/internal/coremod/soul/registered.go`). There is no way to tell the engine "re-read the roster".
- **There is no onboarding barrier.** Between "the VM is created" and "its `soul` agent is online" time passes (boot + cloud-init + bootstrap). The scenario cannot wait until the created hosts become online before applying the role to them — there is no blocking barrier step.

**Solution.** Two capabilities on the existing keeper-side core module `core.soul.registered` (NOT a new module — a user decision: consolidation on `registered`, next to the souls+coven write, is natural; a separate `core.soul.online` / barrier module was rejected as an extra entity).

## Capability 1 — onboarding-await (onboarding barrier, S1)

New **optional** input fields of `core.soul.registered`:

| Field | Type | Req. | Semantics |
|---|---|---|---|
| `await_online` | bool | — | `true` — after writing souls+coven the step BLOCKS waiting for onboarding. Default `false` (the pre-ADR behavior does not change). |
| `await_timeout` | duration | **yes when `await_online: true`** | Upper bound on the wait. Without it, with `await_online: true`, a validation error (the barrier must not hang forever). |
| `await_min_count` | int | — | Minimum online hosts for success. Default — **the number of registered SIDs** (all must come up). `0 < await_min_count ≤ len(sids)`. |
| `await_poll_interval` | duration | — | Presence polling period. Default ~2s (parity with `acolyte_poll_interval`). |

**Barrier semantics.**

1. The step first performs the usual registration (souls+coven, as before the ADR) for **all** registered SIDs.
2. Then, if `await_online: true`, it **blockingly polls presence** with a period of `await_poll_interval` under `context.WithTimeout(await_timeout)`, until the number of online hosts among the registered SIDs reaches `await_min_count`.
3. **The source of truth for "online" is the Redis SID-lease** (`soul:<sid>:lock` EXISTS, [ADR-006(a)](0006-cache-redis.md), `keeper/internal/redis/SoulsStreamAlive`), **NOT** PG `souls.status`. The PG status is a lifecycle snapshot, it lags; a live EventStream lease is the authoritative sign that the agent is actually connected (the same source as the presence filter of the target resolver and the lease-aware Reaper).
4. **B1-strict (failure semantics, user decision).** If by `await_timeout` online `< await_min_count` — the step finishes `failed` → fail-stop of the run → `incarnation.state` is **not committed** → `incarnation.status: error_locked`. A partially onboarded set of Souls does not "leak" into role application: either a quorum is reached, or an explicit fail with diagnostics.
5. **output `register.<name>`** is extended with barrier fields: `online: []string` (the SIDs online at the moment of success/timeout), `pending: []string` (those that did not make it), `satisfied: bool` (whether `await_min_count` was reached). On success `satisfied: true`; on failed — a `failed` event + the same fields for diagnostics.

**`await_timeout` ceiling (DoS-guard, fail-closed).** A new field `keeper.yml::max_await_timeout` (duration). If a step sets `await_timeout` > the ceiling — the step is `failed` (rather than "silently trimmed to the ceiling": an explicit error is better than hidden behavior). The default ceiling is [`DefaultMaxAwaitTimeout`](../../shared/config/keeper.go) (30m). The barrier must not hang forever — this rule protects the cluster from a scenario-DoS (a malicious/erroneous `await_timeout: 100h` would keep the run-goroutine/Acolyte worker busy).

## Capability 2 — mid-run re-resolve roster (S3)

Bringing the flag **`refresh_soulprint: true`** on `core.soul.registered` to life (currently a stub, see Context).

**Semantics.** After the **success** of the `core.soul.registered` step with `refresh_soulprint: true`, the scenario-runner **re-resolves the incarnation roster before the NEXT Passage**. The onboarded hosts become visible to subsequent steps: `soulprint.hosts`, `on: [incarnation.name]`, `soulprint.self.*` already include the current online set.

**Re-resolve = live-snapshot (not monotonic growth).** The re-resolve at the refresh boundary is a **fresh live snapshot of the incarnation's current online set** (`topology.LoadIncarnationHosts → filterAlive`), NOT a union with the old roster. This is the correct semantics for the target scenario: the role rolls out onto the actually-online hosts.

- The set **grows** as the provisioned hosts onboard (the created VMs brought up an EventStream → visible).
- The set can also **shrink**: a P0-roster host that went offline by the refresh boundary (the lease dropped / `status≠connected`) is **excluded** from the live snapshot — there is no need to roll the role out onto an offline host. This is not "removing a host from the plan", but a reflection of the fact: targeting goes to the current online set, just like a normal up-front roster.

**Weakening of the roster stability invariant (amends [ADR-009 §7](0009-scenario-dsl.md)).** The previous invariant — "the run roster is stable for the entire run". The new one — **"the roster is stable within a single Passage; at the refresh boundary it is re-resolved (a live snapshot)"**. Between Passages the roster is re-resolved **if** the finished Passage contained a successful `refresh_soulprint: true` step.

**The barrier/state-commit invariant §7 is NOT weakened.** `incarnation.state` is still committed **once** after the last Passage. Re-resolve is the **roster** axis (whom to target), not the commit axis.

**Re-resolve implementation — S3 (run.go).** Implemented: the stage-loop of run.go at the refresh boundary (`RefreshBoundaries`) calls `resolveRoster` (a live snapshot) and passes the result into the repeated Render of the next Passage; `register.<name>.refreshed` echoes the flag value. A re-resolve failure → abort (not silently on the old roster).

## Stratify — `refresh_soulprint` makes the step passage-defining (S2)

For the re-resolve to take effect, consumers of the updated roster must end up in the **next** Passage relative to the `refresh_soulprint` step. Otherwise — silent-wrong-target ([ADR-056](0056-staged-render-passage.md): the render before dispatch would see the old roster).

**Contract (amends [ADR-056](0056-staged-render-passage.md)).** A **NEW CLASS of passage-defining edge — "roster-refresh"** is introduced, a SEPARATE axis from register dependency and program-order: a `core.soul.registered` task with `refresh_soulprint: true` is a passage-defining emitter of the "roster-refreshed" signal. The stratifier ([ADR-056(b)](0056-staged-render-passage.md)) places any roster consumer (`soulprint.hosts` / `soulprint.where(...)` / `on: [incarnation.name]` / an omitted `on:` / `soulprint.self.*`) into a Passage **strictly AFTER** the `refresh_soulprint` step. The source of this dependency is the static sign `refresh_soulprint: true` on the task (like `register: X` — for a probe), but this is **not the register graph**: the refresh boundary does NOT introduce register references, so the reads⊆refs invariant ([ADR-056](0056-staged-render-passage.md)) is NOT affected (the roster axis is orthogonal to the register axis). The semantics of re-resolve at this boundary is a **live-snapshot** (§S3, not "monotonic growth"). **Implementation — S2** (`shared/config/passage_refresh.go`, the edge is wired into `Stratify`).

## list-SID — registering + awaiting N hosts in a single step (S1)

`core.cloud.provisioned` returns a **list** of created hosts (`register.provision.hosts`). To register and await them in a single barrier step, `core.soul.registered` accepts a **list of SIDs**:

- `params.sid` accepts a **string OR a list of strings** (runtime, `util.StringOrSliceParam`). A list is more natural than a single `loop:` — the `await_online` barrier aggregates presence **across all** SIDs (a common `await_min_count` over the set is needed, not independent per-iteration barriers).
- `params.coven` is applied to **all** SIDs of the list (the common set of the step's Coven labels).
- output `register.<name>`: with the list form of `sid` — an array; with the single form — a string (the historical form is preserved). `online`/`pending` are lists of SIDs; `created`/`removed` reflect the aggregate side-effect.
- A single-string `sid` remains valid (backward compatibility) — internally normalized to a single-element list.

**Manifest-DSL trade-off (uncovered by consolidation, a conscious decision).** The trimmed manifest-input DSL ([`shared/coremanifest`](../../shared/coremanifest/)) **does not express the union `string|list`**, and changing the declared type of `sid` to `list` would break backward compatibility of the single-string author form (`sid: host.example.com`). Therefore `sid` is declared **`type: string`**: a single literal string passes soul-lint as before; **a list arrives as a CEL expression** `${ register.<step>.hosts }` (the main real path — a SID list from `core.cloud.provisioned`), which soul-lint lets past the type-check regardless of the declared type ([ADR-010](0010-templating.md): a `${…}` value is not statically typed). **A literal list `sid: [a, b]`** accordingly **does not pass soul-lint's static type-check** — acceptable: in practice a SID list is always from `register.*` (CEL), and the runtime (`StringOrSliceParam`) correctly accepts both forms. Introducing a union type into the manifest-DSL is a separate ADR (a public-contract change), if a literal SID list is ever needed statically.

## HA — provision scenarios via Voyage

A single-binary provision run with a long `await_online` barrier is vulnerable to an instance crash: the blocking poll holds the run-goroutine. **The recommendation is to run provision→onboarding→role scenarios via Voyage** ([ADR-043](0043-voyage.md)), where recovery is closed ([ADR-027(l)](0027-apply-work-queue.md): an orphaned claim is re-claimed by another worker). Standalone (run-goroutine) staged-recovery for a long barrier is **open** ([ADR-056 §S4](0056-staged-render-passage.md)): if the single-keeper run-goroutine crashes during the `await_online` poll, the run will be orphaned in `applying` and will require a manual unlock (like any standalone run before the Acolyte cutover). Noted in the DoD.

## Contract summary

- `core.soul.registered` input is extended: `await_online` (bool), `await_timeout` (duration, required-when await_online), `await_min_count` (int, opt), `await_poll_interval` (duration, opt), `sid` (string **or** list). `refresh_soulprint` (bool) — brought to life.
- `keeper.yml::max_await_timeout` (duration, default 30m) — the barrier ceiling.
- output `register.<name>` is extended with `online[]` / `pending[]` / `satisfied`.
- the barrier's presence source is the Redis SID-lease, not PG.
- with `refresh_soulprint: true` the barrier waits for presence **AND** the first typed soulprint (`souls.soulprint_facts IS NOT NULL`); the timeout diagnostics distinguishes `not online` / `online but factless` (amendment 2026-07-02).

## Rejected alternatives

- **A separate module `core.soul.online` / a separate barrier step.** Rejected (user decision): the barrier logically adjoins registration (registered the created SIDs → awaited their onboarding — one operation); a separate entity is an extra module in the registry and in naming-rules with no gain.
- **Presence via PG `souls.status`.** Rejected: the status lags (a lifecycle snapshot), it is not the real online; the lease is a constructively authoritative sign of a live EventStream ([ADR-006(a)](0006-cache-redis.md)). A PG-based barrier would falsely "see" a host as online before the actual stream.
- **B0/B2 failure semantics** (best-effort continuation on a partial quorum / warn-without-fail). Rejected in favor of **B1-strict** (user decision): a partially brought-up cluster must not silently receive the role on an incomplete set — better `error_locked` with explicit `pending[]` diagnostics.
- **Silent trimming of `await_timeout` to the ceiling.** Rejected: an explicit `failed` error is better than a hidden change to the declared behavior.
- **Monotonic growth `roster ∪ newly_online` (add-only).** Considered but rejected in favor of the **live-snapshot**: re-resolve reads the incarnation's fresh online set. This is the correct semantics for the target scenario — the role rolls out onto the actually-online hosts; a host that went offline by the refresh boundary is excluded (there is no need to roll the role out onto a downed host). A union with the old roster would drag an offline host into targeting. Determinism is preserved: the roster is stable WITHIN a Passage, re-resolve only at the boundaries.

## Amends

- **[ADR-009 §7](0009-scenario-dsl.md)** — the roster stability invariant is weakened: "stable within a Passage; re-resolved at the refresh boundary (a live snapshot)" (not "stable for the entire run"). The barrier/state-commit invariant §7 is NOT affected.
- **[ADR-056](0056-staged-render-passage.md)** — a new class of passage-defining edge "roster-refresh" is introduced (a separate axis from register/program-order): `refresh_soulprint: true` makes a task a passage-defining emitter; roster consumers are stratified into the next Passage. The refresh boundary does not introduce register references → reads⊆refs is not affected.
- **[ADR-006](0006-cache-redis.md)** — the Redis SID-lease gains an additional consumer: the source of truth for the `await_online` onboarding barrier.
- **[ADR-017](0017-keeper-side-core.md)** — `core.soul.registered` is extended with the onboarding barrier + `refresh_soulprint` is brought to life; a new config ceiling `keeper.yml::max_await_timeout`.

## DoD

- S1 (this slice): `await_online` blocks on the Redis-lease until `await_min_count`/timeout; B1-strict fail; list-SID; the `max_await_timeout` ceiling; guard tests (waits→online; B1 timeout→failed; quorum→ok; source=lease not PG; ceiling).
- S2: the stratifier places roster consumers after the `refresh_soulprint` step (the new edge class "roster-refresh").
- S3: the scenario-runner re-resolves the roster at the refresh boundary (a live-snapshot of the current online set); `register.<name>.refreshed` echoes the flag.
- no_hosts-bypass (amendment 2026-06-28): two classes of provision-from-zero execute on an empty roster — (a) all-keeper (`allKeeperTasks`), (b) mixed with a refresh emitter (`config.HasRefreshEmitter`, the roster is re-resolved mid-run §S2/§S3); host-only and mixed-WITHOUT-refresh keep no_hosts; Essence keeper context on an empty roster; guard tests (keeper-only→executes / host-only→no_hosts / mixed+refresh→reaches dispatch / mixed-without-refresh→no_hosts / unit `allKeeperTasks` + unit `HasRefreshEmitter`).
- size-asserts gate (amendment 2026-06-29): the redis deploy-branch size-asserts (`create/cluster.yml`/`create/sentinel.yml`/`migrate_cluster/cluster.yml`) are gated by `when: "!(has(input.provision) && input.provision.enabled)"` (STATIC, pre-flight placeholder-skip on provision); the roster invariant on the provision path is guaranteed by the `count` formula of `redis-provision.yml` + the `await_online` barrier, NOT by pre-flight; guard tests (provision.enabled+empty roster→skip not fail / without provision+mismatch→`ErrAssertFailed`); pre-flight remains non-staged (the service-DSL is changed, not `EvalAsserts`).
- barrier facts-wait (amendment 2026-07-02): with `refresh_soulprint: true` the barrier counts only SIDs with a written typed soulprint; guard tests (factless online → waits, after facts → passes / facts already in PG → instant pass on the rerun path / timeout factless → failed "online but factless" / both classes in diagnostics / back-compat `refresh_soulprint: false` → presence-only, facts not polled / persistent facts-read failure → failed).
- Open risk: standalone staged-recovery of a long barrier ([ADR-056 §S4](0056-staged-render-passage.md)) — provision scenarios are recommended to be run via Voyage.
