# Oracle - MCP-tools Vigil / Decree registries

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.oracle.vigil.*` / `keeper.oracle.decree.*` (event-driven monitoring registries Beacons, [ADR-030](../../adr/0030-vigil-oracle.md)). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api/oracle.md](../operator-api/oracle.md).

### Oracle (8)

4-segment tool-name `keeper.oracle.<resource>.<action>` ↔ 2-segment permission `<resource>.<action>` (`vigil.create` / `decree.list` / …, selector - NoSelector). Business logic (validation `name`/`interval`/`check`/subject for Vigil; `name`/`on_beacon`/`incarnation`/`scenario`/subject/`where`-CEL for Decree) lives in `oracle.Service`; tool - transport. Tools are only available when the registry is connected; when disabled, the call returns `internal-error` ("oracle registry is not configured"). **Reactor flow (Portent → match Decree → enqueue) is NOT controlled by these tools** ([rbac.md §Oracle](../rbac.md)).

`subject` is a nested object carrying **exactly one** of `sid: [...]` / `incarnation: {service, name}` / `coven: [...]` / `trait: {key, value}` ([NIM-280](../../adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-280-a-rules-subject-reads-both-levels--targeting-only)) - identical to the REST shape, so an operator moves between the two surfaces without relearning the vocabulary. Zero or two dimensions, or half of a pair, is `validation-failed`; an empty array counts as absent. The two label dimensions read **both levels**: a `coven`/`trait` rule reaches a host carrying the label **and** every member of an incarnation carrying it, resolved at match time without writing anything to `souls` ([NIM-281](../../adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited) still holds - a host carries only what an operator attached to it). So `coven: ["<incarnation>"]` still does not reach that incarnation's members: a name is not one of its labels; use `incarnation: {service, name}`. A Decree's `subject.incarnation` (who may fire) is a different field from its top-level `incarnation_name` (what the reaction acts on), and the separate membership check on the latter reads the membership relation, never labels. ★ Targeting only - an Archon's RBAC scope is resolved from the host's own column and is never widened by an incarnation's labels. Details: [operator-api/oracle.md → Subject](../operator-api/oracle.md#subject).

#### `keeper.oracle.vigil.create`

Creates a Vigil at `vigils` (Soul-side check beacons: `check` - core-beacon address + `interval` + a four-dimension `subject`). Read-only by design. Permission: `vigil.create`. Endpoint: [`POST /v1/vigils`](../operator-api/oracle.md). Async: no.

**Input** (`required: name, subject, interval, check`): `{name (kebab 1..63), subject (object, exactly one of sid / incarnation / coven / trait), interval (duration), check (core-beacon address), params? (object), enabled? (default true)}`.

**Output:** `VigilView` — `{name, subject, interval, check, params, enabled, created_by_aid?, created_at, updated_at}`; `subject` carries only the dimension the Vigil was written with.

Errors: `vigil-already-exists` (`name` busy), `validation-failed` (broken `name`/`interval`/`check`, or a subject with zero / two dimensions or half a pair).

#### `keeper.oracle.vigil.list`

Enumeration of Vigils (sort `created_at` DESC, `name` ASC). Permission: `vigil.list`. Endpoint: [`GET /v1/vigils`](../operator-api/oracle.md). Async: no.

**Input:** `{offset?, limit?}`. **Output:** `VigilListReply` — `{items: array<VigilView>, offset, limit, total}`.

#### `keeper.oracle.vigil.label-set`

Replaces the Vigil's **display caption** ([ADR-0085](../../adr/0085-entity-id-and-label.md)). The caption is free text - capitals and spaces allowed, nothing validates its form; `null` (or an omitted `label`) clears it and consumers fall back to showing `name`. `name` addresses the row and is NOT changed. This is the registry's only operator mutation: `interval`, `check` and the subject stay immutable because the Souls holding a `VigilSnapshot` were already told what to run. A Decree reacts through `on_beacon`, which is the name, so changing the caption moves nothing. Permission: `vigil.label-set`. Endpoint: [`PUT /v1/vigils/{name}/label`](../operator-api/oracle.md). Async: no.

**Input** (`required: name`): `{name (^[a-z0-9-]{1,63}$), label? (string|null)}`. **Output:** `Vigil` - the row as it now reads. Errors: `not-found`.

#### `keeper.oracle.vigil.delete`

Deletes Vigil by name (stops distributing to hosts in `VigilSnapshot`; Decrees do NOT cascade). Permission: `vigil.delete`. Endpoint: [`DELETE /v1/vigils/{name}`](../operator-api/oracle.md). Async: no.

**Input:** `{name}`. **Output:** empty object (REST equivalent - 204). Errors: `not-found`.

#### `keeper.oracle.decree.create`

Creates Decree (reactor rule): `on_beacon` (Vigil) × `subject` (who may fire) × `incarnation_name` (what the reaction acts on) → `action_scenario` (named, whitelist) + opt. `where`-CEL predicate over `event.data` + `cooldown`. Default-deny. Permission: `decree.create`. Endpoint: [`POST /v1/decrees`](../operator-api/oracle.md). Async: no.

**Input** (`required: name, on_beacon, subject, incarnation_name, action_scenario`): `{name (kebab 1..63), on_beacon (kebab), subject (object, exactly one of sid / incarnation / coven / trait), incarnation_name, action_scenario (named, ^[a-z][a-z0-9_]*$), where? (CEL), action_input? (object), cooldown? (duration), enabled? (default true)}`.

**Output:** `DecreeView` — `{name, on_beacon, where?, subject, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid?, created_at, updated_at}`.

Errors: `decree-already-exists` (`name` busy), `validation-failed` (broken `name`/`on_beacon`/`incarnation_name`/`action_scenario`/`where`-CEL/`cooldown`, or a subject with zero / two dimensions or half a pair).

#### `keeper.oracle.decree.list`

Enumeration of Decrees (sort `created_at` DESC, `name` ASC). Permission: `decree.list`. Endpoint: [`GET /v1/decrees`](../operator-api/oracle.md). Async: no.

**Input:** `{offset?, limit?}`. **Output:** `DecreeListReply` — `{items: array<DecreeView>, offset, limit, total}`.

#### `keeper.oracle.decree.label-set`

Replaces the Decree's **display caption** ([ADR-0085](../../adr/0085-entity-id-and-label.md)). The caption is free text - capitals and spaces allowed, nothing validates its form; `null` (or an omitted `label`) clears it and consumers fall back to showing `name`. `name` addresses the row and is NOT changed. The reactor is untouched: cooldown state (`oracle_fires`) and the circuit breaker (`oracle_circuit`) are keyed on the name, so no trigger history moves and no breaker resets. Permission: `decree.label-set`. Endpoint: [`PUT /v1/decrees/{name}/label`](../operator-api/oracle.md). Async: no.

**Input** (`required: name`): `{name (^[a-z0-9-]{1,63}$), label? (string|null)}`. **Output:** `Decree` - the row as it now reads. Errors: `not-found`.

#### `keeper.oracle.decree.delete`

Removes Decree by name; cascade clears cooldown-state (`oracle_fires`, `ON DELETE CASCADE`). Permission: `decree.delete`. Endpoint: [`DELETE /v1/decrees/{name}`](../operator-api/oracle.md). Async: no.

**Input:** `{name}`. **Output:** empty object (REST equivalent - 204). Errors: `not-found`.
