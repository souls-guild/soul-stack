package render

import (
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// seal / sealed-paths ([ADR-010] §7.4) — render-time provenance/taint. The
// pipeline marks a params cell path SEALED when its RAW (pre vault-resolve+CEL)
// value is a string with a `${ … }` expression reading a secret source: a
// secret input of the active pass schema, vault(), or transitively sealed
// vars/compute. Detection is AST-based, via [cel.Engine.DetectSealed]
// (shared/cel/seal.go).
//
// SealedSet accumulates the found paths (dot/idx form, matching renderValue
// path EXACTLY) for a SINGLE Render pass. The caller (scenario.run) creates it,
// puts it in [RenderInput.Sealed], and after Render uses the paths for
// seal-aware masking (audit.MaskSecretsSealed) at write points
// (error_summary/status_details). nil → collection disabled (push/trial/Acolyte
// have no seal need).
type SealedSet struct {
	paths map[string]bool
}

// NewSealedSet — an empty accumulator of sealed paths for one Render pass.
func NewSealedSet() *SealedSet { return &SealedSet{paths: map[string]bool{}} }

// add marks a path sealed. nil receiver — no-op (collection disabled).
func (s *SealedSet) add(path string) {
	if s == nil {
		return
	}
	s.paths[path] = true
}

// Paths returns the sealed-path set for audit.MaskSecretsSealed (the map is
// copied — the caller can't mutate internal state). nil receiver → nil.
func (s *SealedSet) Paths() map[string]bool {
	if s == nil || len(s.paths) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.paths))
	for k := range s.paths {
		out[k] = true
	}
	return out
}

// scenarioSealSources builds the PASS-LEVEL [cel.SealSources]: the secret-input
// set of the active schema (the scenario's on a scenario pass, the destiny's own
// on a destiny pass), the registers whose payload carried a declared secret
// ([ADR-0083] §6, derived by [Pipeline.resolveRegisterSecrets] before any root is
// built), the run's sealed `compute:` names and the pass's sealed file-var names.
//
// A task's own `vars:` are NOT here — they are stacked on top per task by
// [Pipeline.taskSealSources], because that layer is the only one of the four that
// is not pass-wide. Every caller that walks a task's params must use that one.
//
// nil schema → empty set (the detector then only catches vault()).
func scenarioSealSources(in RenderInput) cel.SealSources {
	fields := map[string]bool{}
	addFieldAddrs(fields, "input", secretInputNames(in.Scenario))
	addFieldAddrs(fields, "register", in.sealedRegisters)
	addFieldAddrs(fields, "compute", in.sealedCompute)
	addFieldAddrs(fields, "vars", in.sealedFileVars)
	return cel.SealSources{Fields: fields, Roots: in.sealedLoopBinds}
}

// addFieldAddrs writes the `<root>.<name>` address of every name in the set.
// Fields is always freshly allocated by [scenarioSealSources], so the callers
// below may add to it in place without reaching a sibling task's sources.
func addFieldAddrs(fields map[string]bool, root string, names map[string]bool) {
	for name := range names {
		fields[cel.FieldAddr(root, name)] = true
	}
}

// taskSealSources is [scenarioSealSources] plus the taint of THIS task's own
// `vars:` layer — the sources [collectSealed] must walk a task's params with.
//
// ★ The vars taint is read from the RAW `vars:` text and never from the resolved
// per-host value, and the difference is load-bearing. Task vars resolve once per
// task PER HOST (resolveTaskVars), while collectSealed deliberately runs once per
// task, because a cell's provenance is host-invariant. Deriving the mark from a
// resolved value would make the seal depend on a host that the walk it feeds does
// not — the same cell sealed on one host and not on the next, from one collection
// that only ran once.
//
// The layer BELOW is already in scenarioSealSources (a destiny's `vars.yml`, via
// in.sealedFileVars), and the service layer under that one cannot read a secret
// at all — see [RenderInput.sealedFileVars].
func (p *Pipeline) taskSealSources(in RenderInput, task config.Task) cel.SealSources {
	src := scenarioSealSources(in)
	addFieldAddrs(src.Fields, "vars", sealedVarNames(p.cel, task.Vars, src))
	return src
}

// sealedVarNames returns the names of one `vars:` layer whose own value reads a
// secret source. It returns THIS layer's names only; stacking them onto the
// layer below is the caller's move ([Pipeline.taskSealSources] merges them into
// the addresses [scenarioSealSources] already built).
//
// Transitivity WITHIN the layer is why this iterates instead of walking once:
// `a: "${ input.pw }"` beside `b: "${ vars.a }-suffix"` seals both, and
// resolveVarLayer resolves such a layer in topological order rather than map
// order. A fixpoint reaches the same set without a second spelling of that sort
// — a layer is units to tens of names, and each round either marks a new name or
// is the last one.
//
// Only string values participate, matching resolveVarLayer exactly: a `${ … }`
// nested inside a map- or list-valued var is not resolved there either, it
// survives as its own text (docs/destiny/vars.md, "Valid value types").
//
// ★ The layer below is carried down WHOLE, shadowed names included, and the
// reason is the ones it does NOT shadow: a destiny `vars.yml` local that no task
// var redeclares is still `vars.<x>` in that task's params, and its mark exists
// nowhere but the lower layer. Handing the whole set to the collector is what
// puts it in front of the walk.
//
// For a name this layer does shadow, that over-seals — the task layer's value is
// the one `vars.<x>` resolves to. Left that way deliberately rather than
// subtracted: the shadow cannot carry the lower value today (resolveTaskVars
// hands resolveVarLayer the SERVICE layer alone as `lower`, so a shadow-and-derive
// `pw: "${ vars.pw }-suffix"` either derives from a layer with no secret source in
// it, or is ErrVarCycle when `pw` exists only as a file var), and the seal should
// not be the thing that has to notice if that ever relaxes.
func sealedVarNames(engine *cel.Engine, raw map[string]any, base cel.SealSources) map[string]bool {
	out := map[string]bool{}
	if len(raw) == 0 {
		return out
	}
	// A scratch copy of the base addresses that GROWS as names are marked: a var
	// marked in one round is a secret source for the next, which is the transitive
	// step. Copied rather than shared so a layer's own marks do not leak back into
	// the caller's set before it decides to merge them.
	src := cel.SealSources{Fields: make(map[string]bool, len(base.Fields)+len(raw)), Roots: base.Roots}
	for addr := range base.Fields {
		src.Fields[addr] = true
	}
	for {
		grew := false
		for name, val := range raw {
			s, ok := val.(string)
			if !ok || out[name] {
				continue
			}
			if engine.DetectSealed(s, src) {
				out[name] = true
				src.Fields[cel.FieldAddr("vars", name)] = true
				grew = true
			}
		}
		if !grew {
			return out
		}
	}
}

// sealedComputeNames returns the `compute:` names whose expression reads a secret
// source. [Pipeline.resolveCompute] evaluates the block ONCE per run in
// declaration order and entry i may read entries j<i as `compute.<name>`, so the
// taint accumulates in that same order and needs no fixpoint.
//
// It reads the RAW block, not the resolved values, which is what keeps it correct
// on a staged pass: resolveCompute returns an already-computed in.Compute
// untouched, so a taint derived from the resolve would be derived only on the
// pass that happened to do the work.
//
// The `vars.*` a compute expression can reach is the service layer alone
// (resolveCompute's base), which carries no secret — see
// [RenderInput.sealedFileVars]. `register.*` it can reach, and that is in base.
func sealedComputeNames(engine *cel.Engine, in RenderInput) map[string]bool {
	block := in.Scenario.Compute
	if len(block) == 0 {
		return nil
	}
	out := map[string]bool{}
	fields := map[string]bool{}
	addFieldAddrs(fields, "input", secretInputNames(in.Scenario))
	addFieldAddrs(fields, "register", in.sealedRegisters)
	src := cel.SealSources{Fields: fields}
	for _, cv := range block {
		s, ok := cv.Value.(string)
		if !ok {
			continue // literal — resolveCompute passes it through unevaluated
		}
		if engine.DetectSealed(s, src) {
			out[cv.Name] = true
			// Entry i reads entries j<i as `compute.<name>`, exactly as
			// resolveCompute evaluates them, so the taint accumulates in the
			// same declaration order the values do.
			fields[cel.FieldAddr("compute", cv.Name)] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sealedLoopItems reports whether a `loop:` binds its variable out of a secret,
// by asking the RAW `items:` expression rather than the resolved element
// (NIM-822, NIM-823).
//
// ★ The items expression, not the element, and the difference is a decision.
// `items:` resolves once per task (loopInvariantVars carries no soulprint.self),
// while an element is per iteration and `items:` may yield a heterogeneous list.
// Deciding from the expression keeps the taint iteration-invariant, matching a
// collection that runs once per task — at the cost of tainting every iteration
// when only some elements came from a secret. That over-seals in the same safe
// direction the rest of this file does, and the alternative — a per-iteration
// taint — would make one task's params sealed on iteration 3 and not on 4.
//
// Items is `any`: a string expression, or a YAML literal list/map whose cells may
// each be one. Every string in it is asked, because any of them can be what put
// the secret into the element.
func sealedLoopItems(engine *cel.Engine, items any, src cel.SealSources) bool {
	switch t := items.(type) {
	case string:
		return engine.DetectSealed(t, src)
	case []any:
		for _, v := range t {
			if sealedLoopItems(engine, v, src) {
				return true
			}
		}
	case map[string]any:
		for _, v := range t {
			if sealedLoopItems(engine, v, src) {
				return true
			}
		}
	}
	return false
}

// withSealedLoopBind returns a copy of in whose `loop:` bind name is tainted
// WHOLE for the tasks rendered out of that loop. The caller holds RenderInput by
// value, so the mark reaches this loop's iterations and no sibling task.
//
// `index_as:` is deliberately NOT marked: it binds an integer position, never a
// value out of items, and sealing it would mask every `${ i }` in the run for
// nothing.
func withSealedLoopBind(in RenderInput, asName string) RenderInput {
	binds := make(map[string]bool, len(in.sealedLoopBinds)+1)
	for name := range in.sealedLoopBinds {
		binds[name] = true
	}
	binds[asName] = true
	in.sealedLoopBinds = binds
	return in
}

// secretInputNames — names of input parameters declared secret:true in the
// pass schema (scenario.Input / destiny.Input — both config.InputSchemaMap),
// plus fields with a non-empty vault_scope (by config-validator contract that's
// only applicable to secret:true, but we check explicitly — defense in depth,
// a secret's name shouldn't depend on another validator's invariant). nil →
// empty set.
func secretInputNames(scn *config.ScenarioManifest) map[string]bool {
	if scn == nil || len(scn.Input) == 0 {
		return nil
	}
	out := make(map[string]bool, len(scn.Input))
	for name, s := range scn.Input {
		if s != nil && (s.Secret || s.VaultScope != "") {
			out[name] = true
		}
	}
	return out
}

// renderContextInputPrefix — the cell-path prefix for
// render_context.input.<field> (Variant B, ADR-010 §7.4 S-1). render_context
// lives in params under the render_context key (paramRenderContext), input is
// a subsection of the §3.2 root; the cell path of a given input field for
// seal masking is render_context.input.<field>.
const renderContextInputPrefix = paramRenderContext + ".input."

// sealRenderContextInput marks sealed paths render_context.input.<secret> for
// every secret-input of the active pass schema (ADR-010 §7.4, mechanism S-1,
// Variant B). Closes the seal gap left by dropping the `params.vars`
// passthrough: a raw `${ input.secret }` no longer appears in params →
// collectSealed/DetectSealed can't catch it, so provenance is restored
// DECLARATIVELY — BY SCHEMA (the list of secret names), not by expression
// presence.
//
// ★CONDITIONAL (injectInput): the caller must call this ONLY when
// render_context.input is actually injected (the same gate as
// buildRenderContext). Otherwise the secret never lands in params, and its
// seal paths would just produce dead entries for a nonexistent cell — the gate
// keeps the seal set in sync with the real render_context contents.
//
// The list's source is secretInputNames(in.Scenario), which on a destiny pass is
// the DESTINY's own `input:` schema (renderApplyDestiny carries it onto the
// synthetic manifest, NIM-812) — so a `.input.<secret>` read by a destiny's own
// template is marked here the same way a scenario's is. set nil → no-op. Called
// once per task (the path is host-invariant).
func sealRenderContextInput(set *SealedSet, in RenderInput) {
	if set == nil {
		return
	}
	for name := range secretInputNames(in.Scenario) {
		set.add(renderContextInputPrefix + name)
	}
}

// renderContextVarsPrefix — the cell-path prefix for render_context.vars.<name>,
// the sibling of [renderContextInputPrefix].
const renderContextVarsPrefix = paramRenderContext + "." + paramVars + "."

// sealRenderContextVars marks render_context.vars.<name> for every `vars.*` name
// this task's sources hold sealed.
//
// ★ A DECLARATIVE mark is needed here for the same reason as S-1 on
// render_context.input: `core.file.rendered` RELOCATES the cell. renderTaskIter
// pulls params.vars out and deletes the key, so a path collectSealed recorded as
// `vars.pw` off the RAW params matches nothing in what is actually written; and a
// file-var injected from fileVarsForHost was never in raw params to be walked at
// all. Both land under render_context.vars, resolved — which is a params cell
// like any other, written to apply_run_plan.
//
// Names whose var is not injected (referencedFileVars filters the file layer by
// what the template's AST names) produce a dead entry, which is harmless and
// deliberate: predicting that filter here would be a second spelling of it, and
// the two would drift the way a seal path and a masker path do.
func sealRenderContextVars(set *SealedSet, sealedVars map[string]bool) {
	if set == nil {
		return
	}
	for name := range sealedVars {
		set.add(renderContextVarsPrefix + name)
	}
}

// sealedVarNamesOf reads the `vars.<name>` names back out of a built source set,
// so the caller marking render_context does not recompute a taint the params walk
// already derived. The prefix comes from [cel.FieldAddr] rather than a literal —
// same single-spelling rule the addresses themselves follow.
func sealedVarNamesOf(src cel.SealSources) map[string]bool {
	prefix := cel.FieldAddr("vars", "")
	out := map[string]bool{}
	for addr := range src.Fields {
		if name, ok := strings.CutPrefix(addr, prefix); ok {
			out[name] = true
		}
	}
	return out
}

// collectSealed walks RAW params (pre vault-resolve+CEL) with the same path
// walk as renderValue, and marks in set the path of every string cell whose
// `${ … }` expression reads a secret source (engine.DetectSealed). set nil →
// no-op. Called once per task (not per host): a cell's provenance is
// host-invariant (the secret source is the same on every host).
func collectSealed(engine *cel.Engine, set *SealedSet, params map[string]any, sources cel.SealSources, base string) {
	if set == nil {
		return
	}
	walkSealed(engine, set, params, sources, base)
}

// SealedValues returns the RESOLVED values a task's params carry at sealed
// paths — the secrets that actually travelled, for a caller who has to keep
// them out of a free-text channel that masking-by-path cannot reach.
//
// [audit.MaskSecretsSealed] answers "what does this payload look like with the
// sealed cells removed" and needs the payload to BE the params tree. A module's
// failure message is not that tree: it is one string that may quote a value out
// of it. The only thing that helps there is knowing the values, which is what
// this returns.
//
// It lives beside [collectSealed] deliberately and walks the same way: the path
// spelling is the whole contract between the two, and a second spelling of it
// elsewhere is how a masker comes to miss exactly the cell the seal marked.
// Sealed paths are collected from the RAW params and looked up here against the
// RENDERED ones — the same cell, before and after the value arrived.
//
// ★ A sealed path is sealed WITH EVERYTHING UNDER IT, and that is not a
// convenience — it is the common case. The seal is collected from the RAW params,
// where a cell is one string; the value that arrives can be a whole subtree. A
// bare `vault:<mount>/<path>` with no `#field` resolves to the entire KV map
// ([readVaultRef]), and a whole-cell `${ … }` yields a native list or map under
// [ADR-010]'s non-string rule. Recording only the string AT the sealed path would
// therefore find nothing in exactly the shape this exists for — a credentials map
// handed to a keeper-side plugin — and mask nothing at all.
//
// ⚠ This is NOT parity with [audit.MaskSecretsSealed], and the gap is real rather
// than a rounding: that masker replaces the cell at a sealed path whatever its
// TYPE, while this one can only return strings. A numeric secret under a sealed
// path is therefore `***MASKED***` in a payload and legible in a message.
// Widening the type switch is not the fix — `87654321` is a plausible substring
// of a byte count or a timestamp, so masking a number out of free text needs a
// decision about text masking rather than one more case arm here.
//
// Empty strings are dropped: redacting "" would blank every position in the
// message. The result is sorted and deduplicated — it feeds an observable
// channel, which must be byte-identical for identical input.
//
// ★ The cost of the subtree rule, stated because it is paid on every message.
// Vault hands back a whole KV map, so a sealed `creds` cell contributes its
// NON-SECRET siblings as well — `user`, `host`, `port`. Those are then redacted
// from every keeper task's message in the run, core modules included, because the
// seal set is RUN-wide and is matched against each task's own params root. An
// operator reading `await timeout for ***` instead of a hostname is the price of
// not leaking the password that sat beside it, and a short leaf (`svc`, `6379`)
// collides with ordinary words. It is the right direction to err in; it is not
// free.
func SealedValues(params map[string]any, sealed map[string]bool) []string {
	if len(params) == 0 || len(sealed) == 0 {
		return nil
	}
	found := map[string]bool{}
	walkSealedValues(params, sealed, "", false, found)
	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for v := range found {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// walkSealedValues carries inSealed down: once a node's path is sealed, every
// string leaf beneath it is a secret, whether or not its own deeper path was
// ever recorded in the set — a sealed subtree has no unsealed interior.
func walkSealedValues(v any, sealed map[string]bool, path string, inSealed bool, found map[string]bool) {
	inSealed = inSealed || sealed[path]
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			walkSealedValues(val, sealed, joinKey(path, k), inSealed, found)
		}
	case []any:
		for i, val := range t {
			walkSealedValues(val, sealed, joinIdx(path, i), inSealed, found)
		}
	case string:
		if t != "" && inSealed {
			found[t] = true
		}
	}
}

func walkSealed(engine *cel.Engine, set *SealedSet, v any, sources cel.SealSources, path string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			walkSealed(engine, set, val, sources, joinKey(path, k))
		}
	case []any:
		for i, val := range t {
			walkSealed(engine, set, val, sources, joinIdx(path, i))
		}
	case string:
		// A literal `vault:<mount>/<path>` cell is replaced by the secret it
		// names in the vault-resolve phase — walkVaultValue keys off the same
		// prefix — so its provenance is known here without an expression to
		// inspect (DetectSealed reads `${ … }` segments and a bare ref has
		// none). Sealing it keeps the declarative layer of
		// audit.MaskSecretsSealed ahead of the regex last resort, which would
		// mask such a cell only when its KEY looked sensitive and would raise a
		// declarative-gap alarm every time it did.
		if strings.HasPrefix(t, vaultRefPrefix) {
			set.add(path)
			return
		}
		if engine.DetectSealed(t, sources) {
			set.add(path)
		}
	}
}
