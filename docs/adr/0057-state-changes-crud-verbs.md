# ADR-057. `state_changes` — an ordered list of CRUD verbs (`set`/`add`/`modify`/`remove`)

- **Context.** The `state_changes` grammar from [ADR-009 §7.1](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) defined three keys: `sets` (a map `<field>: <CEL>`, implemented) and `appends`/`modifies` (lists of field-paths). The latter two were a **placeholder declaration with no value source** — they were not applied by the engine (see [orchestration.md §7.1 «`appends`/`modifies` — future»](../scenario/orchestration.md#71-grammar-state_changes---list-of-crud-operations)). This is a latent bug: a scenario with `appends: [redis_hosts]` (`add_replica`, `add_replicas`, `add_user`) passed successfully, but `incarnation.state` **did not grow** — the added host/user did not settle into the state. `modifies` (`update_acl`) likewise — the collection patch was not applied. The root cause is that `appends`/`modifies` carried only a **field-path** (what to touch), without a **value source** (what to write) and without a **predicate** (which element). And `sets` can only overwrite a field wholesale — it is not enough for growing collections and pointwise patching of elements.

  In parallel, `sets` is a map, i.e. **unordered**: as the grammar grows (several order-dependent mutations of one collection) a map does not define a deterministic application sequence.

- **Decision.** `state_changes` is an **ordered list of operations** (a YAML list, not a map). Each element is exactly one **CRUD verb** (singular): `set` / `add` / `modify` / `remove`. Multiplicity is expressed by a **match predicate** (CEL over a collection element), not by flags/knobs. Bulk fan-out is via a structural `foreach` (the form is literally like the migration-DSL [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

  ### (a) Verbs

  - **`set: <field>`** + **`value: "${CEL}"`** — overwrite a field wholesale (replaces the former `sets` map; one `sets` entry ≡ one `set` element of the list).
  - **`add: <collection>`** — add an element to a collection:
    - **map collection:** `key: "${CEL}"` (the key) + `value: {obj}` (the entry's value).
    - **list collection:** `value: {obj|scalar}` + optional `match: "<CEL predicate>"` (a dedup predicate — if an element matching this predicate already exists, `on_conflict` applies).
    - **`on_conflict: skip | replace | error`** (DEFAULT `skip`) — behavior on a collision (the map key is already taken / the list `match` already finds an element). `skip` gives an idempotent "add if absent".
  - **`modify: <collection>`** + **`match: "<CEL predicate over an element>"`** + **`patch: { <path-in-element>: "${CEL}" }`** — patch **ALL** matching elements (all-by-default).
  - **`remove: <collection>`** + **`match: "<CEL predicate>"`** — remove **ALL** matching elements.
  - **`foreach: "${CEL list|map}"`** + **`as: <name>`** + **`do: { <verb...> }`** — bulk fan-out of N operations from a collection/map. Inside `do:` the same verbs are available, plus a binding `<name>` to the current iteration element. The form (`foreach`/`as`/`do`) is literally from the migration-DSL ([ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl), [migrations.md](../migrations.md)); the operator recognizes an already familiar pattern.

  ### (b) CEL bindings in `match`/`patch`/`value`

  On top of the full sets context (`input.*` / `incarnation.*` / `soulprint.self.*` / `register.*` / `vars.*` / `essence.*` — the same CEL env as for task `params:`, [ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)) **local bindings of the current collection element** are introduced:

  - **`elem`** — the current element of a list collection (or a scalar, if the collection is a list of scalars).
  - **`key`** / **`value`** — the key and value of the current entry of a map collection.

  The name **`elem`** was deliberately chosen instead of `self` — `self` collides with the per-host `soulprint.self`.

  ### (c) `expect` — optional runtime multiplicity assertion

  **`expect: one | at_most_one | any`** (DEFAULT `any`) — an optional assertion on the number of elements caught by `match` in `modify`/`remove`. If the actual multiplicity ≠ the expected one (`one` requires exactly one, `at_most_one` — zero or one) the run goes into `error_locked` **before committing** the state. Not a required key; the default is `any` (any number, including zero).

  ### (d) Safeguards against a broad match

  - **soul-lint WARN** on a constantly-true predicate: `match: true`, or a missing `match:` on `remove`/`modify` (which would wipe/re-patch the entire collection).
  - **empty-match → no-op** (idempotent) for both `modify` AND `remove`: the predicate caught nothing — the operation quietly does nothing, not an error.

  ### (e) Order and atomicity

  - Operations are applied **in declaration order**, sequentially, to the **intermediate** state (each sees the result of the previous ones — a deterministic chain).
  - The entire chain is **one PG transaction**, **one `state_history` snapshot** (as in [ADR-009 §7](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)). A failure of any operation (a CEL eval error, a violated `expect`, `on_conflict: error`) → `error_locked`, the state is **not committed** (the §7 barrier/state-commit invariant is not weakened — operations are applied AFTER the cross-host barrier).

  ### (f) Collection type — from state_schema

  The collection type (map vs list) is taken from the declared `state_schema` (`service.yml`). `add` into a missing field → materialize an **empty map/list from the schema** (the collection's shape is known from the schema, even if there is no value yet).

  ### (g) Per-RUN semantics

  Values are taken from the run's `input.* / vars.* / incarnation.* / register.*`. This is **per-RUN**, NOT a per-host union. If an expression yields different per-host values (`${ soulprint.self.* }` / `${ register.* }`) — **last-wins by SID sort order** applies (as for `sets` in [ADR-009 §7.1](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation); a per-host fold of collections into a union is NOT introduced).

- **The form of each verb on real scenarios.**

  **`add` (single) — `add_replica`** (replaces `appends: [redis_hosts]`):
  ```yaml
  state_changes:
    - add: redis_hosts
      value: "${ vars.new_sid }"
      match: "elem == vars.new_sid"        # dedup: is the same SID already in the list?
      on_conflict: skip                    # idempotent: a repeat does not duplicate
  ```

  **`add` via `foreach` — `add_replicas`** (bulk, replaces `appends: [redis_hosts]` for a batch):
  ```yaml
  state_changes:
    - foreach: "${ input.new_replicas }"
      as: sid
      do:
        - add: redis_hosts
          value: "${ sid }"
          match: "elem == sid"
          on_conflict: skip
  ```

  **`add` into a map — `add_user`** (replaces `appends: [redis_users]`):
  ```yaml
  state_changes:
    - add: redis_users
      key: "${ input.username }"
      value:
        acl:   "${ input.acl }"
        state: "on"
      on_conflict: error                   # double creation of a user — an explicit error
  ```

  **`modify` all matching — `update_acl`** (replaces `modifies: [redis_users.*.acl, redis_users.*.state]`):
  ```yaml
  state_changes:
    - modify: redis_users
      match: "key == input.username"       # pointwise patch of a single map entry
      patch:
        acl:   "${ input.acl }"
        state: "${ input.state }"
  ```

  **`modify` ALL replicas** (multiplicity via a predicate, no knobs):
  ```yaml
  state_changes:
    - modify: redis_hosts
      match: "elem.role == 'replica'"      # all replicas at once — all-by-default
      patch:
        config_version: "${ input.version }"
  ```

  **`remove` — `remove_replica`**:
  ```yaml
  state_changes:
    - remove: redis_hosts
      match: "elem == input.sid"
      expect: one                          # assert: there was exactly one such host
  ```

- **Multiplicity — via a `match` predicate, not via knobs/flags.** `modify`/`remove` patch/remove **all** elements matching `match` by default (all-by-default). To narrow to one — refine the predicate (`key == X`, `elem.id == Y`) + optional `expect: one`. This is a deliberate refusal of the verb pair `*_one`/`*_all` and of an `all:` flag (see rejected alternatives): one verb + a declarative predicate cover both cases without a dialect.

- **Transition plan (breaking, dual-parse for one release).** `state_changes` changes its form **map → list**. For one release keeper parses **both** forms (dual-parse):
  - The new form — a list of verbs (the canon above).
  - The old map form (`sets:` / `appends:` / `modifies:`) is parsed as **DEPRECATED**, soul-lint emits a warn. The `sets` map is translated into an equivalent sequence of `set` elements. `appends`/`modifies` were no-op placeholders with no source — their deprecated parse remains a no-op (behavior does not change, recorded with a warn "rewrite to `add`/`modify`, otherwise the state does not grow").
  - In the next release the map form is **removed** (parsing the old form → a validation error).

- **Relation to the migration-DSL ([ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).**
  - **`foreach`/`as`/`do` — reuse** of the same structural form (one pattern for two DSLs; the operator does not learn a second loop syntax).
  - **`remove` (state_changes) ≠ `delete` (migration-DSL) — deliberately different names.** `delete` in a migration = "wipe a path in the state structure" (addressing by `path:`, an operation on the shape of the data when the schema changes). `remove` in state_changes = "extract an element from a collection" (addressing by a `match:` predicate over elements, an operation on the content during a runtime scenario). Different operations → different names, so as not to confuse "delete a schema field" and "delete a collection element".

- **Rejected alternatives.**
  - **Paired verbs `remove_one` / `remove_all` (and `modify_one`/`modify_all`).** Doubling the vocabulary for the sake of something that is expressed by the predicate's multiplicity + optional `expect`. Rejected: multiplicity is a property of `match`, not of the verb name.
  - **An `all: true` flag.** The same drawback — a control knob instead of a declarative predicate; `all` is implicit when absent — whereas `expect` explicitly records the author's expectation, and the lint catches a broad `match` regardless of the flag.
  - **A positional `remove` (first/last element).** Not reproducible in JSONB storage (the order of elements in a jsonb array is not part of the state contract); "first by which field" = this is `match` + sorting, not a position. Rejected as non-deterministic.
  - **`clear`** (clearing a collection) — redundant: `set: <field>` + `value: []`/`{}`.
  - **`rename` / `move`** — these are operations on the **shape** of the state when the schema changes; they belong to the migration-DSL ([ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)), not to a runtime scenario. They are not introduced in `state_changes`.
  - **`upsert`** — a merged "add-or-modify". Hides the author's intent (creating something new vs patching an existing one — different audit semantics). Covered by an explicit `add` + `on_conflict: replace`, where the intent is visible.

- **Consequences.**
  - [orchestration.md §7.1](../scenario/orchestration.md#71-grammar-state_changes---list-of-crud-operations) is rewritten for list-of-verbs (the normative spec); the paragraphs about `appends`/`modifies` move to the "deprecated, transition period" section.
  - [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) receives an amendment (the `state_changes` grammar is extended; status `amended`).
  - [naming-rules.md](../naming-rules.md) is augmented with the state_changes verbs and keys.
  - The latent bug "`appends`/`modifies` do not grow the state" is closed by the implementation of `add`/`modify`.
  - The implementation (keeper-side render/apply of state_changes) is a parallel developer slice, outside this ADR.

- **Trade-offs.**
  - The breaking form change (map→list) is justified by a dual-parse window of one release + a soul-lint warn; the migration is mechanical (`sets:` → a list of `set` elements).
  - `expect` is optional, not required: the author decides whether a multiplicity assertion safeguard is needed; the `any` default imposes no overhead.
  - all-by-default for `modify`/`remove` is potentially broad — compensated by the soul-lint WARN on a constantly-true/missing `match` and empty-match-as-no-op (idempotency).

- **Amendment (2026-06-24): day-2 scenarios read the expanded fact from `incarnation.state`.** Symmetric to this ADR's write semantics (how `state` is **written** after `create`), a read convention is fixed: **a day-2 scenario reads the expanded fact about a service from `incarnation.state`, NOT from `essence`/`input`.**

  - **Reason.** `create` translates the operator's `input` (which beats the `essence` defaults) into the expanded configuration and puts it into `state` (`state_changes`, the verbs `set`/`add`/…). On day-2 this `input` is no longer available, and `essence` is only the author's substrate, blind to the operator override. A day-2 scenario that reads `essence` sees **not what was actually applied** → a desync "the scenario thinks one thing, the host has another". `state` is the only place where *what was applied* is recorded.
  - **Boundary.** `essence`/`input` on day-2 are used only for what fundamentally does not reside in `state`. The main case is **secrets** (an infosec invariant: PEM/passwords are not materialized in `state`; only the **path** to the secret on the host resides in `state`). day-2 resolves them via `vault(<ref>)` the same way as `create` (`ref` from the author context `essence.<...>_ref` or a path convention). A known limitation: an operator override of `ref` at `create` is not saved into `state` → day-2 reads the `essence` `ref` (which matches the actually deployed secret when `create` had no override; saving the operator-override ref into `state` is a separate slice upon a real request).
  - **CEL idiom for reading a key with a hyphen.** Checking for the presence of a collection key is done with the operator `'<key>' in <map>`, **not** `has(<map>['<key>'])`: `has()` is a macro only for field-selection, an index argument is rejected by the parser. Protection against no-such-key (the absence of `state` in a push/trial run without State, a not-yet-materialized collection) is `has(incarnation.state) && has(incarnation.state.<col>) && '<key>' in incarnation.state.<col>`. Bracket notation remains for **accessing** the value (`incarnation.state.<col>['<key>']`).
  - **The normative spec of the convention** — [docs/destiny/production-conventions.md §7a "Day-2: source of truth = `incarnation.state`"](../destiny/production-conventions.md#7a-day-2-source-of-truth--incarnationstate). A working illustration — the `restart` of the [`redis`](../../examples/service/redis/scenario/restart/main.yml) service (the TLS discriminator from `incarnation.state.redis_config`, the guard cases `rolling-restart-replicas`/`rolling-restart-tls` on both paths).

- **Status.** amended (2026-06-24: day-2 read convention).
