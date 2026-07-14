package config

// Passage stratification ([ADR-056](../../docs/adr/0056-staged-render-passage.md)).
// A PURE function over a run's task plan ([]Task after ExpandIncludes): computes
// each task's 0-based Passage index from the cross-task register-dependency graph
// and validates that graph (cycle / dangling reference). It executes and renders
// nothing — the stage-loop (render→dispatch→barrier→repeat) lives keeper-side.
//
// It lives in shared/config (not keeper-internal) because the same register-
// dependency graph must be built by (a) the keeper runtime before dispatch, (b)
// keeper tests, (c) OFFLINE soul-lint BEFORE apply. Duplicated logic between them
// risks silent-wrong-target (graph divergence). keeper/internal/render keeps thin
// aliases (Stratify/Passage/codes) onto these symbols.
//
// Why (ADR-056, §"Risks — silent-wrong-target"): a task reading `register.X` in any
// passage-defining register source MUST run strictly AFTER the probe step emitting
// `register: X` (probe and its consumer cannot share a Passage) — else `where:`
// selects hosts on an empty/stale register and a destructive operation SILENTLY hits
// the wrong hosts.
//
// Canonical registry of passage-defining register sources of a Task (ADR-056):
//
//	where · vars · params · apply.input · output · loop.items · loop.when · block (recursion).
//
// All resolve Keeper-side BEFORE dispatch, so they must see the previous Passage's
// register → they define Passage.
//
// roster-refresh boundary (ADR-0061 §S2, a SEPARATE AXIS from register): a
// `core.soul.registered` task with `refresh_soulprint: true` is a passage-defining
// "roster-refreshed" emitter; a roster consumer (on:[incarnation.name] /
// soulprint.hosts / soulprint.self.* / omitted on:) after it moves to the next
// Passage. This is not the register graph (refresh introduces no register refs →
// reads⊆refs is untouched); logic lives in passage_refresh.go, the edge is wired into
// visit() below.
//
// requisites (`onchanges`/`onfail`/`require`) are NOT passage-defining (addressed
// references, not interpolation). Flow-control CEL `when`/`changed_when`/
// `failed_when`/`retry.until` also does not define passage (ADR-056:85) — it is
// Soul-side per-task gating ([ADR-012(d)], evaluated within one ApplyRequest from its
// own register). They are excluded from collectTaskReads: otherwise a register-
// dependent `when` would split a probe and its same-passage consumer across Passages,
// where the Soul cannot see cross-passage register → `no such key` (FC-5). Genuinely
// cross-passage `when` (probe in an earlier Passage for ANOTHER reason) is
// UNSUPPORTED, caught by the separate CrossPassageWhenGating detector (fail-closed,
// symmetric to within-block).
//
// Maintenance invariant: a new keeper-rendered, passage-defining, register-reading
// Task field must appear in lockstep in collectTaskReads (here) AND collectRefs
// (cross-ref validator task_refs.go) AND the ADR-056 registry. Flow-control
// (`when`/`changed_when`/`failed_when`) is a DELIBERATE asymmetry: ∈ collectRefs
// (refs check register existence) but ∉ collectTaskReads (does not define Passage).
// Guard-test reads ⊆ refs (and for flow-control reads ⊊ refs).

import (
	"fmt"
	"sort"
)

// Passage is a run's stratification plan: each top-level task's 0-based passage
// index plus the total Passage count. Index refers to the position in the plan
// (Tasks after ExpandIncludes), matching RenderedTask.Index.
//
// TaskPassage[i] is task i's passage. Count = max(TaskPassage)+1 (>=1). Count==1 is
// the fast-path: no cross-task register dependency, all tasks in passage 0,
// behaviour identical to up-front render (backward-compat).
type Passage struct {
	TaskPassage []int
	Count       int
}

// StratifyError is a register-graph stratification error. It carries a code (for the
// caller: keeper run.go → render_failed; soul-lint → offline diagnostic) and a
// human-readable message. The author's dependency graph is invalid (cycle /
// reference to a nonexistent register) and must stop the run EXPLICITLY, not silently
// (symmetric to unknown_register_reference in the config validator and to silent-
// wrong-target).
type StratifyError struct {
	Code string
	Msg  string
}

func (e *StratifyError) Error() string { return e.Msg }

// StratifyError codes.
const (
	// StratifyCycle — a circular register dependency (probe A reads register.B,
	// probe B reads register.A): no topological order exists.
	StratifyCycle = "register_dependency_cycle"
	// StratifyUnknownRegister — a task reads register.X but NO task in the plan
	// emits `register: X`. Duplicates the config cross-ref validator (a safeguard:
	// stratification must fail explicitly rather than silently stratify on an
	// incomplete graph — silent-wrong-target).
	StratifyUnknownRegister = "unknown_register_reference"
	// CodeWithinBlockRegisterDependency — a block child reads a register emitted by
	// a SIBLING child of the SAME block. A block is atomic per Passage (its whole
	// fan-out in one Passage, ADR-056), peer-register is available only Soul-side
	// AFTER the probe, but where/when/params resolve Keeper-side BEFORE dispatch →
	// where selects hosts on a stale/external register silently (silent-wrong-
	// target). Fail-closed reject offline (soul-lint) and as a runtime safeguard
	// (run.go), not a silent misfire. Fixed by lifting the probe to top-level (probe
	// and consumer in different Passages; Stratify then orders them normally). The
	// code name is kept distinct from the detector function
	// WithinBlockRegisterDependency (a same-named const+func in one package is
	// illegal in Go).
	CodeWithinBlockRegisterDependency = "within_block_register_dependency"
	// CodeCrossPassageWhenGating — a task gates `when:`/`changed_when:`/
	// `failed_when:` on a register emitted in an EARLIER Passage (ADR-056:85, FC-5).
	// Flow-control is Soul-side per-task gating ([ADR-012(d)]) and sees only its own
	// Passage's register; cross-passage register is unavailable to it (a different
	// ApplyRequest) → `no such key` silently. After the narrow-fix flow-control does
	// not split the Passage itself, but the probe may have moved to an earlier
	// Passage for ANOTHER reason (another task with `where: register.X`). Fail-closed
	// reject (offline soul-lint + runtime keeper safeguard), symmetric to
	// CodeWithinBlockRegisterDependency. Fixed by where: (cross-task targeting) or
	// register.self (same-task gating).
	CodeCrossPassageWhenGating = "cross_passage_when_unsupported"
)

// Stratify computes passage indices for a run's task plan from the cross-task
// register-dependency graph. Returns [Passage] or *[StratifyError] (cycle / dangling
// reference). tasks is the flat top-level task list (after ExpandIncludes);
// nil/empty → Passage{Count: 1}.
//
// Algorithm (topological stratification + program-order for emitters):
//
//  1. Emitters: register name → index of the task emitting it (`register: X`),
//     last-wins on duplicates.
//  2. Readers: for each task, the set of cross-task register names from passage-
//     defining sources (where/vars/params/apply.input/output/loop.items, recursively
//     through block; `register.self` excluded).
//  3. passage(T) is a memoized recursion over two edge kinds: the register edge
//     (passage(T) >= 1 + passage(emitter X)) and the program-order edge for a probe
//     emitter (passage(probe) >= passage of any preceding task — yields a re-probe in
//     the Passage AFTER the action, the restart case).
//  4. A cycle over register edges → StratifyCycle. A dangling reference →
//     StratifyUnknownRegister.
func Stratify(tasks []Task) (Passage, error) {
	n := len(tasks)
	if n == 0 {
		return Passage{TaskPassage: nil, Count: 1}, nil
	}

	emitter := emitterIndex(tasks)
	reads := make([][]string, n)
	emits := make([]bool, n)
	// roster-refresh boundary (ADR-0061 §S2): refresh emitters (core.soul.registered
	// with refresh_soulprint: true) and roster consumers (on:[incarnation.name] /
	// soulprint.hosts / soulprint.self.* / omitted on:). Any roster consumer AFTER a
	// refresh emitter (program-order) moves to the next Passage — otherwise it would
	// render on the OLD roster (silent-wrong-target, a BLOCKER for ADR-056). Empty
	// refreshEmitters → boundary inactive, Count bit-for-bit as before ADR-0061.
	var refreshEmitters []int
	readsRoster := make([]bool, n)
	// vault-secrets-generated boundary (ADR-056 amendment, a SEPARATE AXIS): emitters
	// `core.vault.kv-present` (write secrets to targets) and vault consumers (read
	// `${ vault(...) }` in a passage-defining field). Any vault consumer AFTER a vault
	// emitter (program-order) moves to the next Passage — otherwise deploy render
	// (vault_resolve) would read the secret BEFORE it is written → render_failed (the
	// create_from_souls live bug, where the roster axis is absent). Empty vaultEmitters
	// → boundary inactive, Count bit-for-bit as before the amendment. See
	// passage_vault.go.
	var vaultEmitters []int
	readsVault := make([]bool, n)
	for i := range tasks {
		reads[i] = taskRegisterReads(&tasks[i])
		emits[i] = taskEmitsRegister(&tasks[i])
		if taskIsRefreshEmitter(&tasks[i]) {
			refreshEmitters = append(refreshEmitters, i)
		}
		if taskIsVaultEmitter(&tasks[i]) {
			vaultEmitters = append(vaultEmitters, i)
		}
	}
	if len(refreshEmitters) > 0 {
		for i := range tasks {
			readsRoster[i] = taskReadsRoster(&tasks[i])
		}
	}
	if len(vaultEmitters) > 0 {
		for i := range tasks {
			readsVault[i] = taskReadsVaultSecret(&tasks[i])
		}
	}

	// Dangling reference: a reader of register.X that nobody emits. Fail BEFORE the
	// topo-sort — otherwise stratification would proceed on an incomplete graph.
	for i := range reads {
		for _, name := range reads[i] {
			if _, ok := emitter[name]; !ok {
				return Passage{}, &StratifyError{
					Code: StratifyUnknownRegister,
					Msg: fmt.Sprintf(
						"task #%d reads register %q, which no task in the run declares — staged-render cannot stratify (would target on an unresolved register)",
						i, name),
				}
			}
		}
	}

	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make([]int, n)
	memo := make([]int, n)

	var visit func(i int) (int, error)
	visit = func(i int) (int, error) {
		switch state[i] {
		case done:
			return memo[i], nil
		case onStack:
			return 0, &StratifyError{
				Code: StratifyCycle,
				Msg:  fmt.Sprintf("register dependency cycle detected at task #%d — tasks read each other's register in a loop, no Passage order exists", i),
			}
		}
		state[i] = onStack

		level := 0
		for _, name := range reads[i] {
			src := emitter[name]
			if src == i {
				continue
			}
			p, err := visit(src)
			if err != nil {
				return 0, err
			}
			if p+1 > level {
				level = p + 1
			}
		}
		if emits[i] {
			for j := 0; j < i; j++ {
				p, err := visit(j)
				if err != nil {
					return 0, err
				}
				if p > level {
					level = p
				}
			}
		}

		// roster-refresh edge (ADR-0061 §S2): a roster-consumer task moves to a
		// Passage STRICTLY AFTER any PRECEDING (program-order) refresh emitter. This
		// is the roster axis, symmetric to the register edge above: the refresh
		// emitter signals "roster grew", the consumer must see the grown set. No cycle
		// is possible here — the edge is strictly directed by program-order (j < i).
		if readsRoster[i] {
			for _, j := range refreshEmitters {
				if j >= i {
					break // refreshEmitters is ascending (appended in traversal order).
				}
				p, err := visit(j)
				if err != nil {
					return 0, err
				}
				if p+1 > level {
					level = p + 1
				}
			}
		}

		// vault-secrets-generated edge (ADR-056 amendment, the THIRD axis): a vault-
		// consumer task (reads `${ vault(...) }`) moves to a Passage STRICTLY AFTER any
		// PRECEDING (program-order) vault emitter (`core.vault.kv-present`). Symmetric
		// to the roster edge: the emitter signals "secrets written", the consumer must
		// render (vault_resolve) only after the write. No cycle is possible — the edge
		// is strictly directed by program-order (j < i), and kv-present itself does not
		// read vault() (it is a vault emitter, not a consumer).
		if readsVault[i] {
			for _, j := range vaultEmitters {
				if j >= i {
					break // vaultEmitters is ascending (appended in traversal order).
				}
				p, err := visit(j)
				if err != nil {
					return 0, err
				}
				if p+1 > level {
					level = p + 1
				}
			}
		}

		state[i] = done
		memo[i] = level
		return level, nil
	}

	maxP := 0
	for i := 0; i < n; i++ {
		p, err := visit(i)
		if err != nil {
			return Passage{}, err
		}
		if p > maxP {
			maxP = p
		}
	}

	return Passage{TaskPassage: memo, Count: maxP + 1}, nil
}

// emitterIndex builds the map register name → index of the top-level task emitting
// it. A register declared inside a block: is addressed flatly → attributed to the
// CONTAINER's index (a block is an atomic Passage unit). last-wins on a duplicate
// name.
func emitterIndex(tasks []Task) map[string]int {
	idx := map[string]int{}
	for i := range tasks {
		for _, name := range taskEmittedRegisters(&tasks[i]) {
			idx[name] = i
		}
	}
	return idx
}

// taskEmittedRegisters returns all register names emitted by a task and its block
// children.
func taskEmittedRegisters(t *Task) []string {
	var out []string
	if t.Register != "" {
		out = append(out, t.Register)
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			out = append(out, taskEmittedRegisters(&t.Block.Block[i])...)
		}
	}
	return out
}

// taskEmitsRegister reports whether a task (or a block child) emits at least one
// register.
func taskEmitsRegister(t *Task) bool {
	return len(taskEmittedRegisters(t)) > 0
}

// taskRegisterReads returns the sorted unique set of cross-task register names a
// task READS. The task's own register: names (and those of its block children) are
// EXCLUDED — a self-reference within one slice, not a cross-task edge.
func taskRegisterReads(t *Task) []string {
	own := map[string]bool{}
	for _, name := range taskEmittedRegisters(t) {
		own[name] = true
	}
	seen := map[string]bool{}
	collectTaskReads(t, seen)
	out := make([]string, 0, len(seen))
	for name := range seen {
		if own[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// collectTaskReads fills seen with register names a task reads in passage-DEFINING
// sources (the ADR-056 registry): where / vars / params / apply.input / output /
// loop.items / loop.when, recursively through block. Self-filtering is done in
// taskRegisterReads.
//
// Flow-control CEL `when` / `changed_when` / `failed_when` / `retry.until` is
// EXCLUDED (ADR-056:85 — flow-control does not define Passage). It is Soul-side
// per-task gating ([ADR-012(d)](../../docs/adr/0012-keeper-soul-grpc.md)): evaluated
// within ONE ApplyRequest from the register accumulated by its own Passage.
// Including these fields in the passage graph SPLIT a probe and its same-passage
// when-consumer across Passages (probe→Passage 0, when→Passage 1), where the Soul
// cannot see cross-passage register → `no such key` (FC-5). That broke `when` gating
// semantics instead of being a "conservative over-approx". `where` (Keeper-side
// targeting) handles cross-passage (Keeper re-renders with accumulated register),
// `when` does not: the asymmetry is legitimate, so where stays passage-defining and
// when does not.
//
// requisites (`onchanges`/`onfail`/`require`) are excluded for the same reason
// (addressed references, not interpolation). loop.when STAYS passage-defining: the
// loop fan-out is built Keeper-side BEFORE dispatch (like loop.items), not Soul-side.
//
// The cross-ref validator (config.collectRefs) does WALK when/changed_when/
// failed_when — they are register-READING (an "register exists" check) but NOT
// passage-DEFINING. This asymmetry (refs ⊋ passage-reads) is deliberate; a guard
// invariant pins it (TestStratify_FlowControlInRefsNotPassageReads).
func collectTaskReads(t *Task, seen map[string]bool) {
	addCELRefs(t.Where, seen)
	if t.Loop != nil {
		addCELRefs(t.Loop.When, seen)
	}

	addMapRefs(t.Vars, seen)
	addMapRefs(t.Output, seen)
	if t.Loop != nil {
		addValueRefs(t.Loop.Items, seen)
	}
	if t.Module != nil {
		addMapRefs(t.Module.Params, seen)
	}
	if t.Apply != nil {
		addMapRefs(t.Apply.Input, seen)
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			collectTaskReads(&t.Block.Block[i], seen)
		}
	}
}

// addCELRefs extracts cross-task register names from a bare CEL string via the
// canonical ExtractRegisterRefs parser.
func addCELRefs(expr string, seen map[string]bool) {
	for _, name := range ExtractRegisterRefs(expr) {
		seen[name] = true
	}
}

// addMapRefs walks a value map (vars/params/apply.input) and extracts register names
// from string literals (inside the `${ … }` marker). Recurses into map/seq.
func addMapRefs(m map[string]any, seen map[string]bool) {
	for _, v := range m {
		addValueRefs(v, seen)
	}
}

// addValueRefs recursively walks an any value (string / map / seq) and collects
// register names from every string.
func addValueRefs(v any, seen map[string]bool) {
	switch t := v.(type) {
	case string:
		addCELRefs(t, seen)
	case map[string]any:
		for _, sub := range t {
			addValueRefs(sub, seen)
		}
	case []any:
		for _, sub := range t {
			addValueRefs(sub, seen)
		}
	}
}

// CrossPassageRequisite detects a task whose onchanges/onfail source lies in a
// DIFFERENT Passage (ADR-056 amend, R2 — explicit reject until full keeper-side
// gating support in R3). requisites (`onchanges:`/`onfail:`) are NOT passage-
// defining (absent from the Stratify graph), so their source may land in any
// Passage. If consumer and source are in DIFFERENT Passages they travel in DIFFERENT
// ApplyRequests: one Passage's Soul gating cannot see another Passage's source
// register → registerByIdx[remap=sentinel]=nil → the onchanges task is silently
// SKIPPED, the onfail rescue silently does NOT run. The pre-R3 fix is a fail-closed
// reject (symmetric to serial_staged_unsupported), not a silent misfire.
//
// passage is [Stratify]'s result for the same tasks. Returns the coordinates of the
// first cross-passage link found (consumer task name, requisite name, its kind, both
// passages) and ok=true; ok=false — all requisites same-passage (R1-remap fixes
// them). N=1 (passage.Count==1) → always ok=false (a single Passage).
func CrossPassageRequisite(tasks []Task, passage Passage) (info CrossPassageInfo, ok bool) {
	if passage.Count <= 1 || len(passage.TaskPassage) != len(tasks) {
		return CrossPassageInfo{}, false
	}
	emitter := emitterIndex(tasks)
	for i := range tasks {
		consumerPassage := passage.TaskPassage[i]
		for _, req := range taskRequisites(&tasks[i]) {
			srcIdx, known := emitter[req.name]
			if !known {
				// A dangling requisite (no task with that register:) is not our
				// concern: the config cross-ref validator catches it
				// (unknown_register_reference) offline. Here we care only about a
				// cross-passage EXISTING source.
				continue
			}
			if srcPassage := passage.TaskPassage[srcIdx]; srcPassage != consumerPassage {
				return CrossPassageInfo{
					ConsumerName:    taskDisplayName(&tasks[i]),
					RequisiteName:   req.name,
					Kind:            req.kind,
					ConsumerPassage: consumerPassage,
					SourcePassage:   srcPassage,
				}, true
			}
		}
	}
	return CrossPassageInfo{}, false
}

// CrossPassageInfo holds the coordinates of a cross-passage requisite link (for the
// abort message).
type CrossPassageInfo struct {
	ConsumerName    string // name of the requisite-consumer task
	RequisiteName   string // source register name
	Kind            string // "onchanges" | "onfail"
	ConsumerPassage int
	SourcePassage   int
}

// CrossPassageWhenGating detects a task whose flow-control CEL (`when:`/
// `changed_when:`/`failed_when:`) references a cross-task register emitted in an
// EARLIER Passage (ADR-056:85 amend, FC-5 — fail-closed reject of genuinely cross-
// passage when-gating). After the narrow-fix flow-control does not split the Passage
// itself (it is ∉ collectTaskReads), so a register-dependent `when` usually travels
// SAME-passage with the probe → the Soul sees the register → gating works (as
// intended, [ADR-012(d)]). BUT the probe may land in an earlier Passage for ANOTHER
// reason: another task with `where: register.X` (passage-defining) pushed emitter X
// into Passage 0, while the when-consumer moved to Passage 1 by ITS OWN register
// dependency. Then `when: register.X` is genuinely cross-passage: the consumer
// travels in a separate ApplyRequest, its Soul's Passage cannot see register X (it is
// in another Passage's registerByIdx) → when silently eval-fails `no such key` / the
// task is FAILED.
//
// where handles this (Keeper re-renders where with the previous Passage's accumulated
// register BEFORE dispatch), when does not (Soul-side, sees only its own Passage). So
// genuinely cross-passage flow-control gating is UNSUPPORTED → fail-closed reject
// (symmetric to within_block_register_dependency and cross_passage_requisite), not a
// silent misfire. Fixed by: where: for cross-task register targeting OR register.self
// for same-task gating.
//
// register.self does not count (ExtractRegisterRefs strips it — same-task). The
// task's own register is also excluded (self-reference, not a cross-task edge).
// passage is [Stratify]'s result for the same plan. Returns the coordinates of the
// first link found and ok=true; ok=false — all flow-control register references
// same-passage. N=1 (passage.Count==1) → always ok=false (a single Passage, cross-
// passage impossible).
func CrossPassageWhenGating(tasks []Task, passage Passage) (info CrossPassageWhenInfo, ok bool) {
	if passage.Count <= 1 || len(passage.TaskPassage) != len(tasks) {
		return CrossPassageWhenInfo{}, false
	}
	emitter := emitterIndex(tasks)
	for i := range tasks {
		consumerPassage := passage.TaskPassage[i]
		own := map[string]bool{}
		for _, name := range taskEmittedRegisters(&tasks[i]) {
			own[name] = true
		}
		for _, ref := range taskFlowControlReads(&tasks[i]) {
			if own[ref.name] {
				continue // the task's own register — self-reference, not cross-task.
			}
			srcIdx, known := emitter[ref.name]
			if !known {
				// A dangling register is not our concern: the cross-ref validator /
				// Stratify catch unknown_register_reference offline. Here — only a
				// cross-passage EXISTING source.
				continue
			}
			if srcPassage := passage.TaskPassage[srcIdx]; srcPassage != consumerPassage {
				return CrossPassageWhenInfo{
					ConsumerName:    taskDisplayName(&tasks[i]),
					RegisterName:    ref.name,
					Kind:            ref.kind,
					ConsumerPassage: consumerPassage,
					SourcePassage:   srcPassage,
				}, true
			}
		}
	}
	return CrossPassageWhenInfo{}, false
}

// CrossPassageWhenInfo holds the coordinates of a cross-passage flow-control gating
// link (for the abort message / linter diagnostic).
type CrossPassageWhenInfo struct {
	ConsumerName    string // name of the task with the flow-control predicate
	RegisterName    string // register name read by the predicate
	Kind            string // "when" | "changed_when" | "failed_when"
	ConsumerPassage int
	SourcePassage   int
}

// flowControlRead is one flow-control register reference of a task (cross-task
// register name + source field). retry.until is excluded: it gates retry within a
// single task on its OWN register.self (cross-task retry.until.register.X is
// meaningless — a task cannot see another's result in its retry loop), so
// register.self covers it, and a cross-task reference there is a separate error class
// (unknown/misuse), not gating.
type flowControlRead struct {
	name string
	kind string
}

// taskFlowControlReads collects cross-task register names from a task's `when`/
// `changed_when`/`failed_when` AND its block children (a block is an atomic Passage
// unit; its children's flow-control is addressed by the same register names).
// register.self is already filtered by ExtractRegisterRefs. Sorting names within one
// field makes the diagnostic deterministic.
func taskFlowControlReads(t *Task) []flowControlRead {
	var out []flowControlRead
	for _, kv := range []struct {
		expr string
		kind string
	}{
		{t.When, "when"},
		{t.ChangedWhen, "changed_when"},
		{t.FailedWhen, "failed_when"},
	} {
		for _, name := range ExtractRegisterRefs(kv.expr) {
			out = append(out, flowControlRead{name: name, kind: kv.kind})
		}
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			out = append(out, taskFlowControlReads(&t.Block.Block[i])...)
		}
	}
	return out
}

// taskRequisite is one requisite reference of a task (source register name + kind).
type taskRequisite struct {
	name string
	kind string
}

// taskRequisites collects a task's onchanges/onfail names AND those of its block
// children (requisites are addressed flatly by register name; a block is an atomic
// Passage unit). require: is excluded: its semantics are execution order, not
// changed/failed gating by registerByIdx (R2 is strictly about onchanges/onfail).
func taskRequisites(t *Task) []taskRequisite {
	var out []taskRequisite
	for _, name := range t.OnChanges {
		out = append(out, taskRequisite{name: name, kind: "onchanges"})
	}
	for _, name := range t.OnFail {
		out = append(out, taskRequisite{name: name, kind: "onfail"})
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			out = append(out, taskRequisites(&t.Block.Block[i])...)
		}
	}
	return out
}

// taskDisplayName returns a task's name for diagnostics (name: or register: or
// "<unnamed>").
func taskDisplayName(t *Task) string {
	switch {
	case t.Name != "":
		return t.Name
	case t.Register != "":
		return t.Register
	default:
		return "<unnamed>"
	}
}

// WithinBlockInfo holds the coordinates of a within-block register dependency (for
// the abort message / linter diagnostic).
type WithinBlockInfo struct {
	ReaderName   string // name of the register-reading child
	RegisterName string // peer-register read by the reader
	EmitterName  string // name of the emitting child of the same block (or its container)
}

// WithinBlockRegisterDependency detects a block: child reading a register emitted by
// a SIBLING child of the SAME block (ADR-056, §"Risks — silent-wrong-target"). A
// block is an atomic Passage unit: its whole fan-out (probe child + consumer) travels
// in ONE ApplyRequest in one Passage. peer-register becomes available only Soul-side
// AFTER the probe, whereas the consumer's KEEPER-SIDE-resolvable fields
// (where/params/vars/apply.input — those in collectTaskReads) resolve BEFORE the
// block's dispatch — on an empty/external/stale register SILENTLY. Stratify does not
// catch this: the within-block edge does not cross the top-level task boundary
// (emitterIndex stamps the whole block's register with the CONTAINER's index). Hence
// a separate fail-closed detector.
//
// Flow-control `when`/`changed_when`/`failed_when` is EXCLUDED (collectTaskReads no
// longer returns it, FC-5 narrow-fix): within-block `when: register.peer` is VALID —
// the peer-probe runs in the same ApplyRequest BEFORE the consumer, and the Soul sees
// peer-register in the block's accumulated slice at eval time (unlike Keeper-side
// where, which is empty).
//
// Each block's blockEmits is built from ITS STRUCTURE (taskEmittedRegisters minus the
// container's own register), recursively — NOT from the global emitterIndex. This is
// critical: an outer top-level probe emitting the same register name may be read by a
// block child VALIDLY (the probe is a separate Passage before the block, the restart
// case) and must stay ok==false. The check runs only against registers born INSIDE
// this block.
//
// Returns the coordinates of the first within-block dependency found (reader name,
// peer-register, emitter name) and ok=true; ok=false — no block child reads its
// block's peer-register. A plan without block tasks → fast-path ok=false.
func WithinBlockRegisterDependency(tasks []Task) (info WithinBlockInfo, ok bool) {
	for i := range tasks {
		if tasks[i].Block == nil {
			continue
		}
		if bi, bad := blockPeerRegisterRead(&tasks[i]); bad {
			return bi, true
		}
	}
	return WithinBlockInfo{}, false
}

// blockPeerRegisterRead checks one block container: whether any of its children
// (recursively, any depth) reads a register born INSIDE this block by a sibling
// child. blockEmits is the register names of the whole block subtree MINUS the
// container's own register (this block's structure, not the global emitterIndex).
// Then for each child: childReads (collectTaskReads — all 7 passage sources;
// register.self already filtered by ExtractRegisterRefs) is intersected with
// blockEmits but WITHOUT the child's own register (peer, not self) → reject. Nested
// blocks: their children are also checked against the outer block's blockEmits (peer
// inside = peer outside, one Passage), plus the recursive descent catches a peer
// dependency inside the nested block.
func blockPeerRegisterRead(container *Task) (WithinBlockInfo, bool) {
	blockEmits := map[string]string{} // register name → emitting child's name
	for i := range container.Block.Block {
		collectBlockEmits(&container.Block.Block[i], blockEmits)
	}
	if len(blockEmits) == 0 {
		return WithinBlockInfo{}, false // block emits nothing — no peer dependency possible.
	}

	for i := range container.Block.Block {
		if bi, bad := childPeerRead(&container.Block.Block[i], blockEmits); bad {
			return bi, true
		}
		// Nested block: its internal peer dependencies (fully within it) are a
		// separate check with its own blockEmits.
		if container.Block.Block[i].Block != nil {
			if bi, bad := blockPeerRegisterRead(&container.Block.Block[i]); bad {
				return bi, true
			}
		}
	}
	return WithinBlockInfo{}, false
}

// collectBlockEmits fills emits with register names born inside a block child (its
// own register: + block children's registers recursively), mapping each to the
// emitting child's name for diagnostics. last-wins on a duplicate (symmetric to
// emitterIndex).
func collectBlockEmits(child *Task, emits map[string]string) {
	name := taskDisplayName(child)
	for _, reg := range taskEmittedRegisters(child) {
		emits[reg] = name
	}
}

// childPeerRead checks one block child (recursively into its own block children):
// whether it reads a register from blockEmits that is NOT its own (peer, not self).
// The child's own registers are excluded — an intra-task self-reference is not a
// cross-task edge (register.self is already filtered, but a child with its own
// register: X reading register.X in another field is also not a peer dependency).
func childPeerRead(child *Task, blockEmits map[string]string) (WithinBlockInfo, bool) {
	own := map[string]bool{}
	for _, reg := range taskEmittedRegisters(child) {
		own[reg] = true
	}
	seen := map[string]bool{}
	collectTaskReads(child, seen)
	reads := make([]string, 0, len(seen))
	for reg := range seen {
		reads = append(reads, reg)
	}
	sort.Strings(reads) // deterministic diagnostic when there are several peer refs.
	for _, reg := range reads {
		if own[reg] {
			continue
		}
		if emitter, peer := blockEmits[reg]; peer {
			return WithinBlockInfo{
				ReaderName:   taskDisplayName(child),
				RegisterName: reg,
				EmitterName:  emitter,
			}, true
		}
	}
	return WithinBlockInfo{}, false
}
