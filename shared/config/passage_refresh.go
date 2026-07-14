package config

// roster-refresh passage boundary (ADR-0061 §S2, amends ADR-056).
//
// Why. The target ADR-0061 scenario is a single create run provision→onboarding→role:
// step `core.cloud.provisioned` (keeper) creates N VMs, step `core.soul.registered`
// (keeper) with `refresh_soulprint: true` registers and waits for their onboarding,
// and subsequent tasks apply the role to the ALREADY-onboarded hosts via the roster
// (`soulprint.hosts`, `on: [incarnation.name]`, `soulprint.self.*`). The run roster is
// resolved up-front before the first Passage and is stable WITHIN a Passage, but at a
// refresh boundary it is re-resolved into a fresh live snapshot of the online set
// (ADR-009 §7 in the current edition, relaxed by ADR-0061). For the re-resolve (S3) to
// take effect, consumers of the updated roster MUST land in a Passage STRICTLY AFTER
// the `refresh_soulprint` step — otherwise their render (targeting + soulprint.hosts)
// would see the OLD (pre-onboarding) roster.
//
// ★ BLOCKER (ADR-056 §risks, silent-wrong-target): without a passage boundary the
// redis-apply step would ride in the same Passage with the old (empty) roster → a
// destructive operation on the wrong host set, SILENTLY. So `refresh_soulprint: true`
// is a new class of PASSAGE-DEFINING "roster-refreshed" signal, symmetric with the
// probe emitter `register: X` (a signal only — a roster axis, not a register axis).
//
// Mechanism. The refresh emitter is a `core.soul.registered` task with
// `refresh_soulprint: true` (literal) in params. A refresh consumer is a task that
// statically reads the run roster:
//
//   - `on: [incarnation.name]` (literal or `${ incarnation.name }`) — targeting by the
//     root Coven label = the whole incarnation; resolved Keeper-side from the roster (Hosts);
//   - an omitted `on:` (= the whole incarnation, orchestration.md §3) — also roster targeting;
//   - `soulprint.hosts` / `soulprint.where(...)` — the list of run hosts;
//   - `soulprint.self.*` — a host-varying fact (depends on which hosts are in the roster).
//
// Any refresh consumer after the refresh emitter (program-order) rides in a Passage
// ≥ 1 + the emitter's passage. The boundary is active ONLY when a refresh emitter is
// present in the plan; without one it is zero-cost, and the register-dependency graph
// and Count are BIT-FOR-BIT as before ADR-0061 (the N=1 fast-path is preserved).
//
// Over-approximation on the safe side: roster reads are recognized conservatively (an
// omitted `on:` also counts). An extra Passage is safe; a missed one =
// silent-wrong-target — so when in doubt we split. Not a register graph: the refresh
// boundary does NOT add register references, so the reads⊆refs invariant of ADR-056 is
// untouched (refresh is a separate axis).

// RefreshBoundaries returns, for EACH Passage P (0..passage.Count-1), a flag "before
// rendering Passage P the scenario-runner must RE-resolve the roster" (S3, ADR-0061).
// A boundary stands before Passage P if in Passage P-1 at least one successful refresh
// emitter (`core.soul.registered` with `refresh_soulprint: true`) completed — its
// barrier converged → onboarded hosts are written into souls+coven → the live roster
// snapshot changed → consumers of Passage P (stratified by S2 strictly AFTER the
// refresh step) must see the current set.
//
// Re-resolve semantics — a FRESH LIVE SNAPSHOT of the incarnation roster at the
// boundary (run.go: resolveRoster → LoadIncarnationHosts → filterAlive): reflects the
// CURRENT online set. It grows as provisioned hosts onboard, but this is NOT a
// monotonic operation — a host that went offline by the boundary is excluded from the
// snapshot (targeting hits the actually-online set).
//
// out[0] is always false (before the first Passage the roster is already resolved
// up-front). len(out) == passage.Count. If there are no refresh emitters — all false
// (no re-resolve needed, behavior BIT-FOR-BIT as before ADR-0061). N=1 → []bool{false}.
//
// Binding to P-1 (not "to any Passage < P"): the Passage P-1 barrier is the nearest
// point where this Passage's refresh emitter is guaranteed to have completed, so ONE
// re-resolve at the boundary suffices; several refresh emitters in different Passages
// give several boundaries (one per Passage-after-each). passage is the result of
// [Stratify] of the same tasks.
func RefreshBoundaries(tasks []Task, passage Passage) []bool {
	out := make([]bool, passage.Count)
	if passage.Count <= 1 || len(passage.TaskPassage) != len(tasks) {
		return out // single Passage / desync — no boundaries.
	}
	for i := range tasks {
		if !taskIsRefreshEmitter(&tasks[i]) {
			continue
		}
		// refresh emitter in Passage E → re-resolve before Passage E+1.
		if next := passage.TaskPassage[i] + 1; next < passage.Count {
			out[next] = true
		}
	}
	return out
}

// refreshModuleAddr — the only module carrying `refresh_soulprint` (ADR-0061:
// capability 2 lives on the keeper-side core `core.soul.registered`, not a separate
// entity). The author-form task address is base+state.
const refreshModuleAddr = "core.soul.registered"

// HasRefreshEmitter — the plan contains at least one refresh emitter (a
// `core.soul.registered` task with `refresh_soulprint: true`, recursively via block:).
//
// Why separate from [RefreshBoundaries]: the predicate "the plan provisions the roster
// mid-run" is needed BEFORE stratification — for the no_hosts gate in run.go (ADR-0061
// amendment): a run with a refresh emitter legitimately starts on an EMPTY roster even
// if it carries host deploy tasks (they are stratified into a Passage AFTER the refresh
// boundary and see the re-resolved live snapshot). RefreshBoundaries answers a
// different question — "before which Passage to re-resolve" — and requires an
// already-computed [Passage]; here it's a pure check for an emitter's presence in the
// flat task plan.
//
// Pure function, no I/O. Without an emitter — false, no_hosts behavior BIT-FOR-BIT.
func HasRefreshEmitter(tasks []Task) bool {
	for i := range tasks {
		if taskHasRefreshEmitter(&tasks[i]) {
			return true
		}
	}
	return false
}

// taskHasRefreshEmitter — the task (or any of its block: children) is a refresh
// emitter. Block recursively: block is an atomic Passage unit, an emitter inside it
// also provisions the run roster (over-approximation on the safe side, symmetric with
// taskReadsRoster).
func taskHasRefreshEmitter(t *Task) bool {
	if taskIsRefreshEmitter(t) {
		return true
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			if taskHasRefreshEmitter(&t.Block.Block[i]) {
				return true
			}
		}
	}
	return false
}

// taskIsRefreshEmitter — the task emits the "roster-refreshed" signal: it's a
// `core.soul.registered` with params.refresh_soulprint == true (literal bool).
//
// Only a literal true. A `${ … }` expression in refresh_soulprint is statically
// undeterminable (ADR-010: a ${…} value isn't typed), so it does NOT count as an
// emitter — acceptable: refresh_soulprint is always written as a literal true (a
// static behavior flag, not data). false / absence → not an emitter.
func taskIsRefreshEmitter(t *Task) bool {
	if t.Module == nil || t.Module.Module != refreshModuleAddr {
		return false
	}
	v, ok := t.Module.Params["refresh_soulprint"]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return isBool && b
}

// taskReadsRoster — the task statically reads the run roster (see doc above):
// on:[incarnation.name] / omitted on: / soulprint.hosts / soulprint.self.*.
// Recursively via block: (block is an atomic Passage unit; a roster read by any child
// makes the container a refresh consumer).
//
// Keeper-side tasks (`on: keeper`) do NOT read the roster (they have no run hosts —
// keeperVars without soulprint, render_host.go), so they are excluded: the refresh
// emitter is itself `on: keeper` and must NOT depend on the refresh boundary recursively.
func taskReadsRoster(t *Task) bool {
	if onTargetsRoster(t.On) {
		return true
	}
	// soulprint.* (hosts/where/self) in any keeper-rendered CEL field of the task.
	if exprReadsSoulprint(t.Where) {
		return true
	}
	if t.Loop != nil && (exprReadsSoulprint(t.Loop.When) || valueReadsSoulprint(t.Loop.Items)) {
		return true
	}
	if mapReadsSoulprint(t.Vars) || mapReadsSoulprint(t.Output) {
		return true
	}
	if t.Module != nil && mapReadsSoulprint(t.Module.Params) {
		return true
	}
	if t.Apply != nil && mapReadsSoulprint(t.Apply.Input) {
		return true
	}
	if t.Assert != nil {
		for _, that := range t.Assert.That {
			if exprReadsSoulprint(that) {
				return true
			}
		}
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			if taskReadsRoster(&t.Block.Block[i]) {
				return true
			}
		}
	}
	return false
}

// onTargetsRoster — `on:` targets the whole incarnation roster:
//   - nil (omitted on:) → the whole incarnation (orchestration.md §3);
//   - `on: keeper` (string) → NOT the roster (keeper-side, no hosts);
//   - a list containing the root Coven label `incarnation.name` (literal or
//     `${ incarnation.name }`) → the whole incarnation (rosterSQL `$1 = ANY(coven)`).
//
// Other coven labels (a sub-coven like `redis`/`prod`) do NOT count as a roster read:
// they target a SUBset, and although a grown roster could add hosts to it, the refresh
// emitter always tags new SIDs with exactly `incarnation.name` (ADR-0061:
// `coven: ["${ incarnation.name }"]`). For the target scenario the root label is the
// canonical way to address a grown roster. (Sub-coven targeting of new hosts within one
// run is outside S2/S3; extended separately if needed.)
func onTargetsRoster(on any) bool {
	switch v := on.(type) {
	case nil:
		return true // omitted on: = the whole incarnation.
	case string:
		return false // `on: keeper` — the only valid string form, not the roster.
	case []any:
		for _, raw := range v {
			s, ok := raw.(string)
			if !ok {
				continue
			}
			if labelIsIncarnationName(s) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// labelIsIncarnationName — the coven label refers to the root `incarnation.name`:
// either the literal `incarnation.name` (rare, but allowed) or the CEL wrapper
// `${ incarnation.name }` / `${incarnation.name}`. Recognized textually: exact CEL
// parsing isn't needed — the root label form is fixed by the grammar.
func labelIsIncarnationName(s string) bool {
	if !isCELWrapped(s) {
		return false
	}
	// The inside of ${ … } must be exactly `incarnation.name` (with possible
	// whitespace). More complex content (e.g. `${ incarnation.name + "-x" }`) does not
	// count here: that's already a sub-coven, not the root label.
	inner := s[2 : len(s)-1]
	return trimSpace(inner) == "incarnation.name"
}

// trimSpace — a narrow trim of ASCII spaces/tabs at the edges (no unicode
// dependencies, CEL tokens are ASCII). A local helper, to avoid pulling in strings for
// one spot.
func trimSpace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	j := len(s)
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

// exprReadsSoulprint — the CEL string references soulprint.* (hosts/where/self/...).
// Reuses the existing canonical parser reSoulprintRef (`\bsoulprint\b`), a mirror of
// keeper render.reFlowControlSoulprint — a single source of truth for the
// "host-varying/roster predicate" grammar. Any soulprint access = a roster read:
// soulprint.hosts/where is the list of run hosts, soulprint.self is a host-varying fact
// (both depend on the roster composition).
func exprReadsSoulprint(expr string) bool {
	if expr == "" {
		return false
	}
	// Strip CEL string literals so that `'soulprint'` inside data doesn't cause a false
	// positive (like extractSoulprintRefs/ExtractRegisterRefs).
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	return reSoulprintRef.MatchString(stripped)
}

// mapReadsSoulprint — any string value of a map (vars/params/apply.input/output),
// recursively over nested map/seq, reads soulprint.* in `${ … }` interpolation.
func mapReadsSoulprint(m map[string]any) bool {
	for _, v := range m {
		if valueReadsSoulprint(v) {
			return true
		}
	}
	return false
}

// valueReadsSoulprint recursively traverses an any value (string / map / seq).
func valueReadsSoulprint(v any) bool {
	switch t := v.(type) {
	case string:
		return exprReadsSoulprint(t)
	case map[string]any:
		for _, sub := range t {
			if valueReadsSoulprint(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if valueReadsSoulprint(sub) {
				return true
			}
		}
	}
	return false
}
