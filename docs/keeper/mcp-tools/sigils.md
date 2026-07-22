# Sigil-key - MCP-tools for rotating Sigil signature keys

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.sigil.key.*` (rotation of trust-anchor-**signature keys** Sigil, [ADR-026(h)](../../adr/0026-sigil.md), R3-S7). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api/sigils.md](../operator-api/sigils.md). Tolerances of the binaries themselves (allow-list, separate zone) - [mcp-tools/plugins.md](plugins.md).

### Sigil-key (4)

Rotation of trust-anchor-**signing keys** Sigil ([ADR-026(h)](../../adr/0026-sigil.md), R3-S7; registry `sigil_signing_keys`, separate from tolerances `plugin_sigils`). 1:1 with REST `POST/GET /v1/sigil/keys` + `POST /v1/sigil/keys/{key_id}/primary` + `DELETE /v1/sigil/keys/{key_id}`. **3-segment tool-name `keeper.sigil.key.<verb>` ↔ 2-segment permission `sigil.key-<verb>`** (selector NoSelector). Business logic (key-gen, Vault-write, CRUD, publish `sigil:anchors-changed`) - in `sigil.KeyService`; tool - transport. Available only with Sigil configured; else `internal-error` ("sigil is not configured"). **Private is NEVER in the output/log.**

#### `keeper.sigil.key.introduce`

Keeper generates an ed25519 pair, writes the private to Vault KV (`secret/keeper/sigil-keys/<key_id>`), inserts the public part into the registry as active. `make_primary=true` makes the key primary. Permission: `sigil.key-introduce`. Endpoint: [`POST /v1/sigil/keys`](../operator-api/sigils.md). Async: no.

**Input:** `{make_primary?: bool}` (default false; body optional).

**Output:** `{key_id, pubkey_pem, is_primary, status, introduced_at}` - without private. Errors: `sigil-key-concurrent-change` (race primary, retry). Audit: `sigil.key-introduced`.

#### `keeper.sigil.key.list`

Active signing keys (primary first, without `vault_ref`). Permission: `sigil.key-list`. Endpoint: [`GET /v1/sigil/keys`](../operator-api/sigils.md). Async: no.

**Input:** empty object. **Output:** `{keys: array<{key_id, is_primary, status, introduced_at}>}`.

#### `keeper.sigil.key.set-primary`

Makes the active key primary (new Sigils are signed with it after cluster reload). Permission: `sigil.key-set-primary`. Endpoint: [`POST /v1/sigil/keys/{key_id}/primary`](../operator-api/sigils.md). Async: no.

**Input:** `{key_id}` (64-hex). **Output:** empty object. Errors: `sigil-key-not-found`, `sigil-key-concurrent-change` (primary race or retired key), `validation-failed`. Audit: `sigil.key-primary-set`.

#### `keeper.sigil.key.retire`

Outputs the key from the set (Soul forgets the next `SigilTrustAnchors`). Permission: `sigil.key-retire`. Endpoint: [`DELETE /v1/sigil/keys/{key_id}`](../operator-api/sigils.md). Async: no.

**Input:** `{key_id}` (64-hex). **Output:** empty object. Errors: `sigil-key-not-found` (no active record), `sigil-key-last-active` (last active), `sigil-key-primary` (primary directly - first set-primary to another), `validation-failed`. Audit: `sigil.key-retired`.
