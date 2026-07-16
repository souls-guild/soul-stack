# ADR-070. Secret reveal-path — revealing an incarnation's plaintext secret to the operator under an RBAC right

> **Status: accepted, implemented (NIM-74).** The READ twin of [ADR-064](0064-secret-write-path.md) (secret write-path). ADR-064 accepts a plaintext secret **FROM** the operator and writes it to Vault keeper-side; this ADR is the reverse direction: it returns the plaintext **BACK** to the operator under an explicit right. **Amends [ADR-064](0064-secret-write-path.md)** (secret masking: a sanctioned reveal = removing the mask, not a leak) **and [ADR-047](0047-purview.md)** (a new scoped right `incarnation.view-secrets`).

**Context.** The operator sees in the State view that an incarnation has, say, redis users, but cannot look at their passwords: `state`/`spec` in GET responses are masked ([ADR-064](0064-secret-write-path.md), defense-in-depth in [operator-api.md](../keeper/operator-api.md)), and the values themselves live in Vault by ref. For day-2 (hand a password to a user, check a connection by hand) the operator needs a **sanctioned** way to reveal a concrete value — without removing masking globally and without granting direct access to Vault. The mechanism must be **generic** (a property of any service with secrets in state), not a redis hardcode in the Keeper.

Symmetry of directions (the same plaintext-over-the-wire trade-off as in ADR-064, mirrored):

- **ADR-064 (write):** operator → plaintext → Keeper → `WriteKV` into Vault → only a ref in PG. **Accepting** a secret.
- **ADR-070 (read, this ADR):** operator → request → Keeper → `ReadKV` from Vault → plaintext → operator. **Returning** a secret.

## Decision

A declarative **registry of revealable secrets** in the service manifest + two incarnation-scoped endpoints under a new right.

### The `revealable_secrets` registry (manifest `service.yml`, generic)

The service itself declares WHAT of its incarnations is revealable (not a redis hardcode in the core):

```yaml
revealable_secrets:
  - id: redis-users                                            # declaration address (lowercase ident, unique)
    label: "Пароли Redis-пользователей"                        # caption for the UI
    enumerate: state.users                                     # state-path of an array of objects; key = element.name
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
```

- **`id`** — a stable declaration address (lowercase ident, unique within the manifest).
- **`label`** — a human-readable caption for the UI.
- **`enumerate`** — a state-path of an array of objects (form `state.<segment>`); from the element names (`element.name`) the **set of allowed `key` values** is assembled. Mandatory in the MVP (see "Deferred").
- **`vault_ref`** — a Vault-path template with placeholders `{service}` / `{incarnation}` (**both mandatory** — per-service/per-incarnation scoping; the absence of either → diag `vault_ref_not_service_scoped` on load; see "Closing the escalation") / `{key}` (mandatory when `enumerate` is set; +optional `#field` — a field in the KV record).

### Restricted placeholders (not CEL)

`vault_ref` is resolved by **literal substitution** of exactly three validated quantities — `{service}` (= `inc.Service`, valid by the segment pattern — without `/`/`#`/`..`), `{incarnation}` (= `inc.Name`, valid by `NamePattern`), and `{key}` (valid by the ident pattern **AND** required to ∈ the enumerate array). Not CEL, not arbitrary expressions. Rationale:

- **less attack surface** — no computable language in the path to a secret; a foreign path cannot be constructed by an expression;
- **anti-arbitrariness** — `key` must be present in the enumerate array of the **current** state (a path not present in state cannot be revealed);
- **anti version-craft** — the manifest version is always `incarnation.ServiceVersion` (the client does not set the version; parity with `secretSchemaForIncarnation`);
- **traversal-guard** — the resolved path is run through `vault.ParseRef` (cuts `..`/`.`/malformed form) BEFORE reading; on failure → the secret is unrevealable (not a 500, the path is not exposed);
- **positive namespace scoping** — the resolved path must lie under `secret/<service>/<incarnation>/` (the **main** runtime guard; see "Closing the escalation").

### Endpoints

- **`POST /v1/incarnations/{name}/secrets/reveal`** `{secret_id, key}` → `{value}` — reveals a single value. Self-audit `incarnation.secret_revealed` (the fact, WITHOUT the value).
- **`GET /v1/incarnations/{name}/secrets/revealable`** → `{items: [{secret_id, label, state_path, keys}]}` — discovery: what is revealable and with which `key`s (READ, without audit). The UI builds a list from this; an empty list is valid.

### Sanctioned reveal — a DTO past the masking

The 200 body of the reveal endpoint carries plaintext and **does NOT pass through `MaskSecrets`** — this is the only sanctioned exit point of a value from the domain. The right `incarnation.view-secrets` **is** the sanction; the masking of the other sinks ([ADR-064](0064-secret-write-path.md)) is not touched.

## RBAC and the right

A new scoped right **`incarnation.view-secrets`** ([ADR-047](0047-purview.md), catalog — [rbac.md](../keeper/rbac.md#каталог-permissions)):

- **Strictly more privileged than `incarnation.get`** — reading a (masked) incarnation ≠ revealing its secrets; a separate right, not a facet of `get`.
- **Scope as for incarnation mutations** — selectors `coven=`/`service=`/`incarnation=` by the path `name` (parity with `incarnation.update-hosts`/`incarnation.traits-set`).
- **Fail-closed 404 outside scope** — an operator outside scope gets `404` (parity with Get: we do not expose the existence of a foreign incarnation), not `403`.
- **MCP — no (REST-only)** — reveal is a UI action of the State view (like `form-prefill`), not an automatable operation; an MCP tool is not created.

## Security trade-off (following the pattern of [ADR-064 §Security](0064-secret-write-path.md))

Reveal is the reverse leg of the same deliberate relaxation: plaintext goes Keeper → operator over the wire (ADR-064 carried it operator → Keeper). Acceptable GIVEN mandatory mitigations (all — blockers):

- **(a) RBAC gate on the incarnation** — the right `incarnation.view-secrets` + fail-closed scope narrowing (outside scope → 404).
- **(b) Audit of the fact WITHOUT the value — success AND denied** — `incarnation.secret_revealed` writes `{name, secret_id, key, path, result, reason}`; **the secret value is NEVER put in the payload**. Not only success is audited (`result: "ok"`), but also **every denied branch AFTER resolving the incarnation** (`result: "denied"` + `reason` ∈ `out_of_scope` / `unknown_secret_id` / `key_not_in_state` / `ref_invalid` / `out_of_service_scope` / `floor_denied` / `vault_miss` / `read_error` / `field_missing`) — a security trail on key brute-forcing and attempts at a foreign incarnation. (A malformed-request `422` before resolution and a nonexistent incarnation are NOT audited: there is nothing to attribute.) leak-guard tests on every sink (logs / audit / OTel / error text).
- **(c) No body-logging** — plaintext leaves the domain **only** in the HTTP response body (over the TLS transport); it does not get into any log / error text.
- **(d) Key-in-state** — `key` must ∈ the enumerate array of the current state (anti-arbitrariness).
- **(e) Traversal-guard** — `vault.ParseRef` over the resolved path (cuts `..`) + literal substitution instead of CEL.
- **(f) Positive namespace scoping of the Vault path** — the resolved path must lie under `secret/<service>/<incarnation>/` (the **main guard**, runtime) + load-time required `{service}`/`{incarnation}` + floor-backstop; see below "Closing the escalation".
- **(g) Vault-policy read prefix** — the Keeper reads the secret with its Vault policy having a read grant on a deterministic prefix (`secret/data/redis/*`), no wider.

### Closing the "arbitrary `vault_ref`" escalation

The service manifest is **not a trusted** input in the reveal threat model: the service author (or a compromised service repo) could declare a `vault_ref` on the Keeper's own secrets (`secret/keeper/jwt-signing-key`, `secret/keeper/sigil-keys/*`), on a **foreign service namespace**, or on a **foreign incarnation** — blast radius = the **cluster's signing keys**. A git review of the manifest does NOT close this escalation (a single boundary, bypassed by compromising the repo). Closed by **three code-level boundaries** (defense-in-depth, symmetric to the operator-input channel `input_vault` — "positive scope-match + unconditional floor"):

1. **Positive boundary (load-time):** manifest validation requires that `vault_ref` **contain BOTH placeholders — `{service}` AND `{incarnation}`** — the path must be per-service/per-incarnation-scoped; a path without scope placeholders (including a static keeper path) is rejected on load (diag **`vault_ref_not_service_scoped`**).
2. **Positive prefix allowlist (runtime, MAIN guard):** the resolved logical path **must start with** `secret/<service>/<incarnation>/` (`<service>` = `inc.Service`, `<incarnation>` = `inc.Name`), otherwise denied (`reason: out_of_service_scope`), `404`. **A trailing `/` is mandatory** — against prefix confusion (without it `redis-prod` would match `redis-prod-other`). The check is **after `vault.ParseRef`, before `ReadKV`**. It cuts **not only** keeper secrets, but also any foreign service namespace and any foreign incarnation: reveal physically reads ONLY under the secret namespace of its own incarnation of its own service. (`inc.Service` is validated by the segment pattern before substitution — anti-injection of `/`/`#`/`..`.)
3. **Floor-backstop (runtime):** `config.DeniedByVaultFloor` (the non-disableable system floor `secret/keeper/` / `secret/internal/`, the same one as the `input_vault` channel's) unconditionally before `ReadKV` (`reason: floor_denied`) — a safeguard for the **edge case of a service with the reserved name** `keeper`/`internal` (for such a service the positive allowlist would give `secret/keeper/<inc>/` — the floor finishes it off).

## Rejected alternatives

- **A redis-specific hardcoded endpoint** (`GET .../redis/users/{u}/password`). Rejected in favor of the generic `revealable_secrets` registry: reveal is a property of any service with secrets in state; hardcoding redis into the core is a dead end (every new class of service = a change to the Keeper).
- **CEL in `vault_ref`.** Rejected in favor of restricted placeholders `{service}`/`{incarnation}`/`{key}`: a computable language in the path to a secret — extra attack surface without benefit (real paths are parameterized by exactly the service, incarnation, and key).
- **Reusing `incarnation.get`.** Rejected: reveal is **strictly** more privileged than reading — removing masking must require a separate explicit right, otherwise any reader of an incarnation automatically sees its passwords.
- **A git review of the manifest as the only boundary.** Rejected: relying only on a code review of the `revealable_secrets` section (that the author will not write a keeper path) is insufficient — the blast radius of the escalation = the **cluster's signing keys**, and the manifest is not a trusted input. **A code-level guard is mandatory** (the positive prefix allowlist `secret/<service>/<incarnation>/` + required `{service}`/`{incarnation}` + floor-backstop `DeniedByVaultFloor`; see "Closing the escalation"), a git review is an additional layer, not a replacing one.

## Deferred (post-MVP, without breaking changes)

- **Singleton secrets without `enumerate`** — a single secret per incarnation (e.g. an admin password `secret/{service}/{incarnation}#password`), where there is nothing to enumerate (no array, `key` not needed). The MVP requires `enumerate` (a collection form); singletons are an additive extension (`enumerate` optional + reveal without `key`; `{service}`/`{incarnation}` remain mandatory) upon a real request.
- **The live manifest `community.redis`** carrying a `revealable_secrets` section — a change in the module's repository (a follow-up outside the core repo).
- **Threading config-extra-deny into the reveal handler.** Currently the floor is only the system floor (`DeniedByVaultFloor(logical, nil)`); the operator's `keeper.yml → vault.input_deny_paths` (additional deny prefixes, already in effect for the `input_vault` channel) is NOT yet threaded into reveal — a follow-up.
- **Auditing RBAC-403 at the gate level.** Denied branches are audited by the handler AFTER resolving the incarnation; a rejection at the middleware gate (`incarnation.view-secrets` not held at all → `403` BEFORE the handler) is not audited by this event — cross-cutting, common to all routes; a follow-up by a separate decision.

## Impact (implementation — NIM-74, outside this ADR)

`shared/config` (parsing + validation of `revealable_secrets[]`) + Operator API 2 endpoints (+OpenAPI drift-regen) + the reveal handler (scope gate + enumerate-guard + `vault.ParseRef` + the positive namespace allowlist `secret/<service>/<incarnation>/` + floor-backstop + `ReadKV`) + the audit event + the RBAC right (catalog [rbac.md](../keeper/rbac.md#каталог-permissions)) + the vault-policy read prefix + the companion UI (State view, reveal control) + leak-guard tests. **Implemented in NIM-74.**

## Relation to ADRs

- **[ADR-064](0064-secret-write-path.md)** — secret write-path (accepting plaintext FROM the operator). This ADR is the READ twin (returning plaintext BACK); **amends** its secret masking (a sanctioned reveal = removing the mask, not a leak).
- **[ADR-047](0047-purview.md)** — Purview scoped RBAC; **amends** it with the new right `incarnation.view-secrets` (scope as incarnation mutations, fail-closed 404 outside scope).
- **[ADR-053](0053-dependency-tiers.md)** — Vault hard-required (reveal reads from it).
- **[ADR-022](0022-audit-pipeline.md)** — the audit pipeline (the event `incarnation.secret_revealed`).
