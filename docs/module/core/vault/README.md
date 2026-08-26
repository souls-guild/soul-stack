# core.vault

Keeper-side core module for working with Vault KV secrets. One module (base-name
`core.vault`, Registry key) with dispatching by **state** (pattern
`core.cloud` / `core.choir`): author-address of the task - base + state.

| Author address | State | Destination |
|---|---|---|
| `core.vault.kv-read` | `kv-read` (verb) | Explicit reading of a secret from Vault KV with an audit-event entry. |
| `core.vault.kv-present` | `kv-present` | Generate-if-absent: guarantee the existence of secrets by generating the missing ones with a crypto-random value according to password-policy. |

**Keeper-side**, dispatcher `on: keeper` - both states are executed on the Keeper itself,
not on the host (unlike Soul-side core). Starting without `on: keeper` is an error
validation scenario. Implementation - [`kvread.go`](../../../../keeper/internal/coremod/vault/kvread.go)
(base module + `kv-read`), [`kvpresent.go`](../../../../keeper/internal/coremod/vault/kvpresent.go)
(`kv-present`), [`policy.go`](../../../../keeper/internal/coremod/vault/policy.go)
(the params form of the policy). The **grammar** itself - alphabets, length bounds,
validation - lives in [`shared/secretpolicy`](../../../../shared/secretpolicy/policy.go):
`generate_secret()` ([ADR-0083](../../../adr/0083-declared-secret-state-fields.md) §3)
mints by the same policy, and two parsers would drift.

Why these explicit state when there is an implicit `${ vault(...) }` in CEL: implicit-vault
cheap for rendering, but **doesn't** leave a separate entry in audit-trail and only
reads. `kv-read` - explicit form for cases requiring an audit event
`vault.kv-read` (PCI-DSS, SOC2, compliance-neat code). `kv-present` - write form
(on-site secret generation), which the CEL resolve does not have at all. implicit
`${ vault(...) }` remains for the render phase - these are different points.

> **The service's own namespace is fenced** ([ADR-0083](../../../adr/0083-declared-secret-state-fields.md) §7).
> A `path:` of `kv-read` or a `targets[].path` of `kv-present` under `<mount>/<service>/`
> is refused - at load when it is written out, and post-render when it is assembled by CEL
> (`config.ScanRenderedVaultParams`, called by the keeper dispatcher before `Apply`).
> That prefix is derived and owned by the platform: a secret of the service itself is a
> `state_schema` field with `type: secret`, written by
> a [`core.state.<verb>`](../../../keeper/modules.md#corestateverb) capture step. Both states stay
> available for every path OUTSIDE the prefix - a shared TLS CA, another service's
> credential - which is what they are for now.

---

# core.vault.kv-read

Explicit reading of secret from Vault KV (v1/v2, mount version is detected automatically)
on the keeper side with an audit event entry.

## kv-read — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Secret path in Vault, **mount-relative and without the `data/`** segment - the client will substitute `data/` for KV v2 (without it for KV v1). Specify `secret/shared/db-admin`, not `secret/data/shared/db-admin` (otherwise the client will build `secret/data/data/shared/db-admin` and the secret will not be found). The path must lie outside the service's own `<mount>/<service>/` prefix - see the fence above. The KV mount version (v1/v2) resolves to keeper-side `vault.Client` (autodetection via probe, or override `vault.kv_version`, see [config.md → vault](../../../keeper/config.md#vault)); the module already receives a **flat payload** and works identically on both versions (for v2, the `data.data` wrapper is unpacked by the client before being transferred to the module). |
| `fields` | array of string | optional | Which keys to return to `data`. Empty / not specified → entire payload. A requested but missing key is **passed without error** (audit-event has already been spent on reading). |

## kv-read — capabilities / side-effects

- **Keeper-side, does not touch the host.** Reading from Vault on the keeper side; on Soul
nothing is delivered.
- **Does not mutate state** (read operation, `changed=false`).
- **Writes audit-event** `vault.kv-read` (if audit-writer is configured) with
payload `{path, fields}` - **read fact only**, no secret values.
Audit fails the step (otherwise the mandatory compliance step will silently disappear).

## kv-read — output / register

`kv-read` returns to `register.<name>.*`:

| Field | Type | Description |
|---|---|---|
| `path` | string | Echo the requested path. |
| `data` | object | Extracted key→value (after filter `fields`). |
| `fields` | array of string | List of keys in `data` (sorted). |

Plus standard `.changed` (always `false`) / `.failed` DSL cores.

> **WARNING (security).** The secret values ​​themselves in **audit-payload are not
> hit** - only `path` + list `fields` is captured. In register-output
> values are present (`data.*`), but are masked on the write-path
> destiny/scenario via [`audit.MaskSecrets`](../../../../shared/audit/) (known
> secret keys) on all outputs - logs / OTel / UI / reports. CEL processes
> values are normal; masking is on the way out.

## kv-read - example

```yaml
# Explicit reading of the secret on the keeper side for the sake of audit-event vault.kv-read.
# on: keeper is required - this is a keeper-side core. fields optional: without it
# the entire payload will be returned.
- name: Read the shared DB credentials from Vault (audit-tracked)
  on: keeper
  module: core.vault.kv-read
  register: db_creds
  params:
    path:   secret/shared/db-admin
    fields: [username, password]
```

(minimum valid example: calling `core.vault.kv-read` to `examples/`
not yet - see deferred note below).

> **Deferred (backlog).** There are currently no calls in `examples/`
> `core.vault.kv-read` - the example above is compiled as a minimum valid one
> code contract (`path` required, `fields` optional). Replacing with a link
> for a real scenario-example is postponed until the corresponding
> use-case (compliance-accurate read with audit-event).

---

# core.vault.kv-present

Generate-if-absent for Vault KV secrets: for each target ensures that
the specified field exists and is not empty; missing - generates crypto-random
value according to the **password-policy** described by the author (length in characters + alphabet),
present - leaves as is (**does not overwrite**). Author address - base
`core.vault` + state `kv-present`. Twin pair `kv-read` on the **same** module
([ADR-017 amendment 2026-06-28](../../../adr/0017-keeper-side-core.md)).

Purpose - the service **itself** generates missing passwords when `create`, the operator does not
need to pre-sow secrets manually `vault kv put`. Typically - bootstrap secrets
service during first deployment (redis: master password + per-user ACL passwords).

## kv-present - semantics

- For each target the path + field is read (a non-existent path is not an error, but
empty payload: all its fields will be generated).
- **MISSING** (there is no field, it is `null` or **empty string**) → generated
value according to policy and is written `WriteKV`. An empty string is interpreted as "no"
(empty password is useless).
- **PRESENT** (non-empty string) → **no-op**, the value is not affected.
- `changed=true` **only** when something is actually generated; if all the secrets
already were - `changed=false`.
- **Idempotent.** Re-run/re-create is safe: already existing
secrets are reused, new KV versions are not created unnecessarily.
- **Several targets on one path** (different fields) merge into **one**
`WriteKV` over existing path fields (read-merge-write) - adjacent fields are not
are lost, extra KV versions are not produced.
- **`destroy` does NOT clear secrets** - re-create reuses the same passwords
(this state does not perform rotation/deletion of secrets; rotation is a separate scenario).

## kv-present — params

Step-level `policy` sets the general generation default for all targets without their own
`policy`; per-target `policy` overrides it by fields. Absent both here and there
→ default (`length: 32`, `charset: ascii-printable-safe`).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `policy` | object | optional | Step-level default password-policy (see below). |
| `targets` | array of objects | **required, non-empty** | What to guarantee. An empty/missing list is a validation error (the module has nothing to do). |
| `targets[].path` | string | required | Vault KV-path (mount-relative, without `data/` and **without** `#field` - the field is set separately). Same path convention as `kv-read`. |
| `targets[].field` | string | optional (default `password`) | The name of the field inside the secret. |
| `targets[].policy` | object | optional | Override password-policy for this target over step-level. |

### password-policy (object `policy`)

| Field | Type | Required/default | Meaning |
|---|---|---|---|
| `length` | int | optional (default `32`) | The final length of the password **in characters** (not in bytes of entropy). Bounds `8..1024`; out of bounds—validation error. |
| `charset` | string (enum) | optional (default `ascii-printable-safe`) | Named alphabet preset: `alphanumeric` / `hex` / `base64url` / `ascii-printable-safe`. **Mutually exclusive with `allowed_chars`.** |
| `allowed_chars` | string | optional | Explicit alphabet of allowed characters (duplicates are collapsed). The alphabet must have **≥ 2** different characters. **Mutually exclusive with `charset`.** |

`charset` Presets:

| Preset | Alphabet |
|---|---|
| `alphanumeric` | Latin alphabet of both registers + numbers (safe everywhere; already by entropy per character). |
| `hex` | lowercase hex digits `0-9a-f`. |
| `base64url` | url-safe base64 (`-`/`_` instead of `+`/`/`, without `=`). |
| `ascii-printable-safe` (default) | printed ASCII `0x21..0x7E` **minus** characters that break redis.conf/users.acl/shell substitution: space, `"`, `'`, `#`, `\`, and `` ` `` and `$`. Default - the password should not break the target config. |

> `charset` and `allowed_chars` cannot be specified together (alphabet ambiguity) -
> this is a validation error. Empty `allowed_chars` / unknown `charset` —
> is also an error.

## kv-present — capabilities / side-effects

- **Keeper-side, does not touch the host.** Write to Vault on the keeper side; on Soul
nothing is delivered.
- **Writes to Vault KV** (`WriteKV`) only paths with actually generated fields,
one merge over the existing payload path.
- **Mutates state conditionally:** `changed=true` only during actual generation;
no-op (everything has already happened) → `changed=false`.
- **Writes audit-event** `vault.kv-present` (if audit-writer is configured)
**only with `changed=true`** with payload `{paths}` (path → list
generated fields, **no values**). Audit fails a step.

## kv-present — output / register

`kv-present` returns to `register.<name>.*`:

| Field | Type | Description |
|---|---|---|
| `generated` | object | Map `<vault-path>` → **sorted list of names** of generated fields. **No values.** Empty if nothing is generated. |

Plus standard `.changed` / `.failed` DSL cores.

## kv-present - safety (★ ADR-010)

- **The generated value NEVER goes outside.** Neither in register-output nor
in audit-payload, neither in the logs, nor in OTel, nor in the error text. Outside - only
fact + `path` + **names** of generated fields
  ([`kvpresent.go`](../../../../keeper/internal/coremod/vault/kvpresent.go),
reference `sigil.KeyService.Introduce`). `WriteKV` / `ReadKV` errors are only
`path` (invariant `vault.Client`). This invariant is secured by the guard test
(`kvpresent_test.go::TestPresent_SecurityNoLeak` - recursively checks
missing value, including substring, in the entire output and payload tree).
- **crypto/rand, bias-free.** Randomness source - `crypto/rand` (NOT
`math/rand`); the symbol index is chosen uniformly (`rand.Int` - rejection
sampling, without modulo-skew).
- **Keeper-side, not Soul-side - `root`/capability semantics are not applicable.** Generation
is in process of Keeper (`on: keeper`); manifest with `required_capabilities`
there is no module (keeper-internal operation, not host plugin). Running scenario with this
step is controlled by the RBAC operator ([rbac.md](../../../keeper/rbac.md)).
- **Requires Vault-auth Keeper.** Reading and writing are performed by the client
`keeper/internal/vault` under the Keeper cluster account - the module does not accept
token/creds in params and does not increase access; he writes exactly where it is allowed
Keeper policy in Vault.

> **WARNING (security).** Unlike `kv-read` (where values are present in
> register-output and are maintained by masking), `kv-present` **does not carry value names
> at all** - neither output nor audit-payload contain generated secrets,
> paths and field names only.

## kv-present - example

The redis `create` scenario used to open with this step, generating the master password
and the per-user ACL passwords before anything read them back through `${ vault(...) }`.
It no longer does, and it no longer could: those are the service's **own** secrets, so
they are now `state_schema` fields with `type: secret` minted by a `core.state.<verb>`
capture step ([ADR-0083](../../../adr/0083-declared-secret-state-fields.md) §1, §4,
[ADR-0084](../../../adr/0084-explicit-state-capture.md)), and
`secret/redis/<inc>` is exactly the prefix §7 fences. No scenario in `examples/` calls
`kv-present` any more.

What remains is the case the fence leaves open - guaranteeing a secret **outside** the
service's own namespace, where nothing derives a path for you:

```yaml
# A credential shared by several services: no single service owns the path, so no
# state_schema derives it. on: keeper is required. policy is a real YAML map (not a
# CEL string), and `targets` is a one-liner CEL-${…} when it is computed.
- name: Ensure the shared metrics-scrape credential exists
  on: keeper
  module: core.vault.kv-present
  params:
    policy:
      length: 32
      charset: alphanumeric
    targets:
      - path: secret/shared/metrics-scrape
        field: password
  register: scrape
```

> `targets` computed by CEL must be a one-liner `${…}`, not a block-scalar `>-`:
> module.params type-check skips the CEL wrapper only for a string scalar, and a
> block-scalar would be parsed as a literal and rejected as a list-param.

> **Passage-invariant.** A step that writes to Vault must run **before** the render
> phase of any task reading the same path through `${ vault(...) }` (staged-render
> [ADR-056](../../../adr/0056-staged-render-passage.md)) - a render phase resolves what
> is there at the time, and there is no re-render after a later write. For the service's
> own secrets that edge is now a `core.state.<verb>` step → register, guarded by
> [`keeper/internal/trial/redis_secret_mint_test.go`](../../../../keeper/internal/trial/redis_secret_mint_test.go).
> For a shared path like the one above the same rule applies, and the ordering is the
> author's to establish.

## See also

- [README.md](../../README.md) - directory of core modules.
- [keeper/modules.md](../../../keeper/modules.md) - regulatory spec for Keeper-side core modules (`on: keeper` manager).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-step-target---on) - `on:`, step manager between the Soul side and the Keeper side.
- [templating.md](../../../templating.md) - vault-resolve phase and implicit `${ vault(...) }` in CEL.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-0083](../../../adr/0083-declared-secret-state-fields.md) - declared secret state fields; §7 fences the service's own Vault namespace against both states of this module, §4 introduces the keeper-side state write that replaces `kv-present` for a service's own secrets.
- [ADR-0084](../../../adr/0084-explicit-state-capture.md) - explicit state capture; the write is addressed `core.state.<verb>` and lands at the step.
- [ADR-017](../../../adr/0017-keeper-side-core.md) - Keeper-side core modules; `kv-read` (explicit vs implicit vault), `kv-present` (amendment 2026-06-28, generate-if-absent + security-invariant).
