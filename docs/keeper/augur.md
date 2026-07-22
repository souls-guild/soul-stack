# Augur - Soul external access broker

> **Status - MVP-1 implemented (2026-05, in baseline); MVP-2 is postponed.** Status source - [ADR-025](../adr/0025-augur.md) (amendment 2026-06-16). This document standardizes the **Augur** architecture.
>
> - **MVP-1 (broker, `delegate=false`) - IMPLEMENTED:** registries `omens` / `rites` (migrations 032/033), keeper-side fetch Vault / Prometheus / ELK, Soul-side `core.augur.fetch`, routes `/v1/augur/*` (`omens` + `rites`).
> - **MVP-2 (delegation, `delegate=true`, scoped token minting) - POSTPONED:** at the Rite code level with `delegate=true` resolves to `denied`; proto-messages `ScopedVaultToken` / `ScopedStaticCred` are created as a contract, but are not filled out. The order of implementation relative to other backlog is up to the PM/user.

**Augur** - Keeper-side subsystem that gives Soul **live** (during render / apply) access to external systems - Vault, Prometheus, ELK - which is not covered by the pre-resolved Soul Stack model. Metaphor: an augur (oracle) mediates between a mortal and the will of the gods; here Augur mediates between Soul and external systems, without giving Soul master-credentials.

## 1. Why Augur - a border with a pre-resolved model

The current model for accessing external systems is **pre-resolved**: everything that the run needs is resolved by Keeper-side **before** sending the command to Soul. The CEL phase (`vault(...)` / `soulprint.*` / `register.*` / `essence.*`) is executed on Keeper, Vault is read on Keeper, and Soul receives the already rendered `ApplyRequest` ([ADR-012(d)](../adr/0012-keeper-soul-grpc.md)). There is no `cel-go` on Soul, no Vault client, no Vault tokens.

This model does not cover cases where the value is needed **at execution time on the host**, and not at the render stage:

- a secret that should be read as close to use as possible (short-lived dynamic secret Vault, which will go bad if you resolve it on Keeper in advance);
- live query in Prometheus ("how many replicas are in quorum now?") as a condition of the apply step;
- reading from the ELK index during the run.

Augur - **forward-looking layer**: it does not replace the pre-resolved model and does not move its border for normal rendering. It adds a narrow, authorized channel of "Soul is asking for external meaning right now." Pre-resolved remains a default; Augur - for what pre-resolve by its nature cannot give in advance.

## 2. Two phases (one ADR)

Augur is normalized by one [ADR-025](../adr/0025-augur.md), but is implemented in two phases. The boundary between phases is the `delegate` field in Rite and the response form in [`AugurReply`](#52-augurreply-keeper--soul).

### 2.1 MVP-1 - broker (`delegate=false`)

Soul requests value **via Keeper**; Keeper itself goes to the external system and returns the value of Soul to inline.

- For Vault - Keeper reads KV with its existing mechanism (`ReadKV`, same as [`core.vault.kv-read`](modules.md) / implicit `${ vault(...) }`).
- Data flows **via Keeper** (`AugurReply.inline_data`). On Soul, the external token/credential **does not apply**.
- Border [ADR-012(d)](../adr/0012-keeper-soul-grpc.md) **not touched**: external access remains with Keeper, as for normal rendering. Only the **moment** changes (live, at Soul's request), not the **side**.
- AppRole for MVP-1 **not needed** - Keeper uses existing access to Vault.

### 2.2 MVP-2 - delegation (`delegate=true`)

Keeper issues Soul a narrow, short-lived credential, and Soul goes to the external system **directly**.

- **Vault.** Keeper **mints** scoped / short-TTL / limited-use Vault token (`auth/token/create` under master-AppRole Keeper) with policies/TTL/number of uses from Rite, and gives it to Soul (`AugurReply.scoped_vault_token`). Soul reads Vault directly with this ephemeral token. Requires: AppRole + `auth/token/create` right in `keeper/internal/vault` (server-side).
- **Prometheus / ELK.** There is no Vault-style minting. Delegation = issuing a scoped **static** pre-scoped read-key (`AugurReply.scoped_static_cred`), which the operator previously put in Vault and referenced in `omens.auth_ref` as a separate record - this is **not** a master-cred system. Soul makes a direct read-only request with this key.

## 3. End-to-end request flow

```
Soul Keeper (Augur) External system
 │                                │                                       │
 │── AugurRequest ───────────────▶│                                       │
 │   {request_id, apply_id,       │ 1. resolve omen by name               │
 │    omen_name, query}           │ 2. SID ← mTLS peer cert               │
 │                                │ 3. SID → covens (registry)            │
 │                                │ 4. find Rite(omen, coven|sid)         │
 │                                │ 5. query ∈ Rite.allow ?               │
 │                                │ 6. branch by delegate / source_type   │
 │                                │                                       │
 │            delegate=false ─────┤── read KV / query ───────────────────▶│
 │◀── AugurReply.inline_data ─────┤◀── value ─────────────────────────────│
 │                                │                                       │
 │            delegate=true ──────┤── auth/token/create (vault) ──────────▶│
 │◀── AugurReply.scoped_vault_token / scoped_static_cred ─────────────────│
 │── (direct read with ephemeral cred) ──────────────────────────────────▶│
```

The SID in the request is **not transmitted** as identity-claim - identity authority Soul - and this is `Subject Alternative Name` mTLS peer cert ([ADR-012(i)](../adr/0012-keeper-soul-grpc.md)). `apply_id` in the query serves as a correlation with the run (see [§8 Audit](#8-audit)).

## 4. Data model (Postgres)

The Augur registry lives in Postgres, managed via OpenAPI / MCP - similar to [Provider / Profile](cloud.md) ([architecture.md → Artifacts](../architecture.md): runtime-state → Postgres, not git). Two tables.

### 4.1 Table `omens` - registry of external systems

**Omen** is an external system to which Augur mediates access (one Vault-mount, one Prometheus, one ELK cluster). Analogous to [Provider](cloud.md) for clouds.

| Column | Type | Meaning |
|---|---|---|
| `name` | `TEXT PRIMARY KEY` | Omen's name, kebab-case (`vault-prod` / `prom-main` / `elk-logs`). |
| `source_type` | `TEXT` (enum) | External system type: `vault` / `prometheus` / `elk`. Descriptive-enum (see [§7](#7-source_type-enum)). |
| `endpoint` | `TEXT` | External system URL (`https://vault.internal:8200`). |
| `auth_ref` | `TEXT` (vault-ref) | **Always** `vault:<mount>/<path>`-link to the master-credential Keeper (Vault AppRole-secret / pre-scoped read-key). **Master-credential is not stored in the database** - only a vault-ref for it. The vault-ref format is [config.md](config.md) (diagnostics `vault_ref_invalid_format`). |

**Invariant:** `auth_ref` is always vault-ref. Plaintext-credential in `omens` is prohibited - symmetrically `metrics.auth.basic.password_ref` ([config.md → metrics](config.md#metrics)) and `provider.credentials_ref` ([cloud.md](cloud.md)).

### 4.2 Table `rites` - grant / policy-mapping

**Rite** - grant: permission "such and such an entity can, through Augur, obtain such and such values from such and such Omen, in such and such mode." Associates a subject (Coven or specific SID) with an Omen, an allow-list, and a delivery mode.

| Column | Type | Meaning |
|---|---|---|
| `id` | `BIGINT` / `UUID` PK | Rite's surrogate key. |
| `omen` | `TEXT REFERENCES omens(name) ON DELETE CASCADE` | Omen, which grant refers to. **CASCADE**: deleting an Omen removes all of its Rites (see §9 fork). |
| `coven` | `TEXT NULL` | Subject-grant by Coven-label. **XOR** with `sid`. |
| `sid` | `TEXT NULL` | Subject-grant for a specific SID. **XOR** with `coven`. |
| `allow` | `JSONB` | Allow-list of allowed values. The form depends on `source_type` Omen (see below). |
| `delegate` | `BOOLEAN NOT NULL DEFAULT false` | `false` - broker (MVP-1); `true` - delegation (MVP-2). |
| `token_ttl` | `interval` / `TEXT` (duration) `NULL` | **Only for `vault`-Omen with `delegate=true`**: TTL of the mined scoped token. `NULL` for prom/elk. |
| `token_num_uses` | `INT NULL` | **Only for `vault`-Omen with `delegate=true`**: minable token usage limit. `NULL` for prom/elk. |

**Subject is strictly XOR.** Exactly one of `coven` / `sid` is non-empty (CHECK-constraint). `coven`-Rite applies to all Souls with this tag; `sid`-Rite - to one host.

**`allow` to `source_type`:**

| `source_type` | Contents `allow` |
|---|---|
| `vault` | `paths` (KV paths available for reading) and/or `policies` (Vault policies attached to the minable scoped token at `delegate=true`). |
| `prometheus` | `queries` (allowed requests/request patterns). |
| `elk` | `indices` (indexes allowed). |

**`token_ttl` / `token_num_uses` - only vault-delegate.** For prom / elk-delegation Vault-style there is no minting (a static pre-scoped read-key is issued), so both fields are `NULL`. For `delegate=false` both fields are ignored (the broker does not mint the token).

## 5. Transport

Augur **does not introduce new RPC**. It adds **two only-add messages** to `oneof payload` of the existing `EventStream` ([ADR-012(c)](../adr/0012-keeper-soul-grpc.md) forward-compat only-add; [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) "one long-lived stream" - complied with). Messages live in a new file `proto/keeper/v1/augur.proto` (thematic layout [ADR-012(b)](../adr/0012-keeper-soul-grpc.md): one file = one semantic axis).

### 5.1 `AugurRequest` (Soul → Keeper)

| Field | Type | Meaning |
|---|---|---|
| `request_id` | string | Request ID for correlation `AugurRequest` ↔ `AugurReply` within the stream. **Generated by Soul** (ULID / UUID), **unique per-stream**. |
| `apply_id` | string | The run in which Soul makes a request (audit correlation). |
| `omen_name` | string | Omen's name (`omens.name`). |
| `query` | string | Query to Omen: KV-path (vault), promQL (prometheus), index-query (elk). Tested against `Rite.allow`. |

**`request_id` - generation and uniqueness.** Request ID generates **Soul** (ULID / UUID); it must be **unique within one `EventStream`-a**. The purpose is to correlate parallel Augur requests of one apply: within a run, Soul can hold several Augur requests in-flight simultaneously (different steps/Omens), and Keeper echoes `request_id` to `AugurReply` so that Soul matches the response with the expectation. Keeper does not interpret `request_id` as identity / authorization - only as an opaque correlation key.

SID in payload **missing** - taken from mTLS peer cert ([ADR-012(i)](../adr/0012-keeper-soul-grpc.md)).

### 5.2 `AugurReply` (Keeper → Soul)

| Field | Type | Meaning |
|---|---|---|
| `request_id` | string | Echo `AugurRequest.request_id`. |
| `status` | enum | `ok` / `denied` / `error`. |
| `result` | `oneof` | With `status=ok` - one of three (see below). |
| `error` | string | For `status=error` / `denied` - diagnostics. |

`result` (`oneof`):

| Option | When | Meaning |
|---|---|---|
| `inline_data` | `delegate=false` (MVP-1, any `source_type`) | The value read by the Keeper and transmitted to the Soul via the Keeper. |
| `scoped_vault_token` | `delegate=true` + `source_type=vault` (MVP-2) | An ephemeral scoped Vault token (TTL/num_uses/policies from Rite) with which Soul reads the Vault directly. |
| `scoped_static_cred` | `delegate=true` + `source_type ∈ {prometheus, elk}` (MVP-2) | Scoped read-only static cred (pre-scoped read-key), with which Soul makes a direct read-only request. |

Forward-compat: new `result` options are added only-add, without reuse field numbers ([ADR-012(c)](../adr/0012-keeper-soul-grpc.md)).

### 5.3 Shape `inline_data` (shape convention)

`inline_data` - `google.protobuf.Struct` (always a proto-level object). The content is normalized by the convention according to the form of the original result:

- **Scalar** (for example vault-KV `#field` - one value from the secret) - object `{ "value": <scalar> }`. The scalar is wrapped in a single key `value` because `Struct` does not carry a bare scalar at the top level.
- **Map** (entire vault KV / Prometheus-result / ELK-response) - **natural object** "as is" (the keys of the original map become the keys of `Struct`).

**The `#field` projection is made by Keeper when reading Omen**, not Soul. That is: if the request addresses a specific secret field (`#field` notation), Keeper reads the KV, selects the desired field and returns an already projected scalar in the form `{ "value": <scalar> }`. Soul receives a ready-made value and does not parse the entire secret - this maintains the invariant "Soul does not see unnecessary things" (minimization of secret material on Soul, §security).

## 6. Authorization (Keeper-side)

The decision to satisfy `AugurRequest` is made by Keeper. Algorithm:

1. **Omen exists.** `omens` contains an entry with `name == omen_name`. Otherwise → `denied`.
2. **SID → covens.** SID is taken from mTLS peer cert; covens are resolved from registry (`souls.coven[]`, [storage.md](storage.md)).
3. **Rite found.** There is a Rite with `omen == omen_name` and a subject matching the request: either `rites.sid == SID` or `rites.coven ∈ covens(SID)`. Otherwise → `denied`.
4. **Query in allow-list.** `query` ∈ `Rite.allow` (in the form `source_type`: path in `paths`, query in `queries`, index in `indices`). Otherwise → `denied`.
5. **Branching.** From `Rite.delegate` and `Omen.source_type`:

| `delegate` | `source_type` | Keeper action | `AugurReply.result` |
   |---|---|---|---|
| `false` | any | read value itself (vault `ReadKV`/prom-query/elk-query under master-cred) | `inline_data` |
| `true` | `vault` | mint scoped token (`auth/token/create`, TTL/num_uses/policies from Rite) | `scoped_vault_token` |
| `true` | `prometheus` / `elk` | return pre-scoped read-key from `auth_ref` | `scoped_static_cred` |

Any failed audit → `AugurReply{status: denied}` + audit-event `augur.access_denied` (see [§8](#8-audit)).

### 6.1 Vault-delegate - orphan token

The minable scoped Vault token is created as **orphan** (`no_parent=true`): it is not tied to the Keeper instance token and survives Keeper-restart / failover. Otherwise, when rotating/restarting the Keeper instance, the tokens issued to the Souls would be recalled along with the parent - this would break in-flight runs on the hosts. Trade-off and rationale - §9.

### 6.2 Prometheus / ELK delegate via pre-scoped read-key

For prom / elk delegation in `omens.auth_ref`, the operator puts the vault-ref on a **separate pre-scoped read-only read-key** (created in advance in the external system itself and placed in Vault), and **not** the master-credential. Keeper at `delegate=true` gives Soul exactly this key as `scoped_static_cred`. In this case, the Master-credential Keeper does not get to the Soul - the §security invariant is respected.

## Safety requirement (normative invariant)

> **Soul NEVER receives the master-credential of the external system.** Soul receives only an ephemeral scoped token (Vault, `delegate=true`) or a scoped read-only static cred (prom / elk, `delegate=true`), or does not receive a credential at all (`delegate=false` - data comes inline through Keeper).

This is a normative Augur invariant, not a recommendation. Consequences:

- **MVP-1 (`delegate=false`) does NOT touch the [ADR-012(d)](../adr/0012-keeper-soul-grpc.md) border.** External access remains with Keeper - exactly like for a regular render. There is no Vault token or Vault client on Soul.
- **MVP-2 (`delegate=true`) is a narrow deliberate exception to [ADR-012(d)](../adr/0012-keeper-soul-grpc.md).** A **minimal fetch client** appears on Soul (reads Vault with an ephemeral token / makes a read-only request in prom-elk with a pre-scoped key). But even then: master-credential, `cel-go`, sprig-render-context and Keeper Vault tokens are still **not** on Soul. The exception concerns exactly one thing: narrow live-fetch with an ephemeral scoped credential, never master access.

"Safety First" ([requirements.md](../requirements.md)): default `Rite.delegate = false`; delegation is an explicit conscious opt-in of the operator for a specific Rite.

## 7. `source_type` enum

Descriptive closed enum in `omens.source_type`:

| Meaning | External system |
|---|---|
| `vault` | HashiCorp Vault (KV; delegation = minable scoped token). |
| `prometheus` | Prometheus (live-query; delegation = pre-scoped read-key). |
| `elk` | Elasticsearch / ELK stack (index-read; delegation = pre-scoped read-key). |

The enum extension is propose-and-wait + PR to this file and [naming-rules.md](../naming-rules.md). Augur **doesn't add** a new value to audit-`source` enum ([§8](#8-audit)) - Soul's live-fetch falls into the existing `soul_grpc` category.

## 8. Audit

Augur events are written to the general audit-pipeline ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention), [storage.md → audit_log](storage.md)). Names - convention `<area>.<action>` ([naming-rules.md → Audit-events](../naming-rules.md#audit-events)).

| Event | Category `source` | `archon_aid` | `correlation_id` | When |
|---|---|---|---|---|
| `augur.fetch_brokered` | `soul_grpc` | `NULL` | `apply_id` | MVP-1 broker read and returned value (`delegate=false`). |
| `augur.token_minted` | `soul_grpc` | `NULL` | `apply_id` | MVP-2 minted the scoped Vault token (`delegate=true`, vault). |
| `augur.cred_issued` | `soul_grpc` | `NULL` | `apply_id` | MVP-2 issued scoped static cred (`delegate=true`, prom/elk). |
| `augur.access_denied` | `soul_grpc` | `NULL` | `apply_id` | Any check §6 fails. |
| `omen.created` / `omen.revoked` | `api` / `mcp` | AID from JWT | — | CRUD Omen via OpenAPI / MCP. |
| `rite.created` / `rite.revoked` | `api` / `mcp` | AID from JWT | — | CRUD Rite via OpenAPI / MCP. |

Live-fetch-events (`augur.*`) - category **`soul_grpc`** ([ADR-022(b)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)): initiator - Soul (machine actor, no AID → `archon_aid: NULL`), `correlation_id = apply_id`. **Augur does not introduce a new value in `source` enum.** Secret-values ​​are not written in audit-payload (secret-masking - [ADR-010](../adr/0010-templating.md)): the fact + Omen + query is logged, not the value / token itself.

## 9. Accepted design forks

Solutions passed through user + architect (logged as accepted, not open Q):

| Fork | Solution | Rationale |
|---|---|---|
| Rite ↔ Omen lifecycle | `ON DELETE CASCADE` | Rite without Omen is meaningless; deleting Omen should atomically remove all grants to it (no orphan-Rites). |
| Vault token parentage | orphan(`no_parent=true`) | The Scoped token must survive Keeper-restart/failover, otherwise in-flight runs on the hosts break when the instance is rotated. Price - the token is not cascaded when the parent is revoke; offset by the short TTL/num_uses from Rite. |
| prom/elk delegate | separate pre-scoped read-key in `auth_ref` (not master) | There is no Vault-style minting for prom / elk; delegation without distributing master-cred is only possible through a pre-limited read-key. |
| response-wrapping | post-MVP hardening | Vault response-wrapping of a minted token enhances in-transit protection, but does not block MVP-2; is introduced by a separate hardening task. |

## 10. Reconciliation with valid ADRs

| ADR | Relation to Augur |
|---|---|
| [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) (one EventStream) | **Complied** - Augur only-add to existing `oneof`, no new RPC / stream. |
| [ADR-012(c)](../adr/0012-keeper-soul-grpc.md) (forward-compat only-add) | `AugurRequest` / `AugurReply` are added only-add to `oneof payload`, new file `augur.proto`. |
| [ADR-012(d)](../adr/0012-keeper-soul-grpc.md) (render/external access boundary) | MVP-1 **does not touch the border**; MVP-2 (`delegate=true`) is a narrow conscious exception (minimal fetch client on Soul; master-cred/cel-go/sprig-context still not on Soul). |
| [ADR-014](../adr/0014-operator-identity.md) (Vault AppRole - post-MVP) | AppRole, formerly post-MVP, becomes **dependency of MVP-2** (minting a scoped token under master-AppRole). |
| [ADR-017](../adr/0017-keeper-side-core.md) (`core.vault.kv-read`) | **Generalized by Augur.** `core.vault.kv-read` remains for the render phase (pre-resolve, read at the scenario render stage); Augur - live access when apply. Two different moments, not a double. |

## See also

- [architecture.md → ADR-025](../adr/0025-augur.md) - design fixation, 2-phase, exception from ADR-012(d).
- [naming-rules.md → Augur / Omen / Rite](../naming-rules.md) - name dictionary, source_type enum, proto-names, RBAC-perms, audit-events, PG tables.
- [storage.md](storage.md) — Keeper Postgres registries (where `omens` / `rites` will go).
- [cloud.md](cloud.md) - sample Provider / Profile registry in Postgres, managed via API/MCP.
- [modules.md](modules.md) - `core.vault.kv-read` (render phase, generalized by Augur for live access).
- [rbac.md](rbac.md) — RBAC-perms (`omen.*` / `rite.*`).
- [operator-api.md](operator-api.md) - OpenAPI side of CRUD Omen / Rite (start as stub directory).
- [mcp-tools.md](mcp-tools.md) - MCP-tools for Omen / Rite (start as stub directory).
- [architecture.md → ADR-012](../adr/0012-keeper-soul-grpc.md) - EventStream contract (only-add, render border).
- [requirements.md](../requirements.md) - "security comes first", integration with Vault out of the box.
</content>
</invoke>
