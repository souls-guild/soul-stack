package config

import (
	"slices"
	"strings"
)

// RescueRule is one host's handling state under ADR-0091 amendment NIM-993: when
// a unit succeeds (rule 1), which sources the first unit that succeeds handles
// (rule 3), and the applier fold (rule 6). The Soul drives it in message order
// and the Keeper at its step barriers, so both judge a unit the same way.
//
// Positions and units are the driver's: a message position or a plan index, and
// a unit id with 0 for no unit. The rule never decides a unit by itself — a unit
// stands undecided until the driver calls [RescueRule.Decide].
//
// A source is a failed task of no unit. A unit task's failure fails its unit, which
// means "not handled" for the unit's sources — save where its unit does not fire, where
// it is a source like any other (the net of rule 6).
type RescueRule struct {
	tasks map[int]ruleTask
	units map[int]*ruleUnit
	// failures holds every observed source.
	failures map[int]ruleFailure
	held     int
}

type ruleTask struct {
	unit     int
	terminal bool
	children []int
	// named is what the task's `onfail:` names, a terminal standing for its
	// children.
	named []int
}

type ruleUnit struct {
	// sources are what the unit's tasks name, in plan order. A task nested in a
	// unit of another source carries the outer unit, so the outermost unit decides
	// every source it holds (rule 1, nested units of different sources).
	sources  []int
	executed bool
	failed   bool
	decided  bool
	// sourceFailed — a source of it failed while it was registered and undecided:
	// in its own message, since a unit is registered with its message and decided
	// at its end. It fired then, even if a unit before it has handled that source
	// since.
	sourceFailed bool
	// lost — a task of it failed where it did not fire. That failure is a source of
	// its own (the net of rule 6), not the unit's, but the unit can no longer
	// succeed.
	lost bool
}

type ruleFailure struct {
	err     string
	handled bool
}

// NewRescueRule returns a rule with no task registered.
func NewRescueRule() *RescueRule {
	return &RescueRule{
		tasks:    map[int]ruleTask{},
		units:    map[int]*ruleUnit{},
		failures: map[int]ruleFailure{},
	}
}

// Task registers the task at pos. named is its `onfail:` as positions; a
// negative one names nothing — a source `where:` filtered out on this host, or
// one the driver has no position for. A terminal (an applier's register task)
// lists its children, and naming it names them: the terminal is never a source,
// and `onfail: [<applier>]` means a failure its fold reads (rule 6).
//
// A terminal expands only once registered: one named before it is registered (a
// forward `onfail:`) names only itself. That is harmless, because no unit can
// hold a later source — onfail_names_rescue keeps a named applier outside the
// unit, and a unit is decided before later children run.
func (r *RescueRule) Task(pos, unit int, named []int, terminal bool, children []int) {
	t := ruleTask{unit: unit, terminal: terminal, children: children}
	for _, s := range named {
		if n := r.tasks[s]; n.terminal {
			for _, c := range n.children {
				t.named = addPosition(t.named, c)
			}
			continue
		}
		t.named = addPosition(t.named, s)
	}
	r.tasks[pos] = t
	if unit == 0 {
		return
	}
	u := r.unit(unit)
	for _, s := range t.named {
		if i, found := slices.BinarySearch(u.sources, s); !found {
			u.sources = slices.Insert(u.sources, i, s)
		}
	}
}

func addPosition(xs []int, pos int) []int {
	if pos < 0 || slices.Contains(xs, pos) {
		return xs
	}
	return append(xs, pos)
}

func (r *RescueRule) unit(id int) *ruleUnit {
	u := r.units[id]
	if u == nil {
		u = &ruleUnit{}
		r.units[id] = u
	}
	return u
}

// Ran records whether the task at pos executed — any outcome but SKIPPED. A
// terminal runs no module of the unit, so it never counts (rule 1).
func (r *RescueRule) Ran(pos int, executed bool) {
	if t := r.tasks[pos]; executed && t.unit != 0 && !t.terminal {
		r.unit(t.unit).executed = true
	}
}

// Fail records the failure of the task at pos, msg being its error. A unit task
// fails its unit, unless the unit is already decided. A terminal is ignored: the
// Soul never reports one FAILED, and the Keeper records one FAILED only beside
// another failure of its message (rule 1). Any other task becomes a held source, and
// so does a unit task whose unit does not fire: none of its sources is held, and none
// failed while the unit was registered (the net of rule 6). That unit can no longer
// succeed. The Soul never runs such a task, so only the Keeper meets one.
//
// Idempotent: a failure observed at several barriers is one source, and a source
// a unit handled stays handled.
func (r *RescueRule) Fail(pos int, msg string) {
	t := r.tasks[pos]
	switch {
	case t.terminal:
		return
	case t.unit != 0 && r.unit(t.unit).decided:
		return
	case t.unit != 0 && (r.Fires(t.unit) || r.unit(t.unit).sourceFailed):
		r.unit(t.unit).failed = true
		return
	case t.unit != 0:
		r.unit(t.unit).lost = true
	}
	if _, seen := r.failures[pos]; seen {
		return
	}
	r.failures[pos] = ruleFailure{err: msg}
	r.held++
	for _, u := range r.units {
		if _, holds := slices.BinarySearch(u.sources, pos); holds && !u.decided {
			u.sourceFailed = true
		}
	}
}

// Decide decides unit and returns the sources it handled, in plan order. A unit
// succeeds when one of its tasks executed and none failed, and then it handles
// every source it holds. One that failed — where it fired or where it did not — in
// which nothing executed, or that the driver stopped (a Cancel reached it, rule 9)
// handles nothing, and the next unit of its sources gets its chance (rule 3). A unit
// is decided once: a later call returns nil.
func (r *RescueRule) Decide(unit int, stopped bool) (handled []int) {
	u := r.unit(unit)
	if u.decided {
		return nil
	}
	u.decided = true
	if stopped || !u.executed || u.failed || u.lost {
		return nil
	}
	for _, s := range u.sources {
		if r.Held(s) {
			r.failures[s] = ruleFailure{err: r.failures[s].err, handled: true}
			r.held--
			handled = append(handled, s)
		}
	}
	return handled
}

// Reopen sets the source at pos back to held although a unit handled it: the
// fail-closed verdict on a message reported failed whose replay left every source
// of it handled (rule 6). A position that is no source, or one still held, is left
// as it is.
func (r *RescueRule) Reopen(pos int) {
	f, failed := r.failures[pos]
	if !failed || !f.handled {
		return
	}
	r.failures[pos] = ruleFailure{err: f.err}
	r.held++
}

// Sources returns the positions unit holds, in plan order: what its tasks name, a
// named applier standing for its children.
func (r *RescueRule) Sources(unit int) []int {
	u := r.units[unit]
	if u == nil {
		return nil
	}
	return slices.Clone(u.sources)
}

// Held reports whether the task at pos is a source no unit handled.
func (r *RescueRule) Held(pos int) bool {
	f, failed := r.failures[pos]
	return failed && !f.handled
}

// AnyHeld reports whether some source is held.
func (r *RescueRule) AnyHeld() bool { return r.held > 0 }

// Fires reports whether one of unit's sources is held.
func (r *RescueRule) Fires(unit int) bool {
	u := r.units[unit]
	return u != nil && slices.ContainsFunc(u.sources, r.Held)
}

// NamesHeld reports whether the task at pos names a held source. A source a unit
// handled no longer satisfies an `onfail:`, and a unit task's failure satisfies one
// only as a net source, reached through an applier's children.
func (r *RescueRule) NamesHeld(pos int) bool {
	return slices.ContainsFunc(r.tasks[pos].named, r.Held)
}

// UnitOf returns the unit of the task at pos, 0 for none.
func (r *RescueRule) UnitOf(pos int) int { return r.tasks[pos].unit }

// UnitFailed reports whether a task of unit failed before it was decided.
func (r *RescueRule) UnitFailed(unit int) bool {
	u := r.units[unit]
	return u != nil && u.failed
}

// Counts reports whether a failure of the task at pos counts toward the fold of
// an applier over it: an applier fails only by an unhandled failure of its
// children (rule 6). A source a unit handled does not count, nor does a unit
// task's failure once its unit's sources are no longer held, since that failure
// only ever meant "not handled". A unit task's failure where its unit did not fire
// is a source, and counts until a unit handles it. The driver reads it where it
// takes the fold — at the terminal — so a child handled after that still counted
// there.
//
// Only the Soul reads it, where it folds a message's applier, and the Soul never
// records a unit task that does not fire: it skips the task before running it. The
// Keeper does not fold through Counts. Its fold is the requisite gate's
// (crossPassageGate), over the sources no unit handled (failureView.unhandledKeys).
func (r *RescueRule) Counts(pos int) bool {
	if f, failed := r.failures[pos]; failed {
		return !f.handled
	}
	if u := r.tasks[pos].unit; u != 0 {
		return r.Fires(u)
	}
	return true
}

// HeldErrors returns the errors of unit's held sources in plan order, joined with
// "; " — `core.task.failed`'s default message (rule 8). An empty error is left
// out; "" when none is left.
func (r *RescueRule) HeldErrors(unit int) string {
	u := r.units[unit]
	if u == nil {
		return ""
	}
	var msgs []string
	for _, s := range u.sources {
		if f := r.failures[s]; r.Held(s) && f.err != "" {
			msgs = append(msgs, f.err)
		}
	}
	return strings.Join(msgs, "; ")
}
