package render

import (
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/config"
)

// applyConcurrency threads the intra-host concurrency keys (ADR-0075) from a
// parsed task onto its RenderedTask: `async:` as-is, `require:` split into its two
// mutually exclusive forms — the scalar sets RequireAll, the list is parked in
// requireNames for the resolveRequire pass. One helper for every render site, so a
// site cannot thread one key and quietly forget the other.
func applyConcurrency(rt *RenderedTask, task config.Task) {
	rt.Async = task.Async
	if all, names, ok := task.RequireSpec(); ok {
		rt.RequireAll = all
		rt.requireNames = names
	}
}

// ErrRequireUnknownRegister — `require:` references a register name that doesn't
// exist among the run's task registers. Strict variant, mirroring
// ErrOnChangesUnknownRegister: a typo'd barrier would silently degrade into "wait
// for nothing" and the ordering the author declared would never happen.
var ErrRequireUnknownRegister = errors.New("render: require references a non-existent register")

// ErrRequireCrossPassage — `require:` names a source that lands in a LATER
// Passage than the task awaiting it (ADR-056 stratification). The two travel in
// different ApplyRequests, and a Passage is dispatched only after the previous one
// closed on every host: the consumer would run to completion before its source is
// ever started, and the wire has no way to say "wait for a task in a message you
// have not received".
//
// ★ Not the same situation as a cross-passage onchanges/onfail, which ADR-056 R3
// SUPPORTS by resolving the link Keeper-side (crosspassage.go) from the previous
// Passages' accumulated CHANGED/FAILED facts. That works because the question
// there is "did the source change?", and the audit log holds the answer for a
// source that already ran. Here the question is "wait for it", and no Keeper-side
// fact can make a consumer that has already been dispatched wait for a Passage
// that has not started. So this one is a fail-closed reject at render.
//
// The opposite direction is fine and is NOT rejected: a source in an EARLIER
// Passage is already finalized (an async flow never outlives its Passage,
// ADR-0075(d)), so the barrier is satisfied before the consumer's ApplyRequest is
// even assembled.
var ErrRequireCrossPassage = errors.New("render: require references a task in a later Passage")

// resolveRequire turns `require:` register names (RenderedTask.requireNames) into
// task indexes (RenderedTask.RequireIdx) across the whole run plan, and checks the
// Passage invariant on each resolved link. Called by [Pipeline.Render]'s final pass
// alongside resolveOnChanges/resolveOnFail, once every Index/Register/Passage is
// known.
//
// `require: all` carries no names — it is threaded as RequireAll by the render
// sites and never reaches here (the two forms are mutually exclusive).
//
// Unknown name → [ErrRequireUnknownRegister]; source in a later Passage →
// [ErrRequireCrossPassage]. Empty requireNames → RequireIdx stays nil (no explicit
// barrier).
func resolveRequire(tasks []*RenderedTask) error {
	byRegister := registerIndex(tasks)
	passageByIndex := passageIndex(tasks)
	for _, t := range tasks {
		if len(t.requireNames) == 0 {
			continue
		}
		// Per NAME rather than over the flattened result: a name that fanned out
		// (`loop:`, NIM-246) contributes several indexes, so a positional pairing
		// of idxs back onto requireNames would name the wrong source in the error
		// — or run off the end of the slice.
		var idxs []int
		for _, name := range t.requireNames {
			nameIdxs, err := resolveRegisterNames(byRegister, []string{name}, t.Name, "require", ErrRequireUnknownRegister)
			if err != nil {
				return err
			}
			for _, srcIdx := range nameIdxs {
				if srcPassage := passageByIndex[srcIdx]; srcPassage > t.Passage {
					return fmt.Errorf("%w: task %q (passage %d) -> require: [%s] (passage %d)",
						ErrRequireCrossPassage, t.Name, t.Passage, name, srcPassage)
				}
			}
			idxs = append(idxs, nameIdxs...)
		}
		t.RequireIdx = idxs
	}
	return nil
}

// passageIndex maps a task's global Index to its Passage. Built over the whole
// plan (including the placeholders a staged render emits for future Passages —
// those carry a real Passage stamp, which is exactly what the invariant needs).
func passageIndex(tasks []*RenderedTask) map[int]int {
	m := make(map[int]int, len(tasks))
	for _, t := range tasks {
		m[t.Index] = t.Passage
	}
	return m
}
