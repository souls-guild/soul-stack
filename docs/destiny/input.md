# `input:` in destiny

This document describes the **destiny-specifics** of the `input:` block. The format itself is a common standard for a destiny/scenario/module manifest and is described in [`docs/input.md`](../input.md). Here is where `input:` is validated, how it is used within destiny, and what specific rules and hints apply to destiny parameters.

## Source of truth on the format

Exact keys (`type`, `enum`, `pattern`, `format`, `min_length`, `secret`, ...), types (`string`, `integer`, `number`, `boolean`, `array`, `object`) and validation rules - in [`docs/input.md`](../input.md). In case of discrepancies, priority goes to that document. Any new key - propose-and-wait → edit [`docs/input.md`](../input.md) → then this file and examples.

## Where the block lives

At the root `destiny.yml` (see [manifest.md](manifest.md) → field `input:`). Not in `tasks/main.yml`. One destiny - one block `input:`. All `tasks/main.yml` tasks read values from a common set of parameters.

## Where is validated

Defense in depth - two independent rounds of validation, both are required:

1. **Keeper on invocation.** When a scenario or direct API call triggers destiny, Keeper reads `input:` destiny and checks the passed values **before** anything goes to Souls. Error → fail fast, operator diagnostics, zero traffic to hosts.
2. **Soul before apply.** Having received the destiny + values from the Keeper (or from `keeper.push`), Soul re-validates them. Protects against version desynchronization, manual injection and bugs in Keeper.

### What round 1 checks (full parity with scenario)

The Keeper round runs at **render**, when the parent `apply:` task hands its resolved `input:` to the destiny — not on the API request path, because destiny input is computed by the scenario, not typed by the operator. It runs the **same validator** as the scenario pre-flight gate ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-07-26), in this order:

1. `default:` merge for parameters the caller did not pass;
2. `required:` and **`required_when:`** — the conditional predicate is evaluated over the merged input, exactly as in scenario;
3. **value validation** against the schema — `type` / `enum` / `pattern` / `format` / `min_length` / `max_length` / `min_items` / `max_items`, recursively into `array` items and `object` properties;
4. the top-level **`validate:`** rules over the merged input (see below).

Any violation **rejects the render** and names the offending field (`input $.tls.cert_ref …`) or, for a rule, quotes its `message:`. There is no warn-only mode: a destiny whose caller breaks its declared contract fails loudly rather than deploying something the contract forbade.

> **Breaking change (2026-07-26, NIM-167).** Rounds 2–4 above are new. Previously the render pass checked only unconditional `required` plus `default` merge: `required_when` was parsed but never evaluated, and values were never checked against their own `type`/`enum`/`pattern`. A destiny whose caller passes contract-violating values used to render silently and now fails. Two real drifts in `examples/` surfaced the day the check went in — see ["Migration"](#migration-from-the-pre-nim-167-behavior).

See also [`docs/input.md`](../input.md) - a general format standard (where `input:` lives in every artifact).

## `required_when:` and `validate:` in destiny

Both mechanisms work in destiny exactly as they do in scenario — same grammar, same **input-only CEL sandbox**, same validator (no second implementation).

- **`required_when: "<CEL>"`** — a per-field key of the `input:` schema: "this field is required when the predicate over the rest of the input is true". Spec: [`docs/input.md` → "Conditional `required_when`"](../input.md#conditional-required_when).
- **`validate:`** — a top-level section of `destiny.yml` next to `input:`: a list of `[{that, message}]` cross-field invariants. Spec: [scenario/orchestration.md §2.5](../scenario/orchestration.md) — the section is identical, only the enforcement point differs (render, not the request path).

```yaml
# destiny.yml
input:
  redis_type:    { type: string, enum: [standalone, cluster], required: true }
  port:          { type: integer, required_when: "input.redis_type == 'standalone'" }
  cluster_nodes: { type: array, items: { type: string } }

validate:
  - that: "input.redis_type != 'cluster' || size(input.cluster_nodes) >= 3"
    message: "cluster requires at least 3 nodes"
  - that: "!has(input.tls) || !input.tls.enable || input.tls.cert_ref != ''"
    message: "tls.enable requires a cert_ref"
```

**Input-only, by construction.** The `that` predicate's CEL environment declares a single variable — `input`. A reference to `vars` / `soulprint` / `register` / `vault()` / `now()` is a **compile error**, not a silent read: the sandbox comes from the name being undeclared. This fits destiny isolation exactly ([ADR-009](../adr/0009-scenario-dsl.md)) — a destiny sees only its own input anyway.

**`validate:` complements `assert:`, it does not replace it.** `validate:` covers pure input invariants. Anything needing topology, the run roster or a task register stays an `assert:` task in the scenario — note that render-time `assert:` is a **scenario** construct; inside a destiny's `tasks/main.yml` it is not supported.

**`that` / `message` are plain scalars.** Write the predicate as a quoted one-liner; a YAML block scalar (`>-` / `|`) is rejected by the rule grammar.

**Caught statically too.** `soul-lint validate-destiny` runs the same schema validation, so an unparsable `required_when`, a `that` referencing scenario scope, a missing `message` or an empty `validate: []` are reported offline, before any run.

### Migration from the pre-NIM-167 behavior

When strict validation landed, two contract drifts in `examples/` failed immediately — both worth recognizing, because they are the typical shapes:

- **An unconditional requiredness that was meant to be conditional.** `examples/destiny/redis` declared the `cert_ref` / `key_ref` / `ca_ref` fields of its `tls` object mandatory, but every TLS-off caller legitimately passes the dict with empty refs. Plain requiredness cannot express "only when `enable` is true" — the invariant moved into `validate:`, gated on `input.tls.enable`. The spelling at the time was the object-level list `required: [cert_ref, key_ref, ca_ref]`; that form **leaves the dialect entirely** — requiredness is the per-field boolean `required: true` and nothing else ([ADR-0086](../adr/0086-one-schema-dialect.md), refused as `input_required_list_removed`). The lesson is untouched by the rename: neither spelling can be conditional.
- **A caller degrading a mandatory field to an empty string.** The create scenario passed `sha256: "${ default(vars.redis_exporter_sha256, '') }"` into a destiny declaring `sha256` required. An empty string counts as "not passed" ([`docs/input.md` → "Empty strings"](../input.md)), so the mandatory tarball checksum was silently absent — the fix belongs on the caller's side (supply the value), not in the contract.

If a destiny of yours starts failing, read the message: it names the field or quotes the rule. Fix the caller if it passes something the contract forbids; relax the contract only if the contract was wrong.

## How it is used inside destiny

In `tasks/main.yml`, in `templates/*.tmpl` templates and in `when:` conditions referenced values as `input.<name>` (via `${ ... }` in string interpolation, bare form in top-level expression-keys - see [`docs/templating.md`](../templating.md)):

```yaml
# destiny.yml
input:
  action:
    type: string
    required: true
    enum: [apply, restart, ping]
  version:
    type: string
    format: semver

# tasks/main.yml
tasks:
  - name: Install redis-server package
    module: core.pkg.installed
    when: input.action == 'apply'
    params:
      name: redis-server
      version: "${ input.version }"

  - name: Restart redis-server
    module: core.service.restarted
    when: input.action == 'restart'
    params:
      name: redis-server
```

## `input.<name>` vs `params:` tasks

Not to be confused:

| | `input.<name>` | `params.<name>` |
|---|---|---|
| **What** | Destiny parameter declared in `destiny.yml → input:` | Module-specific argument passed in step `tasks/main.yml` |
| **Where is the schema from** | [`docs/input.md`](../input.md) - general standard | Module manifest for a specific state (see [architecture.md → "Module Manifest"](../architecture.md)) |
| **Who validates** | Keeper + Soul (see above) | Soul on apply; `soul-lint` statically by module manifest |
| **Access in templates** | `${ input.action }` (or naked `input.action` in top-level expression-keys) | internal task value, not visible outside |

The names are intentionally different: `input` - *outside inside destiny*; `params` - *inside the module*.

## Destiny-specific rules and tips

Basic tips for schematic authors can be found in [`docs/input.md` → "Hints for Authors"](../input.md). Destiny-specific additions:

- **`action:` parameter is almost always there.** Destiny usually declares a top-level `action: { type: string, enum: [...] }` - it determines which tasks `tasks/main.yml` will be executed through `when:`. This is the point at which "one destiny - several operating modes" (apply / restart / ping / status-check) rests.
- **All secrets are via `secret: true`.** Passwords, tokens, private keys and vault links. Without this, the value will appear in the apply logs when the task fails; such incidents are the cheapest class of leaks.
- **Vault references are `pattern: "^vault:.*"`**, not "string as string". The resolution to real value is done by Keeper before sending destiny to Soul; Until then, destiny operates by reference, not by meaning.
- **`enum:` is more important than `pattern:`.** The finite list of values (`apply | restart | ping`) is much better than the regex `^(apply|restart|ping)$` - better readable, validated by a linter, available in the UI/MCP directory as a dropdown.

## Communication with `input:` scenario

Scenario also has a `input:` block (same format). But these are **different** contracts:

- Scenario `input:` - what the operator passed when running the scenario (`keeper.incarnation.run scenario=add_user inputs={...}`).
- Destiny `input:` - that scenario passed destiny through `apply: { destiny: ..., input: { ... } }`.

Scenario calculates destiny-`input:` from its scenario-`input:`, `vars` (the service's own) and `state` - and passes it to destiny. Inside destiny scenario-`input:` **not visible** - destiny knows only what came to its `input:`.

## See also

- [`docs/input.md`](../input.md) is a general standard for the `input:` format.
- [manifest.md](manifest.md) - where `input:` lies in `destiny.yml`.
- [tasks.md](tasks.md) - how `input.<name>` is used in tasks.
- [architecture.md → "Destiny: input contract and validation"](../architecture.md) — validation rounds and connection with soul-lint.
