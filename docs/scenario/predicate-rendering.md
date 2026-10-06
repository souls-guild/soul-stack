# Rendering a post-module predicate on the Keeper

> **Status: design, not implemented.** Specified by the draft ADR
> [`draft-scenario-linear-barrier.md`](../adr/draft-scenario-linear-barrier.md) (NIM-907), decision 4.
> Today the Soul evaluates `changed_when:`, `failed_when:` and `retry.until:` itself, against the
> registers it accumulated inside one message.

Under linear dispatch no register crosses the wire (decision 4). `changed_when:`, `failed_when:` and
`retry.until:` judge the module's result, so they cannot be *decided* before the module runs, but
everything in them except that result can be *rendered*. The Keeper resolves everything but
`register.self` — another task's register, `vars`, `input`, `incarnation`, `soulprint.self` — and
the Soul receives a predicate that reads only `register.self`:

```yaml
- name: verify replication
  module: core.exec.run
  failed_when: register.self.stdout != register.probe.stdout
```

arrives on the Soul as `register.self.stdout != "bob"`.

## 1. The contract — an equivalence, not an algorithm

For every value the open set (§4) can take, the predicate the Soul receives must give what the
author's predicate gives against that host's full context — the same value, or an error where the
original errs. An error is not a failure of the contract: it is carried (§2, **Errors**).

Where the Keeper cannot guarantee the equivalence for a host on which the task would run, that
host's task fails before dispatch with a named code. Inside a block rolled as one group, where the
child cannot be dispatched alone, the whole group fails on that host, judged by the child's radius.
Every task the message would have carried gets a FAILED record, as for a failure tied to no task
(the ADR's fork 1), except a child the Keeper had already decided to skip there, which gets a
Keeper-written SKIPPED record instead; a child sent `when: false` carries no predicate to fail. The
records feed rescue routing and applier resolution; the failure itself is judged once, by the
child's radius ([settings.md §4](settings.md)). The Keeper never sends a predicate that may answer differently.

How the rewrite is built is S1's to choose. What it must survive is fixed here: six review rounds
found the traps in §2, each confirmed in this tree, and **every one is a case in a differential test
S1 must pass** — the original evaluated against the full context, the rewrite against the open set
alone, over the same values.

## 2. The traps — cases the differential test must cover

- **cel-go's residual pruner is not enough.** `Env.ResidualAst` with `PartialVars`
  (`cel/env.go:785`, `cel/program.go:94`) prunes only a comprehension's range, not its body
  (`interpreter/prune.go:434-438`), leaves — without exhaustive evaluation — both arms of a ternary
  whose condition is open, and the fallback of `default()`, with their references, and lets a bare
  `register` match the unknown pattern whole (`interpreter/attribute_patterns.go:260-261`).
- **Printing.** With macros expanded a comprehension does not print (`parser/unparser.go:110-111`)
  and this tree records no macro calls; `rewriteHostsWhere` parses without macros for that reason
  (`noMacroParser`, `shared/cel/engine.go:100-105`; `shared/cel/hosts.go:20-26`).
- **Field arguments.** `has()` and `default()` take a field, not a value: `has("v")` does not parse
  and `default("v", y)` is refused (`parser/macro.go:550-556`, `shared/cel/default.go:83-85`). Their
  first argument is decided as a whole — `default(register.probe.x, register.self.y)` included.
- **Types.** The Soul type-checks what it compiles (`shared/cel/engine.go:434`), and a number from
  JSON is a double that prints as `3.0` (`parser/unparser.go:296-302`). So `3.0 == 3`,
  `size(register.self.members) >= 3.0` and a ternary with a string arm and a double arm are compile
  errors where the original, over `dyn` variables, evaluates. A substituted value wrapped in
  `dyn(...)` compiles.
- **Errors.** CEL's `&&`, `||` and `?:` absorb an error from an operand they do not need, and so do
  `exists()` and `all()`, which expand to them. With `input.x` absent,
  `has(input.x) && register.self.stdout != input.x` is `false`, and
  `register.self.rc != 0 && register.self.stdout != input.x` is `false` when `rc` is 0. So an error
  the Keeper meets cannot simply fail the task: where no decided operand absorbs it, it travels as a
  term that raises on the Soul, and CEL's own logic decides.
- **Values text cannot carry.** A timestamp, a duration or a `type(...)` value does not print as a
  constant (`parser/unparser.go:320-321`), and NaN or an infinity prints as text that does not parse.
  A timestamp or a duration can be carried by substituting inside its constructor instead
  (`timestamp("…")`, `duration("…")`), and a type by printing its name (`int`, `string`) —
  not `type("…")`, which is always `string` — unless an iteration variable of that name is in scope
  there (see **Hidden references**), which fails with a named code. The parser stops at 100,000 code points for the whole expression
  (`parser/parser.go:69-71`) and at 250 levels of nesting (`:54-56`), which a large or deeply nested
  probe output can exceed where the original evaluates today. Every case the rewrite cannot carry
  fails with a named code.
- **Hidden references.** The check that nothing outside the open set is left must walk the result:
  compiling cannot find one, because every root is declared `dyn` in the flow-control environment
  (`shared/cel/engine.go:269-271`). A bound iteration variable counts as open, and it is resolved by
  scope, not by name: `register.self.members.exists(input, input == "a")` compiles, and its `input`
  is not the root. A type name (`int`, `string`) is an identifier the walk must allow, and an
  iteration variable can shadow it too: in `register.self.l.exists(int, type(int) == int)` the last
  `int` is the variable, not the type.

## 3. Printing, refusals and the fallback

The printed result has its map keys sorted, so the same values give the same text. A bare
`register` (no field after it) and a task registered as `self` are refused at compile, since either
makes "everything but `register.self`" ambiguous; inside a group the same refusal covers a child's
open `when:`.

**The fallback** is to carry the values as typed variables instead of text. That removes the type,
error-term and size traps, and the text-guard and masking work in §5 with them, but costs the
readable form the user chose (`register.self.stdout != "bob"`). If the differential test cannot be
passed with literals, S1 returns to the user with the failing cases; it does not switch on its own.

## 4. The open set

The open set is `register.self`, and inside a group also the group's own registers. A block rolled
as one group puts several tasks in one message, and a child's `when: register.<sibling>` is valid
there today: the within-block detector deliberately leaves flow control out
(`shared/config/passage.go:675-680`). For such a child the sibling's register stays open, and the
Soul evaluates against what it accumulates inside the message — as it does today, and as a destiny
does. Its `when:` is decided on the Keeper only when nothing in it is open.

## 5. What has to be built

Each piece is absent today, except the two existing host-invariance guards, which are lifted.

- **Per-host transport.** The rendered predicate depends on that host's registers, so it differs per
  host. One `RenderedTask` serves every host it goes to (`keeper/internal/scenario/dispatch.go:582`),
  and since NIM-908 it carries per-host params: `ParamsBySID` (`keeper/internal/render/render.go:613`),
  picked for each host by `paramsForHost` (`keeper/internal/render/prototask.go:147`)
  ([ADR-draft per-host params dispatch](../adr/draft-per-host-params-dispatch.md)). Flow control is
  the half NIM-908 left refused: a host-variant flow context fails the render
  (`keeper/internal/render/pipeline.go:713-717`). Every predicate that travels needs the same kind
  of overlay as params — `changed_when:`, `failed_when:` and `until:`, and a grouped child's `when:` whose
  residual is still open; without it, host 1's predicate would silently go to every host. A path
  past render already exists: the requisite gate clones a task per host
  (`keeper/internal/scenario/crosspassage.go:145-150`), and each host's slice reaches the wire
  through `ToProtoTasksForHost(perHost[sid], sid)` (`keeper/internal/scenario/dispatch.go:278`). A `when:`
  the Keeper decides for a step needs none: it decides the host slice, which is per host already.
  Inside a group it does: a child skipped on one host and not another travels as `when: false` on
  the first only.
- **The soulprint guard is lifted for a rendered predicate.** `guardFlowControlHostInvariant`
  (`keeper/internal/render/pipeline.go:1418-1434`, called at `:449`) refuses a `when:`,
  `changed_when:` or `failed_when:` naming `soulprint` on a multi-host target, because today the
  Soul would evaluate it against the first host's snapshot. With `soulprint.self` resolved per host
  on the Keeper, that reason is gone. So is the reason for the second layer NIM-908 kept,
  `flowContextHostInvariant` (`keeper/internal/render/pipeline.go:713-717`): with every `vars` and
  `input` reference substituted, no flow context travels for a scenario task to differ per host.
- **The Soul's text guards must leave a substituted value alone.** `guardUnsupported` matches `now(`,
  `vault(` and `generate_secret(` in the raw text, string literals included
  (`shared/cel/functions.go:107-123`), and `normalize`'s literal regex does not understand an escaped
  quote (`:130-136`), so a value containing `vault(` fails the task and one containing `\"` can have
  its whitespace collapsed. The guards learn literals. Carrying the values as bound variables
  instead is the fallback in §3, and only the user takes it.
- **Masking.** A substituted value can be a secret — from a sealed register, or a `vault()` var — and
  the predicate text is echoed into errors and logs at every evaluation site: `when`
  (`soul/internal/runtime/applyrunner.go:537`), `until` (`:947`), `failed_when` (`:1179`), the shared
  path (`:1268-1281`) and `logFlowControlError` (`:1341-1356`) — and through every `%v` of an error,
  since `ErrCompile`, `ErrEval` and `ErrUnsupported` each embed the expression
  (`shared/cel/eval.go:27-28`, `:40-41`, `:55-56`). The Soul must name the predicate's kind and the
  task there, never the rendered text.

A capability bit gates a rendered predicate: an older Soul compiles the normalized text and echoes
what it receives. A decided `when:` is blanked and needs nothing. A grouped child's `when: false`
and a predicate in which no reference was resolved need no new bit (the ADR's item 10), only the
existing ones a predicate needs today — `CapabilityFlowControl`, and `CapabilityRetry` for `until:`
(`keeper/internal/scenario/soulcompat.go:97-101`).
