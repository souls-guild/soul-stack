# Known limitations - what is not included in beta

The closed small beta is designed for a few operators and a fleet of up to hundreds of hosts. This document honestly lists what is **not** in the beta or what works with limitations - so that the beta tester does not silently rest on this and mistake the absence of a feature for a bug.

Each item is with a link to the canon (ADR / runbook), which describes "how it will be" or "why it's postponed." The design is captured in ADR before the code; "postponed" means "the decision has been made, the code is not in beta."

## Cloud-provisioning - NOT in beta

Beta works with **existing hosts**: the operator himself picks up the VM/hardware and onboards Soul ([getting-started.md → Step 6](getting-started.md#step-6-onboard-one-soul)). Dynamic creation of VMs from Soul Stack is not included in the beta.

Since NIM-761 there is no engine-side cloud step at all: the `CloudDriver` contract, the
`core.cloud` module and the Provider / Profile registries were removed. A VM is created by a
`side: keeper` SoulModule plugin carrying its own credentials and parameters — the mechanism
works (proved end to end against a real cloud on 2026-09-04), but no such plugin ships in this
repository, so out of the box Soul Stack still creates nothing.

If you want dynamic provisioning, this is post-beta. Now: Create a host outside of Soul Stack, then `POST /v1/souls` + `soul init`.

## Keeper-side plugins: the executor is live, nothing ships as one yet

**Closed in NIM-758.** The Keeper routes by the declaration: a keeper-side task address the built-in `coremod.Registry` does not know is looked up among the discovered plugins, and one whose schema document says `side: keeper` runs in the Keeper's own process through the same gRPC-over-stdio infrastructure the Soul host uses. A plugin that did NOT declare the keeper side is refused by name (`declares side=soul and does not execute on the keeper`) rather than being answered "unknown module" or quietly sent to a host; an address nothing declares still fails **`unknown keeper-side module`**, unchanged.

What remains is that nothing in the tree *is* such a plugin yet. This was the **precondition** for epic NIM-757: the separate CloudDriver contract is removed and a cloud driver becomes an ordinary SoulModule plugin declaring `side: keeper`. With the executor in place, the order is **NIM-760** (the cloud driver moves and is verified live) → **NIM-761** (removal) — never removal first, since a SoulModule runs on a host and a VM is created when no hosts exist yet ([ADR-017 amendment 2026-09-01](adr/0017-keeper-side-core.md#amendment-2026-09-01-nim-757-the-clouddriver-contract-is-removed--a-cloud-driver-is-an-ordinary-plugin)).

⚠ **A plugin on the Keeper is the most privileged execution the platform has** — a foreign binary in the Keeper's process tree rather than on a host — and it stands at the door its neighbours already stood at, plus one bolt: the artifact must be in `keeper.yml::plugins.soul_modules`, its capabilities must pass `allowed_capabilities`, its sha256 must match an active Sigil grant, and its module must declare `side: keeper` — a declaration the Sigil seal signs together with the binary ([ADR-026(c)](adr/0026-sigil.md)), so it cannot be flipped without breaking the signature. Nothing confines the process once it starts; that bound is the same one [ADR-020](adr/0020-plugin-infrastructure.md) states for every kind.

⚠ **Where those bytes come from gains a second answer — and for a `side: keeper` plugin the answer is OPEN.** The [2026-09-04 amendment](adr/0065-core-module-installed.md#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) (NIM-794; the **Soul half is implemented** as NIM-796, the keeper half is not — NIM-795) decides that a plugin arrives on a **Soul host** from its artifact source (`source_kind: artifact`), with `FetchModule` retained for hosts without egress. Every bolt listed above is unchanged by it: the catalog entry, `allowed_capabilities`, the sha256 against an active grant, and `side: keeper` signed into the Sigil seal are all read the same way regardless of who served the bytes — which is the point, since the gate is the digest and never rested on the network path.

**The open question, stated rather than answered: does the *Keeper* also pull from the source for a `side: keeper` plugin, or is source-pull the Soul path only?** The amendment settles the Soul side and does not address this one, and the two readings are not equivalent here. Today the Keeper resolves the artifact into its own cache and executes from it; source-pull for a keeper-side plugin would mean the **Keeper process** making the egress call for bytes it is about to run in its own process tree — the most privileged execution the platform has, so the answer is not a detail to pick while implementing. Recorded as open for **NIM-795**; do not read either answer out of the amendment.

### `side: keeper` is enforced by the Keeper only, not by the Soul

The gate is **one-directional**. The Keeper refuses to execute a module that has not declared `side: keeper`; the Soul host does not read the field at all — its plugin registry indexes every `soul_module` in the slot regardless of side. So an artifact declaring `side: keeper`, once distributed to a host, is executable there: a task written as `module: <alias>.<module>.<state>` **without** `on: keeper` routes Soul-side (a scenario cannot derive a plugin's side — [ADR-0087](adr/0087-task-side-derived-from-module-address.md)(f)) and the keeper-side binary runs on every targeted host.

**Latent, not live:** no artifact in the tree declares `keeper` yet, and the first one to do so arrives with NIM-760. Closing it means the symmetric refusal in `soul/internal/runtime`'s plugin registry, which is a Soul-side behaviour change and its own ticket.

## Source-pull is half-wired: a multi-platform grant installs on exactly ONE platform

The Soul half of source-pull ships (**NIM-796**): a `core.module.installed` step whose grant names an artifact source pulls the bytes from it, selects the row for its own platform, and reports `fetch_via` / `fetch_url` on the final event. The **grant's wire form does not** — that is **NIM-795** ([ADR-026](adr/0026-sigil.md#amendment-2026-09-04-nim-794-the-grant-carries-a-list-of-artifacts-and-the-bytes-stop-travelling-through-the-keeper)), and neither does the catalog entry that would fill it ([ADR-020](adr/0020-plugin-infrastructure.md#amendment-2026-09-04-nim-794-the-catalog-entry-gains-a-source-kind-and-an-explicit-artifact-list)).

**What that means in practice.** `SigilRecord` carries `BaseURL` and `Artifacts`, and **nothing populates them from the proto**; the signed block still covers one scalar digest. So a grant carrying several platform rows would install **only on the platform whose row digest equals `BinarySHA256hex`**. On every other platform the host fetches the right-looking bytes and then **fails closed at verify**, with a digest mismatch.

**It fails in the safe direction, and the diagnostic is misleading.** Nothing unverified is installed — verify runs before materialization, which is why the fetch comes last. But the operator sees a **verification failure on a correct artifact**, which reads as tampering rather than as a missing feature. Until NIM-795 lands, a grant is effectively single-platform.

**Not affected:** a grant with no artifact rows, which is every grant that exists today. It takes `FetchModule` exactly as before, and `FetchModule` is not deprecated — it is the path for hosts with no egress ([ADR-065](adr/0065-core-module-installed.md#fetchmodule-remains-a-second-legitimate-mode)).

**Two smaller boundaries of the same change**, both deliberate and both recorded in [ADR-065's amendment](adr/0065-core-module-installed.md#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path):

- **The artifact size ceiling on the source path is a constant, not configurable.** It reuses Keeper's *default* (`plugins.max_artifact_size_mb`'s default), so a cluster that **raised** its own ceiling has artifacts the source path refuses and `FetchModule` would serve. `soul.yml` has no knob; adding one is the fix if it ever bites.
- **Source authentication does not exist.** v1 reads an **anonymously readable** repository, and a `base_url` carrying credentials is refused rather than used — deferred with its reason in [ADR-026](adr/0026-sigil.md#authentication-to-the-source-is-deliberately-deferred).

## MCP does not cover all domains

The primary operator interface is REST (OpenAPI) and MCP. But MCP symmetry with REST is incomplete - some domains are accessible **only through REST/UI**:

- **Cadence** (schedules of regular runs, [ADR-046](adr/0046-cadence.md)) - **MCP tools are not available** ([operator-api/cadences.md](keeper/operator-api/cadences.md)). Creating/changing schedules - only via REST `/v1/cadences*` or Web-UI.
- **Audit-read** (`GET /v1/audit`) - MCP symmetry deferred ([operator-api/audit.md](keeper/operator-api/audit.md)).
- **Choir** (host topology inside incarnation) and **Module-catalog** (`/v1/modules`) are REST-only by design ([operator-api.md → Choir / Module-catalog](keeper/operator-api.md)).

Some MCP-tools are set up as **stub**: they are marked `status=stub` in the manifest and return an honest `not_implemented` (they don't crash, they don't pretend to have worked). In beta stub:

- **`keeper.soul.list`** — `not_implemented`. For the Souls list in beta, use REST `GET /v1/souls` (filters `coven` / `status` / `transport` + pagination). Full MCP coverage - post-beta (M2).
- **`keeper.push.cleanup`** — `not_implemented`. There is no REST analogue in beta (push is performed via `keeper.push.apply`). Post-beta delayed.

If you automate via MCP, check [keeper/mcp-tools.md](keeper/mcp-tools.md): the actually installed MCP tools are listed there. The absence of a tool or its `stub` status is not a bug, but a beta coverage limit.

## Audit-scaling - designed for small beta

`audit_log` is the main consumer of Postgres volume when the fleet grows: at the target scale of 100k VM, the volume of runs depends on the INSERT-rate and the table size. For **small beta** (up to hundreds of hosts) this is not a problem, standard retention is enough (`purge_audit_old`, default 365 days, [operations/infra.md → Retention](operations/infra.md)).

What is postponed until post-beta (not needed for a small fleet):

- **Partitioning `audit_log`** by `created_at` (declarative partitioning / BRIN) - extension, not breaking ([ADR-022](adr/0022-audit-pipeline.md)).
- **Pluggable audit-sink (Kafka upload)** - **designed, not implemented in beta** ([ADR-059](adr/0059-audit-sink-pluggable.md), proposed / deferred). At the target scale, backend audit upload becomes selectable (`audit.sink: pg | kafka | off`, default `pg`); Kafka-sink (at-least-once `acks=all`, fail-closed, downstream deduced by `audit_id`) removes the PG-write load, remains strictly optional (the mandatory PG+Redis+Vault loop does not change, [ADR-053](adr/0053-dependency-tiers.md)). **Replaces the Redis-Stream-buffering option** (Kafka covers the same write-throughput axis more fully; Redis is a hot layer, not a long-term audit buffer). Requires dependency decoupling before implementation: `changed_tasks`/`GET /v1/audit` today derives data from `audit_log` into PG ([ADR-059](adr/0059-audit-sink-pluggable.md) open question).
- **Hot-cold / batched-INSERT** audit for large fleets - backlog of the next releases. **batched-INSERT remains** a cheaper alternative on the write-throughput axis (batch-flush PG-sink without new infrastructure), and is not supplanted by Kafka-sink.

If you are planning a fleet of thousands+ hosts, this is outside the beta profile; keep an eye on the size of `audit_log` and `apply_runs` ([operations/infra.md → Table size](operations/infra.md)).

## Other beta boundaries

### Supply-chain: image signing delayed

`make sign` - documented stub (prints the reason and succeeds). The real cosign/sigstore signing of Docker images requires registry + OIDC-identity from CI, which the local repository does not have ([deploy/README.md → Signing images](../deploy/README.md)). SBOM (`make sbom`) works.

### External pentest - not carried out (internal gate is sufficient for beta)

Independent external pentest was **not running** at the time of beta. The limit of guarantees rests on the internal security gate: deep information security audit 2026-06-12 (0 critical/high), threat-model, clean `govulncheck` for all modules and security-revalidation of the OpenAPI pivot (PASS) - composition and justification in [security/threat-model.md → External audit status / pentest](security/threat-model.md). Decision from 2026-06-15: for public beta this is enough; external independent pentest planned post-beta/pre-GA.

### Operator Identity: JWT only

The Archon credential form in beta is **JWT** (HS256, signing-key from Vault). mTLS-cert form and transit signature JWT - post-MVP, extension via `auth_method` enum without breaking changes ([ADR-014](adr/0014-operator-identity.md), [operations/bootstrap-rbac.md → Machine-identity](operations/bootstrap-rbac.md#machine-identity-ci--scripts)).

**Revoking an Archon takes effect immediately** (2026-08-07, NIM-421): its live tokens are refused with `401 operator-revoked-token` on every authenticated route from the next request. What is still missing is revocation of an **individual token** - there is no blocklist, so a token that leaked without the Archon being revoked stays valid until `exp`, and the answer to that case remains signing-key rotation ([operations/bootstrap-rbac.md → Emergency revocation](operations/bootstrap-rbac.md)) plus a short `ttl_default`. On the **MCP** surface a revoked operator is likewise refused every tool, but the refusal is still rendered as `forbidden` rather than a distinct revoked code, and two paths are not closed at all: `initialize` / `tools/list` (handshake and catalog enumeration, no data) and the SSE event stream for an apply the operator started herself, which stays readable until the stream drops (NIM-551).

### Push (agentless via SSH) - narrow profile

`keeper.push` (Destiny delivery over SSH without agent) works, but without host-CA / `ssh_providers` is a no-op ([ADR-053 → optional-with-degradation](adr/0053-dependency-tiers.md)). Beta profile - pull (daemon agent `soul`), use push only when SSH provider is configured.

### Served OpenAPI describes the full surface of the product

Some of the handles relate to optional domains, which are mounted only when the corresponding feature is enabled in the Keeper config (for example, push/SSH delivery - with `plugins.ssh_providers` + `push.host_ca_ref` configured; Sigil/sigil-keys - with allow-list plugins enabled). If the feature is not enabled on a specific instance, the handle will return `404 "no such endpoint"` - including when trying to call it from /docs (RapiDoc "Try It"). This is expected: spec = stable contract for the entire product, handle availability depends on the deployment configuration (pull-only installation without push/Sigil - standard mode). The authoritative list of optional/feature-gated domains is `pathAllowlist` in `keeper/internal/api/openapi_drift_test.go` (protected by the `TestFullSpec_CoversAllRoutes` guard test).

### Recovery of interrupted runs - disabled by default

`reclaim_apply_runs` (Reaper picks up runs stuck after a Keeper instance crash) **disabled** in the default config - enabled only after rolling out fencing-Soul + `acolytes>0` ([operations/deployment.md → keeper.yml](operations/deployment.md), [keeper/reaper.md](keeper/reaper.md)). For a small single-keeper beta this is not required; The operator restarts a frozen run manually.

### UI `/oracle/fires` - stub

The `/oracle/fires` page in the Web-UI is a placeholder with an explicit WIP message: backend `GET /v1/oracle/fires` is not implemented (the table `oracle_fires` already exists, the query phase is postponed). Viewing Decree triggers in beta - via **Audit Log** with filter `type=decree.fired`. Post-beta (Oracle query-phase) is delayed.

### Sentinel-Redis: without native master-discovery

`redis.addr` accepts one TCP address. Redis Sentinel with automatic master-discovery is not natively supported - rolling out via a TCP proxy to a dynamic master ([operations/infra.md → HA Redis](operations/infra.md#ha-redis)). Single-instance and Redis Cluster are supported. For small beta - single-instance + AOF.

### Semantic-breaking: `create_scenario` (empty NO MORE does not mean auto-`create`)

Semantics of the field `create_scenario` (`POST /v1/incarnations`, MCP `keeper.incarnation.create`) **changed**: empty `create_scenario` **NO LONGER means** auto-run reserved script `create` ([ADR-009 amendment 2026-06-29](adr/0009-scenario-dsl.md)). Now with an empty value: if the service offers create scripts (at least one `create: true`) → **422 `create_scenario_required`** (selection is required, valid names are listed); if none `create: true` → **bare-incarnation** (`StatusReady` without run, `created_scenario = NULL`). Wire circuit is additive (the field existed before), breaking only the meaning. **Affected** are external API/MCP clients that used `create` without `create_scenario` when calculating auto-`create` - they need to explicitly pass `create_scenario`.

## See also

- [getting-started.md](getting-started.md) - quickstart, onboarding path for an existing host.
- [operations/](operations/README.md) - prod-runbook (deployment / infra / scaling / disaster-recovery).
- [architecture.md → Open questions](architecture.md#open-questions) - forks not yet closed by ADR.
