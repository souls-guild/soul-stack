# ADR-067. Mandatory log-shipping (Vector) — the log plane of data services

> **Status: active, implemented.** Architect's design (2026-07-01, `.pm/tasks/2026-07-01-vector/`), user's decisions (sink fork A / name `vector`). Recorded **retrospectively** (2026-07-03, paying off the drift "the implementation exists — the ADR doesn't"): the `vector` destiny and its embedding into redis/dragonfly are already in the tree.
>
> **Note on numbering.** The 2026-07-01 design planned the number "ADR-065"; in the meantime 0064 took secret-write-path, 0065 — `core.module.installed`, 0066 — teleport-onboarding. Therefore the ADR is **0067**. The `ADR-065` references in `examples/**` (essence/create/covenant/migration) to this slice are the same historical shift, fixed separately.

**Context.** The data services (redis, dragonfly, later mongo/qdrant) already have **metrics**: `node-exporter` (host metrics) and `redis_exporter` (Redis metrics) — Prometheus pull, [ADR-024](0024-observability.md). Metrics answer "the service is alive, the load is such-and-such", but not "what exactly happened" — for incident analysis you need the daemon's **logs**, centrally and in real time, rather than `ssh + tail` across hosts. Pull (metrics) and push (logs) are different planes: an exporter waits to be scraped, a log agent pushes lines into a collector itself. The services had no log plane; each operator solved log collection on their own. The user's requirement: log-shipping is installed **into all base data services mandatorily**, "like node-exporter" — as a service invariant, not an option.

## Decision

**(a) Vector as the log plane, complementing the exporters.** We install [Vector](https://github.com/vectordotdev/vector) (`vectordotdev/vector`) — an observability agent `sources → transforms → sinks`, PUSH of logs. Vector **complements**, does not replace node-exporter/redis_exporter: three independent observability layers (host metrics pull / Redis metrics pull / logs push), non-overlapping. The name `vector` is the upstream product's (precedent `node-exporter`/`redis-exporter`; a managed tool, not a Soul Stack entity), fixed in [naming-rules](../naming-rules.md).

**(b) Design — a clone of the node-exporter reference (stateful branch).** The new standalone destiny [`examples/destiny/vector/`](../../examples/destiny/vector/destiny.yml) repeats the prod convention [`production-conventions.md`](../destiny/production-conventions.md) (reference [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/), stateful branch §2):

- **install** — `core.url.fetched` (release tarball, https-only, `allow_private`-opt-out for an internal mirror) with a **mandatory** `sha256` (fail-closed: verification BEFORE materializing the file) → `core.archive.extracted`;
- **account** — `core.group.present` + `core.user.present` (`system`, `/usr/sbin/nologin`, no home) under a **stable uid** (NOT `DynamicUser`): Vector has a persistent disk buffer (`data_dir`) — a queue of read-file offsets and of the sink that survives a restart and requires a fixed owner; `core.file.directory` for `data_dir` (0700) and `config_dir` (0750);
- **service** — `core.file.present src:` (binary) + `core.file.rendered` `vector.yaml` (sources/sinks) + `core.file.rendered` hardened systemd unit + `core.service.running` (enabled) + `core.service.restarted` `onchanges:[config, unit, binary]`;
- **arch** — Vector publishes releases with a Rust triplet (`vector-<v>-<arch>-unknown-linux-gnu.tar.gz`), so `soulprint.self.os.arch` (`amd64`/`arm64`) is mapped to `x86_64`/`aarch64` by CEL in `vars` (a difference from the Prometheus exporters, where arch = `amd64` directly).

The destiny is assembled **entirely from existing core modules** ([ADR-015](0015-core-modules-mvp.md)) — it **introduces no new core module**. Rendering `vector.yaml`/unit is `core.file.rendered` (text/template, [ADR-010](0010-templating.md)).

**(c) Embedding — an unconditional `apply: destiny vector` at the end of `create`.** After the deploy branch and the exporters, `create` **unconditionally** (with no `when:` gate) deploys vector onto every host of the incarnation by composing the reusable destiny via `apply: destiny` (isolated render, [ADR-009](0009-scenario-dsl.md)) — [`redis/scenario/create/main.yml`](../../examples/service/redis/scenario/create/main.yml). This is a **data-service invariant**, not an operator choice. The whole contract is **in essence** (contract A — author-context: versions/checksum/sink are hidden from the Run form, the operator overrides them in `spec.essence`); `covenant`/`form` are untouched. The per-service essence carries a `vector_*` block; the only difference between services is `vector_log_sources` ([redis](../../examples/service/redis/essence/_default.yaml): `/var/log/redis/*.log`; [dragonfly](../../examples/service/dragonfly/essence/_default.yaml): `/var/log/dragonfly/*.log` + `/var/log/redis/*.log` for the sentinel daemon).

**(d) sink — Variant A (essence per-incarnation).** The address of the central collector lives in essence: `vector_sink_type` (`loki`/`elasticsearch`/`vector`/`console`) / `vector_sink_endpoint` / `vector_sink_auth_ref`. The default `sink_type: console` — logs to Vector's own stdout, **with no external infra** (a safe pilot default; the collector is not required). `loki`/`elasticsearch`/`vector` require `sink_endpoint`. **`sink_auth_ref` does NOT land in state** — the secret (a Vault-ref OR an already-resolved value, cascade: the caller passes a ref, resolution is Soul-side; symmetric to `tls.*_ref`) is passed into the unit via `Environment=VECTOR_SINK_TOKEN`, not into `vector.yaml` on disk. It follows the established vault-ref convention (Herald `secret_ref` [ADR-052](0052-herald-notifications.md) / Provider `credentials_ref` [ADR-017](0017-keeper-side-core.md) / Augur `auth_ref` [ADR-025](0025-augur.md)) — this is a read/resolve path, not a write-path.

**(e) state read-model.** state is extended with a `logging.vector_*` object (`version`/`sink_type`/`sink_endpoint`/`log_sources` — **without** `auth_ref`, the secret is excluded) — [`redis/covenant.yml`](../../examples/service/redis/covenant.yml), symmetric to the exporters' `monitoring` object. A `state_schema` bump + a **per-service** migration ([redis `013_to_014.yml`](../../examples/service/redis/migrations/013_to_014.yml): v13→v14, forward-only, `has()`-guard idempotent; a conservative default for pre-vector incarnations: `version: ''` = "not deployed", `console`). dragonfly — with an analogous migration.

## Alternatives (sink)

- **A — essence per-incarnation (chosen, pilot).** Zero infra, the `console` default works out of the box. Downside: `sink_endpoint` is duplicated across incarnations.
- **B — `keeper.yml` globally (follow-up).** One collector address per cluster. Downside: a new config contract `keeper.yml` → scenario-CEL (well-known keeper-settings) — to be introduced once a shared collector appears.
- **C — hybrid (follow-up).** keeper-default + essence-override. Combines A and B; introduced after B.

## Consequences

- A new standalone destiny `vector` + a `vector_*` block in each data service's essence + a `logging` object in state (+ a per-service migration). Replicating onto a new service — a copy of the essence block + a migration + an `apply:` step (like the exporters).
- **There is no new core module / no new dictionary name**: the destiny is from existing core modules, and the name `vector` is already in [naming-rules](../naming-rules.md).
- An operator with no collector gets a valid config (`console`) — vector starts, reads logs, and requires no external infra; with a live Grafana Loki/ES the operator sets `sink_type`/`endpoint`/`auth_ref` in `spec.essence`.
- The collector-access secret does not end up either in `vector.yaml` on disk or in `incarnation.state` — only in the systemd `Environment` and in Vault.
- **Open (follow-up):** a centralized `sink_endpoint` (Variant B/C); replication onto mongo (the mongo epic); qdrant — a **separate epic** (a vector database, not log-shipping).

## Amends / Related

- **Amends [ADR-024](0024-observability.md) (Observability).** Adds the **log plane (push)** as a third dimension of observability alongside metrics (Prometheus pull) and traces (OTel-bridge). ADR-024 did not cover logs.
- **Related — the node-exporter reference (NOT an amend):** a structural clone of [`production-conventions.md`](../destiny/production-conventions.md) (stateful branch) / [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/). node-exporter **has no ADR of its own** — there is nothing to amend, this is a "clone of the pattern" relation (it corrects the phantom "ADR-064 node-exporter" from the 2026-07-01 design: the real [ADR-064](0064-secret-write-path.md) is secret-write-path, unrelated to vector).
- **Related — [ADR-015](0015-core-modules-mvp.md) (core modules):** the destiny is assembled from existing core modules, it introduces no new module (a "uses" relation, not an amend).
- **Related — [ADR-010](0010-templating.md)** (`core.file.rendered` for `vector.yaml`/unit), **[ADR-009](0009-scenario-dsl.md)** (`apply: destiny` — isolated composition), **[ADR-007](0007-versioning-git-ref.md)** (a destiny's version = a git ref, there is no `version:` field).
