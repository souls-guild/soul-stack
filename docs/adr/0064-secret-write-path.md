# ADR-064. Secret write-path — accepting a plaintext secret from the operator and writing it to Vault keeper-side

> **Status: accepted, implementation pending.** Architect's design, user's decision (2026-07-01). Docs-first record BEFORE code. The ADR **generalizes the already existing** keeper-side write-path to Vault (`sigil.Introduce` / cert `issueMaterial` / `core.vault.kv-present`) to a new case: accepting a plaintext secret **from the operator** (not system-generated) via API/UI. **Amends [ADR-052](0052-herald-notifications.md) (Herald `secret_ref` → dual-mode) / [ADR-017](0017-keeper-side-core.md) (Provider `credentials_ref` → dual-mode).**

**Context.** Right now the operator has a **single** path to give Keeper a secret — the vault-ref: the operator puts the secret into Vault themselves and passes only the path `vault:<path>` to the API (Herald `secret_ref`, [ADR-052](0052-herald-notifications.md); Provider `credentials_ref`, [ADR-017](0017-keeper-side-core.md); Augur `auth_ref`, [ADR-025](0025-augur.md)). This matches the security premise "the secret never leaves Vault" ([requirements.md](../requirements.md)) — only the path travels over the wire. But for the operator this is friction: to set up a telegram bot or a cloud account, they must first go into Vault by hand, put the token there under some path, and only then fill in the form. For "friendly" onboarding a second path is needed: the operator enters the secret **as plaintext** in the UI/API → Keeper **itself** writes it to Vault under a deterministic path → only an internal ref is stored in Postgres.

**Key point: the write-path is not new machinery.** Keeper already writes secrets to Vault in three places, all of which reuse `vault.Client.WriteKV` and the ref-building helper:

- **`sigil.Introduce`** ([keeper/internal/sigil/keyservice.go](../../keeper/internal/sigil/keyservice.go)) — Keeper generates an ed25519 pair, writes the private key to Vault KV `secret/keeper/sigil-keys/<key_id>`, and stores `vault_ref` in PG ([ADR-026(d)](0026-sigil.md)).
- **cert `issueMaterial`** ([keeper/internal/reaper/rotate_certs.go](../../keeper/internal/reaper/rotate_certs.go)) — `SignCSR` → `WriteKV` → `warrant.ref` in PG.
- **`core.vault.kv-present`** ([keeper/internal/coremod/vault/kvpresent.go](../../keeper/internal/coremod/vault/kvpresent.go)) — generate-if-absent (redis/mongo passwords, the system generates the value).

Exactly **one** piece is missing: accepting a plaintext value **from the operator** (not generated) + writing it under an auto-path. The ADR adds an application layer on top of the existing infrastructure, **not new infra code**.

## Decision

Introduce dual-mode secret acceptance in Herald and Provider CRUD (Operator API + MCP):

- **The operator passes `secret`** (plaintext) **XOR** `secret_ref` (vault-path, current behavior). Two mutually exclusive fields. In the UI — a "value / path" radio toggle.
- On `secret` (plaintext) Keeper:
  1. builds a **deterministic path** `secret/<domain>/<entity>/<field>` (`domain` = `herald`|`provider`, `entity` = record name, `field` = the logical name of the secret — `secret`/`credentials`);
  2. writes the plaintext to Vault KV with this keeper-side Vault client (`vault.Client.WriteKV` — the same one used by sigil/cert);
  3. stores an **internal ref** in Postgres of the form `vault:<path>#<field>` (like sigil/warrant, the `vaultRefForPath` helper already exists);
  4. the plaintext is **persisted nowhere** — only in Vault; it is not in PG/logs/audit.
- On `secret_ref` (vault-path) — behavior as today: the path is written to PG as-is, Keeper does **not** write to Vault (the operator already put it there).
- **`update` overwrites** the secret at the same deterministic path (idempotent write, it does not create a new version-path).

Resolving the secret at the point of consumption (Herald webhook signing / Provider→CloudDriver creds-flow) does not change — it already reads by ref from PG.

### Scope MVP

- **Herald** — webhook signing-secret (`secret_ref`) + channel-token (telegram/slack bot-token). The operator knows the value.
- **Provider** — cloud credentials (`credentials_ref`). The operator knows the value.

Beyond MVP — see "Deferred".

## Security trade-off (a conscious relaxation, user's decision)

The `secret` (plaintext) mode **breaks** the invariant "the secret never leaves Vault" ([requirements.md](../requirements.md), "security first"): the plaintext travels operator → Keeper over the wire. The vault-ref is deliberately built so this doesn't happen. The relaxation is accepted **for the sake of UX** and is **acceptable only with mandatory mitigations** (all of which are implementation blockers):

- **(a) TLS is mandatory** on the transport carrying the plaintext (Operator API / MCP). Without TLS, accepting `secret` is not performed.
- **(b) Strict masking in all sinks** (logs / audit / OTel / UI / reports) + **guard-tests against leaks**. The field is named `secret` — it falls under `shared/audit.sensitiveKeyRe` (substring match `secret|token|password|…`, [mask.go](../../shared/audit/mask.go)) → auto-masked by key name. **★ the huma request-body does NOT pass through `MaskSecrets` by default** — an explicit audit of every Herald/Provider request-body logging point is required + a guard-test that the plaintext does not leak into any channel (a regression test per sink, [feedback: guard-tests on invariants](../../CLAUDE.md)).
- **(c) the plaintext is persisted nowhere** — only in Vault. In Postgres — only the ref; in a crash-dump/memory the plaintext lives only for the duration of request processing.
- **(d) RBAC / vault-policy** — Keeper writes the secret with **its own** Vault policy (write prefixes `secret/herald/*`, `secret/provider/*`) on behalf of the operator; the operator RBAC is the reused `herald.create` / `provider.create` (see below). Keeper's Vault policy is extended with a write grant on the deterministic prefixes.

The requirements conflict (security premise ↔ UX) is the user's prerogative; the relaxation is **confirmed 2026-07-01**.

## Compatibility: dual-mode (both, not a replacement)

The vault-ref **remains** primary for advanced/GitOps scenarios:

- **essence/GitOps** — plaintext must not be committed to git, ref-only is mandatory for declarative pipelines.
- **advanced security** — the operator can keep full control over Vault paths and versioning.
- **backward-compat** — hundreds of existing tests and configs on `secret_ref`/`credentials_ref` don't break.

plaintext-write is a **friendly layer on top**, not a replacement. The form is `secret` XOR `secret_ref`.

## RBAC and name

- **RBAC — no new permission.** Accepting `secret` goes through the same write endpoint as writing `secret_ref` — it reuses `herald.create` / `provider.create` ([rbac.md](../keeper/rbac.md)). A separate `secret.write` is **rejected**: the right to "set up a Herald/Provider" already includes "set its secret", granularity by the way the secret is passed is not needed.
- **Name — no new dictionary pattern.** The machinery does not introduce a new Soul Stack entity — it is a **DevOps field `secret`** in the API (the rule ["small = DevOps terms"](../naming-rules.md)). Thematic name patterns (`Consign`/`Entrust` — "entrust to Keeper for safekeeping") are **rejected**: generalizing an existing write-path does not warrant a named entity, an extra name inflates the dictionary.

## Rejected alternatives

- **Name pattern `Consign` / `Entrust`.** A thematic entity "entrust the secret to Keeper". Rejected — the write-path already exists three times nameless, accepting plaintext merely generalizes it; a new name in [naming-rules.md](../naming-rules.md) would be over-naming.
- **`oneof`-form contract** (`secret` and `secret_ref` as one oneof-union). Rejected in favor of two explicit mutually exclusive fields + server-side XOR validation: `oneof` in OpenAPI/huma gives worse form UX and weaker client typing than flat optional fields with an "exactly one set" check.
- **New permission `secret.write`.** Rejected — `herald.create`/`provider.create` already cover the right to set a record's secret; separate granularity by the way it is passed is redundant (see RBAC above).
- **ULID-immutable path** (`secret/<domain>/<entity>/<ulid>`, each write is a new immutable path). Rejected for Herald/Provider — there is exactly one current secret per record, and `update` must overwrite at a stable path. The deterministic `secret/<domain>/<entity>/<field>` is simpler (no orphan paths from past versions, no GC debt). ULID-immutable is appropriate where the secret's version history is needed — not in MVP scope.

## Deferred (post-MVP, no breaking changes)

- **Operator-TLS-PEM** (when the operator brings their own certificate/key, not PKI-issued). The secret is the same class — the operator knows the value — but its ref lives in **essence** (git), so a write-path for it entails an `essence → PG` migration (a separate large decision). **Out of MVP**; introduced by a separate ADR.
- **ULID-immutable path** — as an option for secrets with version history (not Herald/Provider), on an actual request.
- **Other secret-acceptance points** (Augur `auth_ref` etc.) — generalized additively with the same pattern, on request.

## Impact (for implementation, outside this ADR)

Operator API Herald/Provider CRUD + OpenAPI (drift-regen) + companion UI (`types.gen` + a "value/path" control, `gen:api`) + domain herald/provider CRUD (+plaintext→`WriteKV`) + `shared/audit` masking (guard leak-tests) + RBAC (reuse) + audit-event + `vault-policy.hcl` (write prefixes) + MCP. The Herald+Provider scale is medium (~10–15 core+UI points). **Implementation is NOT part of this ADR** (fixing the decision by document).

## Relation to ADRs

- **[ADR-052](0052-herald-notifications.md)** — Herald `secret_ref`; this ADR adds dual-mode acceptance (`secret` XOR `secret_ref`).
- **[ADR-017](0017-keeper-side-core.md)** — Provider `credentials_ref` (cloud creds-flow); dual-mode acceptance.
- **[ADR-026](0026-sigil.md)** — the model of the keeper-side write-path to Vault (`sigil.Introduce` → `WriteKV` → PG-ref).
- **[ADR-053](0053-dependency-tiers.md)** — Vault hard-required (the write-path relies on it); the additional relaxation of the security premise is fixed here as a conscious trade-off.
- **[ADR-014](0014-operator-identity.md)** — the pattern of a keeper-side secret in Vault KV.

## Amendment 2026-07-08 (NIM-73): the path of a not-found secret in error texts — flat form bypassing the masker

**Implemented (NIM-73)** — refines mitigation (b) "strict masking in all sinks" for the READ/resolve path of the secret (`vault()` in the CEL phase, `vault:`-ref in params), symmetric to the write-path above.

**Decision.** When a Vault secret is **not found** (KV path not found / no access / no field), the error text carries the **logical path of the secret in flat form** (`secret/redis/<inc>/users/<name>#password`) — WITHOUT the `vault:<mount>/` marker, so it **survives** observability masking (`audit.vaultRefRe` / `MaskSecrets`) and lands in `status_details` / `error_summary` / logs as clear text rather than `***MASKED***`.

**Rationale — the path is a location, not a value.** The path of a not-found secret tells the operator WHAT to seed into Vault; there is no value at a non-existent path — nothing to leak. Actionable diagnostics matter more than masking a non-secret. **Masking of the secret VALUE is preserved:** an actually resolved secret is still masked on output (the masking layer is untouched); the transport details of the Vault error are NOT propagated into the text; the values of neighboring fields of the secret are NOT substituted into the text.

**Implementation:** [shared/cel/vault.go](../../shared/cel/vault.go) (`callVault` / `vaultPathHint`) + [keeper/internal/render/vault_resolve.go](../../keeper/internal/render/vault_resolve.go) (`readVaultRef`) — flat form on both resolve paths.
