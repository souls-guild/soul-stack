# soul-lint

Offline linter for Destiny and service vars. The purpose and place in the system are specified in [ADR-004](adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper): parsing, rendering, checking according to the scheme, static analysis. Works without connecting to Keeper - suitable for CI, IDE and local launch.

This document maintains a list of **planned checks** (TODO) and states the rules/reasons why a linter should perform them.

## Planned checks

### 1. Consistency of literals in `when` with `enum` input parameters

**Problem.** In Destiny, step conditions are written as top-level expression-keys - the entire line is treated as a CEL without a wrapper ([ADR-010](adr/0010-templating.md), [`docs/templating.md §2.1`](templating.md)):

```yaml
input:
  action:
    type: string
    required: true
    enum: [apply, ensure_user, restart, ping, replication_status]

# destiny-<name>/tasks/main.yml:
- module: core.pkg.installed
  when: input.action == 'apply'
  ...
```

The string literal `'apply'` inside `when:` has no formal relationship with the declared `enum`. A typo (`'aply'`) will not cause an error - the task will simply never work, and destiny will externally "work". This is a silent class of errors that must be caught statically.

**What should a linter do.**

1. Parse the expressions in the fields `when:` (and in the string values `input.*`, if we decide to expand it, discuss it separately). The parser uses `cel-go` ([ADR-010](adr/0010-templating.md), [`docs/templating.md`](templating.md)) - the same engine as in runtime, so statics and runtime are consistent.
2. Extract patterns like:
   - `input.X == '<literal>'` / `input.X != '<literal>'`
   - `input.X in [<literal>, <literal>, ...]`
   - analogues for `==`/`in` via `|` / built-in functions - as the syntax is fixed.
3. Check against the block [`input:`](destiny/input.md) destiny:
   - parameter `X` is declared in `input:`;
   - if `X` has `enum` specified - each literal from the expression belongs to this `enum`;
   - otherwise - `error` indicating possible values.
4. **Coverage-warning.** The value `enum`, which is not mentioned anywhere in any `when` destiny, is a candidate for a typo in `enum:` (or a dead branch). Level: `warn`, not `error`.

**Limitations.** Validation covers "simple" comparisons. Arbitrarily complex expressions (calculations, concatenations, function calls over `input.X`) remain outside the check - only `warn` heuristics or explicit escape (`# soul-lint:ignore-when`) are allowed for them. This is a conscious trade-off: we catch 90% of typical errors without trying to analyze turing-complete patterns.

**Dependencies.**
- Expression template fixed [ADR-010](adr/0010-templating.md): CEL for YAML expressions, parser - `cel-go` (see [`docs/templating.md`](templating.md)). Linter uses the same engine - statics and runtime are consistent, otherwise they diverge.
- If in the future we choose the option with a structured matcher (`when: { param: action, equals: apply }`) - part of this check will go into the destiny scheme and the linter will be needed only for the escape form.

## Backlog: scenario checks (after ADR-009)

Not a fixed design, fixed when implementing a scenario resolution. Introduced together with [ADR-009](adr/0009-scenario-dsl.md) and [ADR-008](adr/0008-coven-stable-tags.md).

### B1. Warn: inline mutation in scenario without removal in destiny

**Problem.** [ADR-009](adr/0009-scenario-dsl.md) allowed steps changing `module:` directly in the scenario, but the "reused/critical → in destiny" boundary is a recommendation, not a ban. Without prompting, authors will inline mutations, bypassing independent git versioning of destiny ([ADR-007](adr/0007-versioning-git-ref.md)) - an erosion risk explicitly noted in ADR-009.

**What the linter should do.** Heuristically mark `module:` steps in a scenario whose `<state>` is marked in the module manifest as modifying (`side_effects`), and which are not wrapped in `apply: { destiny: … }`. Level - **`warn`** (not `error`: this is a recommendation). The message indicates the removal criterion from [`docs/scenario/concept.md`](scenario/concept.md) (three "yes"). read-only steps (probe, `changed_when: false`) do not fall under the rule.

### B2. Statistical check of `where:`- and `on:`-literals

**Problem.** The `where:` predicate references the `register:` of the previous probe step; `on:`-literal - to coven-name. A typo (`redis_rol.stdout`, non-existent coven) quietly leads to an empty target - a destructive step will "work" on no one, and this will not be noticeable.

**What should a linter do.**
- In `where:` - check that the mentioned `register`-ids are declared earlier in the flow in this scenario (as requisite checks in destiny). Unknown `register` → `error`.
- In `on:` - check the form (`keeper` / list of coven literals / omitted) and, where statically possible, that the coven literal is not empty and the pattern is syntactically valid. The correspondence of a literal to real covens is runtime (Postgres), not caught by statics; The level for suspicious literals is `warn`.
- Check the register invariant: **each `register.<name>` mentioned in `where:` must be a `register:` probe step that completed earlier in the flow** → otherwise `error`. A purely stable predicate (only `soulprint.self.*`, without a single `register.*`) probe **does not require** and is not an error (see [`docs/scenario/orchestration.md §4`](scenario/orchestration.md)).

**Dependencies.** Template engine fixed [ADR-010](adr/0010-templating.md) (CEL for top-level expression-keys, `cel-go`-parser); scenario uses the same engine ([ADR-009](adr/0009-scenario-dsl.md)). The resolved path of a two-level resource resolution (locally → service-level) is printed by the engine and checked by the linter - see [`docs/scenario/orchestration.md §6`](scenario/orchestration.md).

### B3. Resolve named input types (`$type` / `types:`)

**Problem:** The script references a reusable named type with the `$type: <Name>` directive ([ADR-062](adr/0062-input-types.md), [`docs/input.md → "Reusable named types"`](input.md#reusable-named-types-types--type)). A broken link (a typo in the name, the type is not declared), a cycle in the type graph, or a conflict `$type` with an inline scheme are not visible to the eye and would only appear in runtime (or, with a cycle, they would even loop the resolver). This is a statically caught class - the linter must cut it off.

**What the linter should do.** When checking the script `input:` (and the circuits inside `service/<name>/types.yml`), resolve `$type`-links **service-level** and issue:

- `input_type_unknown` (`error`) - `$type: <Name>` refers to a type that is not present in the `types:` service;
- `input_type_cycle` (`error`) - loop in the type link graph (`A→B→A`, self-link `A→A`); the resolver traverses the graph with a cycle detector and does not unroll indefinitely;
- `input_type_duplicate` (`error`) — duplicate name in section `types:`;
- `input_type_ref_conflict` (`error`) - `$type` is specified along with the inline diagram on the same node (`type:`/`properties:`/`items:`/...); link and inline are mutually exclusive.

**Boundaries.** Resolve strictly service-level (types of the same service) - cross-service and local-per-scenario declarations outside the MVP ([ADR-062](adr/0062-input-types.md), MVP boundaries). After a successful resolution, the expanded circuit is checked with the usual input checks (`input_*`) recursively, like any inline-`object`/`array`.

**Dependencies.** Format source of truth is [`docs/input.md`](input.md); name dictionary (`types:`/`$type`/`x-type`/`input_type_*`) - [`docs/naming-rules.md`](naming-rules.md).

**Implementation status (current MVP).** Cross-ref `register.<name>` vs task plan declarations (including block nesting) - implemented (codes `duplicate_task_address`, `unknown_register_reference`; `register.self.*` excluded). `duplicate_task_address` catches a duplicate in the address space of the subscription `register ∪ id` (two `register`, two `id`, or `register` of one task == `id` of another; ADR-052 §h, [destiny/tasks.md §8](destiny/tasks.md)) - last-wins-resolve would have quietly tied the dependency/alert to the wrong task. Cross-file (address duplicate between the main file and the one connected via `include:`) is checked on the flat plan after expanding include. Additionally, statistical checking of `soulprint.<...>` links in CEL predicates is enabled (`where`/`when`/`changed_when`/`failed_when`/`retry.until`/`loop.when`):
- bare `soulprint.<x>` without `.self`/`.hosts`/`.where` → `soulprint_naked_reference` (canonical form required, see [`docs/soul/soulprint.md`](soul/soulprint.md));
- `soulprint.self.<unknown_top>` (typotype `memmory`/`familly` on the top segment) → `soulprint_unknown_path` with reconciliation against the typed scheme [ADR-018](adr/0018-soulprint-typed.md) (`sid`/`hostname`/`os`/`kernel`/`cpu`/`memory`/`network`/`covens`/`role`);
- `soulprint.hosts.where(...)` / `soulprint.where(...)` are skipped - these are scenario-only accessors, they are validated by `shared/cel.rewriteHostsWhere` in the render phase ([`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md));
- second stage `soulprint.self.<msg>.<unknown_field>` (typo in a subsegment, for example `os.familly`) - **postponed** by a separate slice upon request (the current check only catches typos in the first segment; placeholder in the code is `checkSoulprintSubPath`).

Register-dependency detector is connected inside `block:` for staged-render ([ADR-056](adr/0056-staged-render-passage.md)): a child of block reading register issued by a neighboring child of the **same** block, → `within_block_register_dependency` (`error`). Block is atomic by Passage (the entire fan-out is one Passage), peer-register is available only to Soul-side AFTER probe, and `where`/`params`/`vars`/`apply.input` are resolved by Keeper-side BEFORE dispatch → selecting hosts using the outdated register silently (silent-wrong-target). Flow-control `when` (Soul-side per-task gating after FC-5 narrow-fix) **not** included here - within-block `when: register.peer` is valid. It can be treated by moving the probe and consumer to different top-level tasks (then stratification will routinely separate them by Passage). The same code insures runtime keeper-side. Catalog description - [`docs/naming-rules.md → Parser / validation errors`](naming-rules.md). Symmetric stratify/passage codes (`register_dependency_cycle` - register dependency loop; `cross_passage_requisite_unsupported` - cross-passage requisite without audit log) are detected during stratification/runtime, see the same directory.

Cross-passage flow-control gating detector is connected ([ADR-056](adr/0056-staged-render-passage.md) amend 2026-06-21, FC-5): task gating `when:` / `changed_when:` / `failed_when:` by register issued in **earlier** Passage, → `cross_passage_when_unsupported` (`error`). Flow-control = Soul-side per-task gating ([ADR-012(d)](adr/0012-keeper-soul-grpc.md)) - only sees register **its** Passage; cross-passage register is not available to it (another `ApplyRequest`) → `no such key` silently, task FAILED. `where:` (Keeper-side targeting) cross-passage is capable, `when:` is not: asymmetry is legitimate. The `when` Passage itself **does not split** (flow-control is NOT passage-defining), so the register-dependent `when` usually goes the same-passage with the probe and works; the code only works when the probe left for an early Passage for **another** reason (different task with `where: register.X`). Treated: `where:` for cross-task register targeting OR `register.self` for same-task gating (`register.self` is NOT caught by the detector). The same code insures runtime keeper-side.

For `on:`-literals: format (`kebab-case` / `${ ... }`-CEL / `keeper`) - implemented (codes `enum_invalid`, `name_invalid_format`, `type_mismatch`); The hook `CovenLabelValidator` (interface in `shared/config`, no-op by default) is attached to every non-CEL-wrapped coven literal via `SetCovenLabelValidator`. The real covens directory (Q1b ADR-008-amend) will replace no-op without changing the public API; Until then, the linter does not flag the "existence" of coven (this is runtime).

## Plugin module params: `--modules <alias>=<path>`

A task's `params:` are checked against the module's declared schema — unknown key,
missing required, type mismatch, unknown state. For `core.*` the declarations are
compiled into the linter, so this is always on. A **plugin**'s schema ships with its
artifact, so the linter has to be handed it
([ADR-0076(x–z)](adr/0076-engine-compat-window.md)):

```sh
soul-lint validate-scenario <path> --modules redis=./soul-mod-redis/dist/schema.json
```

That path is the **published sidecar** `soul-mod stamp` writes next to the artifact, so
the usual binding costs no download: the same bytes are stamped into the binary, but
`schema.json` is right there in the checkout.

The flag is **repeatable** — one binding per plugin, appended in order.

**`<path>`** is a `schema.json`, a stamped artifact, or the `dist/` directory holding
one. The two carriers are told apart **by content**, not by extension: a canonical
document is a JSON object and begins with `{`, an artifact does not. Guessing from the
filename would misread `dist/soul-mod-redis.json` and — worse — would read an artifact
named `schema.json` as text and report a parse error instead of a missing trailer.

**The alias is stated on the flag, not inferred**, and this is the part that surprises
people, so it is worth the paragraph.

The artifact declares **no name at all**
([ADR-020(p)](adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)):
nothing in the bytes says what a task should call it. Address level 1 is the
registration alias an operator picks, and the same artifact registered as `redis` and
as `redis-community` serves two address spaces without a byte changing. So the linter
cannot learn an address from a file — **somebody has to state it**, and the author is
the one who knows which alias their cluster used.

It goes on the flag rather than being read off the directory because directory naming
was already rejected once, for the manifest form, and every reason still holds: a
checkout is named after its binary, or after whatever `git clone` produced, while a
task addresses `community.redis`. Infer the alias from the path and a definition's
address starts depending on where a file happens to sit — **rename the folder and the
same definition starts or stops validating, with nothing in the definition changed.**
You write the same `redis` the task writes in `redis.acl.present`, and it means the
same thing in both places.

The alias is rejected if it is malformed (`^[a-z][a-z0-9-]{0,62}$`) or
**[reserved](naming-rules.md#reserved-namespace-names)** — the same grammar and the
same list a registration is checked against. Binding `core=` would let a document of
the author's choosing define what `core.*` accepts, which is exactly what the reserved
list exists to stop; refusing it here also tells the author early that the alias they
had in mind will not survive `plugin.allow` either.

### A broken binding and an absent one are different answers

These two look alike and must not be conflated — the whole value of the flag is in the
difference:

| Situation | Answer |
|---|---|
| An alias **bound on the command line** whose document cannot be read — missing path, missing or malformed trailer, unparseable or invalid document, wrong `kind`, duplicate alias, a binding without `=` | **Fatal, exit 2.** |
| A module **nobody bound at all** | **`plugin_params_unchecked`**, a hint. Never fails a lint. |

**A broken binding is fatal because the author asked for that check explicitly.**
Printing `OK:` while silently not running a check that was requested is the exact
failure this flag exists to remove — and the old tree-walk did it by design, where one
unparseable manifest cost you that module and nothing said so. There is no longer a
tree to be partially readable: there are only bindings the author wrote down.

**An unbound module is a hint because nothing was claimed about it.** That is the case
`plugin_params_unchecked` actually describes, one per module address:

```
main.yml:223:13: hint: [plugin_params_unchecked] params of community.redis were not
checked: no module manifests were supplied
```

Its job is to keep "checked and clean" from looking identical to "never looked", which
is how the drift in NIM-206 survived long enough to be found by hand. A definition's
author usually cannot produce somebody else's schema, so failing them for its absence
would punish the wrong person.

The same check runs inside Keeper, resolving from the plugins the cluster has
allow-listed, so a definition linted here and rendered there is held to the same
schema. Either way an undeclared key fails the task on the host
([ADR-0076(t)](adr/0076-engine-compat-window.md)) — the flag only moves the answer to
where the definition is being written.

## Service identity: `--service-name <name>` (`validate-scenario`)

A service states no name of its own. `name:` left `service.yml` with
[NIM-726](adr/0085-entity-id-and-label.md): the name is assigned once, at
registration, and it is what the registry row is keyed on, what every derived Vault
path is built from, and what `incarnation.service` resolves to. The manifest used to
carry a second copy that nothing compared against the first.

So the one check that needs the name takes it as an argument — the own-namespace Vault
fence ([ADR-0083](adr/0083-declared-secret-state-fields.md) §7), which refuses a task
writing under `<mount>/<service>/`:

```
soul-lint validate-scenario scenario/create/main.yml --service-name redis
```

Without it the fence **cannot run, and says so**: `own_namespace_fence_unchecked`, at
warning level, exit code unchanged. That is the whole point of moving the name onto the
flag. Before NIM-726 an absent or nameless `service.yml` made the rule return nil — the
scenario linted `OK`, and the same paths were refused at render. A check that did not
run now reports that it did not run.

Linting a scenario standalone, before anyone has decided which service will own it, is
ordinary and stays possible; the warning is not an error for exactly that reason.
`make lint` is the exception: over the example corpus the name is always known, so an
unchecked fence there means the wiring broke rather than that the author was undecided,
and the target treats the warning as a FALSE-GREEN and fails.

The offline L0 runner has the same need and no registry either: `soul-trial` defaults to
the service directory's name, overridable per case with `fixtures.service:`. The directory
is a convention, not an authority — a checkout laid out as the documented `service-<name>/`
derives `service-redis` where the registry says `redis`, and that name is non-empty but
matches nothing. So the fence is never silently disabled at L0, but a repository whose
directory is not its registered name must state `fixtures.service:` for it to mean anything.

## Derived secret paths: `list-secret-paths`

A declared secret's Vault path is **derived and never authored**
([ADR-0083](adr/0083-declared-secret-state-fields.md) §1). That is the point of the
design — a path written in the service repository would be a second source of truth,
and reconciling two copies of one path is the bug class that ADR removes. The cost
falls on the author, who cannot read off where their secret lands. Before this
subcommand the only way to know was to read `config.SecretField.VaultPath`.

```
$ soul-lint list-secret-paths service.yml --service-name redis
secret/redis/<incarnation>/admin_password#value               ← state_schema.admin_password
secret/redis/<incarnation>/redis_users/<name>#password        ← state_schema.redis_users[].password
secret/redis/<incarnation>/system_acl_users/<name>#password   ← state_schema.system_acl_users[].password
```

**What is printed is the shape, not a path.** Two segments cannot be known offline:

- `<incarnation>` — there is no incarnation at authoring time, and the linter never
  talks to a keeper.
- the last segment of a **collection** — it comes from state *data*, an
  operator-supplied element name. The declaration says only which sibling property
  supplies it, so `key: name` prints as `<name>`: the key's **name**, never a value it
  will take.

The third unknown is the service, and it is the same `--service-name` the fence above
takes, for the same reason (NIM-726). Here it is **required** rather than advisory: it
is a segment of every line printed, so without it there is nothing to print.

The two declaration shapes read differently, as they derive differently. A collection
carries the extra `<key>` segment and stores the value under its own property name; a
scalar has neither, so its value sits under the constant field `value`. The trailing
`#<field>` is that field, in the form ADR-0083 §1 writes it — a line without it would
name a KV entry and leave the reader guessing which key inside it holds the value.

The mount is the default `secret`: `keeper.yml` is not a file of the service
repository, so an offline linter has no configured mount to read.

Three properties are worth stating, because each is a way the command could quietly
mislead:

- **The output is byte-identical run to run.** Declarations are walked in sorted order
  (`config.CollectSecretFields`), not in map order.
- **A service that declares no secrets prints nothing and exits 0.** An empty list is
  an answer, not a failure — but only when the declarations were actually read. A file
  that does not parse, or that carries no `state_schema:` map, is **refused** (exit 1)
  rather than answered with an empty list. That covers every YAML in a service
  repository which is not the manifest — `types.yml`, `covenant.yml`, a scenario's
  `main.yml` — each of which parses cleanly and would otherwise come back looking like
  a service with no secrets.
- **A refused declaration is never absorbed into silence.** A `type: secret` in a
  position with no place in the formula derives no path; the refusal goes to stderr,
  the exit code is 1, and the command says the list above is incomplete. The same holds
  for a `--service-name` the derivation itself refuses — a reserved namespace
  ([ADR-0083](adr/0083-declared-secret-state-fields.md) §7) or a name that is not a
  safe path segment — and for a `key:` whose *property name* is not a safe segment.
  That last one is legal everywhere else (at runtime the key's **value** becomes the
  segment, and that is what gets checked), but here the name itself is printed, and a
  property called `a/b` would render `<a/b>`: punctuation claiming a segment boundary
  the derivation does not make. A short list that reads as the whole list is the one
  wrong belief this command can produce.

The precedent for printing a derived thing is `passage_plan`, which shows a run order
likewise written nowhere.

## Service vars checks (`validate-service`, `validate-scenario`)

Implemented, [ADR-0082](adr/0082-service-vars.md). All four answer the same class:
a service that resolves to something other than what its author wrote, and
resolves without complaint.

| Code | Level | What it catches |
|---|---|---|
| `stack_step_invalid` | ERROR | `vars/_stack.yaml` cannot be read as written — an unknown key, no steps under `stack:`, `file:` together with `inline:`, `foreach:` without `as:` (or the reverse), an `as:` shadowing the step context, a `strategy:` that is neither `deep` nor `replace`. Also the `_stack.yml` spelling, which is not read at all. |
| `vars_dir_nested` | WARNING | A layer file in a subdirectory of `vars/`. The resolver reads `vars/*.yaml` and does not descend, so the file is never read — this is exactly the shape the retired `essence/coven/` layout had. One diagnostic per subdirectory. |
| `vars_retired_layout` | ERROR | The retired `essence/` directory is still present. Half a migration is worse than none: `vars/` resolves to nothing and every `default(vars.X, y)` in the repo silently takes its fallback. |
| `vars_shadows_service_var` | WARNING | A scenario-level or task-level `vars:` name taking over one of the service's own vars. Deterministic and sometimes deliberate — `conf_dir: "${ vars.conf_dir }/conf.d"` derives from the layer below on purpose — so it warns rather than fails. Modelled on `vars_collision` ([destiny/vars.md](destiny/vars.md)). |

`stack_step_invalid` runs the **same parser the Keeper runs**
(`config.ParseServiceVarsStack`), not a second copy of the schema: a linter with
its own reading of a file format is how a file starts passing one check and
failing the other.

`vars_shadows_service_var` reads the service's var NAMES as a superset — the
top-level keys of every `*.yaml` in `vars/`, without evaluating `_stack.yaml`.
A name a conditional step might contribute still counts, which is the right side
to err on: a `when:`-gated layer that shadows on Tuesdays is precisely the case
an author will not find by reading.

## What is NOT soul-lint

Dynamic run of destiny on a test bench, measurement of runtime-coverage and verification of scripts in docker is a **separate tool**, not part of `soul-lint`. According to ADR-004 `soul-lint` is strictly offline and static. The topic is maintained in [destiny/testing.md](destiny/testing.md).
