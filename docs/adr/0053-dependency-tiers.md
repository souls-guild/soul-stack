# ADR-053. Tiers of infrastructure dependencies

> **Status: canon (active, 2026-06-11).** A classification ADR — it fixes which external dependencies of a Keeper cluster are mandatory, which are optional, and how the optional ones are required to degrade. No code is added by this ADR: the decision describes **already established** behavior and locks a rule for future features. User decision: the no-Vault mode is rejected, Vault stays mandatory.

**Context.** [requirements.md](../requirements.md) presented Vault in the same row as the cross-cutting out-of-the-box capabilities (metrics, OTel, MCP, OpenAPI), with the wording "Vault integration". This created the impression that Vault is an optional integration. In fact Vault is **hard-required**: without it Keeper does not start. The user asked the direct question "what if there is no Vault?" — and it turned out that there was no canon of tiers (what is mandatory, what is optional, how the optional degrades) in the documents. This ADR closes the gap.

**Decision.** The mandatory infrastructure contour of a Keeper cluster is **three components: PostgreSQL + Redis + Vault**. All three are checked at startup and, if unavailable, they fail the launch (**fail-fast**), rather than degrading. Vault is an equal third mandatory component, not an "integration".

### Why Vault is hard-required (points in the code)

| Point | What breaks without Vault | Confirmation |
|---|---|---|
| **vault client at startup** | `setupVault` brings up a client via `NewClient`, which ends with `Ping(ctx)`; any error (addr empty / auth failed / ping did not arrive) → `errSetupFailed`, the process does not start | `keeper/cmd/keeper/daemon.go` (`setupVault`), `keeper/internal/vault/client.go` (`NewClient` → `cl.Ping(ctx)`) |
| **JWT signing-key (operator auth)** | Signing/verification of operator JWTs takes the HS256 key from `secret/keeper/jwt-signing-key` ([ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). No Vault → no key → no authentication of Archons → the control plane is unavailable | `keeper/internal/jwt/verifier.go`, `keeper/internal/bootstrap/signing_key.go` |
| **souls-PKI (mTLS identity of the Souls)** | Issuance and rotation of SoulSeed — signing a Soul agent's CSR via Vault PKI (`pki/sign/<pki_role>`). No Vault → cannot onboard new Souls and rotate existing mTLS pairs | `keeper/internal/vault/pki.go`, `keeper/internal/soulseed/soulseed.go`, `keeper/internal/grpc/bootstrap.go` |

These are not a "convenient integration" but load-bearing nodes: operator auth and the mTLS identity of the Souls. Both are tied to Vault by design ([security premise](../requirements.md): "secrets do not materialize on the disk of the Keeper cluster").

### Tier table

**REQUIRED — fail-fast at startup, no degradation:**

| Component | Role | Behavior without it |
|---|---|---|
| **PostgreSQL** | cold storage of state ([ADR-005](0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)) | startup fails |
| **Redis** | hot layer: presence, lease, pub/sub, leader election ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)) | startup fails |
| **Vault** | secret-store + auth (JWT signing-key) + souls-PKI | startup fails (`setupVault` ping) |

**OPTIONAL-with-degradation — the absence is configurable, the feature turns off clearly, Keeper does not fall:**

| Capability | "Not configured" trigger | Degradation |
|---|---|---|
| **Sigil signing-key** | no active key in the registry / ref not set | plugin integrity check is disabled, **fail-closed** (an unsigned plugin is not admitted — ADR-026) |
| **Augur** (Keeper-side broker) | empty registry of Augur sources | **default-deny**, requests to unconfigured sources are rejected |
| **Herald `secret_ref`** | the channel has no `secret_ref` set | webhook delivery goes **without an HMAC signature** of the body (ADR-052) |
| **push host-CA** | no push block / `ssh_providers` | `keeper.push` — **no-op, push disabled** |
| **metrics basic-auth** | `metrics.basic.enabled: false` | the metrics listener comes up **without auth** |
| **OTel export** | endpoint not set | traces/metrics are **not exported**, in-process work is not affected |
| **Kafka audit-sink** | `audit.sink ≠ kafka` (default `pg`) | audit export goes to PG (`audit_log`), Kafka is not needed; with `audit.sink: kafka` the broker's unavailability degrades **fail-closed** (audit is compliance-critical — the event is not lost: durable-fallback/block the write-path, not fail-open) — [ADR-059](0059-audit-sink-pluggable.md) |

**Rule for NEW features.**
- A new **mandatory** infrastructure dependency (a fourth REQUIRED component) is introduced **only through an explicit user decision** — not "a feature dragged in a dependency as an implementation detail".
- A new **optional** capability must degrade **clearly**: in the absence of a config/backend the feature turns off with an understandable log or an error at the boundary, but **does not fell Keeper**. The choice of fail-open / fail-closed on degradation is a conscious security trade-off, fixed in the feature's ADR (cf. Tempo fail-open [ADR-050(b)](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api) vs Sigil/Purview fail-closed).

**Rationale.**
- **The security premise is inviolable.** "Secrets do not materialize on the disk of the Keeper cluster" ([requirements.md](../requirements.md), the threat model) holds exactly on the fact that the secret-store is an external Vault. Removing Vault = removing this premise.
- **Auth and mTLS identity are not optional.** Without the JWT signing-key there are no operators, without souls-PKI there is no trusted set of Souls. This is the core, not an "integration".
- **Honest docs are a debt.** requirements must not present the hard-required Vault as an optional integration; this ADR + an edit to [requirements.md](../requirements.md) remove the code↔docs desync.

**Rejected alternatives.**
- **(a) No-Vault mode** (auth key from a file/env + a built-in CA instead of Vault PKI). Breaks the security premise: the CA private key ends up on the Keeper's disk or in PG; in a multi-keeper HA ([ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) this private key would have to be spread across all nodes of the cluster — an expansion of the attack surface onto every node. **Rejected by the user (2026-06-11).**
- **(b) A SecretProvider abstraction** (an interface with pluggable backends: Vault / file / cloud-KMS). Premature — there is no multi-backend requirement; an abstraction for the sake of "what if a second backend is needed" is over-engineering. Introduced only on a real requirement.

**Operations note.** "Mandatory Vault" ≠ "a heavy Vault cluster is needed". For a small installation a single-binary Vault with file-storage is enough — operationally this is comparable to running Redis (one process, local storage). The recipe — [`docs/operations/infra.md` → Lightweight Vault for small installations](../operations/infra.md#лёгкий-vault-для-малых-инсталляций). **dev-mode Vault is unsuitable for production** (it loses data on restart) — this is explicitly noted in the recipe.

**Relation to ADRs.**
- **[ADR-005](0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)** / **[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)** — the two other REQUIRED components.
- **[ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)** — the JWT signing-key from Vault (one of the hard-required points).
- **[ADR-026](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)** — the Sigil signing-key (OPTIONAL, fail-closed degradation).
- **[ADR-050](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)** — an example of a conscious fail-open on degradation (a contrast with fail-closed).
- **[ADR-052](0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)** — Herald `secret_ref` (OPTIONAL, delivery without a signature when absent).
- **[ADR-059](0059-audit-sink-pluggable.md)** — Kafka audit-sink (OPTIONAL, **fail-closed** degradation; default `audit.sink: pg` — the mandatory contour is intact, Kafka does NOT become a 4th required).
