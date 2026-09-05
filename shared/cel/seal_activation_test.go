package cel

import (
	"context"
	"testing"
)

// ★ THE COMPLETENESS GUARD (NIM-822/NIM-823).
//
// The seal's whole failure mode has been the same one three times: a name enters
// the activation, nothing classifies it, and the detector answers "not a secret"
// by falling through. Adding a case per incident is how the fourth arrives, so
// what has to fail is the ADDITION of an unclassified binding — not its first
// leak in production.
//
// [activationRoots] is that classification, and this test is what makes it
// binding: it reads the roots the REAL activation builds rather than a list
// re-typed here, in both modes and in both directions. A name added to
// activation() without a line in activationRoots reddens; a line left behind in
// activationRoots after its root is removed reddens too.
//
// Loop binds are excluded on purpose and by construction: their names come from
// `loop.as:`, so no fixed table could carry them, and they are tainted whole
// through [SealSources.Roots].
func TestActivationRootsAreClassified(t *testing.T) {
	loop := map[string]any{"item": map[string]any{"name": "alice"}, "i": 0}

	// Every field populated: activation() must not omit a root merely because
	// this Vars left it nil.
	full := Vars{
		Input:          map[string]any{"password": "x"},
		Register:       map[string]any{"probe": map[string]any{"changed": true}},
		Incarnation:    map[string]any{"id": "redis-prod"},
		SoulprintSelf:  map[string]any{"sid": "a.example.com"},
		SoulprintHosts: []map[string]any{{"sid": "a.example.com"}},
		Vars:           map[string]any{"conf_dir": "/etc/redis"},
		Compute:        map[string]any{"endpoint": "a:6379"},
		State:          map[string]any{"users": []any{}},
		Loop:           loop,
		Ctx:            context.Background(),
	}

	// Ordinary mode, then the migration sandbox: `state` is only in scope there,
	// so one mode alone would leave it looking like a stale table entry.
	got := map[string]bool{}
	for _, migration := range []bool{false, true} {
		for name := range full.activation(migration) {
			if _, isLoopBind := loop[name]; isLoopBind {
				continue
			}
			got[name] = true
		}
	}

	for name := range got {
		if _, classified := activationRoots[name]; !classified {
			t.Errorf("activation root %q is in scope for every expression and is not in activationRoots.\n"+
				"Decide what the seal can say about it: sealedByField if a cell reading it can carry a\n"+
				"secret, or neverSecret WITH THE REASON. Leaving it out is the NIM-822 failure --\n"+
				"readsSecretAddr answers false and the leak is silent.", name)
		}
	}
	for name := range activationRoots {
		if !got[name] {
			t.Errorf("activationRoots classifies %q, which no activation() mode puts in scope -- a stale\n"+
				"entry reads as coverage the seal does not have", name)
		}
	}
}

// A loop bind is tainted WHOLE, under whatever name the author chose, and the
// two spellings NIM-822 and NIM-823 were filed for are one binding: `item` is
// simply the default of `loop.as:`.
func TestDetectSealed_LoopBindRoot(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Roots: map[string]bool{"item": true, "cred": true}}

	for _, expr := range []string{
		"ACL SETUSER ${ item.name } >${ item.password }", // NIM-822, default name
		"${ cred.password }",                             // NIM-823, loop.as: cred
		"${ item }",                                      // bare whole-binding read
		"${ item.creds.pw }",                             // nested below the bind
		"${ has(item.password) ? item.password : '' }",   // both ternary branches
	} {
		if !e.DetectSealed(expr, src) {
			t.Errorf("%q is not sealed -- a loop bound out of a secret list renders plaintext into apply_run_plan", expr)
		}
	}

	// index_as: binds an integer and is never tainted; treating the two names
	// alike would seal every `${ i }` in the plan.
	if e.DetectSealed("shard ${ i } of ${ total }", src) {
		t.Error("an untainted bind was sealed -- over-seal costs every diagnostic that mentions the index")
	}
}

// A bare read of a FIXED root is not a whole-binding read: `input` is tainted per
// field, so `${ input }` is judged by the addresses in Fields and none of them
// matches. Stated as a test because it is a real bound, not an oversight -- a
// whole-map read of a partially secret root is its own question.
func TestDetectSealed_BareFixedRootIsNotWholeTainted(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{FieldAddr("input", "password"): true}}

	if !e.DetectSealed("${ input.password }", src) {
		t.Fatal("the field address itself must still seal")
	}
	if e.DetectSealed("${ input }", src) {
		t.Error("`${ input }` seals now. That is a WIDENING, not a bug fix to leave unremarked:\n" +
			"update the bound stated on SealSources.Roots and in docs/templating.md rather than this line")
	}
}
