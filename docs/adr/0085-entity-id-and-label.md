## ADR-0085. A registry entity carries an immutable `id` and a mutable `label`

**Status:** accepted, **not implemented** (epic NIM-725; this ADR is NIM-727)
**Amends:** [ADR-0079](0079-incarnation-name-template.md) (`name_template` → `id_template`, `name_not_composable` → `id_not_composable`, `IncarnationCreateRequest.name` → `.id`, the listing flag `composes_name` → `composes_id` at [`0079:217-220`](0079-incarnation-name-template.md); the composed string is validated against `IDPattern`), [ADR-0083](0083-declared-secret-state-fields.md) (§1's derivation tuple becomes `(service.id, incarnation.id, <state-field>, <key>)`, and its 2026-08-26 amendment loses one of its four enforcement surfaces — see Consequences), [ADR-029](0029-service-registry.md) (the `service_registry` clause "`name` PK (kebab-case, matches `service.yml → name`)" at [`0029:16`](0029-service-registry.md) — the parenthetical becomes unsatisfiable once the manifest key is gone), [ADR-064](0064-secret-write-path.md) (the deterministic path `secret/<domain>/<entity>/<field>` where `entity` = the record's name at [`0064:21`](0064-secret-write-path.md) → the record's `id`), [ADR-008](0008-coven-stable-tags.md) (the 2026-07-17 / NIM-124 amendment's spelling `${ incarnation.name }`; the selector metavariable `coven=<label>`), [ADR-047](0047-purview.md) (the `incarnation=` dimension row "`incarnation.name` of the target instance", [`keeper/rbac.md:116`](../keeper/rbac.md) — doc-only, the **stored selector string does not change**), [ADR-0081](0081-roster-at-create.md) (the create contract carrying `name` in the body beside the roster input), [ADR-0082](0082-service-vars.md) (`NewServiceVars`' CEL env declares `incarnation`, `shared/cel/engine.go:218-222`, so `vars/_stack.yaml` is a **third** CEL surface reading `incarnation.name`), [ADR-012](0012-keeper-soul-grpc.md) (its 2026-08-03 amendment: `flow_context` is a `Struct`, so a changed map key is invisible to forward-compat only-add), [ADR-025](0025-augur.md) (Omen `name` → `id`; plus the `Rite.ID` surrogate carve-out below), [ADR-030](0030-vigil-oracle.md) (Vigil / Decree `name` → `id`; `Decree.on_beacon`; the mandatory `Decree.incarnation_name`), [ADR-052](0052-herald-notifications.md) (Herald / Tiding `name` → `id`; the `Tiding.herald` FK; feeds ADR-064's `<entity>` slot), [ADR-017](0017-keeper-side-core.md) (the Provider and Profile registries; the self-onboard FQDN prediction), [ADR-032](0032-push-orchestrator.md) (`push_providers.name`, [`0032:63`](0032-push-orchestrator.md)), [ADR-044](0044-choir.md) (the FK **column** renames on `incarnation_choirs` / `incarnation_choir_voices`, `keeper/migrations/060_create_choirs.up.sql:44,69`), [ADR-014](0014-operator-identity.md) (`operators.display_name` → `label`, `keeper/migrations/003_create_operators.up.sql:17`; `aid` stays the id)
**Implemented by:** nothing yet — NIM-728 … NIM-733 (the tail beyond the eight `NamePattern` registries is NIM-732; the operator rename is NIM-733). The deletion of the manifest key `name:` is NIM-726 and lands beside this.

---

## Problem

Every registry entity in Soul Stack is addressed by one field, `name`, and that one field is asked
to do two jobs that pull in opposite directions.

**As an identifier** it must never move. It is the `TEXT PRIMARY KEY` of twelve tables
(`incarnation`, `service_registry`, `providers`, `profiles`, `push_providers`, `omens`, `heralds`,
`tidings`, `vigils`, `decrees`, `rbac_roles`, `synods`), it is the second segment of every derived
Vault path ([ADR-0083](0083-declared-secret-state-fields.md) §1), it is the CEL root
`incarnation.name`, it is a URL path segment in 49 OpenAPI path templates, and it is the value of
the RBAC `incarnation=` scope dimension. Nothing in the tree renames one — there is no rename
operation anywhere, and [ADR-0079](0079-incarnation-name-template.md) records the consequence in so
many words: *"the name is **immutable**: it is the `TEXT PRIMARY KEY` of the `incarnation` table,
globally unique, with no rename operation. Fixing a mistyped name means destroy and recreate"*
([`0079:17-20`](0079-incarnation-name-template.md)).

**As a caption** it must be free to move. An operator wants `Redis — Billing (prod)` on a screen,
not `redis-billing-prod`; a service that gets re-scoped wants its display text to follow. Today the
only place that text could go is the identifier — and on the registries whose identifier becomes a
Vault path segment (service, incarnation, herald, provider) editing it there is not a cosmetic
mistake but an orphaned secret, silently (see "Why `id` is immutable", below).

The two jobs also disagree about grammar. The identifier wants one narrow, lower-case, bounded
alphabet, because it becomes a path segment and a CEL selector. The caption wants capitals, spaces
and punctuation. Because there is one field, the narrow grammar wins everywhere and the caption is
simply not expressible — and even the narrow grammar is not one grammar. Four dialects are in the
tree today, all verified on disk:

| pattern | where |
|---|---|
| `^[a-z0-9-]{1,63}$` | `keeper/internal/augur/augur.go:59`, `keeper/internal/herald/herald.go:25`, `keeper/internal/oracle/validate.go:31`, `keeper/internal/profile/profile.go:19`, `keeper/internal/provider/provider.go:21` |
| `^[a-z0-9][a-z0-9-]{0,62}$` | `keeper/internal/incarnation/incarnation.go:60` |
| `^[a-z][a-z0-9-]{0,62}$` | `keeper/internal/pushprovider/pushprovider.go:28` |
| `^[a-z][a-z0-9-]*$` | `keeper/internal/serviceregistry/types.go:61`, `keeper/internal/rbac/crud.go:63` |

The fourth has **no upper bound at all**, and a `service_registry` name is segment 2 of every
derived secret path.

## Decision

**A registry entity carries two fields instead of one.**

| field | grammar | mutability | role |
|---|---|---|---|
| **`id`** | `^[a-z0-9][a-z0-9-]{0,62}$` | **immutable**, set once at registration | primary key, CEL root, URL path segment, Vault path segment, RBAC scope value |
| **`label`** | free text, capitals allowed | **mutable**, any time | display only; not unique, not required |

Both are set **at registration**. At registration the UI seeds `label` with a Title-cased default
derived from the git path; the operator may change it at any time and nothing else moves.

`service.yml` carries neither. The manifest key `name:` is **deleted** — that removal is NIM-726 and
lands beside this ADR. It is the same shape of decision [ADR-007](0007-versioning-git-ref.md)
already made for `version:`: a manifest field that duplicates an external source of truth does not
exist, because the two drift and any mechanism that keeps them in agreement is one more moving part.
The service's identifier lives in the registry row the operator created, exactly as its version
lives in the git ref.

### THE INVARIANT — `label` participates in nothing derived

This is the whole reason the field is split, and every other clause in this ADR is downstream of it.

**`label` participates in nothing derived.** Not a Vault path. Not an RBAC scope. Not a snapshot
directory. Not `incarnation.<...>` in CEL. Not a selector. Not a resolver. Not an FK. Not a URL.

Only under that invariant is *"I changed the label and nothing moved"* a guarantee rather than a
hope. A `label` that reached one derived surface would make every label edit a potential rename of
something, and the operator would be back to the field they already have. Without the invariant the
split buys nothing.

### Why `id` is immutable — two separate arguments

These are two different mechanisms and they are stated apart on purpose. Merging them produces a
false inference that an earlier draft of this decision carried, and that the architect caught.

#### Clause 1 — a changed id orphans what was written under the old one

`SecretField.VaultPath` assembles `<mount>/<service>/<incarnation>/<state-field>[/<key>]` by
`strings.Join` over the segments, substituting each **verbatim**
(`shared/config/secret_field.go:153-188`, the join at `:180-187`). The only check applied to a
segment is `ValidVaultPathSegment` = `^[a-zA-Z0-9_-]+$` (`shared/config/secret_field.go:59-66`):
capitals pass, nothing folds case, and Vault KV paths are case-sensitive.

**And nothing in the tree rewrites or migrates a Vault path when an identifier changes — no rename
operation exists anywhere.** So a changed id — a case-only change included — makes the platform
derive a *different* path, and the value at the old path becomes unreachable. No error is raised
anywhere: the new path is perfectly well-formed, it simply holds nothing.

No claim about merge-or-replace is needed for this argument, and none is made in it.

**The sharpest instance is one hop shorter than the service/incarnation case.**
`<mount>/herald/<entity>/<field>` and `<mount>/provider/<entity>/credentials` take that `<entity>`
segment **directly from the registry row's name** (`keeper/internal/secretwrite/writer.go:124-135`,
[ADR-064](0064-secret-write-path.md) — *"`entity` = record name"*, [`0064:21`](0064-secret-write-path.md)).
Herald and Provider are two of the eight `NamePattern` registries. Renaming a herald orphans its
signing secret with no state schema in between and nothing to notice: one hop, no error.

#### Clause 2 — replace-not-merge is what makes a *collision* destructive

Separately, and about a different failure: `Writer.WriteKV` dispatches to
`KVv2(mount).Put` (`keeper/internal/vault/client.go:458`) or `KVv1(mount).Put` (`:460`) — **`Put`,
not `Patch`** (`keeper/internal/vault/client.go:436-467`). A field present in the prior version and
absent from the payload is gone from the current one. The package doc states it at
`keeper/internal/secretwrite/writer.go:20-27`: *"a service by that name derives its secrets onto
these paths and `Writer.WriteString` replaces rather than merges."*

That property is what makes **two writers landing on one path** destructive, and it is the argument
behind the reserved-namespace fence of [ADR-0083](0083-declared-secret-state-fields.md) / NIM-706 —
`secret_field.go:164-167` refuses to emit a path whose service segment is a reserved namespace. It
is **not** the rename argument, and it must not be quoted as one.

#### The framing that is truer and easier to defend

All of these identifiers are **already immutable de facto.** Every registry primary key is
`name TEXT PRIMARY KEY`, no surface offers a rename, and
[ADR-0079](0079-incarnation-name-template.md) writes it down for incarnation at
[`0079:17-20`](0079-incarnation-name-template.md). This decision does not *introduce* immutability.
It **names** it, and it creates the escape hatch that was missing.

The capital letter lives in `label` and never in `id`.

### The `id` grammar — one grammar for every registry

**`^[a-z0-9][a-z0-9-]{0,62}$`** — `incarnation`'s existing pattern
(`keeper/internal/incarnation/incarnation.go:60`), adopted unchanged by all of them. Three
properties decide it:

- **All lower-case**, as decided. Case-folding is not an option: a Vault path segment passes through
  `strings.Join` verbatim (clause 1), so `Redis` and `redis` are two different secrets.
- **Bounded at 63.** This closes `serviceregistry`'s `^[a-z][a-z0-9-]*$`
  (`keeper/internal/serviceregistry/types.go:61`) having no upper bound at all while its value
  becomes segment 2 of every derived Vault path. It also closes an existing disagreement between the
  Go constant and the wire schema: `ServiceRegisterRequest.Name` advertises the unbounded
  `^[a-z][a-z0-9-]*$` (`keeper/internal/api/huma_service_op.go:39`) while
  `IncarnationCreateRequest.Service`, naming the same registry row, already advertises the bounded
  incarnation pattern (`keeper/internal/api/huma_incarnation_op.go:44`). Unifying makes the second
  one correct rather than lucky.
- **A leading digit stays legal**, so the change adds no break beyond the rename itself. The live
  fixture `examples/service/redis/scenario/create/tests/provision-badname-rejected/case.yml:16`
  turns on `9redis` being *a valid incarnation name and an invalid VM base* — the case file says so
  at `:2` — and it keeps its premise. Adopting `pushprovider`'s letter-first form would have
  invalidated the test's whole point, which is that the two grammars are different.

`pushprovider`'s letter-first rule has a mechanical reason of its own — the name translates into
`SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` and a leading digit breaks an env-var name
(`keeper/internal/pushprovider/pushprovider.go:25-28`). That is a **local** constraint on one
registry, not a platform grammar, and it stays where it is as an additional check on top of the
shared `IDPattern`.

### `id` means "code word", not "opaque identifier"

State this plainly, because the neighbouring names invite the opposite inference.

> **`id` in Soul Stack means "code word", not "opaque identifier".** A registry entity's `id` is
> lower-case kebab, chosen by a human, and legible in a URL, a Vault path segment and a CEL
> expression — the same kind of thing `SID` (an FQDN) and `AID` (`archon-alice`) already are, and
> the same kind of thing [ADR-0083](0083-declared-secret-state-fields.md)'s reveal id
> (`<state-field>.<property>`) is. Where the platform needs a genuinely opaque handle it uses a
> **surrogate** and says so: `apply_id` / `errand_id` / `AuditEvent.id` are ULIDs and `Rite.id` is a
> database sequence number. A surrogate is not an entity id in this sense, and an entity id is never
> a surrogate.

The awkward neighbours are named here rather than left for a reader to trip over:

- **`augur.Rite.ID int64`** (`keeper/internal/augur/augur.go:107`, surfaced as `RiteView.ID` at
  `keeper/internal/api/huma_augur_reply.go:64`) sits **inside Augur, one of the renamed registries**.
  After the rename `augur.Omen.id` is a kebab code word and `augur.Rite.ID` an int64 surrogate, in
  one package. That is recorded as a carve-out, not fixed here; a possible `Rite.ID` rename is a
  follow-up.
- **`SoulHistoryItem.ID`** (`keeper/internal/api/huma_soul_reply.go:143`) is a ULID —
  `apply_id | errand_id`.
- **`AuditEvent.ID`** (`keeper/internal/api/huma_audit_endpoint_reply.go:45`) is a ULID.

None of the three becomes an entity `id`, and none of them is touched by this decision.

### "label" now names two things in Soul Stack, and they never meet

The word is already live in an unrelated sense, and the overload has to be resolved in prose rather
than left to context. Nothing already recorded is falsified — those sentences stay true precisely
*because* the new `label` participates in nothing derived.

> **"label" now names two things in Soul Stack, and they never meet.** A **matching label** is a
> Coven tag or a Trait pair — it lives in `souls.coven[]` / `souls.traits` / `incarnation.covens` /
> `incarnation.traits`, it is what the RBAC selectors `coven=` and `trait.<key>=` and a rule's
> Subject resolve against, and it is what [ADR-008](0008-coven-stable-tags.md) / NIM-281 means by
> "a label is never inherited". An entity's **`label` field** is a display caption on the registry
> row itself: free text, mutable, non-unique, optional, read by the UI and by nothing else. No Vault
> path, no RBAC scope, no CEL root, no snapshot directory, no selector and no resolver reads an
> entity's `label` — that is the entire reason the field exists, and it is what makes "I changed the
> label and nothing moved" a guarantee rather than a hope. Consequently `coven=<coven-tag>` in the
> selector grammar means a **Coven tag** and never this field, and no entity's `label` is ever a
> Coven tag.

The selector metavariable, which the docs spelled `coven=<label>`, is re-spelled
`coven=<coven-tag>` wherever it appears, so that a reader never meets the two senses on one line: [`naming-rules.md`](../naming-rules.md),
[`keeper/rbac.md`](../keeper/rbac.md), [`keeper/operator-api.md`](../keeper/operator-api.md),
[`keeper/mcp-tools/souls.md`](../keeper/mcp-tools/souls.md), [ADR-033](0033-errand.md) and
[ADR-0074](0074-interactive-console-pty.md). That sweep is mechanical and changes no behaviour.

### Scope — platform-wide, not the eight `NamePattern` registries

`name` is abolished as an entity identifier **everywhere**, not only in the eight registries that
carry a `ValidName` / `NamePattern` pair.

- **All twelve `name TEXT PRIMARY KEY` tables convert**, including `rbac_roles`
  (`keeper/migrations/026_create_rbac.up.sql`), `synods`
  (`keeper/migrations/069_create_synods.up.sql`) and the choir tables' FK **columns**
  (`keeper/migrations/060_create_choirs.up.sql:44,69`).
- **All 49 `{name}` OpenAPI path templates become `{id}`**, including `/v1/roles/{name}`,
  `/v1/synods/{name}` and `/v1/modules/{name}`.

**A half-rename at the URL layer is the same defect that was rejected at the wire and DB boundary.**
An operator reading `/v1/roles/{name}` and `/v1/incarnations/{id}` in one spec has to learn which
registries were in scope on which day, and the word `name` — now meaning nothing in the data model —
keeps its old suggestion of mutability exactly where a caller is most likely to act on it. One word
for one concept, or the concept is not one.

The tail beyond the eight is ticketed **NIM-732** so that it is scheduled rather than assumed.

### `operators.display_name` → `label`

The `operators` registry already has its identifier: `aid`
([ADR-014](0014-operator-identity.md), `keeper/migrations/003_create_operators.up.sql:16`). It does
**not** move, and its grammar is deliberately wider than kebab —
`^[a-z0-9][a-z0-9._@-]{1,127}$` — because AIDs arrive from external identity providers
(ADR-014 amendment 2026-05-29). `aid` is the one entity id in the platform that is not the
`IDPattern`, and that exception is recorded rather than smoothed away.

What moves is the caption: **`operators.display_name` → `label`**
(`keeper/migrations/003_create_operators.up.sql:17`), in this same epic, ticketed **NIM-733**. One
word for one concept across every registry is the whole point; a single registry keeping a private
spelling for the display caption would be the same drift this ADR removes elsewhere. The migration
and the `/v1/operators` OpenAPI break are **accepted costs**, named here so they are not discovered
during implementation.

### CEL reach and the compatibility window

The rename reaches the expression language. `incarnation.name` → **`incarnation.id`**, and with it
the [ADR-0079](0079-incarnation-name-template.md) family: `name_template:` → **`id_template:`**,
`composes_name` → **`composes_id`**, `name_not_composable` → **`id_not_composable`**.

This breaks **every service repository** — a service's scenarios are where `${ incarnation.name }`
is actually written. So the change ships with a **compatibility window**:

1. the engine accepts **both** roots for a period;
2. `soul-lint` warns on the old root, with a file and a line address;
3. the old root is dropped.

**This window is a different axis from [ADR-0076](0076-engine-compat-window.md)'s**, and the two
must not be conflated. ADR-0076 declares, per entity, the range of **keeper versions** that can
execute a definition — a property of the definition, negotiated against the engine. This window is a
**one-off transition of one spelling**, has no per-entity declaration, and ends by deletion rather
than by a version moving past it. A service author does not opt into it and cannot pin it.

## Consequences

### A fence surface is lost (not a security regression — an authoring-time regression)

Deleting `service.yml → name` removes one of the four enforcement surfaces of the reserved-Vault-
namespace fence. [ADR-0083](0083-declared-secret-state-fields.md) names them:
*"Four surfaces, one predicate: the manifest load (`service_name_reserved`), registration over REST
and MCP, the derivation, and reveal"*
([`0083:621-626`](0083-declared-secret-state-fields.md)). The manifest load is the one that goes.

The **floor still holds** — `shared/config/secret_field.go:164-167` refuses to emit a colliding path
at derivation, and registration still refuses the name. What is lost is that the author found out at
**authoring time**, offline, with a line number. After this, an author who names a service `herald`
learns it from a rejected registration instead of from a lint run.

NIM-726 closes the offline half with a `soul-lint --service-name` flag plus a warning when the flag
is absent. Recorded here rather than left to be discovered live.

### The stale CEL root is a runtime failure, not a compile error

`incarnation` is declared `cel.DynType` in all three CEL environments
(`shared/cel/engine.go:50-57` scenario/destiny, `:82-87` flow-control, `:218-222` service-vars; the
declaration itself is `cel.Variable(name, cel.DynType)` at `shared/cel/engine.go:270`). A DynType
root does not type-check its fields, so `incarnation.name` after the drop is a **no-such-key at
evaluation**, not a compile error.

One of those three environments is flow-control, and it is evaluated **on the host**:
`flow_context = {input, vars, incarnation, self}` (`keeper/internal/render/pipeline.go:1358-1359`).
So a stale `when: incarnation.name == …` does not fail at render — it fails mid-run, on the host,
after earlier tasks have already applied. That is precisely the class
[ADR-012](0012-keeper-soul-grpc.md)'s 2026-08-03 amendment recorded for `essence`: `flow_context` is
a `Struct`, a changed map key is invisible to forward-compat only-add, and **a mixed-version fleet
across this boundary is unsupported.**

**Therefore `soul-lint`'s warning is load-bearing, not a courtesy.** It is the only static catcher
the platform has for this rename, on either side of the wire.

### Consumer breakage, named concretely

- **`soul-stack-web`** — `types.gen.ts` is code-generated from the spec, and the path templates it
  routes on change. The build and the routing break **silently from the core's point of view**: core
  `make check` cannot see the companion repo. The companion needs `npm run gen:api`, and the core
  needs `make sync-webui` after it.
- **`soulctl/internal/client`** — the same rename with no codegen behind it. Manual.
- **[`docs/keeper/openapi.yaml`](../keeper/openapi.yaml)** — a **derived** artifact
  ([ADR-054](0054-openapi-code-first.md): Go types → OpenAPI via huma, committed for review and for
  the UI vendor). `make check-openapi` reddens until `make gen-openapi` runs. The huma `pattern:`
  tags are **hand-transcribed** from the Go constants — `keeper/internal/api/huma_augur_reply.go:65`
  carries the transcription in a trailing comment (`// ← augur.NamePattern`) — so they must move with
  the constants or the spec will advertise a grammar the server no longer enforces.
- **MCP tools** — the argument `name` renames across nine tool docs under
  [`docs/keeper/mcp-tools/`](../keeper/mcp-tools/) and the tools themselves. MCP is a **primary**
  operator surface ([ADR-004](0004-binaries.md)), not a wrapper, so this is a contract change and not
  a documentation sweep.
- **Every service repository** — 35 occurrences of `incarnation.name` over 23 files under
  `examples/` alone. External service repos are invisible from here; the compatibility window above
  exists for them.
- **`keeper/migrations`** — twelve `name TEXT PRIMARY KEY` columns plus every `*_name` FK column.
- **`soul-stack-plugins`** — no proto contract change, but the `soul-mod-*` scenario and test
  artifacts and the `soul-lint plugin-init` templates (`soul-lint/internal/plugininit`) need the
  same sweep.

### Two things are NOT decided here and need propose-and-wait before NIM-728 … NIM-733

Both are new names in closed catalogs, so neither can be picked during implementation:

1. **The permission name for the label mutation.** A mutable field needs one, and
   `incarnation.update` is not available to reuse — it was deliberately deleted
   ([`naming-rules.md:912-914`](../naming-rules.md), NIM-330) and the RBAC permission catalog is a
   **closed enum**, so an unparseable grant aborts the enforcer snapshot load rather than failing one
   check.
2. **The matching audit event name.** `shared/audit/event_types_gen.go` is a **derived** file — its
   first line reads *"Code generated by `make gen-audit-catalog`. DO NOT EDIT."* — regenerated from
   the declarations in `shared/audit/event_types.go` and pinned by
   `TestGeneratedEventTypes_NoDrift`. A name added to the declarations and not regenerated fails the
   build.

## Rejected

- **Making `name` mutable in place — adding a rename operation.** It is the shape everyone reaches
  for first, and clause 1 is why it is not cheap: an identifier is substituted verbatim into a Vault
  path, so a rename that does not also migrate Vault silently orphans secrets, and one that does
  needs a path-migration mechanism, a window where both paths are live, and an answer for a crash in
  the middle. The split buys the same outcome — an operator-editable caption — with none of that.
- **A half-rename: `{id}` in the DB and on the wire, `{name}` left in the URL path templates.** It
  is the same defect that was rejected at the wire and DB boundary, one layer out, and the layer it
  survives in is the one an operator reads. See "Scope", above.
- **Renaming everywhere except CEL — leaving the root spelled `incarnation.name`.** It avoids
  breaking service repositories, which is a real cost, but it leaves one entity with two names and
  makes the field's mutability ambiguous exactly where an expression author has to reason about it.
  The compatibility window pays that cost properly instead of declining to.
- **An opaque surrogate (ULID / UUID) as the entity id.** It would make immutability structural
  rather than a rule. It is rejected because it destroys the property `id` is for: an id appears in a
  URL, a Vault path and a CEL expression, all of which humans read and write. The platform already
  distinguishes the two — see "`id` means code word" — and this decision keeps the distinction
  rather than collapsing it.
- **Scoping the rename to the eight `ValidName` / `NamePattern` registries.** Named because it was
  the epic's original text and the user widened it deliberately. Half a data model using `id` and
  half using `name`, with no rule a reader could derive, is worse than either end state.
- **Keeping `operators.display_name` under its own spelling.** One registry with a private word for
  the same concept is the drift this ADR removes everywhere else.
