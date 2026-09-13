## ADR-0086. One schema dialect — `state_schema` is written in the input DSL

**Status:** accepted, implemented — NIM-742 (engine), NIM-743 (`list-secret-paths`), NIM-751 (the input side of §5). NIM-744 (`examples/**` + the WB redis mirror) is outstanding.
**Amends:** [ADR-003](0003-destiny-format.md) (the manifest's typed schema stops being JSON Schema and becomes the platform's own input DSL), [ADR-009](0009-scenario-dsl.md) (`state_schema` joins the `input:`/`output:` grammar instead of sitting beside it), [ADR-010](0010-templating.md) (`secret: true` and `type: secret` now live in one dialect and must be told apart by the reader, not by the file they are in), [ADR-062](0062-input-types.md) (`$type` becomes readable from `state_schema`, and its closed conflict set gains exactly one key there), [ADR-0083](0083-declared-secret-state-fields.md) (§1's declaration is re-spelled in the new dialect; §7's refusal of `required:` on a secret keeps its ground and changes its mechanism)
**Implemented by:** NIM-742 (engine), NIM-743 (`soul-lint list-secret-paths`), NIM-744 (rewrite `examples/**` + the `wb-service-redis` mirror). Recorded by NIM-741, epic NIM-740.

> This ADR was authored **without a number**, as `draft-one-schema-dialect.md`, and stamped
> **0086** when it was squash-merged into the release — the convention decided 2026-09-01 and
> recorded in [docs/adr/README.md](README.md). Two parallel sessions had each read the same
> "highest number" from their own worktree and both reached for the next one; allocating at the
> merge, where the release tip is known and tickets land one at a time, is what removes that.
> The `NNNN-<slug>.md` filename and `ADR-062`-style citation are unchanged — only the moment of
> allocation moved. 0086 had additionally been reserved by hand for this ticket before the
> convention landed; it is the last number reserved that way.

---

### Problem

A service author writes two schemas in one file and the platform never compares them.

`examples/service/redis` is the worked example, and the two statements have already
diverged. `types.yml:20` declares the shared `AclUser` type with

```yaml
    required: [name, perms]
```

and gives `state` a `default: "on"` at `:57-60` — optional, defaulted. The `state_schema`
describing the same accounts declares, at `service.yml:326`,

```yaml
        required: [name, perms, state]
```

Same three properties, same records, two different answers to "is `state` required", and
nothing in the tree reads both. The type is resolved on the input side by
`config.ResolveTypeRefs`; `state_schema` is a raw `map[string]any` that reaches its
consumers unexpanded (§8). There is no diagnostic because there is no comparison, and
there is no comparison because the two texts are not in the same language.

The divergence is a symptom. The cause is that **`state_schema` is JSON Schema and
everything else an author writes is not.** The input DSL has spent this repository's whole
history acquiring a vocabulary — `additional_properties`, `min`, `min_length`, `min_items`,
`exclusive_min` (`shared/config/input_schema.go:322-342`) — a field-level `required: true`,
named types, `required_when`, `prefill_from_state`, `vault_scope`. `state_schema` has none
of it. It has `additionalProperties`, `minimum`, and a `required: [names]` list, validated
by a structural checker that deliberately declines to interpret most of what it reads:

> Extended JSON Schema (`enum`/`pattern`/`min`/`max`/`items`/`additionalProperties`, etc.)
> is deliberately NOT typed in MVP — it's a large draft-07 standard. We catch a malformed
> schema but don't validate the semantics of each key (PM decision).
> — `shared/config/service.go:496-499`

So the author writes one word two ways depending on which half of `service.yml` the cursor
is in, and the half that is JSON Schema is the half nothing checks.

**`required` is the sharpest edge, because it means two different things in the DSL
already.** `shared/config/input_schema.go:8-10` states the split:

- at the parameter level (any type) — a bool, "is this parameter required";
- inside `type: object` — a `[]string` list of required `properties` sub-keys.

The two are decided by YAML node kind at `:349-373` and stored in two Go fields, `Required`
and `RequiredProps` (`:50-52`). One word, two model fields, a parser branch to tell them
apart, and a reader who has to know which context they are in. `state_schema` then uses
only the second meaning, and `types.yml` — which is the input DSL — uses both. That is how
`types.yml:20` and `service.yml:326` came to say different things about one record without
either of them being wrong in its own dialect.

The corpus makes the migration small enough to do rather than fence. There are **28**
authored flow-form `required: [...]` sites across the whole DSL, spanning `service.yml`
state schemas, `types.yml` catalogs and destiny/scenario `input:` blocks. **Block-form
`required:` exists nowhere in the DSL** — all 204 block-form occurrences are
`docs/keeper/openapi.yaml`, a generated artifact this ADR does not touch (§12). So the
rewrite is a pure flow-form rewrite with no multi-line reflow anywhere. `additionalProperties`
is 26 real sites (24 across `examples/service/{redis,dragonfly,mongo}/service.yml`, plus
`soul-lint/testdata/service-golden/redis-ha.yml:12,20`; three further textual hits are
comments). `minimum` is 6 sites — `redis/service.yml:172,183`, `mongo/service.yml:57,63`,
`dragonfly/service.yml:98`, `example-cloud-bootstrap/service.yml:31`. `maximum`,
`minLength`, `maxLength`, `minItems`, `maxItems`, `exclusiveMinimum` and `uniqueItems` have
**zero** authored uses in the DSL. `patternProperties` has zero authored uses at all: the
only occurrences in code we write are the reader itself
(`shared/config/secret_field.go:426` comment, `:456` `secretPropsBag`), and the only other
hit anywhere in the tree is the vendored
`keeper/internal/api/docsassets/rapidoc-min.js`.

### Decision

**`state_schema` is written in the input DSL. There is one schema dialect in this
repository, and JSON Schema is not it.**

#### 1. `state_schema` is an `InputSchemaMap`

The root of `state_schema` is a map `<field name>` → schema, exactly as `input:` is. Three
things are refused **at the root**: `type: object`, the `properties:` wrapper, and the list
`required: [names]`. Requiredness is `required: true` on the field itself.

```yaml
state_schema:
  redis_type:
    type: string
    required: true
    enum: [sentinel, cluster]
  redis_version:
    type: string
```

⚠ **Root-only.** A **nested** object field still declares `type: object` with its fields
under `properties:` — that is how the input DSL spells an object
(`docs/input.md:399-404`), and nothing about it changes here. What disappears is the one
redundant envelope at the top, where `type: object` was mandatory and could not be anything
else (`validateStateSchema`, `shared/config/service.go:500-543`, code
`state_schema_root_not_object`, emitted at `:509`, `:522`, `:534`). A reader who takes
this section to mean "objects no longer have `properties:`" has inverted the decision.

```yaml
  tls:
    type: object                       # nested — unchanged
    additional_properties: false
    properties:
      enable: { type: boolean }
      only:   { type: boolean }
      port:   { type: integer }
```

`validateStateSchema` and `validateJSONSchemaNode` (`shared/config/service.go:567`) are
replaced by `validateInputSchemaMap` (`shared/config/input_schema.go:382`), which is where
every semantic check the "deliberately NOT typed" comment declined to write already lives.

#### 2. The list form `required: [names]` leaves the whole dialect

Not only from the root — from **nested objects and `types.yml` too**. After this ADR
`required` is a bool everywhere in every schema an author writes, and the parser branch at
`shared/config/input_schema.go:349-373` collapses to one arm; `RequiredProps` and
`requiredList` go with it.

This is the change that removes the class of defect in §Problem rather than one instance
of it. Two statements about one property can no longer be written in two places, because
after the change there is exactly one place per property to write it.

Supporting, and the reason the platform ends up with fewer dialects rather than a third
one: **the module param DSL is already bool-only.** `sdk/schema.Param.Required` is a plain
`bool` (`sdk/schema/schema.go:191`), and the struct's own doc comment says the object case
is not expressible there at all (`:184-186`). So this makes three of three dialects agree
on one spelling, where today two of three disagree with the third and with each other.

The cost is named: `AclUser` today states `required: [name, perms]` in one line
(`types.yml:20`) and becomes `required: true` on two of its three properties. That is more
lines and less duplication. Which of the two divergent answers about `state` is the correct
one — optional-with-`default: "on"` per `types.yml:57-60`, or required per
`service.yml:326` — is a per-field decision for NIM-744 and is not settled here. This ADR
settles that the question is now *askable*.

#### 3. The vocabulary is snake_case throughout

Decided with the user 2026-09-01. This is **alignment onto an existing spelling**, not a
new vocabulary: every target below is already the input DSL's word today
(`shared/config/input_schema.go:322-342`, `docs/input.md`).

| JSON Schema in `state_schema` | the input DSL word | authored sites |
|---|---|---|
| `additionalProperties` | `additional_properties` | 26 |
| `minimum` | `min` | 6 |
| `maximum` | `max` | 0 |
| `minLength` / `maxLength` | `min_length` / `max_length` | 0 |
| `minItems` / `maxItems` | `min_items` / `max_items` | 0 |
| `exclusiveMinimum` | `exclusive_min` | 0 |
| `uniqueItems` | `unique` | 0 |
| `patternProperties` | — | 0 |

Two entries need a word. `uniqueItems` → **`unique`** is not a mechanical transliteration —
the input DSL's word is `unique` (`shared/config/input_schema.go:647`), and an implementer
transliterating the table would introduce a key that does not exist. And
`patternProperties` is **refused outright** rather than mapped: it has no input-DSL
counterpart, and with zero authored uses inventing one would be a new entity decided
inside a migration ticket. It is not a blocker and must not be recorded as one.

The reader to change is `keeper/internal/incarnation/secret_schema.go:92`, whose
`CollectStateSchemaSecrets` walk keys off the literal string `"additionalProperties"`. See
§Consequences — that consumer fails **open**, and it fails open on exactly this key.

#### 4. `$type` plus the node's own `properties:` — only in `state_schema`

A `state_schema` node may carry `$type: T` **and** its own `properties:`, overlaying
properties onto the referenced type. Nowhere else, and no other key.

★ **Sharpen the size of this, because it reads far wider than it is.** The refusal set is
closed and short: `input_type_ref_conflict` fires on `{type, properties, items}` and
nothing else (`shared/config/input_types.go:100`). And an overlay already exists — since
ADR-062, `applyRefOverlay` (`shared/config/input_types.go:391-410`) merges the reference
node's `description`, its field-level `required` (bool) and its `required_when` onto the
resolved type. So the rule was never "either a reference or inline"; a closed overlay was
already the design. **This ADR widens that overlay by exactly one key — `properties` — and
only inside `state_schema`.** `type` and `items` stay refused everywhere, including here.

Why `state_schema` and not `input:`: state describes both a shape and the secrets declared
against it, and a secret **does not and must not exist on the input side** — the whole
point of ADR-0083 §1 is that the value never travels through operator input. A shared type
therefore carries the shape, and the state field adds the one property that only makes
sense where the value is stored. On the input side there is nothing to add, so the key
stays refused there and the diagnostic keeps its current meaning.

**Merge semantics: add-only, shallow, fail-closed.** A property name present in both the
type and the overlay is an error, not a winner — code `input_type_ref_overlay_conflict`.
This is adopted **by reference from ADR-009's `extends:` covenant**, which resolves the
same question the same way for the same reason, rather than invented here: two authors
disagreeing about one property is a fact worth reporting, and silently preferring either
one produces a schema neither of them wrote. Shallow, because a deep merge would let an
overlay reach inside the type's nested objects, which is a second composition mechanism
next to `$type` itself.

#### 5. `type: secret` is allowed in `types.yml`

There is no conflict between the two meanings of "secret", because they are two mechanisms
and always were.

- On **input**, a secret is the modifier `secret: true` on an ordinary field
  (`shared/config/input_schema.go:47`). The operator types the value; a `vault:` reference
  is allowed in it within the field's `vault_scope`.
- In **state**, a secret is a **type** — `type: secret` ([ADR-0083](0083-declared-secret-state-fields.md) §1).
  The platform mints the value, it lives in Vault, and it never enters state at all.

The rule, verbatim:

> A property with `type: secret` in a shared type is not asked for on input — the platform
> mints it; in `state_schema` it means a declared secret.

##### Resolved 2026-09-02 by NIM-751 — candidate 1, in three places

Deferred by the user 2026-09-01, decided 2026-09-02: the sentence above is now enforced
by the engine. It was, when this ADR was written, a **statement of intent the engine did
not enforce**, and five things said so:

1. The input `type` vocabulary has no `secret` member, and there was no notion of a
   non-writable property anywhere in `shared/config/input_value.go`. A `type: secret`
   property reached through `$type` from an `input:` block was a type the input validator
   did not know.
2. The property is **declared**, so `additional_properties: false` does not reject an
   operator-supplied value for it — a closed object rejects *undescribed* keys, and this
   key is described. (Nor is `additional_properties` enforced on values at all:
   `validateObjectFields` skips a field with no schema, `shared/config/input_value.go`.)
3. Such a value flowed into `params.value` of the `core.state.set` step like any other
   input.
4. The per-cell seal ([ADR-010](0010-templating.md) §7.4) is provenance-based on
   `secret: true`, **not** on `type: secret`.
5. `StripDeclaredSecrets` (`shared/config/secret_field.go:533`) strips on the way into
   Postgres — after transit through `apply_task_register`.

**The decision is candidate 1: refuse an operator-supplied value.** Silently dropping it
was the alternative and was rejected by the user for the case that matters — a client that
sends a password would be told the request was accepted, and the operator would believe
the value they typed is in force while the platform mints a different one. Candidate 2 —
refusing an `input:` reference to a type that carries a secret at all — stays rejected:
§5's premise is that **one** type is shared between the form and the state, and refusing
the reference forbids exactly the sharing this section grants.

It lands in **three** places, because "not asked for" has three spellings here and closing
any two of them leaves the rule resting on the author:

1. **It is not on the form.** `stripFormSecrets`
   (`keeper/internal/artifact/scenario_types.go:203-321`), called at
   `keeper/internal/artifact/scenarios.go:304`. The operator form travels as a raw
   `map[string]any` re-emitted from YAML on a path that never builds an `InputSchema`, so
   the strip lives there rather than in the typed walk — two walks, one marker
   (`config.SecretTypeName`). It runs **after** `$type` substitution (before it there is
   nothing to strip) and **only** on the `input:` path: `rawStateSchema` projects through
   the same resolver and must keep its declared secrets, which is §8's whole point.

   ★ **The survival rule is emptiness, not contagion**, and the three positions differ.
   `items:` is an array's whole content, so a minted element takes the array with it.
   `additional_properties:` is one of the two ways an object describes content (§14): a
   minted one loses that KEY and the object survives on its `properties:` — dropping the
   object outright took its fillable properties with it, which is over-reach, not
   enforcement. ⚠ Losing the key is a FORM fact, not a gate fact: undescribed keys were
   never checked in depth (point 2 above) and still are not, so what the gate refuses
   under a dropped open map is the minted VALUE, not the arbitrary key. `properties:` is stripped member by member, and an object left with none
   goes, because a widget with no inputs is not a form field. A `properties: {}` the
   author wrote was not emptied by the strip, so it does not itself trigger the rule —
   which is not a promise the node survives: an object whose only other content is a
   minted open map still has nothing to fill, and goes. The
   `form:` layout beside the schema is filtered by the same pass
   (`dropStrippedFormFields`), because a section naming a field whose schema just went
   away would ship a labelled input with nothing behind it: "not on the form" has to be
   true of both halves of the reply. Only names THIS strip removed are dropped — a
   `form:` naming a parameter that never existed stays, because that is an author error
   `form_field_unknown` exists to report.
2. **It is never required of the operator.** §7's refusal of `required:` on a secret runs
   off `state_schema` through `CollectSecretFields`, so a type reached **only** from
   `input:` never passes it. Requiredness therefore fails closed in the value path
   instead — `requireInputValues` and `validateObjectFields` skip it, and
   `mergeInputDefaults` materializes no `default:` for one (same gap, same reason).
   `required_when:` is skipped by the same line and is therefore inert on such a
   property too, deliberately: a conditional demand for a value no caller can produce is
   the unconditional one with an extra step. ★ The predicate is `NotAskedOfOperator`,
   **not** `isDeclaredSecret`: it applies the same emptiness rule the strip does, through
   `items`, `additional_properties` AND `properties`. The form drops a field with nothing
   left to fill, so requiredness must drop the same field,
   or the operator is told a field they were never offered is required. The two walks
   are separate code over two representations and have already disagreed twice, so they
   are pinned against each other by a differential test rather than by this paragraph
   (`TestStripFormSecrets_AgreesWithTypedPredicate`); `NotAskedOfOperator` is exported
   for no other reason.
3. **A value supplied anyway is refused.** `input_secret_type_not_writable` — the code
   reserved above, now spent — as the sentinel `config.ErrSecretTypeNotWritable`
   (`shared/config/input_secret_type.go`), wrapped by `scenario.ErrInputInvalid` into a
   422. ★ The refusal runs **ahead of the type check** in `validateValueAt`: `secret` is
   in no value's type enum, so without that ordering the caller is told the password
   "does not match type `secret`" — a vocabulary complaint where the truth is that the
   property is not theirs to fill, and one printed with the literal **unmasked**, since
   `literalFor` keys off `secret: true`, which a declared secret does not carry. The
   refusal names the path (`$.users[0].password`) and never the value: it lands in
   `incarnation.StatusDetails` and in audit.

   ★ **It reaches `additional_properties` too, and that is a separate mechanism.**
   `validateObjectFields` skips a key the object's `properties:` does not describe —
   point 2 above — so the ordinary walk has no reach there at all, while the form strip
   does. Left alone that is the worst pair available: the property is off the form and
   the value is accepted anyway. `refuseDeclaredSecretValues`
   (`shared/config/input_secret_type.go`) closes it by walking the
   additional-properties schema for **declared secrets only** — no type match, no enum,
   no pattern. Widening that walk into real validation of undescribed values is a
   different decision with its own compatibility cost and is **not** taken here.

**The seal is deliberately not extended.** Candidate 1 as written above paired the refusal
with widening the per-cell seal from `secret: true` to `type: secret`. With the value
refused at the input gate it never reaches a cell, so a seal over it would mask nothing —
and a masking rule that cannot fire is the kind of guard that reads as enforced and is
not, which is §6's own argument. Point 4 above therefore stands as a fact about the seal,
not as a gap.

Writing `type: secret` **directly** in an `input:` block was never the open half: it is
refused at load as `input_type_invalid`, positionally, with a hint naming `secret: true`
instead. Only the `$type` route was open.

⚠ **Offline (L0) it is two places, not three.** `keeper/internal/trial` does not resolve
`$type` at all, so a trial fixture sees `{$type: T}` with an empty `Type` and
`validateValueAt` returns before the refusal. A `tests/<case>/case.yml` supplying the
password therefore passes L0 and 422s in production. This is the pre-existing absence of
the whole `$type` resolve from the trial harness, not something NIM-751 introduced — and
it costs more than this rule: type, `enum`, `pattern` and nested `required` are all
unchecked in L0 for a `$type` field. It is exactly the L0↔prod drift the shared input
gate exists to prevent, and it is **NIM-754**; remove this paragraph when that lands.

#### 6. The `type: secret` node grammar stays closed to `type` / `key` / `label`

`secretNodeKeys` (`shared/config/secret_field.go:93`) is unchanged. Under one dialect the
shared input keys now collide with it, so the refusal set has to be written out rather than
implied, and each entry carries ADR-0083 §1's reason: **a value that never enters state
never passes state validation, so a constraint written next to `type: secret` reads as
enforced and is not.**

Refused, with the reason attached:

- `default` — a default for a value the platform mints is never consulted.
- `enum`, `pattern`, `min_length` / `max_length` — constraints nothing evaluates. The
  generation policy that *does* constrain the value travels in the `SecretRequest` at the
  call site (ADR-0083 §3/§9), where it is next to the expression that uses it.
- `secret` — the other mechanism (§5). Writing both on one node asserts that the value
  both lives in state and does not.
- `prefill_from_state` — pre-fills from a state path that is empty by construction.
- `required_when` — see §7; requiredness of any kind cannot be satisfied.

**`description` is refused too** (user, 2026-09-01). `label` already carries the caption
the UI renders (`secretNodeLabel`, `shared/config/secret_field.go:402-415`), and two words
for one job is the drift this ADR exists to remove, at the smallest possible scale.

#### 7. `required: true` on a `type: secret` property is refused

This restates ADR-0083's *"listing a secret in the element's `required:` is refused"* for
the new dialect. The ground is unchanged and is a satisfiability one: the value is in
Vault, so no state instance can ever contain it, so no state instance can ever satisfy the
requirement.

**The mechanical trap has to be recorded, because the obvious implementation is silently
wrong and the obvious correction is loudly wrong.**

Today the check reads the *list*:

```go
	for _, req := range stringSeq(items["required"]) {
		if req == prop {
```
— `shared/config/secret_field.go:365-373`

`stringSeq` (`:466-478`) returns nothing for anything that is not a `[]any` of strings.
After §2 there is no list, so this loop iterates zero times — **silently, forever**, and
`secret_field_required` becomes a diagnostic that cannot fire. Nothing about that is
visible in a test that only asserts the good cases.

And the naive replacement fires the **wrong** code. Move `required` onto the node and the
node now carries a key outside `secretNodeKeys` (`:93`), so `checkSecretNodeGrammar`
(`:383-399`) claims it first and reports `secret_field_unknown_key` — "type: secret does
not take required (want type/key/label)". That is a grammar complaint where the truth is a
satisfiability one, and it points the author at the wrong fix.

**Requirement: `required` is special-cased ahead of the grammar check**, so
`secret_field_required` fires with its own message. A guard test must pin the code, not
merely the failure.

#### 8. Normative: where `$type` must be expanded

**Once a secret can arrive through a shared type, `$type` resolution must run on the
manifest load path before any consumer of `state_schema`.** This is a requirement, not
advice.

Today `ResolveTypeRefs` (`shared/config/input_types.go:419`) runs on **scenario input
only**. The manifest load path has no resolution step at all: `art.Manifest.StateSchema` is
the raw decoded map, handed to its consumers verbatim. There are six, and every one of them
walks the map structurally:

| consumer | address |
|---|---|
| `CollectSecretFields` | `shared/config/secret_field.go:220-276` (entry at `:231`) |
| `CollectStateSchemaSecrets` | `keeper/internal/incarnation/secret_schema.go:38`, `:75-105`; handler entry `keeper/internal/api/handlers/incarnation_secret_schema.go:61` |
| `StripDeclaredSecrets` | `shared/config/secret_field.go:506` |
| `ScanDuplicateSecretKeys` | `shared/config/secret_collection_keys.go:54`; callers `soul-lint/internal/validate/secret_collection_keys.go:54`, `keeper/internal/artifact/scenario_input_types.go:84` |
| reveal | `keeper/internal/api/handlers/incarnation_reveal_secrets.go:336` |
| the `core.state.*` write path | `keeper/internal/coremod/state/state.go:169`, `:175`; threaded raw at `keeper/internal/scenario/keeper_dispatch.go:296` via `coremodutil.WithStateSchema` (`keeper/internal/coremod/util/runctx.go:64-79`), from `keeper/internal/scenario/run.go:591,748` |

An unexpanded schema makes a `$type`-carried secret **invisible to the collector**. That is
verbatim the failure `docs/adr/0083-declared-secret-state-fields.md:106-110` forbids:

> The dangerous failure here is not a false rejection, it is a shape the collector does not
> recognise passing as *no secrets here* — the author believes the value is in Vault, and a
> plaintext password sits in `incarnation.state` with nothing masking it.

Two properties of the context channel make this sharper. The map is shared, not copied, and
every reader treats it as immutable (`runctx.go:67-69`) — so resolution has to happen where
the artifact is built, not per consumer. And `state.go:166-174` already fails closed on a
nil schema and on a refused declaration; an *unexpanded* schema is neither, and it fails
open.

#### 9. The `<key>` guard does not move

`<key>` comes from state **data** — the name an operator typed. It is the one segment of a
derived Vault path influenced from outside the platform.

`ValidVaultPathSegment` (`shared/config/secret_field.go:65-66`) and the fail-closed
`SecretField.VaultPath` (`:153-189`, which checks every segment and returns an error rather
than emitting a path) stay exactly where they are. **Moving the declaration into a shared
type does not move the check.** A service's own `types.yml` constraining the name — as
`AclUser` does, `types.yml:27` — is not a substitute, and ADR-0083 already said why:

> The service's own `types.yml` constrains it too, but the platform does not rely on a
> service having done so.
> — `docs/adr/0083-declared-secret-state-fields.md:88-95`

A shared type makes that constraint *more* reusable and therefore easier to mistake for
enforcement. It is not enforcement.

#### 10. Where the derived path is legible, and where it deliberately is not

**The state-field segment of a derived path is decided by the place of use, not by the
type.** In `state_schema` the whole path reads off the page — the field name stands
directly above the `$type` that carries the secret. In `types.yml` alone it does not, and
that is correct: a type describes a shape, not an address.

The corpus already shows why. `examples/service/redis/service.yml` declares the *same*
secret shape — `type: secret`, `key: name` — under two different fields, at `:331-334`
(`redis_users`) and `:354-357` (`system_acl_users`), deriving
`…/redis_users/<name>#password` and `…/system_acl_users/<name>#password`. Burning an
address into a shared type would forbid the second use. Whether NIM-744 factors those two
into one shared type is its call; the dialect must permit it either way.

The whole picture — every declared secret and its derived path across a service — is a
tool's job, not a file's. That is **NIM-743** (`soul-lint list-secret-paths`).

Two consequences follow, and both must be written down or they read as bugs:

**(a) A type carrying `type: secret` is neither legal nor illegal in isolation.** It is
legal under an array's `items` and refused one object deeper —
`secret_field_unsupported_location` (`shared/config/secret_field.go:270`), whose message is
*"type: secret is only supported on a top-level property or on a property of a top-level
array's items"*. Legality is a property of the **use site**, so the secret rules are
**deliberately not** among the standalone checks `soul-lint` runs over `types.yml`. Those
checks are catalog-shaped only — duplicate, cycle, unknown reference — and
`soul-lint/internal/validate/type_refs.go:6-23` says so; the file is not even read unless
some scenario's `input:` references a type (`:44-48`). Recording this is the point: the
omission is a decision, not a gap.

**(b) The reserved-name fence survives.** `reservedStateFieldIssue`
(`shared/config/secret_field.go:283-292`, NIM-706) evaluates `IsReservedStateField` on the
**state field name at the use site**. That name is static after `$type` expansion, exactly
as it is static today, so the `tls` fence and `secret_field_reserved_state_name` are
unaffected by anything in this ADR.

#### 11. Breaking, with no transition window — and a dual-parse window would be actively wrong

The precedent is direct. ADR-0083 landed the same way twice —
*"this lands before the release, where compatibility is not yet owed"*
(`docs/adr/0083-declared-secret-state-fields.md:507-509`) and
*"a service already registered under one of the four names stops loading and must be
renamed"* (`:635-637`) — with the `examples/` adopters migrated in the same commit.

Here the argument is stronger than precedent, because a compatibility window would be worse
than no window. **An un-migrated `state_schema: {type: object, properties: {…}}` does not
fail under the new dialect.** It parses. It parses as a state schema with two fields named
`type` and `properties`, and every real field silently moves one level down, out of every
top-level lookup — including `topLevelProperty` (`keeper/internal/coremod/state/state.go:175`),
which is what stands between a capture and a field the schema never declared.

A silent misparse is worse than either form, so the old envelope is refused **by name**:

- `state_schema_legacy_json_schema_form` — a `state_schema` whose root carries
  `type: object` and/or `properties:`;
- `input_required_list_removed` — a `required:` whose value is a sequence, anywhere in the
  dialect.

Both names approved by the user 2026-09-01, together with `input_type_ref_overlay_conflict`
(§4). A dual-parse transition window is rejected in §Rejected for the same reason.

#### 12. Boundary — what this ADR does not touch

Genuine JSON Schema surfaces stay JSON Schema, because they are **published to foreign
contracts** rather than authored in our DSL. Without this paragraph NIM-744 migrates the
wrong files.

- **Plugin schemas.** `sdk/schema.Document` carries `ProfileSchema` for a CloudDriver —
  *"JSON Schema of the VM profile parameters"*, `sdk/schema/schema.go:86-90`, published over
  RPC `CloudDriver.Schema` (`keeper/internal/pluginhost/clouddriver.go:43-44`) — and
  `ParamsSchema` for an SshProvider. **Nine** of the twelve `examples/module/*/schema.json`
  files carry them — the six `soul-cloud-*` and the three `soul-ssh-*`; the three
  `soul-mod-*` are `kind: soul_module` and carry neither — and those nine use exactly the
  vocabulary §3 renames: the six `soul-cloud-*/schema.json`
  spell `required`, `minimum`, `maximum`, `additionalProperties`, and
  `soul-stack-plugin/ssh-static`'s `schema.json` spells a `oneOf` of two `required` lists.
  These are a plugin author's document, not a service author's, and they stay.
- **MCP tool schemas.** `inputSchema` / `outputSchema` are *"JSON Schema draft 2020-12"*
  with *"Required fields in `required: [...]`"* — [mcp-tools.md](../keeper/mcp-tools.md),
  line 47. That is the Model Context Protocol's contract, not ours.
- **`docs/keeper/openapi.yaml`.** Generated, and the source of all 204 block-form
  `required:` occurrences (§Problem). It is not authored and it is not migrated.

#### 13. The wire contract does not change

Both endpoints ship the schema free-form, so nothing about §1–§3 is visible to the
generator:

- `GET /v1/services/{id}/scenarios` — `Scenario.InputSchema` is `map[string]any`
  (`keeper/internal/artifact/scenarios.go:82`), rendered in OpenAPI as
  `additionalProperties: {}` / `type: object` (`docs/keeper/openapi.yaml:3468-3470`);
- `GET /v1/services/{id}/state-schema` — `ServiceStateSchemaReply.Schema` is
  `*map[string]interface{}` (`keeper/internal/api/huma_service_reply.go:66-72`, fed from
  `keeper/internal/artifact/state_schema.go:98`), rendered the same way
  (`docs/keeper/openapi.yaml:3717-3719`).

No OpenAPI change, no codegen, no `soulctl` reconciliation.

**`x-required` is kept as vestigial.** It is produced by
`keeper/internal/artifact/scenario_types.go:29-35,138-141` for the case ADR-062 describes —
a `$type` node's own field-level `required: true` could not go into the DTO key `required`,
because that key was taken by the object-level list of required children. §2 frees the key,
so the annotation is redundant. Retiring it is nonetheless a DTO change plus a web-UI
re-vendor, which buys nothing this ticket needs.
[ADR-042](0042-backend-driven-ui.md) stays untouched.

⚠ **Unverified, and it must be verified in the companion repo before NIM-744 closes.** The
vendored bundle `keeper/internal/webui/assets/assets/index-p3C4pQQ2.js` contains **three**
distinct required-readers, and only one of them survives §2 unchanged:

- the field-level predicate — `e.required===true || e["x-required"]===true`, else
  `e.required_when` — which is already the bool form and keeps working;
- a nested-object reader that builds a set from the parent's list:
  `Array.isArray(s) ? s.filter(…) : []` over `e.properties` / `e.required`;
- an array-of-objects editor reading `Array.isArray(r.items.required) ? r.items.required : []`
  — i.e. exactly the `AclUser`-shaped case.

The two list readers do not crash under the new dialect; they yield `[]`. Whether the child
nodes' own `required: true` is consulted in those two places instead **cannot be determined
from a minified bundle** — if it is not, nested and array-item required children silently
lose their marker in the form. `soul-stack-web` must confirm from source. This is a
correction to an earlier reading of this file, which recorded the branch as top-level only.

#### 14. ★ RESOLVED by NIM-742 — recorded here as it was written

> **Resolved 2026-09-01 with the user as candidate 1 below**, and implemented in the
> same ticket: an object describes its contents through `properties` OR
> `additional_properties`, and the bare `false` counts as neither because it forbids
> keys rather than describing them (`shared/config/input_schema.go:1155-1172`). The
> section is left in its original wording — rewriting it is NIM-741's, the ticket that
> owns this file — but it must not be read as an open question, because the ADR's
> Status line says the ADR is implemented and this heading used to say the opposite.

**Map-shaped state fields cannot be expressed in the input dialect as it stands.**

`validateObjectSchema` (`shared/config/input_schema.go:1005-1015`) raises
`missing_required_field` — *"object parameter must declare properties"* — whenever
`properties` is absent. Unconditionally: there is no exemption when `additional_properties`
carries a schema, and the check runs before `additional_properties` is even looked at
(`:1016-1029`). The normative docs agree — `docs/input.md:403` marks `properties` *(required)*
for `type: object`, and `:532` lists *"absence of `properties` in `object`"* among what the
linter must catch.

Every open map in the current state corpus therefore has no legal spelling. In redis alone:
`redis_config` (`service.yml:135-137`, `additionalProperties: true`), `sysctl_settings`
(`:199-202`), `redis_sentinel.master_settings` (`:387-390`) and `.settings` (`:391-394`);
plus the dragonfly and mongo equivalents and `soul-lint/testdata/service-golden/redis-ha.yml:11-12,18-20`.

The workaround already exists in the tree and is exactly what must not spread:
`examples/destiny/redis/destiny.yml:226-230` writes

```yaml
    properties: {}
    additional_properties:
      type: object
```

— an empty `properties:` written only to satisfy the check. **Do not write `properties: {}`
into an example to make one parse.** It converts a design gap into 26 sites of noise that
nothing will ever clean up.

Two candidate resolutions, **neither chosen and neither recommended here**:

- **Relax the input dialect** so `type: object` may omit `properties` when
  `additional_properties` carries a schema. Note what this is: a change to the **input**
  dialect, not only to state — it reaches every `input:` block, the form generator and
  `docs/input.md`. It needs its own propose-and-wait.
- **Re-express map-shaped state fields another way.** The cost is concrete: `redis_config`
  is where operators put arbitrary Redis configuration, and it is deliberately opaque
  (`service.yml:110-112`).

NIM-742 cannot land without one of these. It is recorded here rather than decided because
the first candidate changes a dialect this ADR was not chartered to change.

### Worked example

**Before** — `examples/service/redis/service.yml:120-137`, `:148-157`, `:322-335`,
abridged to the three shapes that matter:

```yaml
state_schema:
  type: object
  required: [redis_type, redis_config]
  properties:
    redis_type:
      type: string
      enum: [sentinel, cluster]
    redis_config:
      type: object
      additionalProperties: true
    tls:
      type: object
      additionalProperties: false
      properties:
        enable: { type: boolean }
        port:   { type: integer }
    redis_users:
      type: array
      items:
        type: object
        required: [name, perms, state]
        additionalProperties: false
        properties:
          name:  { type: string }
          perms: { type: string }
          state: { type: string, enum: [on, off] }
          password:
            type: secret
            key: name
            label: "Redis user password"
```

**After:**

```yaml
state_schema:
  redis_type:
    type: string
    required: true
    enum: [sentinel, cluster]

  # redis_config is an OPEN map and has no legal spelling yet — see §14.
  # Do not write `properties: {}` here to make it parse.

  tls:
    type: object                       # nested object — unchanged (§1)
    additional_properties: false
    properties:
      enable: { type: boolean }
      port:   { type: integer }

  redis_users:
    type: array
    items:
      $type: AclUser                   # the shape comes from types.yml
      properties:                      # the state_schema-only overlay (§4)
        password:
          type: secret
          key: name
          label: "Redis user password"

  admin_token:                         # a scalar declared secret: no key:
    type: secret
    label: "Admin token"
```

and `types.yml` loses its list (§2), which is where the divergence of §Problem dies:

```yaml
types:
  AclUser:
    type: object
    additional_properties: false
    properties:
      name:
        type: string
        required: true
        pattern: "^[a-z][a-z0-9_-]*$"
      perms:
        type: string
        required: true
        pattern: "…"
      state:
        type: string
        enum: [on, off]
        default: "on"
```

There is now one statement about `state`, in one file. Today there are two —
`types.yml:57-60` and `service.yml:326` — and they disagree.

### Consequences

**One consumer fails OPEN and must migrate in the same commit as the parser.**
`CollectStateSchemaSecrets` (`keeper/internal/incarnation/secret_schema.go:75-105`) keys off
the literal strings `"properties"`, `"items"` and `"additionalProperties"`. With no root
`properties:` wrapper the walk starts a level too high and returns an **empty** set — and
an empty set is not an error anywhere: `StateSchemaSecrets` returns a nil interface for it
(`:39-42`), and every caller degrades to `audit.MaskSecrets`, the vault+regex layer, by
design (`:26-28`, `keeper/internal/api/handlers/incarnation_secret_schema.go:26-29`). So
read-path masking and the form-prefill secret exclusion degrade **silently** to regex
masking, and a `secret: true` state field stops being masked with no diagnostic anywhere.

**In the verb engine the fail-direction differs per verb, and only one of the two is
dangerous.** Both verbs read through `stateop.schemaFieldType`
(`keeper/internal/stateop/ops.go:489-504`), which returns `""` when `schema["properties"]` is
absent — under the new dialect, for **every** field.

`add` fails **closed**. `collectionKind` (`:470-487`) consults the schema only in the
`!present` branch — a value already in state answers from its own Go type (`:471-479`) — and
where the schema *is* consulted and yields `collKindUnknown`, `applyAddOp` returns an error at
`:428`: *"field %q is not a collection (map/list) and the type can't be inferred from
schema"*. Loud and safe. The sting worth keeping is the wording: it blames the schema's
**content** when the cause is its **dialect**, so an author reading it goes looking in the
wrong place.

`append` fails **open**, and it is the only silent state-corruption path in the verb engine.
`applyAppendOp` (`:343-357`) guards its `nil` branch with
`if schemaFieldType(schema, op.Field) == "object"` (`:351`), and its own comment states that
this is the lookup's whole purpose: *"The kind lookup exists only to REJECT a map field —
append has no meaning there, and silently coercing one would lose data"* (`:340-342`). With
`schemaFieldType` returning `""` the guard never fires, `:354` executes
`out[op.Field] = []any{op.Value}`, and a **list** is materialized where the schema declares a
map — wrong shape in `incarnation.state`, no diagnostic anywhere. **`ops.go:351` belongs to
NIM-742** and moves in the same commit as the parser.

The L0 twin is not a third occurrence of either. `trial.collectionKind` /
`trial.schemaFieldType` (`keeper/internal/trial/diff.go:28-58`) have **no call site anywhere
in the repository** — inside `trial` the only reference to `collectionKind` is its own
definition and the only reference to `schemaFieldType` is `collectionKind` calling it. So the
harness holds a stale duplicate that **reads as coverage and is not**: it neither reproduces
the `append` defect nor catches it. It migrates with its prod twin to keep the two texts
honest, not for a behavioural reason.

The ordinary breaking ones, each of which fails loudly:

- `validateStateSchema` / `validateJSONSchemaNode` (`shared/config/service.go:500-543`,
  `:567`) are replaced by `validateInputSchemaMap`; `state_schema_root_not_object` retires
  and `state_schema_legacy_json_schema_form` takes over the case it used to catch (§11).
- The `RequiredProps` model and its readers go with §2 —
  `shared/config/input_schema.go:50-52`, `:349-373`, and `applyRefOverlay`'s comment about
  the two coexisting model fields (`input_types.go:394-398`).
- `CollectSecretFields`' entry through `schema["properties"]`
  (`shared/config/secret_field.go:231`) and `scanSecretNodes`' `inProps` argument
  (`:432-449`, whose `secretPropsBag` at `:456` also names `patternProperties` — §3 refuses
  the key, so the entry becomes dead and should go with it).
- `soul-lint validate-service`, and every diagnostic that names a `state_schema` YAML path.
- The fixture corpus: `examples/**`, `soul-lint/testdata/**`,
  `keeper/internal/trial/trial_test.go:1043` and
  `keeper/internal/trial/vault_namespace_guard_test.go:71` (both write the old envelope
  inline as a Go string literal), and
  `examples/destiny/{vector,node-exporter,redis-exporter}/_trial/service.yml`.

**For the service author.** One dialect, one `required`, one place per property. `$type`
reaches state, so a record described once is described once. The price is §14: until it is
resolved, an open map has no spelling.

### Rejected

- **Keep the inline schema, without `$type` in `state_schema`.** It preserves the exact
  duplicate that produced the divergence in §Problem: `AclUser` in `types.yml` and the same
  three properties written out again at `service.yml:326-335`. Rejected by the user.
- **A dual-parse transition window** — accept both the old envelope and the new root for a
  release. Rejected on the mechanism, not on taste: the old form does not fail under the new
  parser, it **misparses** into two fields named `type` and `properties` (§11). A window
  whose failure mode is silent is worse than the breaking change it defers, and the corpus
  it protects is 28 `required:` sites and 26 `additionalProperties:` sites in one repository.
- **An amendment to [ADR-062](0062-input-types.md) instead of a new ADR.** The decision
  amends five ADRs and one of them — [ADR-003](0003-destiny-format.md), which is where "YAML
  + typed schema (JSON Schema → CUE)" is recorded — cannot host it without inverting its own
  title. And "`state_schema` stops being JSON Schema" filed under an ADR called *Named input
  types* is a decision nobody will find when they look for it.
- **Mapping `patternProperties` onto a new input key.** Zero authored uses (§Problem), no
  existing counterpart, so it would be a new entity invented inside a migration ticket.
  Refused outright instead, and recorded as a refusal rather than as a blocker.

### Cross-references

- [ADR-045](0045-param-dsl.md) — the module param DSL is already bool-only
  (`sdk/schema/schema.go:191`). Supports §2: three of three dialects agree afterwards.
- [ADR-042](0042-backend-driven-ui.md) — untouched. `x-required` is kept as vestigial (§13).
- [ADR-057](0057-state-changes-crud-verbs.md), [ADR-070](0070-secret-reveal-path.md),
  [ADR-0084](0084-explicit-state-capture.md) — cross-reference only; the verbs, the reveal
  path and the capture step are unaffected by a change of spelling. What §Consequences does
  affect is the *reader* the verb engine uses to resolve a field's collection kind.
- [ADR-064](0064-secret-write-path.md) — the segment grammar behind §9 is its write-path
  grammar, lifted into `shared/config` by ADR-0083 §1. Unchanged.
- [ADR-019](0019-state-migration-dsl.md) — **verified untouched.** `keeper/internal/statemigrate`
  never references `state_schema` outside two doc comments (`statemigrate.go:1`,
  `dsl.go:3`), and a migration's `state.*` paths are never validated against it —
  `keeper/internal/incarnation/crud.go:155-156` and `:187-188` both state that key existence
  is not checked against the service's `state_schema`, and
  `keeper/internal/coremod/state/state.go:124-126` records that `Validate` cannot reach the
  schema at all. Recorded so nobody re-audits it. [migrations.md](../migrations.md) needs no
  change.
- [ADR-023](0023-trial-test-runner.md) — the L0 `assert` contract is unaffected, and
  `trial.collectionKind` / `trial.schemaFieldType` (`keeper/internal/trial/diff.go:28-58`)
  are **dead code** — no call site in the repository (§Consequences), so L0 neither
  reproduces the `append` fail-open nor catches it. They migrate with their prod twin to keep
  the duplicate honest, not because L0 behaviour changes.
- **NIM-734 / NIM-735 (the parallel migrations epic)** — its `schema.lock` fingerprints the
  **parsed** schema, so this dialect switch changes every fingerprint exactly once. That is
  a one-time re-stamp and it has to be sequenced with NIM-742, not discovered by it.
