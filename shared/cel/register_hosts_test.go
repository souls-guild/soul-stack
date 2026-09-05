package cel

import (
	"errors"
	"strings"
	"testing"
)

// keeperVars — the ONLY context that opts into the per-host register root
// (keeper/internal/render/dispatch.go keeperVars is the single producer).
func keeperRegisterVars() Vars {
	return Vars{
		Register: map[string]any{"provision": map[string]any{"vm_ids": []any{"vm-1"}}},
		RegisterHosts: map[string]any{
			"node_id": map[string]any{
				"node-a": map[string]any{"stdout": "aaa"},
				"node-b": map[string]any{"stdout": "bbb"},
			},
		},
		AllowRegisterHosts: true,
	}
}

func TestRegisterHosts_ResolvesInKeeperContext(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression(`register.hosts.node_id["node-b"].stdout`, keeperRegisterVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != "bbb" {
		t.Fatalf("register.hosts.node_id[node-b].stdout = %v, want bbb", got)
	}
}

// TestRegisterHosts_CoexistsWithOwnRegister — the accessor is an ADDITIONAL root,
// not a replacement: a keeper task's own `register.<name>` chaining ([ADR-0083] §5,
// the keeper bucket) still resolves alongside it.
//
// Mutation: have [Vars.registerRoot] build the map from RegisterHosts alone.
func TestRegisterHosts_CoexistsWithOwnRegister(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression("register.provision.vm_ids.size()", keeperRegisterVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("register.provision.vm_ids.size() = %v, want 1", got)
	}
}

// TestRegisterHosts_ARegisterNamedHostsIsUnreadable pins what the parse-time
// reservation exists to avoid ([config] register_name_reserved): a register named
// `hosts` cannot be read from EITHER side. In a keeper task the accessor occupies
// that field of the `register` root and wins, so the read returns the SID-keyed map
// rather than the author's payload; on a host task the very same expression is
// refused at compile as the keeper-only accessor, whether or not such a register
// exists. Two different failures, neither naming the line that chose the name —
// hence one diagnostic at parse instead.
//
// Mutation: in [Vars.registerRoot], set the hosts field BEFORE copying Register
// (the keeper half then reads the author's payload).
func TestRegisterHosts_ARegisterNamedHostsIsUnreadable(t *testing.T) {
	e := newEngine(t)
	v := keeperRegisterVars()
	v.Register["hosts"] = map[string]any{"stdout": "an author's own register"}

	out, err := e.EvalExpression(`register.hosts.node_id["node-a"].stdout`, v)
	if err != nil {
		t.Fatalf("EvalExpression (keeper): %v", err)
	}
	if got := out.Value(); got != "aaa" {
		t.Fatalf("keeper: register.hosts.node_id[node-a].stdout = %v, want aaa (the accessor must win)", got)
	}

	// The same run's host side: the cut-off is syntactic, so owning the register
	// buys nothing — `register.hosts.stdout` never reaches the payload.
	_, err = e.EvalExpression("register.hosts.stdout", Vars{Register: v.Register})
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("host: err = %v, want *ErrUnsupported (owning the register must not open the accessor)", err)
	}
}

// TestRegisterHosts_DoesNotMutateRegister — [Vars.registerRoot] must COPY: Register
// is the run's live bucket, threaded into every host's context in the same render
// ([render.hostRegister]). Writing `hosts` into it would leak the cross-host map
// into the host tasks the isolation gate exists to keep it out of — and would do so
// invisibly, because those tasks compile against a flag that says it is absent.
//
// Mutation: assign into v.Register instead of the copy in registerRoot.
func TestRegisterHosts_DoesNotMutateRegister(t *testing.T) {
	e := newEngine(t)
	vars := keeperRegisterVars()
	if _, err := e.EvalExpression("register.hosts.node_id.size()", vars); err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if _, ok := vars.Register[registerHostsAccessorTestKey]; ok {
		t.Fatalf("Register was mutated: %v", vars.Register)
	}
}

// registerHostsAccessorTestKey mirrors the unexported field name the accessor is
// injected under, so the test above states what it is looking for.
const registerHostsAccessorTestKey = "hosts"

// TestRegisterHosts_CutOffWithoutFlag — ★ GUARD: every context that does not opt in
// (host tasks in the scenario pass, the destiny pass, `on:` resolution) is refused
// at COMPILE, with [ErrUnsupported] — not left to resolve as an empty map, which
// would make `.size() == 0` and an empty foreach read as facts.
//
// Mutation: drop the guardRegisterHosts call in [Engine.compile] (or, for the index
// spelling alone, the CallKind branch of [isRegisterHosts]).
func TestRegisterHosts_CutOffWithoutFlag(t *testing.T) {
	e := newEngine(t)
	for _, expr := range []string{
		"register.hosts.node_id",
		"register . hosts . node_id", // CEL permits whitespace around `.`
		"has(register.hosts)",        // the presence test answers "does it exist here"
		`register["hosts"].node_id`,  // the index spelling of the same read
	} {
		_, err := e.EvalExpression(expr, Vars{Register: map[string]any{}})
		var unsupported *ErrUnsupported
		if !errors.As(err, &unsupported) {
			t.Fatalf("%s: err = %v, want *ErrUnsupported", expr, err)
		}
		if !strings.Contains(unsupported.Feature, "register.hosts") {
			t.Fatalf("%s: feature = %q, want it to name register.hosts", expr, unsupported.Feature)
		}
	}
}

// TestRegisterHosts_CutOffInsideWherePredicate — a `.where("<predicate>")` argument
// is a STRING literal at the point the gate would normally look, so a reference
// hidden inside it is invisible until [Engine.rewriteHostsWhere] inlines it. The
// gate therefore runs on the rewritten text as well.
//
// Mutation: move the guardRegisterHosts call in [Engine.compile] ABOVE
// rewriteHostsWhere.
func TestRegisterHosts_CutOffInsideWherePredicate(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression(
		`soulprint.hosts.where("sid == register.hosts.node_id").size()`,
		Vars{AllowHosts: true},
	)
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *ErrUnsupported for register.hosts inside a where predicate", err)
	}
}

// TestRegisterHosts_FieldNamedHostsIsUntouched — the gate keys on the SHAPE
// `register.hosts` (Select `hosts` on the bare `register` ident), so a payload that
// happens to carry a `hosts` field (`register.probe.hosts`) is ordinary data.
//
// Mutation: match on the field name alone in [isRegisterHosts].
func TestRegisterHosts_FieldNamedHostsIsUntouched(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression("register.probe.hosts.size()", Vars{
		Register: map[string]any{"probe": map[string]any{"hosts": []any{"a", "b"}}},
	})
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(2) {
		t.Fatalf("register.probe.hosts.size() = %v, want 2", got)
	}
}

// TestRegisterHosts_FlowControlForcesCutOff — flow-control CEL (`when:` /
// `changed_when:` / `until:`) is evaluated by the SOUL in its own sandbox, which
// holds one host's register and no cross-host data at all. The engine forces the
// flag off rather than trusting the caller, symmetric to AllowHosts.
//
// Mutation: drop `allowRegisterHosts = false` from the e.flowControl branch in
// [Engine.compile].
func TestRegisterHosts_FlowControlForcesCutOff(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	_, err = e.EvalExpression("register.hosts.node_id.size() > 0", keeperRegisterVars())
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *ErrUnsupported (flow-control has no cross-host data)", err)
	}
}

// TestRegisterHosts_CompileCacheIsFlagKeyed — ★ GUARD: the compile cache is keyed
// on the flag, like AllowHosts. Without it, ONE engine is shared across a run's
// keeper and host tasks, so the first keeper task to compile an expression would
// cache a program that a host task then reuses — the isolation gate skipped
// entirely, and only for the second caller onwards. That ordering-dependence is
// exactly the kind of bug a single-context test never sees.
//
// Mutation: drop the `\x03` prefix from the cacheKey in [Engine.compile].
func TestRegisterHosts_CompileCacheIsFlagKeyed(t *testing.T) {
	e := newEngine(t)
	const expr = "register.hosts.node_id.size()"

	if _, err := e.EvalExpression(expr, keeperRegisterVars()); err != nil {
		t.Fatalf("keeper context: %v", err)
	}
	_, err := e.EvalExpression(expr, Vars{Register: map[string]any{}})
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("host context after keeper context: err = %v, want *ErrUnsupported", err)
	}
}

// TestRegisterHosts_UnknownNameNamesTheRegister — the `hosts` field is placed even
// when RegisterHosts is empty (no host registered anything yet, or the run has no
// hosts). The author's mistake is then reported as the register they misspelled,
// not as "the accessor does not exist here" — which in a keeper task, where it DOES
// exist, would send them looking in the wrong place.
//
// Mutation: early-return in [Vars.registerRoot] when RegisterHosts is empty.
func TestRegisterHosts_UnknownNameNamesTheRegister(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression("register.hosts.node_id", Vars{AllowRegisterHosts: true})
	if err == nil || !strings.Contains(err.Error(), "no such key: node_id") {
		t.Fatalf("err = %v, want it to name the register (no such key: node_id)", err)
	}
}

// TestRegisterHosts_SealedRegisterStaysSealedAcrossHosts — ★ GUARD: reading a
// SEALED register through the accessor taints the cell exactly as reading it
// directly does. A module-declared `secret: true` output puts a HOST task's
// register name into [SealSources.Fields] as a register.<name> address
// ([render.secretOutputRegisters], [ADR-0083] §8), and its payload reaches
// RegisterByHost in the clear — so before NIM-711 that plaintext was simply
// unreachable from a keeper task, and now it is one expression away. Unsealed, the
// cell holding every host's credential goes unmasked into apply_run_plan.params and
// status_details.
//
// The walk sees `register.hosts` one level down, whose field is `hosts` — reserved
// at parse and never a sealed name — so without the flattening in [selectBaseField]
// the sealed name is never tested at all.
//
// Mutation: drop the isRegisterHosts branch in [selectBaseField].
func TestRegisterHosts_SealedRegisterStaysSealedAcrossHosts(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{FieldAddr("register", "creds"): true}}

	for _, expr := range []string{
		"${ register.hosts.creds }",
		`${ register.hosts.creds["node-a"].password }`,
		`${ register["hosts"].creds }`,
	} {
		if !e.DetectSealed(expr, src) {
			t.Fatalf("%s reads a sealed register across hosts, must seal the cell", expr)
		}
	}
	if e.DetectSealed("${ register.hosts.node_id }", src) {
		t.Fatal("an unsealed register read across hosts must NOT seal the cell")
	}
}
