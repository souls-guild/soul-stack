package render

import (
	"maps"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

// A linear run's hosts as each step saw them (ADR-0091 amendment NIM-993, rules 2-3,
// "What the run goroutine keeps"). A step reads a host's standing as it stood when
// that step was dispatched, so every pass renders an earlier step the same way: a
// host held or dropped later is live there, and one that returned later is still
// held there — a return would otherwise grow the roster of the steps it missed.

// Standing is the hosts of a linear run and the steps at which each left or
// rejoined it. nil — the caller's Hosts, HeldHosts, Dropped and RegisterByHost hold
// for every step.
type Standing struct {
	// Roster — every host of the run, live, held, returned or dropped, in SID order.
	Roster []*topology.HostFacts
	// BySID — the hosts whose standing changed. A host absent here is live at every
	// step.
	BySID map[string]HostStanding
}

// HostStanding is when one host's standing changed; a nil step never came.
type HostStanding struct {
	// Periods — every time the host was held, in step order, none overlapping the
	// next: a returned host can fail again with another unit ahead and be held again
	// (rule 2).
	Periods []HoldPeriod
	// DroppedAt — the first step at which it is dropped. It overrides Periods: a host
	// dropped with sources still held is dropped, not held.
	DroppedAt *int
}

// HoldPeriod is one stretch of steps a host was held for.
type HoldPeriod struct {
	// HeldFrom — the first step at which the host is held (rule 2): the one after the
	// barrier that held its failure.
	HeldFrom int
	// ReturnedAt — the first step at which it is back in the run (rule 3): the one
	// after the barrier of the unit that handled the last failure it held or, past a
	// default block whose hosts were chosen while it was held, the first step after
	// that block. nil — the host has not returned.
	ReturnedAt *int
	// Synth — register name → the register a skipped task carries, for every task the
	// host missed until it rejoins (rule 3), the rest of a default block whose hosts
	// were chosen while it was held included. No apply_runs row stands behind one, so
	// it is laid under the host's own registers, and only from ReturnedAt on.
	Synth map[string]any
}

type standingKind int

const (
	standingLive standingKind = iota
	standingHeld
	standingDropped
)

func (h HostStanding) at(step int) standingKind {
	if reachedBy(h.DroppedAt, step) {
		return standingDropped
	}
	for _, p := range h.Periods {
		if p.HeldFrom <= step && !reachedBy(p.ReturnedAt, step) {
			return standingHeld
		}
	}
	return standingLive
}

// synthAt is the registers synthesized for the periods the host returned from by
// step; nil for none. They stay from the return on whatever the host's standing, as
// the registers of an apply_runs row do. A return at or after DroppedAt never
// happened: a drop cannot return the host ("What the run goroutine keeps"). The stop
// at a period still open is defensive; Periods never has one before another.
func (h HostStanding) synthAt(step int) map[string]any {
	var out map[string]any
	for _, p := range h.Periods {
		if p.ReturnedAt == nil {
			break
		}
		if len(p.Synth) == 0 || !reachedBy(p.ReturnedAt, step) || reachedBy(h.DroppedAt, *p.ReturnedAt) {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(p.Synth))
		}
		maps.Copy(out, p.Synth)
	}
	return out
}

func reachedBy(at *int, step int) bool {
	return at != nil && *at <= step
}

// ForStep is in as step sees the run: its live, held and dropped hosts as they stood
// at that step, and the registers synthesized for each period a host had returned
// from by then — in `register.<name>` and in `register.hosts.<name>` alike, both
// read RegisterByHost. They are laid beneath the host's own registers as `in`
// carried them before any ForStep, so it may be applied to its own result. nil
// Standing — in unchanged.
func (in RenderInput) ForStep(step int) RenderInput {
	s := in.Standing
	if s == nil {
		return in
	}
	if !in.registerBaseSet {
		in.registerBase, in.registerBaseSet = in.RegisterByHost, true
	}
	in.Hosts, in.HeldHosts, in.Dropped = nil, nil, nil
	reg := in.registerBase
	cloned := false
	for _, h := range s.Roster {
		hs := s.BySID[h.SID]
		switch hs.at(step) {
		case standingDropped:
			in.Dropped = append(in.Dropped, h)
		case standingHeld:
			in.HeldHosts = append(in.HeldHosts, h)
		default:
			in.Hosts = append(in.Hosts, h)
		}
		synth := hs.synthAt(step)
		if synth == nil {
			continue
		}
		if !cloned {
			reg = maps.Clone(reg)
			if reg == nil {
				reg = map[string]map[string]any{}
			}
			cloned = true
		}
		own := make(map[string]any, len(reg[h.SID])+len(synth))
		maps.Copy(own, synth)
		maps.Copy(own, reg[h.SID])
		reg[h.SID] = own
	}
	in.RegisterByHost = reg
	return in
}

// keepHosts is the hosts among hosts whose SID is in keep, in their order.
func keepHosts(hosts []*topology.HostFacts, keep map[string]bool) []*topology.HostFacts {
	out := make([]*topology.HostFacts, 0, len(hosts))
	for _, h := range hosts {
		if keep[h.SID] {
			out = append(out, h)
		}
	}
	return out
}

// sidSet is the SIDs of hosts; nil for none.
func sidSet(hosts []*topology.HostFacts) map[string]bool {
	if len(hosts) == 0 {
		return nil
	}
	out := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		out[h.SID] = true
	}
	return out
}
