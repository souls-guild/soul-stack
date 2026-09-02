## ADR-0083. A secret is a declared state field — the author never writes a Vault path

**Status:** accepted, implementing (NIM-698)
**Amends:** [ADR-070](0070-secret-reveal-path.md) (`revealable_secrets` is absorbed — reveal derives its path instead of reading an authored template), [ADR-064](0064-secret-write-path.md) (the deterministic-path convention extends from operator input to state fields), [ADR-009](0009-scenario-dsl.md) (`state_schema` gains a field-level secret declaration), [ADR-010](0010-templating.md) (a new CEL function `generate_secret` and type `SecretRequest`; `vault()` is fenced out of the service's own namespace), [ADR-017](0017-keeper-side-core.md) (a new keeper-side module `core.state`, addressed `core.state.set` after [ADR-0084](0084-explicit-state-capture.md)), [ADR-056](0056-staged-render-passage.md) (keeper-side register becomes readable by Soul-side consumers), [ADR-012](0012-keeper-soul-grpc.md) (per-field `secret: true` on module output retires `no_log`)
**Implemented by:** NIM-698

### Problem

A service author who needs a generated password writes a Vault path. Not once — in
every channel that touches it, by hand, and nothing checks that the copies agree.

`wb-service-redis` is the worked example. The same path is spelled in four places:

- `scenario-create.yml:127` — `compute.users_path`, read by the Soul-side consumers;
- `scenario/deploy.yml:22` — `vars.users_path`, because `compute.*` is not in scope on
  an `on: keeper` task, so the keeper-side generator recomputes it;
- `service.yml:67` — `revealable_secrets[].vault_ref`, so the UI can find the value;
- `types.yml:11-12` — in prose, so a human reading the type knows where to look.

The comment above the second one states the invariant and its enforcement mechanism:

> It must match compute.users_path, or we generate passwords at one path and read them
> from another.

(translated — `wb-service-redis` keeps its comments in Russian by design)

The invariant is enforced by a comment. Change the shape of one and the run generates
passwords at one path and reads them from another — no diagnostic, no test, a Redis that
rejects every client. There are 17 `${ vault(...) }` call sites in that service, and each
one is an opportunity to disagree with the other three channels.

**The authored path is also a security boundary the platform pays for twice.**
[ADR-070](0070-secret-reveal-path.md) needed three independent code-level guards —
a load-time placeholder requirement, a runtime prefix allowlist, and the
`DeniedByVaultFloor` backstop — for exactly one reason, stated there: the manifest is
not a trusted input, and an author (or a compromised service repo) could point
`vault_ref` at `secret/keeper/sigil-keys/*`. Those guards are correct and stay. But
they exist to contain a capability the author never needed: the ability to name an
arbitrary path. Remove the authoring and the escalation class stops existing at its
root rather than being fenced at three checkpoints.

**And plaintext has nowhere safe to travel.** A keeper-side task that generates a
password can hand it onward only through its register, and register payloads are
persisted in `apply_task_register`. So the value sits in Postgres in the clear until
`purge_apply_task_register` collects it — bounded, but a window that exists for no
reason other than the transport.

### Decision

#### 1. A secret is a field in `state_schema`

The declaration is where the field is, not where the store is:

```yaml
redis_users:
  type: array
  items:
    type: object
    required: [name, perms, state]
    properties:
      name:  { type: string }
      perms: { type: string }
      state: { type: string, enum: [on, off] }
      password:
        type: secret
        key: name                                    # sibling property that addresses this secret
```

`type: secret` marks the property. `key:` names the sibling property whose value
addresses one element's secret within the collection; a scalar secret field omits it.
That is the whole declaration — the schema says *this is a secret and this is how it is
addressed*, and nothing else. **How a missing value is minted is not declared here**: the
policy travels inside the `SecretRequest` the author writes at the call site (§3), where
it is visible next to the expression that uses it. A schema-side default would be a
second place to write the same thing, and reconciling the two is precisely the class of
problem this ADR exists to remove.

The platform derives the Vault path from `(service, incarnation, field, key)`:

```
collection:  secret/<service>/<incarnation>/<state-field>/<key>#<property>
scalar:      secret/<service>/<incarnation>/<state-field>#value
```

so redis's `default_admin` password lands at
`secret/redis/<incarnation>/redis_users/default_admin#password`.

Every derived segment is validated against the `secretwrite` segment pattern
(`^[a-zA-Z0-9_-]+$`, [ADR-064](0064-secret-write-path.md)) and fails closed. This is not
a formality: `<key>` comes from state **data** — an operator-supplied user name — and is
therefore the one segment an attacker can influence. The service's own `types.yml`
constrains it too, but the platform does not rely on a service having done so. The
pattern moves out of `keeper/internal/secretwrite` into `shared/config` and both callers
read it there: a derived path and an operator-supplied secret must be checked by one
rule, not by two copies that drift.

The path is always under `secret/<service>/<incarnation>/` by construction, which is
the prefix ADR-070's runtime allowlist already requires. That guard and the
`DeniedByVaultFloor` backstop stay in force — defence in depth, now protecting a
boundary that is no longer reachable by authoring.

Two consequences of "only two shapes", both load-time errors rather than best-effort
guesses. A `type: secret` **anywhere else** — nested one object deeper, in an array of
arrays, under `additionalProperties`, inside a combinator — has no place in a formula
with one field segment and one optional key segment. And the declaration is refused
**totally**: the schema is first scanned for every `type: secret` node wherever it sits,
the two supported shapes then claim theirs, and whatever is left over is reported. The
dangerous failure here is not a false rejection, it is a shape the collector does not
recognise passing as *no secrets here* — the author believes the value is in Vault, and
a plaintext password sits in `incarnation.state` with nothing masking it.

The node's grammar is closed to `type`/`key`/`label` for the same reason §9 closed the
policy grammar: a `minLength: 40` written next to `type: secret` reads as enforced and
is not, because a value that never enters state never passes through state validation.
Listing a secret in the element's `required:` is refused on the same ground — it could
not be satisfied by any state instance.

Finally, `type: secret` must not be confused with the older, orthogonal marker
`secret: true` on a state property ([ADR-010](0010-templating.md) §7.4). That one
says *this value lives in state, mask it on the way out*; `type: secret` says the
opposite — the value never lives in state at all. Both stay, and neither replaces the
other. A `type: secret` path is nevertheless added to the masking walk: nothing writes
there, so the mask is normally inert, and it is there so a value arriving by some other
route — an old snapshot, a migration, a bug — is masked rather than printed.

#### 2. `revealable_secrets` is deleted

The registry declared four things: `id`, `label`, `enumerate`, `vault_ref`. Three of
them are now consequences of the schema — the field is the id, the collection is the
enumeration, the path is derived — and the fourth is the one being removed. `label`
survives as an optional property of the declaration for the UI caption.

The id an operator passes to reveal becomes `<state-field>.<property>` for a collection
secret and the bare `<state-field>` for a scalar one. The property has to be in it: one
element may carry more than one secret, and `redis_users` alone would not say which.

Reveal keeps its endpoints, its right `incarnation.view-secrets`, its audit event, and
its fail-closed 404 semantics from ADR-070 unchanged. Only path resolution changes:
derived from the schema and the current state instead of substituted into an authored
template. ADR-070's load-time guard `vault_ref_not_service_scoped` is deleted along
with the field it validated.

This also settles ADR-070's deferred "singleton secrets without `enumerate`" for free:
a scalar `type: secret` field has no `key:` and needs none.

The discovery response gains one field, `collection`, saying which of the two shapes a
declaration has. It is additive and it is not decoration: without it a client cannot tell
a collection whose state is currently empty from a scalar, and both render as a list of
zero ids.

#### 3. `generate_secret()` returns a request, not a value

```yaml
value: >-
  ${ compute.acl_inventory.map(u, merge(u, {
       'password': generate_secret({ 'length': 32, 'charset': 'alphanumeric' })
     })) }
```

The call evaluates to a `SecretRequest` — a typed marker carrying the policy — not to a
string. A `core.state.*` write resolves it at write time: the field already holds a value,
the marker is dropped; the field is empty, a value is minted per the marker.

The alternative, returning plaintext at render, is rejected in §Rejected. The short
version is that render is re-run per passage and per retry, so a generating function
mints a different value every time it is evaluated; a marker makes the expression pure
and keeps plaintext from existing at all until something durable is ready to hold it.

Argument form is a map because CEL has no keyword arguments and no `=` token —
`generate_secret(length=32)` fails in the lexer, not the type checker. The policy is
parsed at the call, against §9's grammar, so a typo is a render error pointing at the
expression the author wrote rather than a surprise surfacing inside a module two phases
later.

Registration needs no macro, unlike `vault()`: that one has a macro solely to inject the
hidden `__vault_resolver` argument, and a request carries nothing hidden. `SecretRequest`
is an **opaque** CEL type — no readable fields and no string form — so
`generate_secret({}).length` and `"pw-${ generate_secret({}) }"` are errors instead of
things that render. Only a cell that is exactly one `${ … }` yields a native value, and
that is the only shape `core.state.*` accepts. It is registered in the ordinary
scenario/destiny pass alone; in migration-CEL, flow-control and service-vars the call is
refused, because a request nobody resolves is inert data that looks like it did
something.

Leaving CEL, the request travels as an ordinary map under one reserved key —
`{"__secret_request": {"length": …, "allowed_chars": …}}` — because the two boundaries
it crosses (CEL→params, params→protobuf) carry no Go types. It carries the RESOLVED
policy, not the author's spelling, so nothing has to agree about what a charset name
means on the far side. An author can write that map out by hand; it is a forgery with no
privilege, since the effect is identical to calling the function and the payload goes
through the same bounds either way.

The seal detector is deliberately **not** extended to treat `generate_secret` as a
secret source. A request holds no secret material, and sealing is whole-cell: marking
the `value:` cell would mask the entire inventory — names, permissions, states — out of
the diagnostics for a task that failed, which is precisely the all-or-nothing coarseness
§8 removes from `no_log`. What does need masking is the consuming side, where a
reference resolves to plaintext; that is §6's concern, not this one's.

#### 4. `core.state.set` is the write

```yaml
- name: Redis users
  on: keeper
  module: core.state.set
  register: redis_users
  params:
    field: redis_users
    value: "${ compute.acl_inventory }"
```

One call is read-or-write. On a secret property the semantics are `present`, never
`set`: an existing value is kept and the incoming `SecretRequest` discarded. An
incidental re-render must not rotate a live credential, and deliberate rotation is
therefore not expressible here by design — it gets its own decision and its own ticket.

The module writes the secret value to the derived Vault path, and the record reaches
Postgres through the same state engine every other write goes through, with every
`type: secret` property stripped on the way in. Non-secret properties of the same object
are written normally. One write path to Postgres, not two — the module has no privileged
side channel, and a secret cannot reach a column even if a caller hands the field back
verbatim. *(Amended by [ADR-0084](0084-explicit-state-capture.md): the record lands at
the step, not in an end-of-run `state_changes` commit. What is stripped, and by which
engine, is unchanged.)*

**The register carries the effective state** — what is now stored, existing values
included, not what the caller proposed. This is the property that makes the whole
design work: only the writer knows which value won, so only the writer can be quoted.
A generator's output is a candidate; a writer's output is the truth.

**A register crosses the `include:` boundary downwards, and only downwards.** The
writer sits in a service's `main.yml` while the consumers that quote its register live
in included bodies (`_common/`, per-scenario partials), and `validateTaskRefs` runs
**per file** — so before this change a body reading `register.redis_users` failed with
`unknown_register_reference` although the name resolves perfectly at run time, where the
plan is one flat list. `ExpandIncludes` now collects the `register:` names declared at
each level (recursing through `block:`, which shares the flat address space) and threads
them into the bodies it splices in, transitively down the include chain.

The asymmetry is deliberate. **Sibling branches do not see each other's additions** —
each level builds a fresh map from the inherited one, so an include cannot borrow a name
from a file spliced in beside it. And the **includer→included direction only**: a main
file referencing a register declared *inside* a body it includes stays a per-file error,
because a conditional include is dropped as a whole group at render
([ADR-056](0056-staged-render-passage.md) group-drop) — a reference into a body that may
not exist in the plan must fail while the author is reading it, not at render.
Position within a level does not matter: an `include:` placed before the task declaring
the register it reads is legal, since resolution is by name over the flat list.

#### 5. Keeper-side register becomes readable by Soul-side consumers

Today a Soul-side task cannot read a keeper task's register: keeper results land in
`RenderInput.KeeperRegister`, read only by `keeperVars`, while `hostRegister` reads
`RegisterByHost[sid]` with a fallback to flat `Register`. `render.go:245-256` gives the
reason for the isolation:

> so a host doesn't accidentally read keeper-register when its per-host bucket is empty
> in a mixed Passage

That hazard is about **fallback ordering**, not about a host task deliberately naming a
keeper register. The keeper bucket is therefore **unioned into** each per-host bucket
rather than left as a fallback: the bucket is never "empty, therefore keeper's". Register
names are unique across a scenario — `task_refs.go:96` flags duplicates, block-to-top-level
included — so the union is unambiguous. A guard test keeps the old fallback path from
firing.

Consumers then read the field like any other register value:

```yaml
users: >-
  ${ merge(register.redis_users.effective.map(u, {
       u.name: { 'perms': u.perms, 'state': u.state, 'password': u.password }
     })) }
```

The stratifier orders this without help — `register.redis_users` in an apply input is a
register edge, the same axis `core.vault.kv-present` rides today.

`incarnation.state` is **not** touched: it remains the pre-run snapshot taken under the
row lock, invariant across passages (`run.go:331-336`). Refreshing it per passage was
considered and rejected (§Rejected).

#### 6. A secret in a register rides as a reference

`u.password` above is a reference in the register payload, resolved to plaintext during
render of the consuming task — the mechanism `vault:` params already use. Plaintext
of a **declared** secret therefore never enters `apply_task_register`, and the window
described in §Problem closes for it without touching the purge grace, which stays
load-bearing for cross-Keeper correctness ([ADR-002](0002-transport-grpc-ha.md)). A
credential a module returns in its `output:` is the other case and keeps that window —
§8 says why, and what bounds it instead.

The author writes `u.password` and never learns the difference.

#### 7. The service's own namespace is fenced off from every authoring channel

An author-written Vault path resolving under `<mount>/<service>/` is rejected — in
**every** spelling: `${ vault(...) }` in CEL, a `vault:` ref in `params:`, and the
`path:` of `core.vault.kv-read` / `core.vault.kv-present`. At load when the path is
written out; on the evaluated path or on the rendered params when it is not (the last
two paragraphs of this section). Fencing
only the CEL macro would leave the four-copies problem intact for anyone who preferred
one of the other spellings, and `kv-present` in particular is the same write this ADR
replaces, wearing a module's name.

The fence is on the **namespace**, not on the mechanism: each channel survives unchanged
for paths outside the service's own prefix. `vault()` survives for reads **outside**
the service's own namespace — a shared TLS CA,
a credential belonging to another service. That class is real (`examples/service/*/vars/`
carries `tls_cert_ref` pointing at `secret/services/<name>/tls#cert`) and it has no
replacement yet; deleting the function outright would break it. Declared cross-namespace
reads are named in §What this ADR does not carry.

The diagnostic is `vault_path_in_own_namespace`
([naming-rules.md](../naming-rules.md#error-codes)), raised at three points: `soul-lint`
over the include-expanded task list, keeper at `artifact.LoadScenarioManifestResolved`,
and — as a render abort — `render.Pipeline.Render`. The last is not redundant: an
`include:` body is parsed inside `config.ExpandIncludes`, *after* the load-time entry
point has returned, so a path smuggled into a sibling file is only visible offline (where
soul-lint expands includes itself) and at render. The render check sits before the first
Vault read of the run. Two further points catch what none of the three can see — the
path that only exists after evaluation; they are the last two paragraphs of this
section.

Neither the mount nor the incarnation segment is compared. The mount, because what makes
a path the service's own is the service segment, and load time does not know
`vault.kv_mount`; the incarnation, because at load it is still `${ incarnation.name }`.
The fence is therefore service-wide — strictly broader than the runtime gate of §6
(`render.ownNamespaceRef`), which is the correct direction: the whole
`<mount>/<service>/` prefix belongs to the platform now.

Not comparing the mount is only safe while the mount is exactly one segment, which is why
`vault.kv_mount` is now checked at load (`vault_kv_mount_invalid`). The comparison finds
the service in the first two segments of the path; a two-segment mount (`apps/kv`) pushes
it to the third and the fence stops matching, with nothing else changing — the one silent
failure this whole section exists to prevent.

Two cases static text cannot reach, both by construction rather than oversight. A path
assembled entirely out of variables (`vault(vars.some_ref)`) carries no literal segment
to compare against. And `vault_scope` on an input field is a prefix glob: `secret/*`
covers the namespace while naming no matchable segment, so a check that caught only the
written-out form would read as coverage it does not have.

Both are caught instead on the **evaluated** path, by a guard the render pass binds to
its context next to the vault memo (`render.ownNamespaceVaultGuard`, consulted in
`cel.callVault` before the memo lookup and therefore before any Vault read). How the
path was assembled stops mattering once CEL has produced it. The guard is scoped to the
whole `<mount>/<service>/` prefix, not to the incarnation being rendered — reaching
sideways into a sibling incarnation is the same second-copy failure, not a lesser one —
and it is a no-op for a caller with no service identity (push, unit eval), the same
condition §6's resolution applies. **Trial is deliberately not in that list**: the L0
harness carries a service identity of its own, so the fence is live in L0. Until NIM-726
that identity was the manifest's `name:`; the manifest states none now, so the harness
derives it from the service DIRECTORY, overridable per case with `fixtures.service:`
(`trial.trialServiceIdentity`). Neither source can be empty, which is the property that keeps
the fence from switching itself off — but the derived form is a CONVENTION, not an
authority: a checkout whose directory is not the registered name fences a name no path
will match, and such a repository must state `fixtures.service:`. Offline nothing can
detect that mismatch, so `soul-trial run` PRINTS the name it fenced on for every case,
marked `(from directory)` when nobody stated it. It has to be live — a fence that only the live keeper enforces is one an
author meets for the first time in production, and the two layers this paragraph
describes are precisely the ones a static reading of the scenario cannot stand in for.
The other direction, a `vault:` ref built by interpolation, needs no guard: vault-resolve
runs *before* CEL, so a ref a CEL cell produces is never rescanned, and a ref containing
`${` is rejected outright.

That guard reaches every path the macro produces and no other. `core.vault.kv-read` and
`core.vault.kv-present` take their path as a *parameter*, so they enter Vault through the
module rather than through `vault()`, and a rendered `path: ${ vars.p }` is invisible to
both halves above — to the static scan because it named no segment, to the guard because
no `vault()` call was involved. Those two addresses are therefore scanned a second time
where the interpolation is already gone and the value simply *is* the path:
`config.ScanRenderedVaultParams`, called by the keeper dispatcher (`applyKeeperTask`)
before `mod.Apply`, so a fenced task fails with the module never invoked. Only those two
addresses are scanned — anywhere else a rendered string that resembles a path is just a
string, and flagging it would fence `/opt/<service>/`.

#### 8. Per-field `secret: true` on module output

A module declares which of its output fields are secret; the platform masks those
fields wherever the output is observable. This replaces `no_log`, which is all-or-nothing
and set by the task author rather than by the module that knows what it returns — so it
is simultaneously too coarse (suppressing an entire task's diagnostics to hide one
field) and unreliable (the author must know a module's output shape to set it).

`no_log` is removed rather than deprecated: nothing has shipped that depends on it
outside this repository's own examples. Writing the key is `unknown_key` carrying a
hint that names the replacement, rather than an ignored leftover — a scenario still
spelling it was written expecting suppression, and accepting it silently would promise
a masking the platform no longer performs from that side.

`no_log` covered four channels at once, and each one is answered separately. Naming
them is the point: what replaces a blanket flag is never a single thing, and two of the
four are honestly narrower than what they replace.

- **`params:`** — masked per cell by the seal derived from the input schema
  ([ADR-010](0010-templating.md) §7.4), which is provenance-based rather than a task
  flag. A declared secret (§1) never renders as plaintext at all: the cell holds a
  `vault:` ref. **Wider** than `no_log`, which suppressed the whole block or nothing,
  and only when an author remembered to write it.
- **`output:` / `register_data`** — masked per field, on the observable copy only. The
  live register keeps the plaintext so the next task can read what this one produced;
  audit, the SSE frame and the run-detail endpoint get the masked copy. **Wider** in
  reach and **narrower** in damage: the operator now keeps the whole step and loses one
  field of it, where before the step vanished entirely.

  That live register is the `apply_task_register` row, so on this one channel the
  plaintext window of §Problem does **not** close — `no_log` skipped the write outright
  (`accumulateKeeperRegister`), and per-field masking cannot, because the field is the
  next task's input. The trade is deliberate and it is the same one §8 opens with: a
  flag that protects a value by deleting the row protects it by breaking every consumer
  of that row. What bounds it here is `purge_apply_task_register` (grace 1h, shipped
  enabled), not the declaration. §6's "plaintext never enters `apply_task_register`" is
  a statement about a **declared state secret**, which rides as a ref and never has a
  plaintext form in the register at all; a module that returns a credential in its
  `output:` is the other case, and it is bounded rather than eliminated.
- **`error.message` (task stderr)** — **narrower**, deliberately. Nothing suppresses it
  per task any more. What stands is the write-path masking of vault-refs
  (`audit.MaskSecrets` / `MaskSecretsSealed`) and the operator-SSE floor, which withholds
  `message` for **every** failed task rather than for flagged ones. A module that prints
  its own credential to stderr is covered by neither — which is precisely why the
  declaration belongs on the module's output field and not on the task: the module is the
  only party that knows what it emits.
- **`state_changes`** — the source-side drop is gone. `buildRegisterByHost` used to skip
  a `no_log:` task's register outright, which also broke the register chain the next
  Passage renders from ([ADR-056](0056-staged-render-passage.md)). Protection moved to
  §1 (a declared secret is not in state) and §6 (the register name is sealed, so a
  state capture reading it writes the ref through), neither of which costs a
  consumer its input.

#### 9. The generation policy grammar is the existing one

`length` (8..1024 characters, default 32), `charset`
(`alphanumeric` | `hex` | `base64url` | `ascii-printable-safe`, default the last), and
`allowed_chars` (mutually exclusive with `charset`, deduplicated) — the grammar
`core.vault.kv-present` already parses. `keeper/internal/coremod/vault/policy.go` moves
to shared code so both callers parse one definition; nothing about it is redesigned.
`crypto/rand` with rejection sampling stays.

One tightening, which is a bug fix rather than a redesign: a key outside the grammar is
now rejected instead of ignored. `{'charsett': 'hex'}` used to hand out secrets from the
default alphabet while the YAML said otherwise, and nothing reported it.

### Rejected

- **A generator module that owns the store** (`core.secret.generated` with a `field:`
  parameter, resolving the path itself). It puts a second state manager beside
  `core.state` and decides a storage path inside a module whose name says it
  generates strings — reintroducing, one layer down, the opacity this ADR removes. The
  module is dropped from the ticket entirely; `generate_secret()` (§3) covers minting.
- **`generate_secret()` returning plaintext at render.** Render is re-run per passage and
  per retry, so this is the first impure function in the expression language: it mints on
  every evaluation, for every element, including those that already have a value.
  Nothing breaks *today* — the write refuses to overwrite — but correctness then rests
  entirely on that refusal, and a future `force:` or a second writer turns it into silent
  rotation of every credential. It also makes `soul-lint` and Trial output
  non-deterministic on an unchanged tree.
- **Refreshing `incarnation.state` per passage** so a consumer could read what this run
  wrote. It works, and it spends the one-snapshot-under-`FOR UPDATE` invariant that the
  rest of the render path is written against. §4's effective-state register buys the same
  thing without it.
- **A new CEL root `secret.*`** resolving declared fields at render. Necessary only while
  the writer's result is unavailable to consumers; §5 makes it unnecessary, and a root
  introduced with a plan to retire it is a name that outlives its plan.
- **Keyword arguments for `generate_secret`.** Not a style choice: CEL has no assignment
  operator, `=` is not a token, and macros run on a parsed AST — so nothing can intervene
  on text that fails to lex. A map argument keeps names at the call site.
- **Character-class minimums** ("at least one digit") in the policy. They lower entropy,
  turn generation into a retry loop, and NIST has recommended against composition rules
  for years. `allowed_chars` with a longer `length` covers a target system that demands
  them without encoding the anti-pattern.
- **Entropy-based sizing** (`min_entropy_bits` instead of `length`). Rejected once already,
  with the reason recorded in `policy.go`: the author would no longer see in YAML how long
  the string is.
- **Deleting `vault()` or `core.vault.kv-*` outright in this ticket.** See §7 — the
  cross-namespace class has no replacement yet, and removing the mechanisms would break
  reads and writes that are legitimate. The fence is on the namespace, not the spelling.

### What this ADR does not carry

- **Declared cross-namespace reads (`external_secrets`).** The mechanism that would let
  §7's fence become a deletion. `wb-service-redis` needs zero entries, so it is not on
  this ticket's critical path.
- **Rotation.** Deliberately inexpressible under §4. Making it expressible is a decision
  about who may rotate what and what happens to the previous value — its own ticket.
- **Explicit state capture and the retirement of end-of-run `state_changes`** (NIM-699).
  `core.state` arrives here because §4 needs it; the broader question of whether
  `state_changes` survives is separate. *(Answered by
  [ADR-0084](0084-explicit-state-capture.md): it does not.)*

### Consequences

**For the service author.** A secret is declared once, next to the field it belongs to,
and referenced by name. `wb-service-redis` loses its `revealable_secrets` block, its
prose path in `types.yml`, both `users_path` computations, and all 17 `vault()` call
sites; the `merge(merge(...), has(input.users) ? ... : {})` construction in
`deploy.yml:67-85` collapses to a single `.map()` over the register, because the state
record already holds the merged inventory. The comment warning that two path expressions
must agree is deleted along with both expressions.

**For the platform.** One channel into Vault for a service's own secrets, one derivation,
one place to audit. The `vault_ref` escalation class from ADR-070 is removed at the root
rather than fenced; its runtime guards remain as defence in depth. A declared secret no
longer transits `apply_task_register` in the clear (a module-declared `output:` secret
still does, bounded by the purge — §8).

**Breaking, deliberately.** Derived paths do not match the paths existing incarnations
use, `revealable_secrets` stops parsing, and `no_log` is removed. No migration is
provided: this lands before the release, where compatibility is not yet owed.

**Guards this ADR requires as real tests**, each proven by mutating the code in the form
of code rather than by asserting on text:

- a scenario naming a path under `secret/<service>/<incarnation>/` fails to load, in
  each of the four spellings (`vault()`, a `vault:` ref, `core.vault.kv-read`,
  `core.vault.kv-present`) and for a path assembled by interpolation;
- a `type: secret` value never appears in `apply_task_register` after the generating run,
  and never in an audit payload;
- a derived path segment containing `/`, `.`, or `..` fails closed;
- a second run keeps the first run's password (a declared secret resolves, it does not rotate);
- a Soul-side task reads a keeper register through the union, and the old empty-bucket
  fallback still does not fire.

### Amendment 2026-08-25 (NIM-699, [ADR-0084](0084-explicit-state-capture.md)): the two params are renamed `field:` / `value:`

§4's example above is written in the new spelling. `key:` → **`field:`**, `set:` → **`value:`**,
and the register's echo key `key` → **`field`** with it. Nothing about the module's behaviour
moves; what moves is the word.

Both old names collide with the [ADR-057](0057-state-changes-crud-verbs.md) verb grammar that
[ADR-0084](0084-explicit-state-capture.md) ports onto this module. There `key:` addresses an
**element inside** a collection (the map key an `add`/`remove` operates on) while 698 spelled the
**containing field** the same way — one word for a whole and its part, in params that will sit side
by side. And `set` becomes a sibling state (`core.state.set`), so it cannot also be a parameter of
`present` without the address and the parameter disagreeing about what the word means.

This is breaking for every author, and deliberately unversioned: it lands before the release, and
the adopters in `examples/` are migrated in the same commit.

### Amendment 2026-08-25 (NIM-699, [ADR-0084](0084-explicit-state-capture.md)): the address becomes `core.state.set`, and the write lands at the step

Two things in §4 move, and neither is a behaviour change to what 698 built.

**The address.** `core.state.present` becomes **`core.state.set`**. ADR-0084 turns the module's
state suffix into the [ADR-057](0057-state-changes-crud-verbs.md) verb, one address per verb, and
what 698's module does to a field's ordinary content is overwrite it — which that dictionary calls
`set`. The word `present` is then free for the operation ADR-057 would expect under it: write the
field only if it has no value yet. Breaking for every author, deliberately unversioned, and the
`examples/` adopters migrate in the same commit as the rename.

**The secret rule stops being the verb's.** §4 states it as *"on a secret property the semantics are
`present`, never `set`"*, and that sentence stays exactly true — but it is a property of a **declared
secret**, not of the address it was first written under. Every verb resolves a `type: secret`
property the same way: an existing Vault value is kept, a missing one is minted, the register carries
a `vault:` reference. `core.state.set` overwrites the field and still does not rotate a live
credential. Rotation remains inexpressible, for the reason §4 gives.

**The commit point.** The sentence about the record reaching Postgres "through the run's ordinary
`state_changes` commit" is superseded: `state_changes` is retired and the field lands in
`incarnation.state` at the step. The stripping of declared secrets is unaffected — it happens in the
same engine, now called from the module instead of from the end-of-run barrier — so a secret still
cannot reach a column. What changes is *when* the row is written, which is ADR-0084's subject, not
this one's.

**`incarnation.state` no longer stays frozen for the whole run, and "per-passage state refresh" is
no longer rejected.** §5 closes on *"`incarnation.state` stays the frozen pre-run snapshot it is"*,
and the Rejected list carries per-passage refresh. Both were right for a single end-of-run commit and
one verb: nothing could observe a write, so freezing cost nothing. Carrying the verb grammar makes
them wrong — `add` with `on_conflict: skip` inserts twice against a frozen snapshot, `expect` asserts
against a state that no longer exists, and a `modify` after an `add` silently patches nothing. Read-
modify-write verbs only mean anything if the write is observable, so ADR-0084 takes accumulation:
`incarnation.state` in a step's expression reflects every capture that has already run in this run.
The three lines of §5 that this does **not** touch stand unchanged — the keeper register is still
unioned into every per-host bucket, duplicate register names are still a load error, and a consumer
still lands in a later Passage than its producer.

### Amendment 2026-08-26 (NIM-706): §7's fence has a second axis — the name that opens the namespace

§7 fences every authoring channel out of `<mount>/<service>/`. It assumed the prefix
*belongs* to the service that names it. Nothing enforced that assumption, and two words
break it in opposite directions.

**A service name can BE a platform namespace.** The derivation of §1 opens
`<mount>/<service>/…`, and three other path families in the same mount fix their own
first segment: `<mount>/keeper/<name>` for keeper's own runtime secrets
([ADR-014](0014-operator-identity.md)), and `<mount>/herald/<entity>/<field>` /
`<mount>/provider/<name>/credentials` for the [ADR-064](0064-secret-write-path.md) write
path. A service named `herald` derives its incarnation onto that family's `<entity>`
slot and its state field onto its `<field>` slot — one KV entry, two writers, and Vault
KV v2 replaces an entry rather than merging into it, so on the `secretwrite` path the
second write deletes the first one's fields with no error anywhere. The `keeper` family
is milder only by accident: the mint reads before it writes.

Nothing reserved those words. `serviceregistry.ValidName` checks a grammar, and every
one of them satisfies it.

The rule is therefore a **closed list of reserved Vault namespaces** —
`keeper`, `herald`, `provider`, `internal` — refused as a service name at every surface
that names one, and enforced last by the derivation itself
(`config.SecretField.VaultPath` returns an error rather than emitting a colliding path).
It is deliberately narrower than the registration-alias list of
[ADR-020](0020-plugin-infrastructure.md): an alias that collides shadows an *address*, a
service name that collides destroys a *secret*, and the two lists answer different
questions even where they overlap. `internal` is on it while nothing writes there — the
name is free today and reserving it costs nothing, where taking it back later renames
every incarnation of a live service. The tables live in
[naming-rules.md § Reserved Vault namespaces](../naming-rules.md#reserved-vault-namespaces).

**And one platform path lives INSIDE a service's namespace, where no rule about service
names can reach it.** Keeper issues an incarnation's TLS material to
`<mount>/<service>/<incarnation>/tls/{cert,key}` (`keeper/internal/certissue`). A
collection secret declared on a state field named `tls`, with an element key `cert`,
derives that identical path. The element key is state *data* and cannot be constrained,
so the fence goes on the state field name — the last static point there is — as a second
closed list, today just `tls`, diagnosed `secret_field_reserved_state_name`. Only a field
that *declares a secret* is checked; the plain `tls:` object of
`examples/service/redis` holds ports and cipher lists, derives nothing, and is
untouched.

`certissue.VaultPath` also spelled its mount as the literal `"secret"`, so on a
deployment with a non-default `vault.kv_mount` it wrote outside the configured mount
entirely. It now takes the mount from `keeper.yml`, threaded through both callers. The
same defect class is what removed `config.VaultInputFloor`: a list of literal path
prefixes, spelling the default mount and naming neither `herald` nor `provider`, has been
replaced by `config.PathUnderReservedNamespace` — mount-agnostic, segment-whole, and
reading the same closed list as everything else.

Three surfaces, one predicate: registration
over REST and MCP (422, not 409 — nothing holds the name), the derivation, and reveal
(denied before Vault is read, `reason=floor_denied`). Reveal keeps a second, path-shaped
half of the check, because a **mount** that spells a reserved word
(`vault.kv_mount: keeper`) puts every service's secrets under `keeper/…` while each
service name is blameless — a comparison on the name structurally cannot see it.

**Breaking, deliberately, and cheaply**: a service already registered under one of the
four names stops loading and must be renamed. This lands before the release, and the
`examples/` tree names none of them.

### Amendment 2026-09-01 (NIM-741, [One schema dialect](0086-one-schema-dialect.md)): the declaration is spelled in the input dialect

`state_schema` stops being JSON Schema and is written in the **same dialect as `input:`** — a map
`<field name>` → schema, no `type: object` / `properties:` wrapper at the root, `required: true`
on the field instead of a root `required: [names]` list, and a snake_case vocabulary. §1's
declaration is untouched in substance: the two supported shapes, the `key:` sibling, the derived
path, the closed node grammar and the total refusal all stand. Four things change, and one of them
changes an error code that today fires for the wrong reason. **Design only, not implemented** —
NIM-742 (engine), NIM-743 (`soul-lint list-secret-paths`), NIM-744 (`examples/` and the WB redis
service).

**(a) The `required:` refusal survives, but it has to be re-hung on a different key, and the naive
move breaks it.** §1 refuses listing a secret in the element's `required:` on the ground that no
state instance could satisfy it — the value is in Vault, so it is never present in the state the
schema describes. That ground is unchanged, and after the move it applies to `required: true` on
the `type: secret` property. But the check that enforces it reads the **list**:
`shared/config/secret_field.go:365-373` iterates `stringSeq(items["required"])` and matches the
property name. Once the list form is gone that loop iterates nothing, and `secret_field_required`
silently stops firing. ⚠ The obvious repair — just let the node carry `required` — fires the
**wrong code**: `checkSecretNodeGrammar` (`:383-399`) rejects any key outside `secretNodeKeys`
(`{type, key, label}`, `:93`), so the author gets `secret_field_unknown_key` reading *"type: secret
does not take required (want type/key/label)"*. That message is true about the grammar and useless
about the mistake: the author does not learn that a required secret is unsatisfiable, only that a
key was not recognised. `required` must therefore be **special-cased ahead of the grammar check**,
keeping `secret_field_required` with its own message. The same node keeps its own reason for the
closed grammar — see (b).

**(b) The node grammar stays `type`/`key`/`label`, and the shared input keys are refused on it.**
Sharing one dialect with `input:` makes a long list of keys syntactically available beside
`type: secret` that were never meant for it — `default`, `enum`, `pattern`, `min_length` /
`max_length`, `secret`, `prefill_from_state`, `required_when`. All are refused, on §1's own ground:
*a value that never enters state never passes through state validation*, so a constraint written
there reads as enforced and is not. ★ **`description` is refused too**, and for a different reason
than the rest — not "unenforceable" but "already spelled": `label` is this node's caption, and two
keys for one caption is exactly the drift this ADR exists to remove. The one addition the shared
dialect brings is `required`, which is refused with its own code per (a) rather than as an unknown
key. Note the direction of travel: `secret: true` remains legal *elsewhere* in `state_schema` and
keeps its orthogonal meaning (§1, [ADR-010](0010-templating.md) §7.4) — it is refused **on a
`type: secret` node**, where it would assert that a value living in Vault lives in state.

**(c) "Exactly two legal positions" stays decidable only at the point of USE — which is why the
secret rules are not `soul-lint`'s `types.yml` checks.** §1's two shapes are positional: a
top-level property, or a property inside a top-level array's `items` next to `key:`. Once
`type: secret` is legal in a shared type ([ADR-062](0062-input-types.md), amendment of the same
date), the same catalog entry is legal or illegal depending on where it is referenced — legal under
an array's `items`, refused one object deeper with `secret_field_unsupported_location`
(`shared/config/secret_field.go:265-274`). A type is therefore **neither legal nor illegal in
isolation**, and a standalone verdict over `types.yml` would have to be one or the other: refusing
would ban a legal use, accepting would bless an illegal one. This is why the secret rules are
deliberately **absent** from `soul-lint`'s standalone type checks
(`soul-lint/internal/validate/type_refs.go:6-77`), which are scoped to what genuinely needs the
catalog — `input_type_duplicate` / `input_type_cycle` / `input_type_unknown`. That is a boundary,
not a gap: the position check runs after expansion, where the position exists.

The **reserved-name fence of the 2026-08-26 amendment survives the move for the same reason, from
the other side.** `IsReservedStateField` is evaluated on the **state field name**
(`reservedStateFieldIssue`, `:283-292`), and a state field name is a `state_schema` key — it does
not come from the referenced type and does not move when `$type` expands. So the fence stays static
after expansion, exactly as the 2026-08-26 text requires: the collision is decided by the field
name alone, because the element key that would complete the path is state DATA.

**(d) The `<key>` guard is untouched, deliberately.** Nothing about the dialect reaches the one
segment an attacker can influence. `<key>` still comes from state data — an operator-supplied user
name — `ValidVaultPathSegment` still enforces the ADR-064 grammar `^[a-zA-Z0-9_-]+$`
(`shared/config/secret_field.go:59-66`), and `SecretField.VaultPath` (`:153-189`) still fails
closed on any segment that does not match, plus the reserved-namespace refusal. A schema dialect is
an authoring surface; this is a data-path check, and the two must not be conflated because the
migration touches one of them.

**Two engine hazards the parser change must carry, and they fail in opposite directions.**

- **The derivation collector fails LOUD.** `CollectSecretFields` reads
  `schema["properties"]` at the **root** of `state_schema` (`:231`) and claims from there, while
  `scanSecretNodes` finds every `type: secret` node structurally. Under the new dialect the root has
  no `properties`, so nothing is claimed and every declared secret comes back as
  `secret_field_unsupported_location` — wrong, but noisy, which is the tolerable half.
- **The masking walk fails OPEN**, silently, and is the reason this cannot ship in two commits:
  `keeper/internal/incarnation.CollectStateSchemaSecrets` hardcodes `properties` / `items` /
  `additionalProperties` (`keeper/internal/incarnation/secret_schema.go:82`, `:89`, `:92`) and
  returns an **empty** set, which is byte-identical to *"this service declares no secrets"*. Full
  account in [ADR-010](0010-templating.md), amendment of the same date. The walk migrates in the
  same commit as the parser.

One detail of the scan is easy to get wrong and expensive to get wrong: `scanSecretNodes` carries an
`inProps` flag saying "this map's keys are author-chosen field names", and it exists so the skip set
`{default, const, enum, examples}` (`:453`) never applies to a field that happens to be **named**
one of those. Today the `state_schema` root is not such a bag; under the new dialect it **is**, so
the root call must pass `inProps: true`. Otherwise a state field named `default` takes its whole
subtree out of the walk and a declaration inside it comes back as "no secrets here" — the single
outcome that function's contract rules out (`:417-431`). Relatedly, `secretPropsBag` (`:456`) lists
`patternProperties` beside `properties`; the new dialect refuses `patternProperties` outright (it
has no counterpart in the input DSL and zero authored uses in the tree), so that entry becomes dead
weight rather than a rule.

### Amendment 2026-09-02 (NIM-746): `present` over a populated field **fails closed** — the verb is not what decides a mint

The 2026-08-25 amendment above states the secret rule as *"Every verb resolves a `type: secret`
property the same way: an existing Vault value is kept, a missing one is minted"* (`:551-556`). The
first half holds under every verb. **The second half does not**: nothing in the module mints on the
strength of a value being missing. A mint needs a `generate_secret({…})` marker in the value the
step proposes, and `core.state.present` deliberately arranges that a populated field never has one.

**The deciding pair is the state of the field and the presence of the marker, not the verb.**
`present` alone pre-reads state before any Vault work
([`keeper/internal/coremod/state/state.go:207-216`](../../keeper/internal/coremod/state/state.go)):
a field that already holds a non-nil value discards the incoming proposal, resolves the **stored**
value instead, and carries `noMint` into the resolve. Every other verb resolves what the author
proposed. The reasoning is recorded at that call site (`state.go:201-206`) and it is right — minting
for a write that is then thrown away leaves a live credential nothing in state points at.

So a derived path that holds no value has **three** outcomes, and only one of them mints:

| the field | the value being resolved | outcome |
|---|---|---|
| absent / nil | carries `generate_secret({…})` | **mints** at the derived path (`state.go:684-689`) |
| absent / nil | no marker | **fails closed** — *"has no value yet and none was requested -- set it to generate_secret({…})"* (`state.go:682`) |
| populated — `present` only | the **stored** value, re-resolved | **fails closed** — *"is stored but was never minted in Vault -- core.state.present keeps the stored value and cannot mint one for it"* (`state.go:679-681`) |

The first two rows are also what a verb **other than** `present` gets over a *populated* field: it
resolves the proposal, so the marker decides and the field's contents do not. Only `present` reaches
the third row, and only because it substituted the stored value for the proposed one.

The third row is **forced rather than incidental**. `config.StripDeclaredSecrets`
([`shared/config/secret_field.go:533-547`](../../shared/config/secret_field.go)) deletes every
declared secret property on the way into state, so a stored element structurally cannot carry a
marker; `requestedSecret` then returns `intent{mint: false}` (`state.go:597-600`), and under `noMint`
even a stray marker that did arrive is refused rather than honoured (`state.go:590-594`).

**What this changes for a reader** is the case of a schema change that re-points the path a secret is
derived under. Taken at its word, *"a missing one is minted"* says such a change self-heals on the
next run. It does not, and the two routes fail in **opposite** directions: a day-2 step re-resolving
an already-stored record breaks **loudly**, while a create-class step carrying `generate_secret({…})`
mints **silently** at the new path, leaving the incarnation holding a credential the running service
does not know. That consequence is already recorded — from the code rather than from this ADR — in
[ADR-019](0019-state-migration-dsl.md)'s amendment of 2026-09-01 (NIM-735), §7; the two texts
disagreed until now, and this one was the wrong one.

**A fourth shape fails in neither direction: an empty collection is a value.** The pre-read tests the
**field**, and an empty list or map is non-nil, so `present` yields to it; a collection with no
elements then offers no declared-secret position to resolve, and the step neither mints nor refuses.
The live example of a re-pointed derived path is
[`examples/service/redis/migrations/015_system_acl_users/main.yml`](../../examples/service/redis/migrations/015_system_acl_users/main.yml)
— v14 minted under `secret/redis/<inc>/users/<name>`, v15 derives
`secret/redis/<inc>/system_acl_users/<name>` — and it is exactly this shape: the step defaults the new
field to `[]`, so a v14 incarnation's next day-2 run (`core.state.set` over
`${ default(incarnation.state.system_acl_users, []) }`, e.g.
[`scenario/restart/main.yml`](../../examples/service/redis/scenario/restart/main.yml) `:62-69`)
writes an empty list and reports success. **Cite it for the path shape only** — its own description
comment claims that run fails closed on the missing secret, and NIM-738 corrects it.

**No code change.** The module does what this ADR wants: an incidental re-render must not rotate a
live credential, and a mint whose value nothing can reach is worse than a refusal. What was
imprecise is the sentence describing it — here, and in the three places that quoted it:
[ADR-0084](0084-explicit-state-capture.md#secret-resolution-is-orthogonal-to-the-verb),
[docs/keeper/modules.md](../keeper/modules.md) (`core.state.<verb>`) and
[docs/scenario/orchestration.md](../scenario/orchestration.md) (§7.1, both halves), all corrected in
the same commit. Rotation stays inexpressible, for the reason §4 gives.
