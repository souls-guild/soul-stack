# Prod-readiness — GA-gap roadmap

Which is **not ready** for production/GA. Beta `v0.1.0-beta.1` released and feature-complete; This document is a list of gaps between beta and GA, based on the results of the GA-readiness audit code (2026-06-17).

Double source of truth along the boundaries:

- [known-limitations.md](known-limitations.md) - what is NOT included in **beta** (limitation from the beta user side).
- **this file** - what needs to be closed before **GA** (border on the food-readiness side).

> **Not to be confused with [roadmap.md](roadmap.md).** `roadmap.md` drifts relative to the actual code and is not a source of truth along GA boundaries - it will be updated separately. If there is a discrepancy, trust known-limitations.md + this file (both are checked with the code).

## GA-scope solutions (committed 2026-06-17)

These three decisions set the framework for what goes into GA and what remains post-GA. They re-prioritize the items below.

| Solution | Meaning | Corollary for roadmap |
|---|---|---|
| **Cluster topology** | Elastic (autoscaling) | Balancing scale-out (**Shepherd**) is raised in P0: without it, autoscale is meaningless (the new instance is idle). |
| **Cloud-provisioning** | **Post-GA** (not in GA-scope) | Cloud-CRUD remains an explicit known-limitation, not a GA blocker. |
| **Target Fleet** | **Thousands** of hosts | Audit partitioning and ready-made monitoring - P1 (thousands → tangible PG INSERT-rate); full 100k-ramp - P2. |

---

## P0 - GA prod-blockers

GA cannot be released without closing these seven points.

### 1. e2e-live (L3b): prove with green + do blocking

Nightly-job `e2e-live` runs a **real `soul` binary in a privileged container** (full apply/Scry-pipeline), but costs with `continue-on-error: true` ([.github/workflows/nightly.yml](../.github/workflows/nightly.yml)). A real apply can be broken unnoticed - job does not block. You need to: achieve a stable green run and remove `continue-on-error`, making it a blocking gate.

### 2. Clean-room getting-started (DoD-1)

A living person builds a cluster "from scratch" on a **clean machine** strictly according to [getting-started.md](getting-started.md) / [operations/deb-onboarding.md](operations/deb-onboarding.md), without any hints from the author's head. The goal is to catch hidden preconditions and gaps in the onboarding doc. Estimate: ~1 person-day.

### 3. Release-distribution (supply-chain)

Release now - **manual `make`-targets**; there is no automated `release.yml` in `.github/workflows/` (there is only `ci.yml` / `nightly.yml`).

- registry images are not published;
- `make sign` - **stub** (prints the reason, `exit 0` - [Makefile](../Makefile), [known-limitations.md → Supply-chain](known-limitations.md#supply-chain-image-signing-delayed)): there is no real cosign/sigstore signature.

We need a reproducible release pipeline: assembly of artifacts → publication of images in the registry → real cosign signature.

### 4. Shepherd - load balancing with scale-out

Not implemented (**0 code**, grep on `shepherd` is empty; described as PLANNED in [operations/scaling.md → Shepherd](operations/scaling.md)). With an **elastic cluster** (GA-scope) this is P0: the new instance after scale-out is idle until the natural churn of streams - autoscale without active rebalance is meaningless.

### 5. recovery-lease (`reclaim_apply_runs`) live under instance crash

Rule `reclaim_apply_runs` (Reaper picks up runs stuck after a Keeper instance crash) is implemented ([keeper/internal/reaper/](../keeper/internal/reaper/voyage_reclaim.go)), but **disabled-by-default**, and its behavior under a real instance crash **has not been proven live** (there is a runbook - [operations/recovery-reclaim-apply-runs.md](operations/recovery-reclaim-apply-runs.md), [known-limitations.md → Recovery](known-limitations.md#recovery-of-interrupted-runs---disabled-by-default)). On a multi-keeper GA cluster, the rule **must be ON**: otherwise, if the instance crashes, runs hang in `applying`. Needed: live validation after killing an instance + switching to ON for multi-keeper.

### 6. External pentest + identity spaces

- Independent **external pentest** was not performed ([known-limitations.md → pentest](known-limitations.md#external-pentest---not-carried-out-internal-gate-is-sufficient-for-beta)); for GA - required.
- **No immediate JWT revocation** before `exp`: after `revoke` Archon its tokens live until expiration, emergency revocation - only by signing-key rotation ([known-limitations.md → Identity](known-limitations.md#operator-identity-jwt-only)).
- **mTLS-cert identity operator** - post-MVP ([ADR-014](adr/0014-operator-identity.md)); for GA close either immediate revoke or machine-identity.

### 7. Remove `continue-on-error: true` from three check classes in CI

Now informational (merge is not blocked) - structural risk, quiet regression will not stop PR:

| Job | File | What doesn't block |
|---|---|---|
| `integration` (testcontainers) | [ci.yml](../.github/workflows/ci.yml) | Integration tests |
| `govulncheck` | [ci.yml](../.github/workflows/ci.yml) | Go Module Vulnerability Scanner |
| `e2e-live` | [nightly.yml](../.github/workflows/nightly.yml) | Real apply/Scry (see P0-1) |

For GA, these three classes must be blocking (with preliminary stabilization by flaky - see P1).

---

## P1 — hardening (before GA, after P0)

- **Audit partitioning** `audit_log` by `created_at` (declarative partitioning / BRIN - [ADR-022](adr/0022-audit-pipeline.md)). Fleet of "thousands" → noticeable PG INSERT-rate (load showed ≈2 INSERT/host on Voyage - [load-testing.md §8.3](testing/load-testing.md)).
- **Ready-made Grafana dashboards + Prometheus alerts** - not available in the repo; metrics/OTel are published ([observability.md](observability.md)), but there is no out-of-box observability.
- **MCP-completeness** - Cadence and Audit-read without MCP tools; `keeper.soul.list` / `keeper.push.cleanup` - `not_implemented`-stub ([known-limitations.md → MCP](known-limitations.md#mcp-does-not-cover-all-domains)).
- **Multi-keeper load-run shard buffer `applybus`** - fix maxclients-cliff (sharded channels) is not checked for cross-keeper paths under load.
- **Stabilization of flaky integration tests** (≈6 in `keeper/internal/api`, order-dependent / auth-race) - not currently quarantined; needed before `integration` becomes blocking (P0-7).
- **Voyage presence-resolve → Redis-lease** instead of PG `souls.status` (hot → Redis invariant, non-synchronous PG-write on hot path).
- **Push: Teleport `proxy_jump`** - not completed (narrow push profile - [known-limitations.md → Push](known-limitations.md#push-agentless-via-ssh---narrow-profile)).
- **SoulBeacon live-loop e2e** + UI `/oracle/fires` - backend `GET /v1/oracle/fires` not implemented, stub page ([known-limitations.md → /oracle/fires](known-limitations.md#ui-oraclefires---stub)).
- **DR**: work on `restore` for staging + CLI commands `keeper --check-config` / `conclave-evict` / `issue-token` (not in `soulctl` now - [soulctl/](../soulctl/README.md)).
- **Cloud-CRUD → explicit known-limitation** (post-GA): 6 CloudDriver plugins are available, but not REST `/v1/providers` - operationally unavailable ([known-limitations.md → Cloud-provisioning](known-limitations.md#cloud-provisioning---not-in-beta)).
- **Coverage-report** in CI (coverage visibility; hard gate - P2).

---

## P2 - nice-to-have (possible post-GA)

- **100k-load-run** (F2: distributed harness) - for the target "thousands" 25k streams are covered with a reserve calculation ([load-testing.md §6](testing/load-testing.md), F2 - backlog).
- **Conclave-metrics** (`keeper_conclave_*`).
- **Reproducible builds** (`SOURCE_DATE_EPOCH`).
- **Hard coverage-gate** (the coverage threshold blocks merge).
- **Soul auto-upgrade fleet** - clarify the self-upgrade mechanism for `soul` agents.

---

## Strong points (ready, not stubs)

So that the roadmap does not read as "nothing is ready" - this already works for real, not stubs:

- **End-to-end capabilities** - metrics, OTel, hot-reload + writeback config, log rotation, Vault integration, RBAC, OpenAPI - implemented and working ([requirements.md](requirements.md), [observability.md](observability.md)).
- **Core** - pull mode (daemon agent `soul`), scenario-DSL, Voyage (fleet batch), Scry (probe), RBAC - ready and proven on a live stand.
- **SBOM** (CycloneDX, `make sbom` - [Makefile](../Makefile)) - ready (as opposed to signature).
- **Module-path rename** to `github.com/souls-guild/soul-stack` - done.

---

## Proven load

Source of truth - [testing/load-testing.md](testing/load-testing.md) (F0 + F1 **measured up to N=25000** on a live test bench 2026-06-17, 24 vCPU/30 GiB, 1 keeper instance; full 100k-ramp - calculated, F2).

- **Keeper is linear in streams** up to a measured N=25000 connections (connect p99 ≤ 185 ms, 0 errors); per-soul RSS increase ≈ **0.12 MiB/soul** for N=10k–25k (increase coefficient, not absolute - [§8.1](testing/load-testing.md)).
- **Extrapolation to 100k by factor** ≈ 11–12 GiB RSS → **3–4 instances** per 100k by model (in the budget [scaling.md → Sizing](operations/scaling.md)); cliff at ≤ 25k **not achieved**, exact per-soul under 100k remains an F2 task.
- **Single-host limit** — the 50k probe reached ≈ 28222 streams (exhaustion of ephemeral loopback ports on the harness side, **not Keeper**); true 50k+ → distributed generator (F2).
- **Read-API** holds with a reserve: `GET /v1/souls` 3476→1488 rps for fleet growth, p99 < 140 ms; directories p99 < 5 ms. **Write-axis** ≈ 234 rps p99 5–7 ms under 25k fleet.
- **applybus maxclients-fix** (holder-skip + sharded channels, `fec7e02`) eliminated cliff on ~10k command-Voyage (did not finalize 10k at all before the fix); cross-keeper test under load - P1.
- **Tempo-preview rate-limit untied** (`34d85a9`, bucket `voyage_preview` 30/60) — preview ≈ 10 → 33 rps.

---

## See also

- [known-limitations.md](known-limitations.md) - beta limits (from the user side).
- [testing/load-testing.md](testing/load-testing.md) - measured load + F2 plan.
- [operations/scaling.md](operations/scaling.md) - sizing, bottlenecks, Shepherd / Conclave / Acolyte.
- [security/threat-model.md](security/threat-model.md) — status of internal security-gate and external pentest.
- [RELEASING.md](../RELEASING.md) - release procedure (docs-currency gate, tags).
