package cel

import (
	"errors"
	"strings"
	"testing"
)

func hostScopeEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return e
}

// hostBound is a per-host activation: a host's facts and one destiny input.
func hostBound() Vars {
	return Vars{
		SoulprintSelf: map[string]any{"sid": "a.example.com", "network": map[string]any{"primary_ip": "10.0.0.1"}},
		Input:         map[string]any{"master_addr": "10.0.0.1", "marker": "fixed"},
	}
}

// hostFree is the same data reached from a context that has no host, declaring
// `master_addr` as the name that renders per host.
func hostFree(scope HostScope) Vars {
	v := hostBound()
	v.SoulprintSelf = nil
	v.HostScope = scope
	v.HostVariantInputs = []string{"master_addr"}
	return v
}

// TestHostScope_EveryStanceNamesItselfAndAWayOut — the case list is checked against
// [hostScopeCount] rather than kept by hand: a new stance whose strings nobody wrote
// fails here instead of shipping an error that names an empty context and offers an
// empty hint.
func TestHostScope_EveryStanceNamesItselfAndAWayOut(t *testing.T) {
	stances := []HostScope{
		HostFreeLoopAxis,
		HostFreeCovenList,
		HostFreeKeeper,
		HostFreeCompute,
		HostFreeStateMatch,
		HostFreeStaticWhen,
		HostFreeIncludeWhen,
		HostFreeTemplatePath,
	}
	if len(stances)+1 != hostScopeCount { // +1: HostBound names no context
		t.Fatalf("the case list covers %d stances, hostScopeCount says %d — a new stance was added without strings",
			len(stances)+1, hostScopeCount)
	}
	for _, s := range stances {
		context, selfHint, variantHint := s.Describe()
		if context == "" {
			t.Errorf("stance %d names no context", s)
		}
		if selfHint == "" {
			t.Errorf("stance %d (%s) offers no way out for a soulprint.self reference", s, context)
		}
		if variantHint == "" {
			t.Errorf("stance %d (%s) offers no way out for a per-host input reference", s, context)
		}
	}
	if c, sh, vh := HostBound.Describe(); c != "" || sh != "" || vh != "" {
		t.Errorf("HostBound must describe nothing, got %q/%q/%q", c, sh, vh)
	}
}

// ★ THE CACHE. A program is looked up before any guard runs, so an expression
// compiled in a host-bound context would be served straight back in a host-free one
// unless the stance is part of the key. Same Engine, same text, both directions.
func TestHostScope_CacheDoesNotCarryAVerdictAcrossContexts(t *testing.T) {
	const expr = "soulprint.self.sid"

	t.Run("bound first, then free", func(t *testing.T) {
		e := hostScopeEngine(t)
		if _, err := e.EvalExpression(expr, hostBound()); err != nil {
			t.Fatalf("host-bound eval: %v", err)
		}
		_, err := e.EvalExpression(expr, hostFree(HostFreeLoopAxis))
		if !errors.Is(err, ErrNoHostBound) {
			t.Fatalf("the cached host-bound program was served in a host-free context: %v", err)
		}
	})

	t.Run("free first, then bound", func(t *testing.T) {
		e := hostScopeEngine(t)
		if _, err := e.EvalExpression(expr, hostFree(HostFreeLoopAxis)); !errors.Is(err, ErrNoHostBound) {
			t.Fatalf("host-free eval: expected ErrNoHostBound, got %v", err)
		}
		// A refused expression never reaches the cache, so this must compile and run.
		out, err := e.EvalExpression(expr, hostBound())
		if err != nil {
			t.Fatalf("host-bound eval after a refusal: %v", err)
		}
		if got := out.Value(); got != "a.example.com" {
			t.Fatalf("got %v, want a.example.com", got)
		}
	})
}

// ★ THE CACHE, second half: the per-host input NAMES are part of the key. The Engine
// outlives a run, and two runs of the same scenario against different destinies
// genuinely have different variant sets — so a marker byte alone would let one run's
// verdict answer for another's.
func TestHostScope_CacheDoesNotCarryAVerdictAcrossVariantSets(t *testing.T) {
	e := hostScopeEngine(t)
	const expr = "input.master_addr"

	// Run A: master_addr is host-invariant here, so the read is fine.
	other := hostFree(HostFreeLoopAxis)
	other.HostVariantInputs = []string{"some_other_name"}
	out, err := e.EvalExpression(expr, other)
	if err != nil {
		t.Fatalf("a name outside the variant set must read normally: %v", err)
	}
	if got := out.Value(); got != "10.0.0.1" {
		t.Fatalf("got %v, want 10.0.0.1", got)
	}

	// Run B: same text, same Engine, and now master_addr IS per-host.
	_, err = e.EvalExpression(expr, hostFree(HostFreeLoopAxis))
	if !errors.Is(err, ErrNoHostBound) {
		t.Fatalf("run A's cached program answered for run B's variant set: %v", err)
	}
}

// A host-bound context ignores HostVariantInputs entirely: every name resolves
// against its own host, so there is nothing to refuse. Without this the guard could
// pass its other tests while making a per-host input unreadable everywhere, which
// would leave NIM-908 with a refusal and no feature.
func TestHostScope_HostBoundReadsVariantInputs(t *testing.T) {
	e := hostScopeEngine(t)
	v := hostBound()
	v.HostVariantInputs = []string{"master_addr"}
	out, err := e.EvalExpression("input.master_addr", v)
	if err != nil {
		t.Fatalf("a host-bound context must read a per-host input: %v", err)
	}
	if got := out.Value(); got != "10.0.0.1" {
		t.Fatalf("got %v, want 10.0.0.1", got)
	}
}

// Fail-closed on a shape no field name survives. `size(input)` and `input[k]` reach
// the whole namespace, which includes the per-host name, and the walk cannot say
// they do not.
func TestHostScope_WholeNamespaceReadIsRefused(t *testing.T) {
	for _, expr := range []string{"size(input) > 0", "input['master_addr'] == 'x'", "size(soulprint) > 0"} {
		t.Run(expr, func(t *testing.T) {
			e := hostScopeEngine(t)
			_, err := e.EvalExpression(expr, hostFree(HostFreeLoopAxis))
			if !errors.Is(err, ErrNoHostBound) {
				t.Fatalf("expected ErrNoHostBound for a whole-namespace read, got: %v", err)
			}
		})
	}
}

// soulprint.hosts is the run's roster, identical on every host, and must stay
// readable from a host-free context — it is the only way one reaches host facts.
func TestHostScope_SoulprintHostsIsNotPerHost(t *testing.T) {
	e := hostScopeEngine(t)
	v := hostFree(HostFreeLoopAxis)
	v.AllowHosts = true
	v.SoulprintHosts = []map[string]any{{"sid": "a.example.com"}, {"sid": "b.example.com"}}
	out, err := e.EvalExpression("size(soulprint.hosts)", v)
	if err != nil {
		t.Fatalf("soulprint.hosts in a host-free context: %v", err)
	}
	if got := out.Value(); got != int64(2) {
		t.Fatalf("got %v, want 2", got)
	}
}

// The message must name the reference, the context and a way out — an error that
// says only "not allowed here" sends the author back to guessing, which is the
// failure NIM-619 documented and this guard inherits the shape from.
func TestHostScope_MessageNamesReferenceContextAndHint(t *testing.T) {
	e := hostScopeEngine(t)
	_, err := e.EvalExpression("input.master_addr", hostFree(HostFreeCovenList))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"input.master_addr", "on: [covens]", "host-invariant input"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not contain %q: %s", want, msg)
		}
	}
	var typed *ErrHostOutOfScope
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrHostOutOfScope, got %T", err)
	}
}

// GuardHostFreePredicate is the same guard for a predicate this engine never
// compiles (a static `when:`, an include's `when:` — both decided against a
// flow_context in the Soul-side sandbox). Inert with no variant set, which is what
// keeps every existing scenario untouched.
func TestHostScope_GuardHostFreePredicate(t *testing.T) {
	e := hostScopeEngine(t)

	if err := e.GuardHostFreePredicate("input.master_addr == 'x'", HostFreeStaticWhen, nil, nil); err != nil {
		t.Fatalf("no variant set: expected nil, got %v", err)
	}
	if err := e.GuardHostFreePredicate("", HostFreeStaticWhen, []string{"master_addr"}, nil); err != nil {
		t.Fatalf("empty predicate: expected nil, got %v", err)
	}
	if err := e.GuardHostFreePredicate("input.marker == 'x'", HostFreeStaticWhen, []string{"master_addr"}, nil); err != nil {
		t.Fatalf("a host-invariant name: expected nil, got %v", err)
	}
	err := e.GuardHostFreePredicate("input.master_addr == 'x'", HostFreeStaticWhen, []string{"master_addr"}, nil)
	if !errors.Is(err, ErrNoHostBound) {
		t.Fatalf("expected ErrNoHostBound, got %v", err)
	}
	if !strings.Contains(err.Error(), "a static when:") {
		t.Errorf("the message does not name the static-when context: %v", err)
	}
}

// ★ InterpolationReads inlines `.where("…")` before scanning. The predicate is a
// string LITERAL in the author's text, so a walk over the raw string reports
// `soulprint.hosts` and nothing else while the inlined form reads `soulprint.self`.
// This is what NIM-908's apply.input classification stands on: a value read as
// host-invariant here becomes a value handed to every host.
func TestInterpolationReads_DescendsIntoWherePredicate(t *testing.T) {
	e := hostScopeEngine(t)

	reads := e.InterpolationReads(`${ soulprint.hosts.where("sid != soulprint.self.sid") }`, []string{"soulprint"})
	if !reads["soulprint"].Fields["self"] {
		t.Fatalf("soulprint.self inside a .where predicate was not seen: %+v", reads["soulprint"])
	}

	// The counterweight: an ordinary predicate over host-row fields names no self.
	reads = e.InterpolationReads(`${ soulprint.hosts.where("role == 'replica'") }`, []string{"soulprint"})
	if reads["soulprint"].Fields["self"] {
		t.Errorf("a host-row predicate was misread as a self reference: %+v", reads["soulprint"])
	}
	if !reads["soulprint"].Fields["hosts"] {
		t.Errorf("soulprint.hosts itself was not seen: %+v", reads["soulprint"])
	}
}

// A literal cell reads nothing; a cell with several blocks unions them; unscannable
// text is reported as reading everything rather than nothing, because a caller that
// narrows on this answer must not act on text that is about to fail.
func TestInterpolationReads_UnionAndFailClosed(t *testing.T) {
	e := hostScopeEngine(t)

	plain := e.InterpolationReads("just text", []string{"input"})
	if plain["input"].Whole || len(plain["input"].Fields) != 0 {
		t.Errorf("a literal cell must read nothing: %+v", plain["input"])
	}

	multi := e.InterpolationReads("${ input.a }:${ input.b }", []string{"input"})
	if !multi["input"].Fields["a"] || !multi["input"].Fields["b"] {
		t.Errorf("blocks were not unioned: %+v", multi["input"])
	}

	broken := e.InterpolationReads("${ input.a", []string{"input"})
	if !broken["input"].Whole {
		t.Errorf("unscannable text must be reported as a whole read: %+v", broken["input"])
	}
}
