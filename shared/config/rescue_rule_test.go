package config

import (
	"slices"
	"testing"
)

// ADR-0091 amendment NIM-993: the rule both drivers share — rule 1 (when a unit
// succeeds), rule 3 (the first unit that succeeds handles) and rule 6 (the
// applier fold). Positions are what a driver passes: message positions on the
// Soul, plan indices on the Keeper.

func ordinary(r *RescueRule, pos int, named ...int) { r.Task(pos, 0, named, false, nil) }

func unitTask(r *RescueRule, pos, unit int, named ...int) { r.Task(pos, unit, named, false, nil) }

func terminalTask(r *RescueRule, pos, unit int, children ...int) {
	r.Task(pos, unit, nil, true, children)
}

func TestRescueRule(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, r *RescueRule)
	}{
		{
			name: "a unit that executed something and failed nowhere handles its source",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Ran(1, true)
				if !r.Held(0) || !r.Fires(1) {
					t.Fatalf("before Decide: held=%v fires=%v, want the source held until the driver decides", r.Held(0), r.Fires(1))
				}
				if got := r.Decide(1, false); !slices.Equal(got, []int{0}) {
					t.Errorf("Decide = %v, want [0]", got)
				}
				if r.Held(0) || r.AnyHeld() || r.Fires(1) {
					t.Errorf("after Decide: held=%v anyHeld=%v fires=%v, want the source handled", r.Held(0), r.AnyHeld(), r.Fires(1))
				}
			},
		},
		{
			name: "a unit in which nothing executed has not succeeded",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Ran(1, false)
				if got := r.Decide(1, false); got != nil {
					t.Errorf("Decide = %v, want nothing handled", got)
				}
				if !r.Held(0) {
					t.Error("source handled by a unit that executed nothing")
				}
			},
		},
		{
			name: "a unit whose task failed handles nothing, and the next unit of its source gets its chance",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				unitTask(r, 2, 2, 0)
				r.Fail(0, "src broke")
				r.Ran(1, true)
				r.Fail(1, "u1 broke")
				if got := r.Decide(1, false); got != nil {
					t.Errorf("Decide(u1) = %v, want nothing handled", got)
				}
				if !r.UnitFailed(1) || !r.Fires(2) {
					t.Fatalf("u1 failed=%v, u2 fires=%v, want u1 failed and u2 firing", r.UnitFailed(1), r.Fires(2))
				}
				r.Ran(2, true)
				if got := r.Decide(2, false); !slices.Equal(got, []int{0}) {
					t.Errorf("Decide(u2) = %v, want [0]", got)
				}
			},
		},
		{
			name: "a unit task's failure where its unit fires is no source",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Fail(1, "u1 broke")
				if r.Held(1) {
					t.Error("a unit task's failure is held, want it its unit's")
				}
				if !r.UnitFailed(1) {
					t.Error("unit not failed by its task's failure")
				}
			},
		},
		{
			// The net of rule 6. A later unit naming the applier over the task holds it,
			// and the applier's fold counts it until that unit handles it. Mutations:
			// Fail without its Fires check → no held source; Counts reading the unit
			// before the source → the fold does not count it.
			name: "a unit task's failure where its unit does not fire is a source",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				terminalTask(r, 2, 0, 0, 1)
				unitTask(r, 3, 2, 2)
				r.Fail(1, "u1 broke")
				if !r.Held(1) || r.UnitFailed(1) {
					t.Fatalf("held=%v unitFailed=%v, want a held source and its unit not failed", r.Held(1), r.UnitFailed(1))
				}
				if !r.Counts(1) {
					t.Error("the fold does not count a held source that is a unit task")
				}
				if !r.Fires(2) || !r.NamesHeld(3) {
					t.Fatalf("fires=%v namesHeld=%v, want the unit naming the applier firing", r.Fires(2), r.NamesHeld(3))
				}
				r.Ran(3, true)
				if got := r.Decide(2, false); !slices.Equal(got, []int{1}) {
					t.Errorf("Decide = %v, want [1]", got)
				}
				if r.Counts(1) {
					t.Error("the fold counts a handled source")
				}
			},
		},
		{
			name: "a source failing after a unit task does not fire it",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(1, "u1 broke")
				r.Fail(0, "src broke")
				if !r.Held(1) || !r.Held(0) || r.UnitFailed(1) {
					t.Errorf("held 1=%v 0=%v unitFailed=%v, want two held sources", r.Held(1), r.Held(0), r.UnitFailed(1))
				}
			},
		},
		{
			// The net of rule 6: the task's failure is a source, and its unit, though it
			// executed and did not fail, can no longer succeed. Mutation: Decide not
			// reading lost → the unit handles the source that failed after its task.
			name: "a unit whose task failed where it did not fire handles nothing",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Ran(1, true)
				r.Fail(1, "u1 broke")
				r.Fail(0, "src broke")
				if got := r.Decide(1, false); got != nil {
					t.Errorf("Decide = %v, want nothing handled", got)
				}
				if !r.Held(0) || !r.Held(1) {
					t.Errorf("held 0=%v 1=%v, want both sources held", r.Held(0), r.Held(1))
				}
			},
		},
		{
			// A fail-stop Soul ran a second unit of a source the first unit handled in
			// the same message (rule 6, fail-closed). The source failed while that unit
			// was registered, so the unit fired. Mutation: Fail without its sourceFailed
			// check → u2's failure is a held source and its unit not failed.
			name: "a unit task's failure fires through a source of its message an earlier unit handled",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				unitTask(r, 2, 2, 0)
				r.Fail(0, "src broke")
				r.Ran(1, true)
				if got := r.Decide(1, false); !slices.Equal(got, []int{0}) {
					t.Fatalf("Decide(u1) = %v, want [0]", got)
				}
				r.Ran(2, true)
				r.Fail(2, "u2 broke")
				if r.Held(2) || !r.UnitFailed(2) {
					t.Errorf("held=%v unitFailed=%v, want u2's failure its unit's", r.Held(2), r.UnitFailed(2))
				}
			},
		},
		{
			// Mutation: mark every unit naming a source that ever failed, registered or
			// not → u2's failure is its unit's, and nothing holds it.
			name: "a unit task's failure is the net's where its source failed and was handled before its unit was registered",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Ran(1, true)
				r.Decide(1, false)
				unitTask(r, 2, 2, 0)
				r.Ran(2, true)
				r.Fail(2, "u2 broke")
				if !r.Held(2) || r.UnitFailed(2) {
					t.Errorf("held=%v unitFailed=%v, want u2's failure a held source", r.Held(2), r.UnitFailed(2))
				}
			},
		},
		{
			name: "a terminal does not mark its unit executed",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				terminalTask(r, 2, 1, 1)
				r.Fail(0, "src broke")
				r.Ran(1, false)
				r.Ran(2, true)
				if got := r.Decide(1, false); got != nil {
					t.Errorf("Decide = %v, want nothing handled: only the terminal ran", got)
				}
			},
		},
		{
			name: "a terminal the Keeper recorded FAILED is no source",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				terminalTask(r, 1, 0, 0)
				r.Fail(1, "keeper failed the group")
				if r.Held(1) || r.AnyHeld() {
					t.Errorf("held=%v anyHeld=%v, want the terminal ignored", r.Held(1), r.AnyHeld())
				}
			},
		},
		{
			// A block with onfail: [a] holding a task with onfail: [b]: the task
			// carries the outer unit, which decides both.
			name: "nested units of different sources: the outermost succeeding handles both",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				ordinary(r, 1)
				unitTask(r, 2, 1, 0)
				unitTask(r, 3, 1, 0, 1)
				unitTask(r, 4, 1, 0)
				r.Fail(0, "a broke")
				r.Fail(1, "b broke")
				r.Ran(2, true)
				r.Ran(3, true)
				r.Ran(4, true)
				if got := r.Decide(1, false); !slices.Equal(got, []int{0, 1}) {
					t.Errorf("Decide = %v, want [0 1]", got)
				}
			},
		},
		{
			name: "nested units of different sources: a failure anywhere in the outermost leaves the inner source held",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				ordinary(r, 1)
				unitTask(r, 2, 1, 0)
				unitTask(r, 3, 1, 0, 1)
				unitTask(r, 4, 1, 0)
				r.Fail(0, "a broke")
				r.Fail(1, "b broke")
				r.Ran(2, true)
				r.Ran(3, true)
				r.Ran(4, true)
				r.Fail(4, "o2 broke")
				if got := r.Decide(1, false); got != nil {
					t.Errorf("Decide = %v, want nothing handled", got)
				}
				if !r.Held(1) {
					t.Error("b handled although its outermost unit failed")
				}
			},
		},
		{
			name: "a unit naming an applier names its children",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				ordinary(r, 1)
				terminalTask(r, 2, 0, 0, 1)
				unitTask(r, 3, 1, 2)
				r.Fail(1, "c1 broke")
				if !r.Fires(1) || !r.NamesHeld(3) {
					t.Fatalf("fires=%v namesHeld=%v, want a child's failure to fire the unit", r.Fires(1), r.NamesHeld(3))
				}
				r.Ran(3, true)
				if got := r.Decide(1, false); !slices.Equal(got, []int{1}) {
					t.Errorf("Decide = %v, want [1]", got)
				}
			},
		},
		{
			name: "a task naming a handled source no longer fires",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				ordinary(r, 2, 0)
				r.Fail(0, "src broke")
				if !r.NamesHeld(2) {
					t.Fatal("a task naming a held source does not fire")
				}
				r.Ran(1, true)
				r.Decide(1, false)
				if r.NamesHeld(2) {
					t.Error("a task naming a handled source still fires")
				}
			},
		},
		{
			name: "the fold skips a handled child",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "c0 broke")
				if !r.Counts(0) {
					t.Fatal("a held child does not count")
				}
				r.Ran(1, true)
				r.Decide(1, false)
				if r.Counts(0) {
					t.Error("a handled child counts")
				}
			},
		},
		{
			name: "the fold skips a unit task's failure once its unit's sources are no longer held",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				unitTask(r, 2, 2, 0)
				r.Fail(0, "c0 broke")
				r.Ran(1, true)
				r.Fail(1, "c1 broke")
				r.Decide(1, false)
				if !r.Counts(1) {
					t.Fatal("a failed unit task whose source is still held does not count")
				}
				r.Ran(2, true)
				r.Decide(2, false)
				if r.Counts(1) {
					t.Error("a failed unit task whose source a later unit handled counts")
				}
			},
		},
		{
			name: "a unit the driver stopped handles nothing",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Ran(1, true)
				if got := r.Decide(1, true); got != nil {
					t.Errorf("Decide(stopped) = %v, want nothing handled", got)
				}
				if !r.Held(0) {
					t.Error("source handled by a stopped unit")
				}
			},
		},
		{
			name: "a unit is decided once",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Ran(1, true)
				r.Decide(1, true)
				if got := r.Decide(1, false); got != nil {
					t.Errorf("second Decide = %v, want nothing: the first decision stands", got)
				}
			},
		},
		{
			name: "a unit task failing after its unit was decided does not flip the decision",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Ran(1, true)
				r.Decide(1, false)
				r.Fail(1, "late")
				if r.UnitFailed(1) || r.Held(0) {
					t.Errorf("unitFailed=%v held=%v, want the decided unit to stand", r.UnitFailed(1), r.Held(0))
				}
			},
		},
		{
			name: "a source failing again is one source, and a handled one stays handled",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				unitTask(r, 1, 1, 0)
				r.Fail(0, "src broke")
				r.Fail(0, "src broke")
				r.Ran(1, true)
				r.Decide(1, false)
				r.Fail(0, "src broke")
				if r.Held(0) || r.AnyHeld() {
					t.Errorf("held=%v anyHeld=%v, want the source handled once and for all", r.Held(0), r.AnyHeld())
				}
			},
		},
		{
			name: "a reopened source is held again, and a position that is no handled source is left as it is",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				ordinary(r, 1)
				unitTask(r, 2, 1, 0)
				unitTask(r, 3, 2, 1)
				r.Fail(0, "src broke")
				r.Fail(1, "other broke")
				r.Ran(2, true)
				r.Decide(1, false)
				r.Reopen(0)
				if !r.Held(0) || !r.Fires(1) {
					t.Fatalf("held=%v fires=%v, want the reopened source held and its unit firing", r.Held(0), r.Fires(1))
				}
				r.Reopen(1)
				r.Reopen(2)
				r.Ran(3, true)
				if got := r.Decide(2, false); !slices.Equal(got, []int{1}) || r.AnyHeld() != true {
					t.Errorf("Decide(u2) = %v anyHeld=%v, want [1] with the reopened source still held", got, r.AnyHeld())
				}
				if r.Held(2) {
					t.Error("reopening a unit task made it a source")
				}
			},
		},
		{
			name: "a unit's sources: what its tasks name, an applier standing for its children, in plan order",
			run: func(t *testing.T, r *RescueRule) {
				ordinary(r, 0)
				ordinary(r, 1)
				terminalTask(r, 2, 0, 0, 1)
				ordinary(r, 3)
				unitTask(r, 4, 1, 3)
				unitTask(r, 5, 1, 2)
				if got := r.Sources(1); !slices.Equal(got, []int{0, 1, 3}) {
					t.Errorf("Sources = %v, want [0 1 3]", got)
				}
				if got := r.Sources(9); got != nil {
					t.Errorf("Sources of an unknown unit = %v, want none", got)
				}
			},
		},
		{
			name: "a negative position names nothing",
			run: func(t *testing.T, r *RescueRule) {
				unitTask(r, 0, 1, -1)
				r.Fail(-1, "tied to no task")
				if r.Fires(1) || r.NamesHeld(0) {
					t.Errorf("fires=%v namesHeld=%v, want a negative position to name nothing", r.Fires(1), r.NamesHeld(0))
				}
			},
		},
		{
			// Failed in time order b, c, a, d; named in reverse; c failed with no
			// error; d was handled by an earlier unit.
			name: "held errors: the unit's held sources in plan order, joined",
			run: func(t *testing.T, r *RescueRule) {
				for pos := range 4 {
					ordinary(r, pos)
				}
				unitTask(r, 4, 2, 3)
				unitTask(r, 5, 1, 2, 1)
				unitTask(r, 6, 1, 0)
				unitTask(r, 7, 1, 3)
				r.Fail(1, "b broke")
				r.Fail(2, "")
				r.Fail(0, "a broke")
				r.Fail(3, "d broke")
				r.Ran(4, true)
				r.Decide(2, false)
				if got, want := r.HeldErrors(1), "a broke; b broke"; got != want {
					t.Errorf("HeldErrors = %q, want %q", got, want)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, NewRescueRule())
		})
	}
}
