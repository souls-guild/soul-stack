# `input:` in destiny

This document describes the **destiny-specifics** of the `input:` block. The format itself is a common standard for a destiny/scenario/module manifest and is described in [`docs/input.md`](../input.md). Here is where `input:` is validated, how it is used within destiny, and what specific rules and hints apply to destiny parameters.

## Source of truth on the format

Exact keys (`type`, `enum`, `pattern`, `format`, `min_length`, `secret`, ...), types (`string`, `integer`, `number`, `boolean`, `array`, `object`) and validation rules - in [`docs/input.md`](../input.md). In case of discrepancies, priority goes to that document. Any new key - propose-and-wait → edit [`docs/input.md`](../input.md) → then this file and examples.

## Where the block lives

At the root `destiny.yml` (see [manifest.md](manifest.md) → field `input:`). Not in `tasks/main.yml`. One destiny - one block `input:`. All `tasks/main.yml` tasks read values ​​from a common set of parameters.

## Where is validated

Defense in depth - two independent rounds of validation, both are required:

1. **Keeper on invocation.** When a scenario or direct API call triggers destiny, Keeper reads `input:` destiny and checks the passed values **before** anything goes to Souls. Error → fail fast, operator diagnostics, zero traffic to hosts.
2. **Soul before apply.** Having received the destiny + values ​​from the Keeper (or from `keeper.push`), Soul re-validates them. Protects against version desynchronization, manual injection and bugs in Keeper.

See also [`docs/input.md`](../input.md) - a general format standard (where `input:` lives in every artifact).

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
| **Where is the diagram from** | [`docs/input.md`](../input.md) - general standard | Module manifest for a specific state (see [architecture.md → "Module Manifest"](../architecture.md)) |
| **Who validates** | Keeper + Soul (see above) | Soul on apply; `soul-lint` statically by module manifest |
| **Access in templates** | `${ input.action }` (or naked `input.action` in top-level expression-keys) | internal task value, not visible outside |

The names are intentionally different: `input` - *outside inside destiny*; `params` - *inside the module*.

## Destiny-specific rules and tips

Basic tips for schematic authors can be found in [`docs/input.md` → "Hints for Authors"](../input.md). Destiny-specific additions:

- **`action:` parameter is almost always there.** Destiny usually declares a top-level `action: { type: string, enum: [...] }` - it determines which tasks `tasks/main.yml` will be executed through `when:`. This is the point at which "one destiny - several operating modes" (apply / restart / ping / status-check) rests.
- **All secrets are via `secret: true`.** Passwords, tokens, private keys and vault links. Without this, the value will appear in the apply logs when the task fails; such incidents are the cheapest class of leaks.
- **Vault references are `pattern: "^vault:.*"`**, not "string as string". The resolution to real value is done by Keeper before sending destiny to Soul; Until then, destiny operates by reference, not by meaning.
- **`enum:` is more important than `pattern:`.** The finite list of values ​​(`apply | restart | ping`) is much better than the regex `^(apply|restart|ping)$` - better readable, validated by a linter, available in the UI/MCP directory as a dropdown.

## Communication with `input:` scenario

Scenario also has a `input:` block (same format). But these are **different** contracts:

- Scenario `input:` - what the operator passed when running the script (`keeper.incarnation.run scenario=add_user inputs={...}`).
- Destiny `input:` - that scenario passed destiny through `apply: { destiny: ..., params: { ... } }`.

Scenario calculates destiny-`input:` from its scenario-`input:`, `vars` (essence) and `state` - and passes it to destiny. Inside destiny scenario-`input:` **not visible** - destiny knows only what came to its `input:`.

## See also

- [`docs/input.md`](../input.md) is a general standard for the `input:` format.
- [manifest.md](manifest.md) - where `input:` lies in `destiny.yml`.
- [tasks.md](tasks.md) - how `input.<name>` is used in tasks.
- [architecture.md → "Destiny: input contract and validation"](../architecture.md) — validation rounds and connection with soul-lint.
