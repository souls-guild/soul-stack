# Rendering a post-module predicate on the Keeper

> **Status: built on the Keeper, behind linear dispatch.** Specified by the draft ADR
> [`draft-scenario-linear-barrier.md`](../adr/draft-scenario-linear-barrier.md) (NIM-907), decision 4.
> The rewrite and its differential test are `RenderPredicate` in `shared/cel/predicate_render.go`
> (NIM-928), the parse-time refusals of §3 are in `shared/config`. The rewrite only substitutes
> (§3); simplifying what the host context alone decides is NIM-970. A run executing linearly
> settles its step's flow control on the Keeper (NIM-943, `keeper/internal/render/flow.go`); a staged
> run — every run until NIM-952 — still sends the predicates as written, and the Soul evaluates
> them against the registers it accumulated inside one message. The Soul's half of §5 is built
> (NIM-929), and so is the Keeper's capability gate (NIM-946, §5).

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

arrives on the Soul as `register.self.stdout != dyn("bob")` (§2, **Types**).

## 1. The contract — an equivalence, not an algorithm

For every value the open set (§4) can take, the predicate the Soul receives must give what the
author's predicate gives against that host's full context — the same value, or an error where the
original errs. An error is not a failure of the contract: it is carried (§2, **Errors**).

Where the Keeper cannot guarantee the equivalence for a host on which the task would run, that
host's task fails before dispatch with a named code — inside a group also where a child's open
`when:` (§4) or a requisite on a sibling might have skipped it, since the Keeper cannot send what
it cannot render. Inside a block rolled as one group, where the
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
  (`noMacroParser`, `shared/cel/engine.go:100-106`; `shared/cel/hosts.go:20-26`).
- **Field arguments.** `has()` and `default()` take a field, not a value: `has("v")` does not parse
  and `default("v", y)` is refused (`parser/macro.go:550-556`, `shared/cel/default.go:83-85`). Their
  first argument is decided as a whole — `default(register.probe.x, register.self.y)` included.
- **Types.** The Soul type-checks what it compiles (`shared/cel/engine.go:471`), and a number from
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
  (`shared/cel/engine.go:283-285`). A bound iteration variable counts as open, and it is resolved by
  scope, not by name: `register.self.members.exists(input, input == "a")` compiles, and its `input`
  is not the root. A type name (`int`, `string`) is an identifier the walk must allow, and an
  iteration variable can shadow it too: in `register.self.l.exists(int, type(int) == int)` the last
  `int` is the variable, not the type.

Building the rewrite found four more, each now a case of the same test. A randomized differential
test over generated predicates found all four, against an earlier version that also simplified; the
first is why this one does not:

- **A part of a node can type narrower than the node.** The checker typed the original over its
  `dyn` operands; the arm of a ternary, or `default()`'s fallback, alone can type narrower.
  `{"x": 1}.filter(y, default(input.missing, y) || register.self.b)` reduced to `y || …` has `y` a
  string, which `||` refuses; an index is a free type parameter the next overload binds
  (`dyn(1) / register.self.l[false]` checks as `double`). This is why the rewrite only substitutes:
  every node of the original stays, and `default()` over a closed field keeps its own expansion,
  ternary included.
- **The error term's own type.** An empty map literal gets fresh type parameters, so a bare
  `{}.f` unifies with whatever stands beside it and can narrow an enclosing ternary. The term is
  `dyn({}).keeper_eval_error`.
- **cel-go prints what its parser does not read back.** `!(!x)` prints as `!!x`, and the parser drops
  an even run of `!` or `-` — for a non-bool `x`, a value where the original errs; `(-x).f` prints
  as `-x.f`, which parses as `-(x.f)`. The author's own text can hold either, so such an operand is
  grouped by `dyn(...)` before printing.
- **cel-go's checker is order-dependent.** Entering a comprehension's scope drops the filter on
  cross-type numeric comparisons, and it is not restored on exit, so `0 < 2.5` type-checks after
  the first comprehension the check enters and is refused before it (cel-go v0.29.2). A read the
  Keeper evaluates alone is evaluated inside a one-element comprehension, and a rendering that no
  longer type-checks because a substitution took a comprehension with it — from an index key, or
  from the field of a `has()`/`default()` — is sent inside one:
  `[0].map(v0, dyn("a") == register.self.v && size(register.self.l) >= 2.5)[0]`.

## 3. Printing, refusals and the fallback

**The rewrite only substitutes** (user, 2026-10-07). Each read of the host context outside the open
set — a root, a field of one, an index into one by a key that reads nothing open — becomes its
value on that host; nothing is simplified or dropped, so the Soul checks and evaluates the
author's tree node for node. `has(x)` over such a read becomes the bool it evaluates to (x is a
field, so it cannot become a value), and `default(x, y)` becomes its own expansion
`(true ? dyn(<x>) : <y>)` or `(false ? dyn({}).keeper_eval_error : <y>)` — for a root x,
`default(x, y)` is x itself, `dyn(<x>)`. Where x's operand errs, `has(x)` becomes the error term,
and so does the condition of `default()`'s expansion.

The printed result has its map keys sorted, so the same values give the same text. A task registered
as `self` is refused at compile, since it makes "everything but `register.self`" ambiguous. A bare
`register` (no field after it) is the open register and the decided ones as one map, so no text
reads one without the other; it is refused when rendered, without a rule of its own at compile — no
predicate in the corpus writes it (user, 2026-10-07). Inside a group the same applies to a child's
open `when:`. A `when:` reading `register.self` is refused at compile: it runs before the module,
so the task's own result does not exist there — on the Soul today that predicate fails on every
host.

| Refusal | Code | Where |
|---|---|---|
| `register: self` | `register_name_reserved` | parse, scenario and destiny |
| `when:` reads `register.self` | `when_register_self_unsupported` | parse, scenario and destiny |
| a value the text cannot carry, a bare `register`, or a rendering the Soul would not accept as sent | `predicate_not_renderable` | render |
| the rendering still reads the host context outside the open set | `predicate_residual_reference` | render — guards the rewrite itself |

A value the Keeper substitutes is printed as `dyn(<literal>)`; NaN and the infinities as
`dyn(double("NaN"))`, `dyn(double("+Inf"))`, `dyn(double("-Inf"))`; a timestamp as
`dyn(timestamp("<RFC 3339>"))` with its offset — refused where that text does not read back through
`timestamp()` to the same instant: an offset with seconds, an offset or a local year the parser
rejects, an instant outside the years 0001–9999 UTC; a duration as
`dyn(duration("<Go duration>"))`; a type by its name. A refusal never quotes the rendered text — only the author's predicate — since
the text can hold a substituted secret. Because nothing is simplified, a value travels to the host
even where the context alone decides the condition — `size(vars.password) > 8` carries the password
— as `flow_context` carries it today; deciding such parts on the Keeper is NIM-970.

The parse-time checks are textual like the rest of `shared/config` and read the dot form
`ExtractRegisterRefs` reads: a comprehension variable named `register` reads as the root there, an
escaped quote inside a string literal can make the literal's text read as code, and
`register .self` (a space before the dot) or a backtick-quoted field is not seen; the render resolves
names by scope.

**The fallback** is to carry the values as typed variables instead of text. That removes the type,
error-term and size traps — and would have spared the text-guard work in §5, now built, though not the masking: cel-go quotes a missing key whatever carried it — but costs the
readable form the user chose (`register.self.stdout != dyn("bob")`). If the differential test cannot be
passed with literals, S1 returns to the user with the failing cases; it does not switch on its own.

## 4. The open set

The open set is `register.self`, and inside a group also the group's own registers. A block rolled
as one group puts several tasks in one message, and a child's `when: register.<sibling>` is valid
there today: the within-block detector deliberately leaves flow control out
(`shared/config/passage.go:676-681`). For such a child the sibling's register stays open, and the
Soul evaluates against what it accumulates inside the message — as it does today, and as a destiny
does. Its `when:` is decided on the Keeper only when nothing in it is open.

## 5. What has to be built

The Keeper's two pieces are built for a run executing linearly (NIM-943), and the Soul side — text
guards, masking, async barriers and the announcement of the capability bit — by NIM-929.

- **Per-host transport.** The rendered predicate depends on that host's registers, so it differs per
  host. One `RenderedTask` serves every host it goes to (`keeper/internal/scenario/dispatch.go:892`),
  and since NIM-908 it carries per-host params: `ParamsBySID` (`keeper/internal/render/render.go:648`),
  picked for each host by `paramsForHost` (`keeper/internal/render/prototask.go:189`)
  ([ADR-draft per-host params dispatch](../adr/draft-per-host-params-dispatch.md)). Flow control is
  the half NIM-908 left refused: a host-variant flow context fails the render
  (`keeper/internal/render/pipeline.go:782-786`). Every predicate that travels needs the same kind
  of overlay as params — `changed_when:`, `failed_when:` and `until:`, and a grouped child's `when:` whose
  residual is still open; without it, host 1's predicate would silently go to every host. A path
  past render already exists: the requisite gate clones a task per host
  (`keeper/internal/scenario/crosspassage.go:158-163`), and each host's slice reaches the wire
  through `ToProtoTasksForHost(perHost[sid], sid)` (`keeper/internal/scenario/dispatch.go:503`). A `when:`
  the Keeper decides for a step needs none: it decides the host slice, which is per host already.
  Inside a group it does: a child skipped on one host and not another travels as `when: false` on
  the first only. *Built:* `RenderedTask.FlowBySID`, read by `ToProtoTasksForHost`.
- **The soulprint guard is lifted for a rendered predicate.** `guardFlowControlHostInvariant`
  (`keeper/internal/render/pipeline.go:1491-1507`, called at `:496`) refuses a `when:`,
  `changed_when:` or `failed_when:` naming `soulprint` on a multi-host target, because today the
  Soul would evaluate it against the first host's snapshot. With `soulprint.self` resolved per host
  on the Keeper, that reason is gone. So is the reason for the second layer NIM-908 kept,
  `flowContextHostInvariant` (`keeper/internal/render/pipeline.go:782-786`): with every `vars` and
  `input` reference substituted, no flow context travels for a scenario task to differ per host.
  *Built:* both are skipped in a linear scenario pass; a destiny's tasks keep them.
- **The Soul's text guards leave a substituted value alone — built (NIM-929).** They used to match
  `now(`, `vault(` and `generate_secret(` in the raw text, string literals included, and
  `normalize` collapsed whitespace with a literal regex that did not understand an escaped quote, so
  a value containing `vault(` failed the task and one containing `\"` could become a different
  literal. The guards now read the calls in the parse tree (`shared/cel/functions.go`, the same way
  the `__` guard does since NIM-965), and `normalize` only trims the edges, so the Soul compiles the
  text it receives. The Keeper's own check of a rendering against the Soul
  (`checkSoulAccepts`, `shared/cel/predicate_render.go`) runs the same code, so it no longer
  refuses either case. Carrying the values as bound variables instead is the fallback in §3, and
  only the user takes it.
- **Masking — built (NIM-929).** A substituted value can be a secret — from a sealed register, or a
  `vault()` var — and the predicate text used to be echoed into errors and logs at every evaluation
  site, and through every `%v` of an error: `ErrCompile`, `ErrEval` and `ErrUnsupported` each embed
  the expression, and cel-go's own message quotes the source on a compile error and the key on a
  lookup that misses (`no such key: <value>`). The Soul now names the predicate's kind, the task and the error's class
  (`cel.RedactedReason`), never the text (`soul/internal/runtime/applyrunner.go`). The error types
  are unchanged: soul-lint and the Keeper print what an author wrote, with its line address. The
  Soul's flow-control engine compiles without a cache (`cel.WithoutCompileCache`): a rendered
  predicate is unique per host and run, and a cache keyed by its text would grow with every run and
  hold every substituted secret for the life of the process.
- **The Soul's async barriers read a predicate's registers from its parse tree — built (NIM-929).**
  A predicate naming `register.<name>` waits for that task's async flow, and fail-stops there if
  the flow failed. The names used to come from `config.ExtractRegisterRefs`, whose quote-pairing
  regex read `register.web` inside `dyn("a\" register.web b")`; they now come from
  `Engine.RegisterNames`, so neither a substituted value nor a literal the regex misreads names a
  flow.

The guards and `normalize` sit on the shared compile path, so reading what the parser reads
changes every engine, not only the Soul's, and what an author wrote wherever they misread it: a
literal or comment holding `now(`, `vault(` or `generate_secret(` is no longer refused, and no text
is rewritten where collapsing its whitespace changed the parse — a literal after an escaped quote or
inside triple quotes (`'''it's  x'''`), a `//` comment and the line after it, a backtick name with
spaces. An older Soul keeps its own reading of those shapes, and for its async barriers the
regex's: a `register.<name>` its quote pairing misreads as code makes it wait ([known-limitations.md](../known-limitations.md#an-older-soul-reads-some-authored-predicates-as-before-nim-929)).

A capability bit, `rendered_predicate`, gates a rendered predicate: an older Soul compiles the
whitespace-collapsed text and echoes what it receives. The Soul announces it (NIM-929) and the
Keeper gates it (NIM-946). A decided `when:` is blanked and needs nothing. A grouped child's `when: false`
and a predicate in which no reference was resolved need no new bit (the ADR's item 10), only the
existing ones a predicate needs today — `CapabilityFlowControl`, and `CapabilityRetry` for `until:`
(`keeper/internal/scenario/soulcompat.go:168-184`, read off what the host is sent). That an older Soul reads a few shapes of an
author's own text as it always did is a known limitation, not the bit's concern.

Whether a value will be substituted depends on the text alone, so the Keeper knows it before the
run: `Engine.PredicateResolves` is the static half of `RenderPredicate`, held to its `Resolved`
on the whole differential corpus. A linear run is therefore refused **before its first step**
when a Soul that did not announce the bit is the target of a later step's rendered predicate —
the first pass surveys every task against the hosts its targeting resolves to before the run
(`keeper/internal/render/soul_needs.go`). Each step is gated again, after the requisite gate, on
what each host is sent; it refuses late only what the survey cannot know — a task whose targeting
reads a register or `incarnation.state` an earlier capture changes (directly or through `compute:`
or its `vars:`), a host or a task a refresh boundary or a dropped host brings in, a destiny task
behind a static `when:`, a conditional include or its own `on:`/`where:`, the `when: false` a
grouped child is sent where the Keeper skips it, and a task the survey could not judge. The refusal
is `soul_capability_unsupported` and names the host, the bit, and the step and task that need it:

```
host b.example.com: update the soul binary, it does not announce rendered_predicate (step 1 "check")
```
