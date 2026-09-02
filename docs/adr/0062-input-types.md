# ADR-062. Named input types — reusable named input schemas via `types:` + `$type`

> **Status: active.** The user's decision is made (propose-and-wait closed), recorded in the canon BEFORE the code (the "architectural decision in the same move" rule). Wave 1 = the mechanism on the current `AclUser {name, perms, state}`; MVP boundaries are at the end of the ADR.

**Context.** The input DSL ([docs/input.md](../input.md), `config.InputSchema`) describes the input contract of a scenario/destiny/module manifest. The same composite type (e.g., a user record `{name, perms, state}` in the `redis` service) shows up **in several scenarios** of the same service — `add_user`, `update_acl`, `create` (an array of such records). Today it has to be **duplicated inline** in each `input:`. The duplicate drifts: an edit in one place doesn't reach the others, and `additional_properties: false` / `required` start to differ between scenarios for what is logically one type.

The previous groundwork for reuse — **`$ref` to an external JSON Schema file in a `schemas/` folder** (`input: { $ref: "../../schemas/user.yaml" }`) — was declared in [architecture.md](../architecture.md) and [service/manifest.md](../service/manifest.md), but **never implemented**. It had three problems: (1) it introduces a **second schema DSL** (an external JSON Schema alongside our own input DSL — a divergence of `properties`/`required` semantics, the `type` vocabulary, `input_*` error codes); (2) a relative-path `$ref` — a file-resolution and path-traversal surface; (3) `$ref` under the `input:` key syntactically conflicts with the input block itself (what does `input: {$ref: ...}` mean — replace the whole block? one parameter?).

**Decision (fixed by the user).** Replace the unimplemented `$ref`/`schemas/` with **named input types**: reusable named schemas in the **same input DSL**, declared in a service-level file, referenced by the `$type` directive.

1. **A `types:` section in the service-level file `service/<name>/types.yml`.** A map `<Name>` → a schema in the **same** InputSchema DSL (`type`/`properties`/`required`/`items`/`enum`/`pattern`/`format`/… — the entire [docs/input.md](../input.md) vocabulary, including nesting). No external JSON Schema. The type name is `PascalCase` (`^[A-Z][A-Za-z0-9]*$`), visually and in the parser distinguishing a type reference from a parameter name (snake_case).

   ```yaml
   # service/redis/types.yml
   types:
     AclUser:
       type: object
       additional_properties: false
       properties:
         name:  { type: string, required: true, pattern: "^[a-zA-Z0-9_-]+$" }
         perms: { type: string, required: true }
         state: { type: string, required: true, enum: [on, off] }
   ```

   > ⚠ The requiredness above was originally written as an object-level `required: [name,
   > perms, state]`. That list form leaves the whole dialect — see the [amendment of
   > 2026-09-01](#amendment-2026-09-01-nim-741-one-schema-dialect-three-deltas-to-named-types)
   > at the end of this ADR. **Not implemented yet** (NIM-742).

2. **A reference `$type: <Name>` as a standalone field** OR **`items: {$type: <Name>}`** for an array of such elements:

   ```yaml
   # scenario/add_user/main.yml
   input:
     user:
       $type: AclUser            # a single object of the declared type

   # scenario/create/main.yml
   input:
     users:
       type: array
       items:
         $type: AclUser          # an array of the declared type
       min_items: 1
   ```

   `$type` is a **resolve-time directive**: at the input stage it is replaced by the expanded schema of the type from `types:`. After resolution, the regular input DSL takes over (value validation is recursive, as for any inline `object`/`array`).

3. **Resolution — service-level (MVP).** The `$type` name is looked up only in the `types:` of the **same service**. **NOT** local-per-scenario (types are not declared inside `scenario/<name>/`), **NOT** cross-service (you cannot reference another service's type). This is a deliberate boundary: one source of truth per service, no cross-service schema dependencies.

4. **Cycle detection — mandatory.** A type can reference a type (nested `$type` inside `properties`/`items` of a declared type). The resolver walks the reference graph and **must** catch a cycle (`A → B → A`, including a self-reference `A → A`) → error `input_type_cycle`, not an infinite expansion. The depth boundary is not limited by a number — it's limited by the absence of a cycle.

5. **DTO `/v1/scenarios` — backend-side resolve + an `x-type` annotation.** When projecting a scenario's schema into the DTO of the scenario-catalog endpoint, `$type` is **resolved backend-side BEFORE projection**: the client gets an **already-expanded** schema (the UI builds the form from the familiar inline format, with no knowledge of `types:`) plus a **forward-compat annotation `x-type: <Name>`** on the node where `$type` stood. The UI ignores it today; going forward it allows a specialized widget for the named type, without breaking current clients. `x-type` is a purely read-only DTO annotation, it is not written in the YAML source itself.

6. **Replacing `$ref`/`schemas/`.** The previous unimplemented `$ref` channel and the `schemas/` folder are **removed from the canon**: mentions in [architecture.md](../architecture.md) (the repo tree + the "Optional `$ref`" block) and [service/manifest.md](../service/manifest.md) are rewritten for `types.yml`/`$type`. Since `$ref` was never implemented, no migration is needed — this is a replacement of groundwork, not a breaking change.

**Format — summary.**

| What | Where | Form |
|---|---|---|
| Type declaration | `service/<name>/types.yml` | `types: { <PascalCase>: <InputSchema> }` |
| Reference (object) | any scenario `input:` | `<param>: { $type: <Name> }` |
| Reference (array) | any scenario `input:` | `<param>: { type: array, items: { $type: <Name> } }` |
| Type→type nesting | inside a schema in `types:` | `$type` under `properties.<f>` / `items` of the declared type (cycle-checked) |
| DTO annotation | the `/v1/scenarios` response (read-only) | `x-type: <Name>` on the resolved node |

**Resolution — order.** At Keeper's input stage (the same phase as merging defaults / the `required` check): load the service's `types.yml` → for each `$type` in the scenario's input schemas, substitute the expanded schema of the type (with cycle detection) → then the regular input resolution (merge → required → value validation) runs on the already-expanded schema. `$type` never reaches the render phase anywhere — it's a structural expansion, not a value.

**Error classes** (`soul-lint` diagnostics and backend resolution, the `input_type_*` area, [naming-rules.md → Parser / validation errors](../naming-rules.md#error-codes)):

| Code | When |
|---|---|
| **`input_type_unknown`** | `$type: <Name>` refers to a type absent from the service's `types:`. |
| **`input_type_cycle`** | A cycle in the type-reference graph (`A→B→A`, a self-reference `A→A`). |
| **`input_type_duplicate`** | A duplicate name in the `types:` section (one name declared twice). |
| **`input_type_ref_conflict`** | `$type` is specified **together with** an inline schema on the same node (`type:`/`properties:`/`items:`/…) — a reference and inline are mutually exclusive, a node is either `$type` or its own schema. |

**MVP boundaries (what is NOT included).**

- **`object` + `array-of-type` + type→type nesting** — supported. With mandatory cycle detection.
- **Scalar-alias** (`types: { Port: {type: integer, min:1, max:65535} }` and `$type: Port` on a scalar field) — **not included** (can be added later, same form, no breaking change; the decision is a separate propose-and-wait on a real request).
- **Generics / parameterized types** — not included.
- **Cross-service** references to another service's types — not included (resolution is strictly service-level).
- **Local-per-scenario `types:`** (declaring a type inside a single scenario) — not included; types are service-level only.

**Impacted consumers.**

- **`config.InputSchema` (`shared/config`)** — the input-DSL parser gets a node-level `$type` directive (a reference field) + a node-level `items: {$type}`; the `types.yml` source is loaded service-level; the resolver with cycle detection is called at the input stage. Imperative value validation (recursion over the expanded schema) is unchanged.
- **`soul-lint validate-service`** — new static checks `input_type_unknown` / `input_type_cycle` / `input_type_duplicate` / `input_type_ref_conflict` (see [soul-lint.md](../soul-lint.md)). Type resolution is part of validating a scenario's `input:`.
- **DTO `/v1/scenarios`** — the backend expands `$type` before projection, adds an `x-type` annotation (forward-compat for a UI widget).
- **`docs/input.md`** — a new section on the `types:` file and the `$type` reference (format, resolution, errors, MVP boundaries).
- **`architecture.md` / `service/manifest.md`** — replacing the unimplemented `$ref`/`schemas/` with `types.yml`/`$type`.

**Rejected alternatives.**
- **(a) Keep `$ref` to an external JSON Schema file.** Rejected: a second schema DSL alongside our own input DSL (a divergence of vocabulary/semantics/error codes), the path-resolution surface of a relative `$ref`, a syntax conflict of `$ref` under the `input:` key. Named types live **in the same DSL** — one vocabulary, one set of `input_*` errors.
- **(b) Local-per-scenario types.** Rejected for the MVP: types are reused **between** scenarios of a service — their place is service-level, otherwise the whole point of reuse is lost. (An extension is possible later without a breaking change.)
- **(c) Cross-service references.** Rejected: introduces a cross-service schema dependency (versioning another service's `types.yml`, a git resolve) — disproportionate to wave 1's goal. Resolution is strictly within one service.

**Cross-ref.** [ADR-045](0045-param-dsl.md#adr-045-module-param-dsl--typed-input-fields-for-the-run-command-ui-form) (bringing modules' input DSL closer to `config.InputSchema` — named types apply to the same `InputSchema`); [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) (scenario `input:` — the main consumer of `$type`); [ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-in-the-ui--the-ui-does-not-hardcode-dynamic-catalogs) (the backend expands `$type` before projection — the UI gets a familiar inline schema, doesn't hardcode knowledge of `types:`).

## Amendment 2026-09-01 (NIM-741, [One schema dialect](0086-one-schema-dialect.md)): three deltas to named types

`state_schema` stops being JSON Schema and is written in the **same dialect as `input:`**. This ADR
already said the reuse mechanism lives *"in the same DSL — one vocabulary, one set of `input_*`
errors"*, and rejected `$ref` precisely to avoid a second one; the move finishes that argument by
removing the last schema in the tree that was written in the other dialect. A `types.yml` catalog
is now shared by both contracts, which lands three deltas here. **All three are design only** —
engine NIM-742, `soul-lint list-secret-paths` NIM-743, `examples/` and the WB redis service
NIM-744.

**(1) The list form `required: [names]` leaves `types.yml` too.** Requiredness is `required: true`
on the property, everywhere in the dialect — not only at a `state_schema` root, but inside nested
objects and inside a declared type, so the `AclUser` example in §1 above is rewritten. This is the
narrower half of a wider removal: what the new dialect refuses **at the root only** is the
`type: object` / `properties:` wrapper, and a `types.yml` entry is a schema node rather than a
root, so `AclUser` keeps its `type: object` and its `properties:` bag. The list form has no such
exemption — it is gone from the whole dialect. Refused as `input_required_list_removed`. There are
28 flow-form `required: [...]` sites in the DSL corpus to migrate; the block form is authored
nowhere.

**(2) `type: secret` becomes legal in a shared type.** The rule, verbatim: *a property with
`type: secret` in a shared type is not asked for on input — the platform mints it; in
`state_schema` it means a declared secret.* One `AclUser` can then carry `password` once and serve
both the `input:` of `add_user` and the `redis_users` field of `state_schema`, which is the
duplication this ADR exists to remove, applied to the one property that could not participate
before. The input half was deferred when this amendment was written and is **decided and built
since 2026-09-02 (NIM-751, [ADR-0086](0086-one-schema-dialect.md) §5)**: such a property is
**stripped from the projected operator form**, is **never required and takes no default**, and a
value supplied anyway is **refused** — `input_secret_type_not_writable`, reserved by this amendment
and spent by that one. The render seal stays provenance-based on `secret: true` deliberately: a
value refused at the input gate reaches no cell to mask.

**(3) The `$type` overlay widens by exactly one key — `properties` — and only in `state_schema`.**
Frame this as a widening of an **existing closed overlay set**, not as a carve-out. `$type`
already coexists with some of the referring node's own keys: `applyRefOverlay`
(`shared/config/input_types.go:391-410`) overlays `description`, the reference's field-level
`required` and `required_when` on top of the resolved type. What is refused beside `$type` is the
closed set `{type, properties, items}` (`input_types.go:100`, `input_type_ref_conflict`), because
each of those three *redefines the type's shape*. In `state_schema` a `properties:` beside `$type`
is a different act — **adding** fields a shared type does not carry, without touching the ones it
does — so `properties` moves out of the refusal set and into the overlay set, in that pass only.
**`type` and `items` stay refused everywhere**, including `state_schema`: those two replace a
shape rather than extend it, and a reference that replaces the shape is not a reference.

The merge is the one this platform already ratified for a shared contract — [ADR-009](0009-scenario-dsl.md)'s
`extends:` covenant, **by reference and not by re-derivation**: an **add-only shallow merge, fail-closed**.
A key the type already declares is not overridden and not deep-merged; it is an error, the new code
`input_type_ref_overlay_conflict`. Deep merge and last-wins were rejected there for the reason that
applies verbatim here — a silent override across a file boundary makes the shared definition
unreadable from the site that uses it.

**Consequence for offline checking, stated so it is not read as an omission.** The secret rules are
deliberately **not** among the standalone checks `soul-lint` runs over `types.yml`. That file's
checker (`soul-lint/internal/validate/type_refs.go:6-77`) is scoped to what needs the type
**catalog** — `input_type_duplicate` / `input_type_cycle` / `input_type_unknown` — and the secret
rules are not catalog questions: a type carrying `type: secret` is neither legal nor illegal in
isolation, only at the point of use. Full argument and the position table —
[ADR-0083](0083-declared-secret-state-fields.md), amendment of the same date.
