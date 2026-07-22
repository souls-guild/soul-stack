# soul-lint

Offline linter for Destiny and Essence. The purpose and place in the system are specified in [ADR-004](adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper): parsing, rendering, checking according to the scheme, static analysis. Works without connecting to Keeper - suitable for CI, IDE and local launch.

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

## What is NOT soul-lint

Dynamic run of destiny on a test bench, measurement of runtime-coverage and verification of scripts in docker is a **separate tool**, not part of `soul-lint`. According to ADR-004 `soul-lint` is strictly offline and static. The topic is maintained in [destiny/testing.md](destiny/testing.md).
