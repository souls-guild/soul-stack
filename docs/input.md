# input - standard for describing input parameters

This document is the **mandatory standard** for the `input:` block format in Soul Stack. Applies equally to **destiny**, **scenario** and **module manifest**. One DSL - one place of truth.

> **Note for authors and AI agents.**
> - All "parameter validation options" are documented **here**, not by location.
> - If there is a need for a key that is not in the tables below, this is a new format entity. First propose-and-wait on [CLAUDE.md](../CLAUDE.md), then editing this file, then everything else.
> - If there is a discrepancy between this file and the actual implementation / manifests / examples, this file takes precedence. The rest is brought to him.

## Where applicable

| Where | File | Block | Template link | Validated |
|---|---|---|---|---|
| destiny | `destiny.yml` | `input:` | `{{ input.<name> }}` | Keeper upon invocation + soul before apply ([destiny/input.md → Where is validated](destiny/input.md)) |
| scenario | `scenario/<name>/main.yml` | `input:` | `{{ input.<name> }}` | Keeper at script start |
| module manifest | manifest in the module itself (see ["Module Manifest"](architecture.md#module-manifest)) | `input:` inside each `state` | n/a (validated before Apply) | soul before calling the module state form |

Linter ([soul-lint](soul-lint.md)) checks the schema statically in all three cases.

## Resolving values in runtime

The schema (`default`/`required`/`type`/`pattern`/…) describes **what** the value should be. At runtime, BEFORE the render phase (CEL/text-template), the operator-passed values are converted to **effective input** in one step:

1. **Merge defaults.** For each parameter for which `default` is declared, but the value is not passed, `default` is substituted. After this step, `${ input.<name> }` always resolves for default parameters - even if the operator did not specify them.
2. **Checking `required` / `required_when`.** Parameter with `required: true` without the passed value and without `default` - resolve error (clear: names the parameter), the run does not reach render. Parameter with `required_when: "<CEL>"` is required **conditionally** - the error is raised only if the predicate over the effective input is true AND the value is missing (see ["Conditional Required"](#conditional-required_when)). This is a runtime check of the passed values, and not a static check of the schema (the latter is done by the linter).
3. **Validation of passed values** against schema: matches `type`, matches `enum`, matches `pattern` (for `type: string`). For `array`/`object`, validation is recursive: each array element is checked against the `items` schema, each object field is checked against `properties.<name>`, plus the presence of `required` object fields is checked (with a clear path in the error, for example `$.users[1].acl`).

> **Expression values.** If the passed value is an expression (`${ … }` / `{{ … }}`), it **is not validated** against `type`-`format` / `enum` / `pattern` at its level (at any nesting depth): the final form will appear only after the render phase (CEL/text-template), it is unknown here. This is a conscious decision, not a blank - the correctness of the result of the expression remains the responsibility of the operator. For parameters with `secret: true`, the value-expression still goes through the usual vault resolution and masking in logs/traces/UI.

Where it happens:

- **scenario** — Keeper when starting the script (scenario-runner before render). Effective input goes to both `${ input.<name> }` tasks and `state_changes.sets`.
- **destiny / module manifest** - where the block is validated (see the table "Where it is used"). `apply: input:` destiny defaults are resolved in an isolated destiny render pass.

> **Empty lines.** An empty line `""` for `type: string` without `allow_empty: true` is treated at this step as "no value passed" (see ["Empty lines"](#empty-lines)): `default` is applied or `required` error is raised.

> **One source of truth merge.** This resolution is the same in production and in L0-trials (soul-trial): an L0-case can only submit mandatory input, defaults will be corrected in the same way as in production. A case that provides all values ​​explicitly should not mask the absence of a merge phase.

## Reading optional-without-default input: canonical has()-guard

A parameter with `required: false` and **without** `default` after the merge phase may be **missing** in the effective input (the operator did not pass a value - there is nothing to substitute). A direct link `${ input.<name> }` to such a parameter crashes in the CEL-render phase with error `no such key: <name>`, and the run does not reach the end.

**Canon.** Any reading of an optional-without-default input turns into a has()-guard:

```yaml
# string: none → empty string
version: "${ has(input.redis_version) ? input.redis_version : '' }"

# object: absence -> empty object
config:  "${ has(input.config) ? input.config : {} }"

# array: missing -> empty list
extra:   "${ has(input.extra) ? input.extra : [] }"
```

The fallback literal is selected by the `type` parameter (`''` / `{}` / `[]` / `0` / `false`) - and must match in meaning what the value consumer expects (for example, for `core.pkg` the empty `version` = "without `=version`-pin, latest").

> **Short form is `default(x, y)`.** A pure value-or-default over a select-chain is written via the CEL function [`default(x, y)`](templating.md): `default(input.config, {})` ≡ `has(input.config) ? input.config : {}`, `int(default(essence.tls_port, 7379))` ≡ `int(has(essence.tls_port) ? essence.tls_port : 7379)`. Equivalent, without a greedy crash on a missing key (macro is expanded into the same has() ternary in the compile phase). Applies to optional-without-default `input.*` and optional-fields `essence.*`. The conditional construction of map (`has(x) ? {key: x} : {}`) and calculated fallback (`has(x) ? <arithmetic> : ...`) under `default()` **do not fall under** - an explicit ternary remains there.

> **Guard is needed EVERYWHERE where optional-without-default input** is read - not only in `params:`/`apply: input:` tasks, but also in **`state_changes.sets`** (and in any other CEL expressions `state_changes`). This is a frequent source of a hidden bug: tasks may not read the parameter (or read through guard), but `state_changes.sets` fixes it to `incarnation.state` directly - an unprotected link there drops the rendering of sets already AFTER successfully apply on the hosts, translating incarnation to `error_locked`. L0-test (soul-trial) renders `state_changes` along with tasks, so it catches such a case without a separate assertion.

> **When guard is NOT needed.** The parameter with `default` or with `required: true` after the merge phase is always present in the effective input (see "Resolving values ​​in runtime") - for it `${ input.<name> }` is written directly, without a guard.

## Available types

`type` is the only required key for each parameter.

| `type` | Description | Example of a valid value |
|---|---|---|
| `string` | Line. Supports regex, format, lengths. | `"hello"`, `"redis-master-01"` |
| `integer` | Signed integer. | `42`, `-7` |
| `number` | Number (integer or fraction). | `3.14`, `100` |
| `boolean` | True/false. | `true`, `false` |
| `array` | List of values. The element type is in `items`. | `[a, b, c]` |
| `object` | A structure with named fields. Fields are in `properties`. | `{ key: value }` |

## Shared keys (available on any `type`)

| Key | Type | Default | Description |
|---|---|---|---|
| `type` | string | — *(required)* | Value type. See table above. |
| `required` | boolean | `false` | The parameter is required **unconditionally**. `false` - absence allowed. |
| `required_when` | string | — | The parameter is required **conditionally** - when the CEL predicate over `input.*` is true. See ["Conditional Mandatory"](#conditional-required_when). |
| `default` | (by `type`) | — | Default value if no parameter is passed. Must match `type` (linter checks). Implies `required: false`. |
| `prefill_from_state` | string | — | The path `state.<path>` to `incarnation.state`, whose **current** value the UI substitutes as a pre-fill day-2 form. This is a UI tooltip, **not** part of the value resolver (NOT a default). See ["Pre-fill from state"](#pre-fill-from-state-prefill_from_state). |
| `enum` | array | — | List of valid values. Any value must be included in the list. |
| `secret` | boolean | `false` | The value is masked in logs, traces, reports and UI. For passwords, tokens, keys. |
| `description` | string | `""` | Human-readable description. Used in UI, MCP, `soul-lint` help. |

> **Hint.** `enum` is compatible with other restrictions: you can specify `type: string`, `enum: [...]`, `secret: true` at the same time. The linter will additionally check that the values ​​in `enum` match `type`.

### Conditional: `required_when`

`required: true` makes the parameter **always** required. Often you need something else: the parameter is required **only in one mode**. Example from the `redis` service: the number of shards `shards` makes sense only in `redis_type == 'cluster'` mode—there is no need to set it in `sentinel` mode. The unconditional `required: true` is incorrect here (it would reject a valid `sentinel` run without `shards`), and the scheme does not know how to express "mandatory subject to condition".

For this - `required_when: "<CEL predicate>"`:

```yaml
input:
  redis_type:
    type: string
    enum: [sentinel, cluster]
    default: sentinel
  shards:
    type: integer
    min: 3
    required_when: "input.redis_type == 'cluster'"      # required only in cluster
```

**Semantics.** At the input stage (after merge defaults, BEFORE the render phase), the predicate is evaluated over the **effective input**. If the predicate is true AND the parameter is missing (not passed and without `default`) - a resolve error, like `required: true`, but conditional. If the predicate is false, the absence of a parameter is acceptable.

**Predicate context is `input.*` only.** This is input validation, not render: `essence.*` / `soulprint.*` / `register.*` / `incarnation.*` / `vault()` / `now()` in `required_when` **not available** (the predicate must depend only on other input values of the same run). Linter (`soul-lint`) checks that the expression is a parsable CEL.

**Contrast with `required: true`.** `required: true` - absolutely required (missing → always an error). `required_when: "<expr>"` is required when `<expr>` is true. There is no point in specifying both on one parameter: the unconditional requirement absorbs the conditional one (the linter does not prohibit it, but `required_when` with `required: true` is useless).

> **Why not shell-guard.** Previously, the conditional requirement was written with a guard task on the host (`core.cmd.shell` with `test "${ has(input.X) }" = true || exit 1`) - an arbitrary shell for the sake of control-flow, executed on Soul. `required_when` transfers the check to the input stage of the Keeper declaratively: the operator receives an error before the start of the run, the attack-surface of an arbitrary shell does not grow.

> **`required_when` (presence) vs `validate:` (ratio).** `required_when` answers "is this field required?" When you need to check the **correlation of several input fields** ("`sentinel_quorum` is not more than `1 + replicas`"), this is not about presence - for this, the script has a top-level section `validate:` (declarative input-invariants, the same input-only context, the same 422 `validation-failed`). Speca - [scenario/orchestration.md §2.5](scenario/orchestration.md).

### Pre-fill from state: `prefill_from_state`

Day-2 scenario rules an already existing incarnation. Often it is more convenient for the operator to open the form **with the current values** of the corresponding fields and correct the delta, rather than enter everything again. Example from the `redis` service: the `update_config` form for the `redis_version` field should open with the `redis_version` that is currently written in `incarnation.state`.

For this - `prefill_from_state: state.<path>`:

```yaml
input:
  redis_version:
    type: string
    prefill_from_state: state.redis_version          # substitute the current value from state
  max_memory:
    type: string
    prefill_from_state: state.config.max_memory       # nested path - dot notation
```

**Path syntax.** Root is `state`, then ≥1 segment through dot (`state.<field>[.<field>…]`). Segments - snake_case (`[a-z][a-z0-9_]*`). Same root `state` + dot notation as [statepredicate (ADR-047)](keeper/rbac.md), but here it is a **literal path-reference to a single value**, not a CEL predicate. Linter (`soul-lint`) checks the path syntax (broken/without root `state`/with a foreign root → `input_prefill_from_state_invalid`).

**How ​​it works.** The UI calls `POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill` (permission `incarnation.get`); backend reads `incarnation.state` of the specified incarnation, resolves **only** the `prefill_from_state` paths declared in the schema, and returns `{values: {field: current-value}}`. Fields whose path is not present in the current state are **omitted** (the form will open with an empty field). The client does not pass the path - the backend takes many paths strictly from the script diagram (no arbitrary reading of state through the endpoint).

**Secret fields are excluded.** A field whose state path is marked `secret` (in `state_schema` of the service or as secret-input) is **excluded completely from the prefill response** - the mask pre-fill (`***`) is useless, and you cannot give the raw secret to the form.

**Border with `default`.** These are two different keys with different roles - they **coexist** on the same field:

| | `default` | `prefill_from_state` |
|---|---|---|
| When to use | when resolving values ​​(merge phase), if the parameter is not passed | when preparing day-2-form (UI) |
| Source of value | static literal from schema | current `incarnation.state` |
| Gets into effective input | **yes** (if the parameter is not passed) | **no** - UI tooltip only |

`default` is "what to fill in if the operator is silent" (part of the effective input). `prefill_from_state` is "what to show the operator on the form as a starting point"; it is **not** included in the resolution of values ​​(`${ input.<name> }`, `state_changes`). Therefore, `incarnation.state` does not flow through `prefill_from_state` into effective input - this is a structural guarantee, not a convention.

## Reusable named types: `types:` + `$type`

It's not uncommon for the same composite type to appear in **multiple scenarios** of the same service. Example from the `redis` service: the ACL user record `{name, perms, state}` is needed in `add_user` (one object), in `update_acl` (one object) and in `create` (an array of such objects). Duplicate it inline in each `input:` - source of drift: edit `required` / `additional_properties` in one place does not reach others.

For this purpose - a **named type**, declared once and reused by reference. Committed [ADR-062](adr/0062-input-types.md).

### Announcement: file `service/<name>/types.yml`

Section `types:` - map `<Name>` → circuit in the **same** input-DSL described in this document (`type` / `properties` / `required` / `items` / `enum` / `pattern` / `format` / ... - the entire dictionary, including nesting). No external JSON Schema.

Type name - `PascalCase` (`^[A-Z][A-Za-z0-9]*$`): distinguishes a reference type from a parameter name (snake_case) both visually and in the parser.

```yaml
# service/redis/types.yml
types:
  AclUser:
    type: object
    additional_properties: false
    required: [name, perms, state]
    properties:
      name:  { type: string, pattern: "^[a-zA-Z0-9_-]+$" }
      perms: { type: string }
      state: { type: string, enum: [on, off] }
```

### Link: `$type: <Name>`

`$type` is placed as an **independent field** (for a single object of a declared type) or under `items:` (for an array of such elements):

```yaml
# scenario/add_user/main.yml
input:
  user:
    $type: AclUser            # single object of type AclUser

# scenario/create/main.yml
input:
  users:
    type: array
    items:
      $type: AclUser          # array of elements of type AclUser
    min_items: 1
```

`$type` - **resolution directive**: at the input stage it is replaced by the expanded type schema from `types:`. After resolution, the usual input-DSL continues to work - value validation is recursive, exactly like inline-`object` / `array`.

### Nesting type→type and cycle-detection

A type can refer to another type (`$type` inside a `properties` / `items` declared type). The resolver traverses the link graph and is **obliged** to catch the loop (`A → B → A`, including the self-link `A → A`) - this is an error `input_type_cycle`, not an infinite sweep. The depth of nesting is not limited by number; limitation - lack of a cycle.

### Resolve - service-level

The name `$type` is looked up **only** in `types:` of the same service:

- **NOT** local-per-scenario - types are not declared inside `scenario/<name>/`, only in service-level `types.yml`;
- **NOT** cross-service - cannot reference another service type.

Resolution occurs at the input stage of Keeper (the same phase as merge defaults and `required`-check): `$type` is expanded into a type schema, then a regular resolve (merge → required → value-validation) occurs on the expanded schema. `$type` does not reach the render phase - this is a structural unwrapping, not a value.

### DTO `/v1/scenarios` — backend-resolve + `x-type`

When projecting a script schema into the DTO of the endpoint of the backend script directory **resolves `$type` BEFORE projection**: the client receives an **already expanded** inline schema (UI builds the form using a familiar format, without knowing about `types:`) plus the forward-compat annotation **`x-type: <Name>`** on the node where it stood `$type`. The UI ignores it today; for growth, it allows a specialized widget for a named type without breaking current clients. `x-type` is a read-only DTO annotation; it is not written in the YAML source.

### Error classes

| Code | When |
|---|---|
| `input_type_unknown` | `$type: <Name>` refers to a type that is not present in the service's `types:`. |
| `input_type_cycle` | Loop in type reference graph (`A→B→A`, self-reference `A→A`). |
| `input_type_duplicate` | Duplicate name in section `types:`. |
| `input_type_ref_conflict` | `$type` is specified **together** with an inline scheme on the same node (`type:` / `properties:` / `items:` / ...) - they are mutually exclusive: the node is either `$type` or its own scheme. |

### MVP Limits

- **`object` + `array-of-type` + nesting type→type** - supported (with mandatory cycle-detection).
- **Scalar-alias** (for example `Port: {type: integer, min: 1, max: 65535}` and `$type: Port` on a scalar field) - **not included** in MVP. Expansion is possible later using the same form, without breaking change (separate propose-and-wait).
- **Generics / parameterized types** - not included.
- **Cross-service** and **local-per-scenario** type declarations are not included.

## Type `string`

| Key | Type | Default | Description |
|---|---|---|---|
| `pattern` | string | — | RE2-regex (Go-regexp syntax). A **complete** match is checked, not a partial one. |
| `format` | string | — | Predefined format (see table below). Replaces regex for known types. |
| `min_length` | integer | — | Minimum line length (in Unicode codepoints). |
| `max_length` | integer | — | Maximum line length. |
| `allow_empty` | boolean | `false` | Allows the empty string `""` as a valid value. By default, `""` is interpreted as "no value passed" - see below. |
| `vault_scope` | string | — | Prefix-glob allowing the operator to pass `vault:`-ref as a field value (scoped resolve on the keeper side). Applies **only** to `secret: true`. See ["vault_scope"](#vault_scope-scoped-resolve-vault-ref-in-operator-input). |

### Empty lines

**Default rule.** An empty string `""` for a parameter of type `string` is treated **as no value** - equivalent to the parameter not being passed. Then the usual rules apply:

- `required: true` → error (as if the value did not exist).
- `default` is defined → `default` is used.
- Otherwise → the parameter is considered missing.

The choice was made deliberately for config management: "empty password", "empty hostname", "empty command" - almost always not an intention, but an underfilled field. Quietly taking `""` in this context is dangerous.

**Opt-in: `allow_empty: true`.** If an empty string is really needed as a valid value (a rare case - remove previously-set value, "no config" marker), enable the flag explicitly:

```yaml
input:
  note:
    type: string
    allow_empty: true        # "" is a valid value, not "absent"
```

With `allow_empty: true`, the empty string is passed as a normal value, and the rest of the rules apply (including `min_length` if it is specified - but `allow_empty: true` + `min_length >= 1` is considered a contradiction by the linter).

### `vault_scope`: scoped-resolve `vault:`-ref in operator-input

By default, the value of the secret input field is a **ready literal** (password, token), which the operator passes directly or which the script reads from Vault itself through the CEL function `${ vault(...) }` in the render phase (trusted author channel, [templating.md §2.3/§4](templating.md)). The raw `vault:` value in the operator-input itself is default-deny: otherwise the operator could force Keeper to read any path of its Vault token - including `secret/keeper/jwt-signing-key`.

The `vault_scope` key opens a **restricted** channel for the field: the operator can pass `vault:<mount>/<path>[#<field>]` as a value, and Keeper will resolve it **keeper-side**, but only within the declared prefix-glob.

```yaml
input:
  redis_password:
    type: string
    required: true
    secret: true                         # required for vault_scope
    min_length: 16
    vault_scope: "secret/services/redis/*"   # one prefix-glob
    description: Redis password. Can be a literal or scoped vault:-ref.
```

With this scheme the operator sends:

```jsonc
// effectively resolves to the value of the secret/services/redis/prod#password field
{ "redis_password": "vault:secret/services/redis/prod#password" }
```

**Rules.**

- **Applicability.** `vault_scope` is only valid on `type: string` + `secret: true`. On a non-secret field - schema error (`input_vault_scope_requires_secret`); on non-string - `input_key_invalid_for_type`.
- **Form.** One prefix-glob `<mount>/<path-prefix>/*` (single trailing `*` = prefix-match) or exact logical path `<mount>/<leaf>` (without `*` = exact match). Intermediate `*` are not supported. Invalid form - schema error (`input_vault_scope_invalid`).
- **Default-deny.** The field **without** `vault_scope`, the value of which is passed `vault:`-ref, is **resolve error**, not a literal. Only a literal or (for the author's channel) `${ vault(...) }` in the script itself.
- **Logic for resolving one ref:** is there `vault_scope`? no → reject. Path match scope? no → reject. Path to hard deny-list? yes → reject. Otherwise, read Vault KV.
- **Hard deny-list (insurance).** Paths under `secret/keeper/*` and `secret/internal/*` are denied **always**, of course, **even if** `vault_scope` mistakenly covers them (for example `secret/*`). Checked **after** the scope match. This system-floor cannot be disabled by the config - only added (`keeper.yml → vault.input_deny_paths`).
- **When it resolves.** Once on the keeper side, in the input resolve phase: **merge defaults → scoped vault resolve → value validation**. `pattern`/`enum`/`min_length` are checked against the **already resolved** value, not against the `vault:...` line. The resolved value then goes to render as a regular secret (it is masked in logs/trace/UI).
- **Audit.** Each resolution (successful and rejected) writes audit-event `input.vault_resolved` from `field` / `incarnation` / `scenario` / `aid` initiator / **logical way** Vault (path is not a secret) / `result` (`ok`/`denied`) and `reason` for refusal. The secret value is **not** included in the audit. The refusal is audited as a security signal.

> **Border with the author's channel.** `vault_scope` concerns **only** operator-input values. Copyright `vault:`-refs and `${ vault(...) }` in `params:` tasks - a separate trusted channel (the author of the service), it does not require `vault_scope` and does not fall under the deny-list of this channel.

### Valid values `format`

| `format` | What does it validate | Example of a valid value |
|---|---|---|
| `hostname` | RFC 1123 hostname (no dots, no TLD) | `redis-01` |
| `fqdn` | Fully qualified domain name | `redis-01.prod.example.local` |
| `ipv4` | IPv4 address | `10.0.0.1` |
| `ipv6` | IPv6 address | `2001:db8::1` |
| `cidr` | CIDR (IP + prefix length) | `10.0.0.0/24` |
| `email` | Email (RFC 5322 compliant) | `ops@example.com` |
| `uri` | URI / URL | `https://example.com/path` |
| `uuid` | UUID of any version | `550e8400-e29b-41d4-a716-446655440000` |
| `semver` | Semantic version | `1.4.2`, `2.0.0-rc1` |
| `duration` | Go duration syntax | `30s`, `5m`, `1h30m` |

> **Hint.** If there is a suitable `format` for the value, use it instead of `pattern` - it's better readable, consistent between destiny, cheaper to review, less likely to make a regex with an error.

## Type `integer` / `number`

| Key | Type | Default | Description |
|---|---|---|---|
| `min` | number | — | Minimum, **inclusive**. |
| `max` | number | — | Maximum, **inclusive**. |
| `exclusive_min` | number | — | Minimum, **exclusive**. Do not combine with `min`. |
| `exclusive_max` | number | — | Maximum, **exclusive**. Do not combine with `max`. |

> **Hint.** For ports - `{ min: 1, max: 65535 }`. For always-positive values, `min: 0` or `exclusive_min: 0` depending on whether zero is considered valid.

## Type `array`

| Key | Type | Default | Description |
|---|---|---|---|
| `items` | schema | — *(required)* | The diagram of each element of the array. Recursively the same DSL. |
| `min_items` | integer | — | Minimum number of elements. |
| `max_items` | integer | — | Maximum number of elements. |
| `unique` | boolean | `false` | Elements must be unique. |

## Type `object`

| Key | Type | Default | Description |
|---|---|---|---|
| `properties` | map | — *(required)* | Map `<name>` → schema. Describes the fields of an object (recursively). |
| `required` | array of string | `[]` | List of required keys inside `properties`. |
| `additional_properties` | bool/schema | `true` | `true` - undescribed keys are allowed (open); `false` - prohibited (closed); schema - undescribed keys must match it. |

> **Hint:** For a strict structure (no extra fields) use `additional_properties: false`. Open by default for compatibility and extensions.

## Examples

### Minimum required

```yaml
input:
  name:
    type: string
    required: true
```

### With enum and default

```yaml
input:
  action:
    type: string
    required: true
    enum: [apply, restart, ping, stop]
    description: What to do with the service.

  log_level:
    type: string
    default: info        # implies required: false
    enum: [debug, info, warn, error]
```

### Format and regex

```yaml
input:
  master_host:
    type: string
    required: true
    format: hostname      # RFC 1123 - no regex needed

  redis_version:
    type: string
    pattern: "^[0-9]+\\.[0-9]+\\.[0-9]+$"   # own check, format is not suitable
```

### Numeric boundaries and secret

```yaml
input:
  port:
    type: integer
    default: 6379
    min: 1
    max: 65535

  password:
    type: string
    required: true
    secret: true                # is masked in the logs
    min_length: 16
    description: Master-auth password for Redis.
```

### Nested object

```yaml
input:
  cloud:
    type: object
    required: true
    properties:
      provider:
        type: string
        enum: [aws, gcp, yandex]
      region:
        type: string
        pattern: "^[a-z0-9-]+$"
      count:
        type: integer
        min: 1
        max: 50
    required: [provider, region, count]
    additional_properties: false
```

### Array with uniqueness and format

```yaml
input:
  allowed_users:
    type: array
    items:
      type: string
      format: email
    min_items: 1
    unique: true
```

## Tips for authors

- **The stricter the scheme, the less surprise in runtime.** Don't leave `type: string` without `enum` / `pattern` / `format` if not any text is actually acceptable.
- **`secret: true` is required for sensitive fields.** Passwords, tokens, private keys, secret-URI. Without it, the value will appear in the logs if there is an error.
- **`enum` is better than `pattern`** for a finite list of values: readable, validated by a linter, displayed in UI/MCP.
- **`format` is better than `pattern`** for known types (`hostname`, `email`, ...): uniformity, cheaper review, fewer errors in regex.
- **`default` implies `required: false`.** Don't write both keys - choose one meaning.
- **`required_when` instead of shell guard** for "required in one mode". Declarative input validation instead of `core.cmd.shell` with `test ... || exit 1` (see ["Conditional Required"](#conditional-required_when)).
- **`additional_properties: false`** is a good habit for strict APIs. Protects against typos in field names.
- **Recursion works.** Circuits of any depth can be nested in `items` and `properties`. Don't overuse it - deeper than 2-3 levels, it's better to move the parameter into a separate one.

## Hints for agents (AI/linters/code-gen)

- **No new keys without editing this file.** Any new key - propose-and-wait, then updating this document, then everything else.
- **This file is the source of truth** for any discrepancies with examples, manifests or code.
- **Linter must catch:** use of unknown keys, incompatible type in `default`, literals in expressions outside `enum`, regex syntax, absence of `items` in `array`, absence of `properties` in `object`, unparsable CEL in `required_when`, invalid path in `prefill_from_state` (without root `state`/foreign root/broken segment), as well as named type errors: `$type` for a non-existent type (`input_type_unknown`), cycle in the type graph (`input_type_cycle`), double name in `types:` (`input_type_duplicate`), `$type` along with an inline diagram (`input_type_ref_conflict`).
- **When generating the `input:` block** for a new destiny / scenario, first apply the most stringent restrictions (enum / format / pattern), then loosen them as needed. Never set `type: string` without at least one of {`enum`, `pattern`, `format`, `min_length`/`max_length`} if the value is actually structural (hostname, port-in-line, version, etc.).

