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
- `input_type_ref_conflict` (`error`) - `$type` is specified along with the node's own shape; the checked set is closed and is exactly `{type, properties, items}` ([`shared/config/input_types.go`](../shared/config/input_types.go)) - link and inline are mutually exclusive.
- `input_type_ref_overlay_conflict` (`error`) - **`state_schema` only.** There a `$type` node may also carry its own `properties:`, overlaid onto the resolved type add-only and shallow; a property declared on **both** the reference and the type is this code. Inside `input:` the overlay does not widen and `$type` + `properties:` stays `input_type_ref_conflict` ([ADR-0086](adr/0086-one-schema-dialect.md)).

**Boundaries.** Resolve strictly service-level (types of the same service) - cross-service and local-per-scenario declarations outside the MVP ([ADR-062](adr/0062-input-types.md), MVP boundaries). After a successful resolution, the expanded circuit is checked with the usual input checks (`input_*`) recursively, like any inline-`object`/`array`.

> **The one-schema-dialect codes** (NIM-742). `state_schema` is written in the input DSL,
> which gives `validate-service` two more refusals and takes the list form of requiredness
> away from every schema the linter checks:
> `state_schema_legacy_json_schema_form` (the old `{type: object, properties: {…}}`
> envelope at the root, refused **by name** because it otherwise parses silently as two
> state fields called `type` and `properties`) and `input_required_list_removed` (the
> object-level `required: [names]`, at every level and in `types.yml` too).
> `state_schema_root_not_object` survives with a narrower job: the root must still be a
> mapping, it just no longer has to be a schema document. Spec:
> [`docs/service/manifest.md` → Format `state_schema`](service/manifest.md#format-state_schema).

**Dependencies.** Format source of truth is [`docs/input.md`](input.md); name dictionary (`types:`/`$type`/`x-type`/`input_type_*`) - [`docs/naming-rules.md`](naming-rules.md).

**Implementation status (current MVP).** Cross-ref `register.<name>` vs task plan declarations (including block nesting) - implemented (codes `duplicate_task_address`, `unknown_register_reference`; `register.self.*` excluded). `duplicate_task_address` catches a duplicate in the address space of the subscription `register ∪ id` (two `register`, two `id`, or `register` of one task == `id` of another; ADR-052 §h, [destiny/tasks.md §8](destiny/tasks.md)) - last-wins-resolve would have quietly tied the dependency/alert to the wrong task. Cross-file (address duplicate between the main file and the one connected via `include:`) is checked on the flat plan after expanding include. Additionally, statistical checking of `soulprint.<...>` links in CEL predicates is enabled (`where`/`when`/`changed_when`/`failed_when`/`retry.until`/`loop.when`):
- bare `soulprint.<x>` without `.self`/`.hosts`/`.where` → `soulprint_naked_reference` (canonical form required, see [`docs/soul/soulprint.md`](soul/soulprint.md));
- `soulprint.self.<unknown_top>` (typotype `memmory`/`familly` on the top segment) → `soulprint_unknown_path` with reconciliation against the typed scheme [ADR-018](adr/0018-soulprint-typed.md) (`sid`/`hostname`/`os`/`kernel`/`cpu`/`memory`/`network`/`covens`/`role`);
- `soulprint.hosts.where(...)` / `soulprint.where(...)` are skipped - these are scenario-only accessors, they are validated by `shared/cel.rewriteHostsWhere` in the render phase ([`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md));
- second stage `soulprint.self.<msg>.<unknown_field>` (typo in a subsegment, for example `os.familly`) - **postponed** by a separate slice upon request (the current check only catches typos in the first segment; placeholder in the code is `checkSoulprintSubPath`).

Register-dependency detector is connected inside `block:` for staged-render ([ADR-056](adr/0056-staged-render-passage.md)): a child of block reading register issued by a neighboring child of the **same** block, → `within_block_register_dependency` (`error`). Block is atomic by Passage (the entire fan-out is one Passage), peer-register is available only to Soul-side AFTER probe, and `where`/`params`/`vars`/`apply.input` are resolved by Keeper-side BEFORE dispatch → selecting hosts using the outdated register silently (silent-wrong-target). Flow-control `when` (Soul-side per-task gating after FC-5 narrow-fix) **not** included here - within-block `when: register.peer` is valid. It can be treated by moving the probe and consumer to different top-level tasks (then stratification will routinely separate them by Passage). The same code insures runtime keeper-side for a run under the deprecated staged rollback; a linear run's Keeper refuses it only inside a block rolled as one group ([ADR-0091](adr/0091-scenario-linear-barrier.md), item 12), while `soul-lint`, which knows no execution model, refuses it in every block. Catalog description - [`docs/naming-rules.md → Parser / validation errors`](naming-rules.md). Symmetric stratify/passage codes (`register_dependency_cycle` - register dependency loop; `cross_passage_requisite_unsupported` - cross-passage requisite without audit log) are detected during stratification/runtime, see the same directory.

A destiny has its own counterpart, `within_destiny_register_dependency` (`error`, `validate-destiny`, NIM-939): a destiny task reading, in `where`/`vars`/`params`/`loop`/`on`, a register declared by a task of the **same** destiny - a sibling's or its own. The Keeper renders a whole destiny before any of its tasks runs, against an empty `register` root, so the read fails at render or, under `has()`, silently takes its fallback. Checked on the flat plan after include expansion; the Keeper refuses the same destiny when it loads it.

The register NAME must be written as a dot-select, and the index form is reported as `register_index_form` ([ADR-010](adr/0010-templating.md) amendment 2026-09-28, NIM-909) with the YAML path: `register["probe"]` / `register.hosts["probe"]` is invisible to every one of the extractors above, so nothing orders the run by that dependency, nothing checks the producer exists, and the Keeper's cross-passage flow-control refusal (retired here, see below) goes blind with it. The rule asks the same `shared/cel` engine the render asks, through the same `.where(...)` rewrite — so a `soulprint.hosts.where("register['probe'] …")` predicate, a string literal until that rewrite inlines it, is seen by both halves rather than by the run alone. They can still differ in which refusal you see, not in whether there is one: offline assumes the host accessor is open, so on a host task the run reports the `register.hosts` isolation error first. An index INTO a register (`register.<name>["key"]`, `register.hosts.<name>["<sid>"]`) names the register first and is not reported. Covered where the compute rules are — the scenario's own task list including `block:` children and the `compute:` block — and, like them, not inside a destiny's `tasks/` file or tasks spliced in by `include:`, which the compile-time refusal still catches.

⛔ Retired in `soul-lint` by NIM-944 ([ADR-0091](adr/0091-scenario-linear-barrier.md), S1): under linear dispatch the Keeper decides a `when:` across steps and renders the other register reads into the post-module predicates, so the shape below is legal there; the Keeper keeps the refusal for a run executing staged. What the detector did: cross-passage flow-control gating ([ADR-056](adr/0056-staged-render-passage.md) amend 2026-06-21, FC-5): task gating `when:` / `changed_when:` / `failed_when:` by register issued in **earlier** Passage, → `cross_passage_when_unsupported` (`error`). Flow-control = Soul-side per-task gating ([ADR-012(d)](adr/0012-keeper-soul-grpc.md)) - only sees register **its** Passage; cross-passage register is not available to it (another `ApplyRequest`) → `no such key` silently, task FAILED. `where:` (Keeper-side targeting) cross-passage is capable, `when:` is not: asymmetry is legitimate. The `when` Passage itself **does not split** (flow-control is NOT passage-defining), so the register-dependent `when` usually goes the same-passage with the probe and works; the code only works when the probe left for an early Passage for **another** reason (different task with `where: register.X`). Treated: `where:` for cross-task register targeting OR `register.self` for same-task gating (`register.self` is NOT caught by the detector). The same code insures runtime keeper-side.

A `serial:` needs no written `serial_scope`: the default `task` rolls a block by task, and a canary writes `serial_scope: block` ([settings.md §3.1](scenario/settings.md)). Checked on the flat plan after include expansion: a `serial:` or a barrier-kind setting on a descendant of a block rolled as one group → `group_child_key_unread` (`warning`): the group has one width and one barrier.

For `on:`-literals: format (`kebab-case` / `${ ... }`-CEL / `keeper`) - implemented (codes `enum_invalid`, `name_invalid_format`, `type_mismatch`); The hook `CovenLabelValidator` (interface in `shared/config`, no-op by default) is attached to every non-CEL-wrapped coven literal via `SetCovenLabelValidator`. The real covens directory (Q1b ADR-008-amend) will replace no-op without changing the public API; Until then, the linter does not flag the "existence" of coven (this is runtime).

## Whole-service mode: `validate-service-tree <dir>`

Implemented, NIM-753. One invocation checks a whole service repository and reports
**every** part of it:

```sh
soul-lint validate-service-tree . --service-name redis
```

```
OK: service.yml
OK: types.yml
scenario/create/main.yml:124:11: warning: [form_field_uncovered] input.provision is not placed in any form section
OK: scenario/create/main.yml
OK: scenario/update_users/main.yml
OK: migrations
```

The positional is the service **directory**, or the `service.yml` inside it — both
spellings address the same tree and produce the same report. A directory with no
manifest is refused (exit 2) rather than walked: it is not a service tree, and
reporting a service's worth of absences would bury the one fact that matters. The
three flags are the per-file family's, and mean the same things
([`--modules`](#plugin-module-params---modules-aliaspath),
[`--service-name`](#service-identity---service-name-name-validate-scenario-validate-service)); an
unresolvable `--modules` binding is fatal for the whole run, and fatal **once**.

### The rule it moves into the tool

**One broken part does not hide the others.** That is the whole point, and it is
not a convenience.

The per-file commands check one document per invocation, so a service repository
had to orchestrate the sequence itself — and the orchestrator, an ordinary `set -e`
script, stopped at the first non-zero exit. In one such service that first exit
was a one-line manifest error, and behind it sat **eight** accumulated divergences
that nobody saw for as long as the manifest stayed red: a dead `state_changes:`
block (thirteen state fields silently not written, NIM-739), the renamed capture
params (`key`/`set` for `field`/`value`), the changed L0 case form
(`assert.state_changes` → `assert.state_after`), a `required: true` next to a
`default:`. None of them were new. The service had been drifting behind the engine,
and the silence lasted exactly as long as the manifest was red.

So no failure in this mode ends the walk. A part that cannot even be **read**
contributes an `io_error`; a check that *crashes* on one contributes
`lint_internal_panic`, because a panic is the one route by which a single part
could still take the whole report down. Only a caller error — this is not a service
tree, that binding does not resolve — is fatal, because then there is no run to
speak of rather than a partly broken one.

### What it walks

| Part | What it is |
|---|---|
| `service.yml` | the manifest, checked exactly as `validate-service` checks it |
| `types.yml` | the type catalog, **parsed on its own account** |
| `scenario/<name>/main.yml` | every scenario, checked as `validate-scenario` checks it — and with it the covenant it `extends:` and every `include:` body reached from the entry point |
| `upgrade/<slug>/main.yml` | the same, for the second scenario auto-discovery channel ([ADR-0068](adr/0068-service-upgrade-v2.md) §3) |
| `migrations/` | the state-schema ladder, checked — continuity, step layout, each step's own document, and the `schema.lock` beside it (the lock through the manifest part, which is where the schema is) |

What is **not** in that table is as much the point as what is. A scenario's
`tests/<case>/case.yml` fixtures are **not read** — L0 cases belong to `soul-trial`,
which renders them, and `soul-lint` is static by [ADR-004](adr/0004-binaries.md)
(see [What is NOT soul-lint](#what-is-not-soul-lint)). Neither is `vars/` walked
here beyond what `validate-service` already does with it.

Three of the rows are worth their own paragraph.

**`types.yml` is the part that adds a check rather than moving one.** A catalog is
otherwise parsed only as a side effect of resolving a `$type` — from the manifest's
`state_schema:` and from each scenario's `input:`. A service whose catalog is
broken and whose current schemas happen to reference nothing from it therefore
passes **every** per-file command, and meets the breakage later, at the keeper, on
the first scenario that starts using a type. Here the catalog is a part like any
other, referenced or not.

**Covenants have no row of their own on purpose.** A covenant is a fragment; what
it means is decided only once merged into a scenario, so it is checked through
each scenario that extends it (`ResolveScenarioCovenant`) rather than in isolation.

**`upgrade/` is walked because it is a scenario channel, not a variation on one.**
The keeper discovers scenarios from `scenario/` and `upgrade/` alike
(`artifact.ListScenarios` beside `artifact.ListUpgrades`); the two differ in what
is done with the result, not in the file, so they are checked by identical rules.
A walk that covered only the first would print a clean tree having never opened
the other half of it.

**One thing about an upgrade scenario surprises people, and the linter mirrors it
rather than correcting it.** Its `include:` resolves out of `scenario/<slug>/` and
`scenario/` — *not* out of `upgrade/<slug>/`, the directory the file is sitting in.

That is not an accident and not an engine defect: the two levels are **fixed
directories**, keyed on the scenario NAME, "whichever file the `include:` was
written in" ([orchestration.md §6](scenario/orchestration.md)) — the rule that
keeps a body's meaning independent of which file spliced it. The keeper's resolver
is handed the name and never the channel, and `scenarioTemplatePrefix` is
name-keyed for the same reason, so `templates/` and `include:` agree. A body
written next to an upgrade scenario is therefore unreachable at run time, and the
lint reports it as `include_resolve_failed` naming both levels the engine will try,
with a hint saying which directories to move it to. Resolving it instead — being
right about the file while disagreeing with the engine — would be a green lint over
a definition that fails on the run, which is the failure class this page is about.

A directory in either channel whose name begins with `_` or `.` is **not** a
scenario — it holds shared `include:` bodies. The rule is asked of its owner
(`config.IsSharedDirName`), the same function the keeper's listing uses, rather
than re-derived here. A directory that does not begin with one and has no
`main.yml` is an **error**: `<channel>/<name>/` is how a scenario is addressed, so
a name that answers to nothing runnable is a defect, not something to pass over.

### Two codes it raises that no per-file command can

One is about the walk rather than about a document; the other is about a document,
and exists only because the walk has more to lose than a single-file run.

| Code | Level | What it says |
|---|---|---|
| `service_tree_no_scenarios` | WARNING | the tree declares no scenario in **either** channel — the service parses, registers, and can never be run. A warning, not an error: it does not fail a lint, but a walk that reported one part and looked complete would be the silence this mode removes. |
| `lint_internal_panic` | ERROR | a check crashed on this file. A panic is the ultimate early exit — it would abort the process over a half-written report and leave every part after it unchecked — so each part runs behind a recover, and the crash comes back as a finding naming the file that provoked it. It accuses the linter, not the author; the rest of the tree is still reported and the run is red. |

### Output

Human mode prints each part's diagnostics followed by `OK: <path>` when that part
came out clean — which is what makes the report say **which** parts ran. A tree
whose scenarios were never reached would otherwise look exactly like a tree whose
scenarios are fine. A part that was only *discovered* prints no `OK:` line: saying
"OK" about something nobody looked at is the same failure, spelled optimistically.

`--json` is the same JSON-Lines stream the per-file commands produce — one object
per diagnostic, in walk order, and nothing else — so a consumer written against
one mode reads the other unchanged. **It carries no part boundaries**, which is a
real loss against human mode: there is no `OK:` record, so a machine consumer sees
"no diagnostics from the scenarios" and cannot tell that from "the scenarios were
never reached". Use the exit code for the verdict and human mode when the question
is which parts ran.

Output is **byte-identical run to run**: parts are visited in a fixed order —
manifest, catalog, `scenario/`, `upgrade/`, migrations, each directory sorted by
name.

**Nothing is de-duplicated**, and that is a decision. One file is read by several
parts — a broken `types.yml` is reported by the manifest's `$type` resolve, by
each scenario's, and by its own part — so an identical sentence can appear more
than once. Collapsing them costs more than it buys: a part's verdict is computed
from its own diagnostics, so a part whose every finding had already been printed
elsewhere would come out looking clean and take an `OK:` line — a green line on a
red part, which is precisely the failure this mode exists to abolish. Keeping every
part whole buys a contract worth stating instead: **the report is the per-file
reports concatenated in a fixed order, plus the parts no per-file command covers.**
A repeated sentence also carries information — it says which parts this one broken
file is breaking.

Exit codes are the per-file family's: `0` no errors anywhere, `1` an error
somewhere, `2` the caller is wrong.

### The per-file commands stay

They are what an editor calls on the buffer being typed in, and what an author
reaches for to re-check one file after fixing it.

**One of them did change behaviour**, and it is worth stating rather than filing
under "unchanged": `validate-scenario` shares the include resolver with the walk,
so `soul-lint validate-scenario <svc>/upgrade/<slug>/main.yml` now resolves from
`scenario/<slug>/` and `scenario/` like the engine does, where before it treated
that path as a loose file — resolving a sibling body the run cannot reach, and
downgrading a genuinely unresolvable include to a hint with exit 0. An upgrade
scenario that keeps its bodies at either level is unaffected; one that keeps them
beside `main.yml` goes from silently green to `include_resolve_failed`, which is
the point. Nothing else about the per-file family changed.

## Plugin module params: `--modules <alias>=<path>`

A task's `params:` are checked against the module's declared schema — unknown key,
missing required, type mismatch, unknown state. For `core.*` the declarations are
compiled into the linter, so this is always on. A **plugin**'s schema ships with its
artifact, so the linter has to be handed it
([ADR-0076(x–z)](adr/0076-engine-compat-window.md)):

```sh
soul-lint validate-scenario <path> --modules redis=./redis/dist/schema.json
```

That path is the **published sidecar** `soul-mod stamp` writes next to the artifact, so
the usual binding costs no download: the same bytes are stamped into the binary, but
`schema.json` is right there in the checkout.

The flag is **repeatable** — one binding per plugin, appended in order.

**`<path>`** is a `schema.json`, a stamped artifact, or the `dist/` directory holding
one. The two carriers are told apart **by content**, not by extension: a canonical
document is a JSON object and begins with `{`, an artifact does not. Guessing from the
filename would misread `dist/redis.json` and — worse — would read an artifact
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
task addresses `redis`. Infer the alias from the path and a definition's
address starts depending on where a file happens to sit — **rename the folder and the
same definition starts or stops validating, with nothing in the definition changed.**
You write the same `redis` the task writes in `redis.acl.reloaded`, and it means the
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
main.yml:223:13: hint: [plugin_params_unchecked] params of redis.instance were not
checked: no module manifests were supplied
```

Its job is to keep "checked and clean" from looking identical to "never looked", which
is how the drift in NIM-206 survived long enough to be found by hand. A definition's
author usually cannot produce somebody else's schema, so failing them for its absence
would punish the wrong person.

### A `core.*` address is never a binding question

`core` is reserved, so `--modules` can neither help nor be blamed for a `core.*`
step. Its answer comes from the core catalog compiled into the tool
([module/README.md → Catalog status](module/README.md#catalog-status)), in one of three
forms:

| The address | Answer |
|---|---|
| A served module with a schema (`core.pkg.installed`) | Checked like any module: `unknown_param`, `missing_required_param`, `module_state_unknown`. |
| A served module that ships no schema (`core.augur.fetch`, `core.cert.registered`) | `plugin_params_unchecked`, a hint, saying the engine serves the module and has no schema for it. |
| An address no binary serves (`core.cloud.created`) | **`core_module_unknown`, an ERROR** (NIM-888). |

**The last one is an error and not a hint because nothing on this engine can make it
run.** A plugin's schema can be absent from one checkout and present in another; a
`core.*` module this engine's binaries do not serve stays unserved whatever is bound or
registered, because no plugin may supply a reserved name — only an engine release that
ships the module can. As
a hint it printed `OK:` with exit 0, and the runtime did not fail loudly either: an
address the side catalog does not know is routed Soul-side, so a removed keeper-side
module silently changed side and a keeper-only plan aborted `no_hosts` (NIM-863). The
Keeper raises the same error when a run (or its input) is parsed, because it reads the
same validator. Service registration does not parse scenarios, so it still accepts such
a service — `soul-lint` is the check that stops it before it reaches a cluster.

### A step written in an `include:`d body

It is checked like any other, at its own file and line, in `validate-scenario` and
`validate-service-tree` alike — and until **NIM-779** it was not checked at all.
`params:` are checked per document, and a body pulled in by `include:` is a document
of its own, reached only through include expansion. That path carried neither the
bound manifests into the body nor the body's non-fatal findings back out, so a plugin
step living there produced **nothing**: no `unknown_param`, and no
`plugin_params_unchecked` either. This is the exact outcome the hint exists to
prevent, and the path NIM-778 took — a `tls: "true"` written as a string, in a downstream
redis service's `scenario/provision.yml`, past a lint that said `OK:`.

The second half is what the first rests on: expansion now carries a body's hints out
instead of discarding them, so an unbound manifest reaches `soul-lint`'s output at the
included file's own line rather than dying inside the expander.

**Keeper does not do this half.** `include:` expansion in the keeper runs with no
manifest resolver, and every keeper caller filters the diagnostics to errors before it
formats them — so a plugin step in an included body is neither checked nor hinted at
there, and the paragraph below is about a scenario's own `main.yml`. Wiring it is a
policy question rather than an oversight: `unknown_param` is an error and the keeper
aborts a run on one, so the check would start refusing runs that dispatch today. That
fork is NIM-785. Until it is decided, the keeper-side backstop for an included body is
the runtime gate on the host, not the render. What is no longer in the way is the
wiring: since NIM-790 it lives in `shared/definition`. On the scenario side that makes
adoption four one-line swaps — `run.go`, `preflight.go`, `render_host.go` and
`deprecations.go` each already hold a resolver, and each calls
`config.ExpandIncludes` where it would call `definition.ExpandScenario`. The destiny
side costs more: `artifact.DestinyLoader` has no manifest source and `parseTasks`
returns no diagnostics, so it needs a field and a signature. And **neither** makes the
hint visible on its own — every one of those callers keeps only `diag.HasErrors`, so
surfacing it needs the report channel NIM-785 has to decide on. The check is one line;
the answer to "what does a cluster do with it" is the ticket.

### A step in a destiny's `tasks/main.yml`

`validate-destiny` checks it, and everything the task file includes, since
**NIM-783** — the third file of this shape and the one that was worst, because
`destiny.yml` carries no tasks at all. The manifest holds the contract
(`input:`/`validate:`/`compat:`); the definition lives in the sibling
`tasks/main.yml`, which the command read **twice** for cross-file facts (the compat
floor, the vars collision) and judged zero times. So `--modules`, which the command
has always accepted, reached nothing, and a step with an undeclared param printed
`OK:`.

What is reported now is the file's whole verdict, not the plugin half of it. That is
not a wider bar for a destiny: the keeper's own loader refuses the artifact on any
error out of the same two calls, so this is the keeper's answer said offline. The one
finding the linter adds is the one the cluster cannot produce — the `--modules`
bindings stand in for the Sigil grants it resolves from.

`include:` inside a destiny resolves **one tier**, strictly inside its own `tasks/`
directory: a destiny is its own git artifact with no service level to fall back on.
So an unresolvable target is a plain error here, never the
`stage_include_unresolved` hint a loose scenario gets — there is no later resolution
for it to defer to.

A `destiny.yml` with no readable `tasks/main.yml` beside it gets
**`destiny_tasks_unchecked`**, a hint:

```
d/destiny.yml: hint: [destiny_tasks_unchecked] tasks of this destiny were not
checked: there is no d/tasks/main.yml beside this manifest
```

A hint rather than an error because a manifest is routinely linted where it is being
written rather than at the root of its artifact; but not silence, because the keeper
**requires** that file and fails the load without it, and an `OK:` on its own would
say the definition had been checked when nothing in it was read.

The same check runs inside Keeper for a definition's own document, resolving from the
plugins the cluster has allow-listed, so a definition linted here and rendered there is
held to the same schema. Either way an undeclared key fails the task on the host
([ADR-0076(t)](adr/0076-engine-compat-window.md)) — the flag only moves the answer to
where the definition is being written.

### `soul-trial` takes the same flag

The L0 runner spells it the same way and parses it with the same loader
(**NIM-790**), because it had the same gap and had it in all three places at once:

```sh
soul-trial run examples/service/dragonfly/scenario/add_user --modules redis=examples/module/redis
```

L0 loaded every scenario and every destiny task file with empty validate options,
expanded every `include:` with no resolver, and filtered both sets of diagnostics to
errors before printing. So a case rendering `redis.acl.present` printed `PASS` over
`params:` nobody had checked — hundreds of such steps across the corpus — and
`plugin_params_unchecked` was produced on every one of them, discarded, and never
seen.

The runner reports a case's non-error findings under its result, at their own level
and file, whether the case passed or failed:

```
PASS  examples/service/dragonfly/scenario/add_user/tests/add-user-plaintext/case.yml
    hint [plugin_params_unchecked] scenario/add_user/main.yml:190: params of
    redis.acl.reloaded were not checked: no module manifests were supplied
```

They do not fail a case — an unbound manifest is not the case author's defect, the
same reason it is a hint in the linter. What fails is the corpus gate: `make trial`
binds the bindings above and refuses a leftover `plugin_params_unchecked`, mirroring
the guard `make lint` has carried since NIM-294.

Four tools grew this wiring one at a time and three of them shipped missing a piece,
so it is no longer per-tool: `shared/definition` holds the loader for the flag and the
two entry points that parse a definition, expand its includes with the same manifests,
and carry **every** diagnostic out. `validate-scenario`, `validate-destiny` and
`soul-trial` all go through it.

They are held to the same answer by a shared **fixture**, not by a single test — the
three tools live in three Go modules and no test process runs all of them.
`shared/definition/deftest` carries one definition with a plugin step on both wiring
paths (inline, and behind an `include:`), and each tool's test asserts its verdict
over those exact bytes in counts that tell the two paths apart. A tool that quietly
stops threading the manifests loses one of the two findings and fails its own count,
rather than reporting a smaller set nobody compares against anything.

## Service identity: `--service-name <name>` (`validate-scenario`, `validate-service`)

A service states no name of its own. `name:` left `service.yml` with
[NIM-726](adr/0085-entity-id-and-label.md): the name is assigned once, at
registration, and it is what the registry row is keyed on, what every derived Vault
path is built from, and what `incarnation.service` resolves to. The manifest used to
carry a second copy that nothing compared against the first.

So the checks that need the name take it as an argument. Two commands read it:

- `validate-scenario`, for the own-namespace Vault fence
  ([ADR-0083](adr/0083-declared-secret-state-fields.md) §7), which refuses a task
  writing under `<mount>/<service>/`:

  ```
  soul-lint validate-scenario scenario/create/main.yml --service-name redis
  ```

- `validate-service`, as the service of the stub incarnation it evaluates the service
  view on ([Var evaluation checks](#var-evaluation-checks)). Without the flag the stub's
  service is the placeholder `stub-service`, and a definition reading
  `incarnation.service` is evaluated against that.

`validate-service-tree` passes its own `--service-name` on to both: to the manifest and
to every scenario it walks. `list-secret-paths` takes the same flag and requires it
([below](#derived-secret-paths-list-secret-paths)). `validate-config`,
`validate-destiny` and `validate-manifest` share the `validate-*` parser, so they accept
the flag, and nothing in them reads it.

Without it `validate-scenario`'s fence **cannot run, and says so**: `own_namespace_fence_unchecked`, at
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
| `stack_step_invalid` | ERROR | `vars/_stack.yaml` cannot be read as written — an unknown key, no steps under `stack:`, a step without `file:`, `foreach:` without `as:` (or the reverse), an `as:` shadowing the step context, a `strategy:` that is neither `deep` nor `replace`. Also the `_stack.yml` spelling, which is not read at all. Since NIM-1014 ([ADR-0082 amendment 2026-10-15](adr/0082-service-vars.md#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late), point 14) also an `inline:` step — removed; the hint says to put the values in a file under `vars/` and name it with `file:` — and a `when:` / `foreach:` / `file:` selector naming `vars.*`, which would read a value before the final merge. |
| `vars_dir_nested` | WARNING | A layer file in a subdirectory of `vars/`, at any depth, that no `_stack.yaml` step can include. The lexical scan reads `vars/*.yaml` and does not descend, so only a `file:` step reaches a nested file — a literal one names one file (a leading `./` is cleaned away, as the Keeper's path join does), and one carrying `${ … }` (`coven/${ coven }.yaml`) any file matching its cleaned literal skeleton, each block standing for text inside one path segment: `${ c }.yaml` names no nested file. A file nothing can name is never read — this is exactly the shape the retired `essence/coven/` layout had — and the var checks below do not lint it. One diagnostic per subdirectory. |
| `vars_retired_layout` | ERROR | The retired `essence/` directory is still present. Half a migration is worse than none: `vars/` resolves to nothing and every `default(vars.X, y)` in the repo silently takes its fallback. |
| `vars_shadows_service_var` | WARNING | A scenario-level or task-level `vars:` name taking over one of the service's own vars. Deterministic and sometimes deliberate — `conf_dir: "${ vars.conf_dir }/conf.d"` derives from the layer below on purpose — so it warns rather than fails. Modelled on `vars_collision` ([destiny/vars.md](destiny/vars.md)). |

`stack_step_invalid` runs the **same parser the Keeper runs**
(`servicevars.ParseStack`, `shared/servicevars/stack.go`: the shape, then the
selectors' no-`vars.*` rule), not a second copy of the schema: a linter with
its own reading of a file format is how a file starts passing one check and
failing the other.

`vars_shadows_service_var` reads the service's var NAMES as a superset — the
top-level keys of every `*.yaml` in `vars/`, without evaluating `_stack.yaml`.
A name a conditional step might contribute still counts, which is the right side
to err on: a `when:`-gated layer that shadows on Tuesdays is precisely the case
an author will not find by reading.

## Var evaluation checks

NIM-1014, [ADR-0082 amendment 2026-10-15](adr/0082-service-vars.md#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late)
point 12. The Keeper evaluates a var only when something reads it, so a broken var
nobody reads no longer fails a run. These checks are where it fails instead —
once per edit, not once per run.

| Code | Level | What it catches |
|---|---|---|
| `var_cycle` | ERROR | A definition that reaches itself through `vars.<name>` / `vars['<name>']` reads — `has()` and `default()` included, since both evaluate their operand — in any view the artifact forms: the service view, a scenario task's (service `vars/` → the scenario's `vars:` → block + task) or a destiny task's (`vars.yml` → block + task). The service view is the layer files the Keeper can read, in its order: without `_stack.yaml` the files directly inside `vars/`, lexically; with one, each step's files in step order and with the step's `strategy:`, whatever its `when:`. A step whose `foreach:` and `file:` read nothing of `incarnation.*` is evaluated as the Keeper evaluates it, so its files come in binding order (`foreach: "${ ['b', 'a'] }"` stacks `b` under `a`); otherwise a composed `file:` contributes every file its skeleton matches (see `vars_dir_nested`), lexically, at its step's position. A layer only some incarnations stack — a `when:`-gated step, or one whose `foreach:` or `file:` reads `incarnation.*` — is conditional; `optional:` alone does not make a present file conditional, and a constant `foreach:` with a constant `file:` is stacked by every incarnation. A conditional step whose `file:` is composed and that has no `foreach:` — `file: "inc/${ incarnation.id }.yaml"` — is a one-of step: it stacks at most one file per incarnation, so its files are alternatives no incarnation stacks together. A view is walked in stacks an incarnation can have — S_all, every layer but the files of one-of steps; S_base, the always-stacked layers alone; S_L, the always-stacked layers and one conditional layer L, for each L in stack order, each file of a one-of step included — and inside one stack a read resolves as the Keeper resolves it: to the name's top definition, merged deep with the lower ones of its rung unless `strategy: replace` cuts them. `validate-service` walks S_all, S_base, then each S_L; `validate-scenario` walks the service rung in S_all and S_base only, under the scenario's own rungs; a destiny's views hold no conditional layer, so each is one stack. A stack that is the same as one already walked is not walked again. So a coven file shadowed by another coven file is still judged: `tuning: "${ vars.tuning }-prod"` in `coven-prod.yaml` with nothing below is the cycle an incarnation in `prod` alone raises, though `coven-staging.yaml` sorts above it. A ring of the always-stacked layers that a conditional layer changes in S_all is found in S_base. Two files of one one-of step close no ring: `inc/a.yaml`'s `maxmemory: "${ vars.mem }-headroom"` and `inc/b.yaml`'s `mem: "${ vars.maxmemory }+headroom"` are no cycle, since no incarnation stacks both, while a ring inside one of them is. And a ring that needs one name from two layers at once is none: `10-a.yaml`'s `x: "${ vars.y }"` and `20-b.yaml`'s `y: "${ vars.x }"`, beside its own `x: 2`, close no ring whichever covens select them. The walk misses a cycle that needs two or more particular conditional layers, short of all of them — in `validate-service` the stub evaluation below builds the stack its own incarnation selects. It does not decide mutually exclusive `when:` conditions, so S_all may be a stack no incarnation builds. It does not evaluate a `foreach:` that reads `incarnation.*`: that step's files stack lexically at the step's position, not in the order an incarnation's list binds them, so a self-read from one of them to another is judged in lexical order. And a scenario's views, which walk the service rung in S_all and S_base only, miss a ring through a conditional layer that a higher conditional layer shadows in S_all, or through a one-of step's file: `10-a.yaml`'s `x: "${ default(vars.s, 0) }"` under `20-b.yaml`'s `x: 2`, both `when:`-gated, with the scenario's `s: "${ vars.x }"`, is the `s → x → s` an incarnation in `a` alone raises, and `validate-scenario` does not report it. A self-reference reads the definitions below the layer that wrote it, a later file or step over an earlier one or a higher rung over a lower one, and is no cycle while the stack holds a definition below; with none (`a: "${ vars.a }"` in `00-base.yaml`, `has()`-guarded or not) it is the one-node cycle. It is no cycle either when the rungs below are unknown — a scenario linted without its service, a service rung that does not assemble (`var_invalid`), a `vars.yml` that does not parse (`yaml_parse_error`) — since an unknown rung may define the name. A `'a' in vars` test evaluates nothing, and where it holds something below defines `a`; a condition built of such tests implies keys where it is true and where it is false — the test implies `a` where true, a bool literal every key on the side it never takes (`false` where true, `true` where false), `!` swaps the sides, `&&` implies where true what any term does and where false what every term does, `\|\|` the reverse, `== true` and `!= false` keep the sides, `== false` and `!= true` swap them, and any other condition, a `?:` included, implies nothing — and a read is exempt in the branch of `?:` on the side its condition implies `a`, and in a term of `&&` (`\|\|`) beside a term that implies `a` where true (false), since CEL's `&&` and `\|\|` drop an error once one term decides the result. `a: "${ 'a' in vars && vars.a != '' ? vars.a + '/x' : '/etc/x' }"` and `a: "${ 'a' in vars \|\| false ? vars.a : 'd' }"` with nothing below are no cycle; `a: "${ 'a' in vars ? 'd' : vars.a }"` and `a: "${ 'a' in vars && vars.n == 1 ? 'd' : vars.a }"` read `a` where it is absent, and are. A read whose result a bool literal decides is no edge at all: in the branch of `?:` a literal condition never takes (`true ? 1 : vars.y`), and in a term of `&&` beside a term that is always false (`false && vars.y`, `vars.y && false`) or of `\|\|` beside one that is always true (`true \|\| vars.y`), CEL skips the read or drops its error, a cycle's included — always true or false meaning built of bool literals with `!`, `&&`, `\|\|` and a comparison with a bool literal — so `x: "${ true ? 1 : vars.y }"` with `y: "${ vars.x }"` is no cycle, and `x: "${ false ? 1 : vars.y }"` is; a whole read there is none either, except a whole comparison: the Keeper materializes the view before it evaluates an expression that may compare the root, whatever decides the result, so `s: "${ false && vars != {} }"` and `w: "${ true \|\| [vars].exists(m, m != {}) }"` are whole reads and, with nothing below, the one-node cycle. The root reaches a comparison through what holds it — a literal list or map, an index or a select into one, a comprehension's variable, and what a call returns, `merge({'x': vars}).x` included, unless the call is one whose result cannot hold an argument's value (`size()`, `type()`, a scalar conversion, a comparison). A whole read of `vars` — `${ vars }`, `merge(vars, …)`, `vars == …`, `vars in […]`, and, conservatively, `type(vars)` and `'a' in vars ? vars : {}` — reads every name of the view, the definition's own name below it included, so with nothing below it is the one-node cycle; `size(vars)`, `'k' in vars` and an iteration over the keys read no value. A read inside a `soulprint.hosts.where("…")` predicate counts: the engine inlines the predicate before compiling — but not one inside a map or struct literal, which the engine's rewrite does not reach. The message is the Keeper's text for the same definitions — `var_cycle: a → b → a`, rotated to the alphabetically smallest name — at the first definition of the cycle the command owns, at its string that reads the next name and in the layer that wrote that string: in a map merged deep across files that can be a lower file than the name's top (`m.a: "${ vars.p }"` of `00-base.yaml` closes `m → p → m` under `20-top.yaml`'s `m: {c: 3}` and is cited at `00-base.yaml`, `$.m.a`); the hint names the same layers. A ring names a name once per layer it passes, as the Keeper does: `var_cycle: a → b → c → a → a` is a coven file's `a: "${ vars.a }-x"` over a base ring `a → b → c → a`. Definitions that reach one another along more than one ring report every distinct ring among the first 16 the walk finds in their group of one stack, those through the group's alphabetically smallest name first, so a group reports at most 16: a ring that repeats a reported text, or cites no definition the command owns, counts toward the 16 too, and a ring through another definition of a larger group may go unreported until those are broken. A ring is reported once per view, at the first stack that finds it, compared by its names, so a second definition that closes the same names into a ring from another layer is reported only once the first is fixed: over the base's `y: "${ vars.x }"`, `10-a.yaml`'s and `20-b.yaml`'s `x: "${ vars.y }-…"`, both `when:`-gated, are one `x → y → x`, cited at `20-b.yaml`. Which ring the Keeper raises depends on its evaluation order and on the incarnation, which the text does not decide, and in a group of more than 16 rings it may be one the walk does not reach. In `validate-service`, a ring the stub evaluation below raises and the walk did not report — compared by its names, whichever layer of a repeated name its text starts at — is this error too, at its first var's file and line (a self-reference at its lowest definition, the one that finds nothing below), with a hint saying only that evaluating the service view on the stub incarnation found it. |
| `var_unknown_ref` | ERROR | A definition reading a name that neither its own rung nor a rung below it defines. A service var that only works once a task supplies the name is this error, reported by `validate-service` — the service view that `compute:` and telemetry read would fail on it — and a scenario `vars:` definition that needs a task's name is the same error in `validate-scenario`; overriding a name from above is fine. A guarded read is exempt: a read of `vars.x` in an expression that also tests `has(vars.x)` or `'x' in vars` on that same root name — what `default(vars.x, y)` expands to, the hand-written ternary, or `'x' in vars ? vars.x : y` — is an optional-override hook (`tls: "${ default(vars.tls_override, false) }"`), not a need; a key-only test evaluates nothing, so it guards as `has()` does. `has(vars.x.y)` / `default(vars.x.y, d)` guard `y`, not `x`: `x` is still checked. A read whose result a bool literal decides (`true ? 1 : vars.x`, `false && vars.x`; see `var_cycle`) is no need. The test is per `${ … }` block and textual, for `in` as for `has()` (`!has(vars.x) ? vars.x : y` and `'x' in vars ? y : vars.x` pass it too); the L0 strict mode is what evaluates the shapes it lets through. Not judged for a scenario linted outside a service tree, whose service names are unknown, nor when the service rung cannot be read (`var_invalid`), nor in a destiny whose `vars.yml` does not parse (`yaml_parse_error`). |
| `var_invalid` | ERROR | A definition that does not compile in its rung's environment: a service file's in `cel.NewServiceVars` (`incarnation.*` and `vars.*` only), a scenario `vars:` one in `cel.NewScenarioVars` — so `soulprint.*`, `register.*`, `vault()` and `generate_secret()` there are this error, in a string at any depth — and a block + task or `vars.yml` one in the environment render compiles it in (a host task's with its loop binding, a keeper task's, the destiny pass's). Also, in `validate-service`, a definition that fails when the whole service view is evaluated on the stub incarnation below; the message names the var, its file and the cause. Also a service view the stub cannot assemble — a step whose selector fails for it, a missing file a step requires — reported at `vars/`. A literal `file:` (no `${ … }`) of a step that is not `optional:` and names a file the tree lacks is this error at the step's `file:`, whatever its `when:` or `foreach:`: the path is the same for every incarnation that stacks the step, and the Keeper refuses each of them. A composed `file:` that reads `incarnation.*`, itself or through the binding of a `foreach:` that reads it, and renders for the stub a file the tree lacks is not this error — even where every incarnation lacks it, as `file: "coven/${ c }.yaml"` over `foreach: "${ incarnation.covens + ['zz'] }"` does: the stub's id and covens are made up (`file: "inc/${ incarnation.id }.yaml"` asks for `inc/stub-incarnation.yaml`), and the walk lints every file the step can name. A composed one that reads neither names one path too, and is this error when the stub's stack reaches it. Also a service rung the walk cannot assemble — a layer file that does not parse, or a malformed `_strategy` — reported on one line at the layer file, at the YAML error's line and column or the `_strategy` key's, its path shown as the command shows the path it was given, by `validate-service` (unless `stack_step_invalid` already explains it) and by `validate-scenario`, whose views carry that rung. A destiny's `vars.yml` that does not parse is `yaml_parse_error` instead, the code every destiny file that does not parse gets, at its line and column, on one line — a list or a scalar where the mapping is due included. The code name is pending confirmation. |

Each command reports the definitions it owns: `validate-service` the service
rung, `validate-scenario` the scenario's `vars:` and its tasks' (a cycle a task
closes through a service var included), `validate-destiny` its `vars.yml` and
its tasks'. A scenario's views carry the service rung so that its own
definitions are judged against it.

The walk compiles every definition against its own rung's environment and
follows the reference graph the text spells. `vars[expr]` with a non-literal key
names no edge, so it is left to the stub evaluation below and to the L0 trial, which renders in a strict mode that
materializes whole every task view its render builds (each host task × host × loop iteration,
each keeper task, each `assert:` on its decision host, each destiny task × host ×
loop iteration); the loop axis, an include `when:` and a `template:`
path still evaluate only what they read, and the stand-in host of an empty target or
roster is no host, so its view is not materialized.

`validate-service` additionally evaluates the whole service view — every name of
`vars/`, assembled as the Keeper assembles it — against a stub incarnation: the
service name given by `--service-name`, else a placeholder (a manifest no longer
states its own name, NIM-726; see
[Service identity](#service-identity---service-name-name-validate-scenario-validate-service)), a placeholder id
and `service_version` (every registered incarnation has one), empty covens and an
empty `traits` map — as the database row has it, so `incarnation.traits` is present
and only a key in it can be missing. Every name is evaluated, so each failing definition is reported, once;
a failure the static walk already reported for that definition — its name in that layer — is not repeated, and a
cycle it raises is `var_cycle`, once per ring the walk did not already report. Any
evaluation error fails the check. Empty traits are intended: a trait is optional
per incarnation, so a definition reading `incarnation.traits.<k>` without `has()`
fails for every incarnation that lacks it, and the check says so first. A layer file only a coven-gated `_stack.yaml`
step includes is not selected by the stub; the static walk above covers it, since
the walk reads every file any step can include.

## The state-schema lock: `schema-stamp` and the check

Implemented, [ADR-019](adr/0019-state-migration-dsl.md) clause 4 (NIM-737). Full
spec — [docs/migrations.md → The schema lock](migrations.md#the-schema-lock).

A service describes the shape of `incarnation.state` **twice** — declaratively in
`state_schema`, imperatively in the `migrations/` ladder — both by hand, and nothing
reconciles the two: not the linter before this, and not the upgrade transaction,
which applies the chain and never loads the target schema. `migrations/schema.lock`
is the one artifact that holds both, and it is generated:

```sh
soul-lint schema-stamp <service dir>     # or the service.yml inside it
```

It writes the top of the ladder and a hash of the **parsed** `state_schema`, and it
refuses to write anything while the **manifest or the ladder** is red — a stamp
claims that a schema and a ladder were, at one moment, both sound and in agreement,
which a broken one of either has not established. Those two and no more: a broken
scenario or a malformed `vars/_stack.yaml` still stamps, since neither is evidence
about the shape of `incarnation.state`, and refusing on them would push the author
towards writing the file by hand. Two runs over one tree produce byte-identical
output; a generated file in git is worth having only if re-running the generator
leaves no diff.

`validate-service` compares both halves, so `validate-service-tree` does too — the
check hangs off the manifest part, where the schema is, rather than off the ladder
part.

| Code | Level | What it catches |
|---|---|---|
| `schema_lock_stale` | ERROR | The stamped fingerprint is not the hash of the current `state_schema`: the schema was edited with no ladder step behind it. Both hashes are in the message, so the disagreement can be quoted into a review rather than reproduced. |
| `schema_lock_version_stale` | ERROR | The stamped `version` is not the ladder's top. Below it, a step was added without re-stamping. **Above** it, a published step was deleted off the top — the one failure nothing else in the repository can see, because the version is derived and the ladder no longer remembers being taller. |
| `schema_lock_missing` | ERROR | The ladder holds at least one step and there is no lock beside it. |
| `schema_lock_malformed` | ERROR | The lock cannot be read: not YAML, an unknown key, a version below 1, a digest that is not `sha256:<hex>`. It is generated, so the answer is a re-stamp rather than reading what is left. |
| `schema_lock_unreadable` | ERROR | The lock is present and I/O on it failed. |
| `schema_lock_unchecked` | ERROR | `state_schema` still holds an unresolved `$type`, so there was nothing honest to hash. It says the comparison did not run rather than reporting a schema edit that did not happen — but unlike `plugin_params_unchecked` and `own_namespace_fence_unchecked` it is an **error**, because nothing is missing from the *invocation*: every route here is the service contradicting itself, and one of them (a `types.yml` that exists and cannot be read) is only a warning upstream. Green would mean an arbitrary `state_schema` edit reported by nothing. The **version** half still runs underneath it. |

**The hash is over the parsed schema and never the bytes.** A comment, a reordered
key, a re-quoted value and block-versus-flow form all leave it where it was; a field
added, removed, renamed or re-typed moves it — and so does any other key of the
schema node, `description:` included. A YAML `#` comment is outside the schema; a
`description:` is part of it, and re-stamping for one is the price of a walk with no
hand-maintained list of what counts. That is what makes a changed lock worth
reading, and the artifact's own bypass depends on it being worth reading: re-stamping
without adding a step is **allowed**, and what stops it being free is the reviewer
seeing a changed lock beside an unchanged `migrations/`.

**A lock that is there is always checked; an absent one is an error only once the
ladder has a step.** A service adopts the lock with one `schema-stamp` — which stamps
a version-1 service too — and cannot then quietly stop being checked. Demanding one
from every directory that merely holds a `service.yml` would fail a loose manifest
that owns no repository at all.

**The version half is skipped while the ladder's layout or continuity is broken.** A
gap moves the derived version, and the author's fix there is to renumber — which
moves it again — so reporting the lock as stale on top of a `migration_chain_broken`
would point at the wrong repair. A step whose `main.yml` is missing does *not* skip
it: the rung is on disk, so the top is where it says it is. The fingerprint half
never depends on the ladder.

## What is NOT soul-lint

Dynamic run of destiny on a test bench, measurement of runtime-coverage and verification of scripts in docker is a **separate tool**, not part of `soul-lint`. According to ADR-004 `soul-lint` is strictly offline and static. The topic is maintained in [destiny/testing.md](destiny/testing.md).
