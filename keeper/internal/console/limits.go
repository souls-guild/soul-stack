package console

import "time"

// Resource envelope of the Keeper-side console plane.
//
// The split against the Soul side is deliberate: `console:` in `soul.yml` is
// HOST policy (may a shell run here at all, how many on this machine), while
// these are OPERATOR policy (how many terminals one Archon may hold, how much
// output Keeper is willing to buffer for a browser). A host cannot answer the
// operator question — a multi-console wall of 30 hosts is one session on each,
// and only Keeper sees the whole wall.
const (
	// DefaultMaxSessionsPerAID caps live consoles per Archon. The wall is the
	// sizing case: an operator opens one terminal per selected host, so this is
	// "how wide may a wall be". 30 covers the walls the UI is built for
	// (docs/keeper/console.md); beyond that an operator is not reading output,
	// they are running a fan-out — that is what an Errand is for.
	DefaultMaxSessionsPerAID = 30

	// DefaultMaxSessionsGlobal caps live consoles on ONE Keeper instance across
	// all operators. A backstop against many operators each within their own
	// limit: every session costs a pty on some host plus a buffer here.
	DefaultMaxSessionsGlobal = 256

	// DefaultIdleTimeout closes a session with no operator input for this long.
	// An abandoned terminal is a root shell nobody is watching, so it is closed
	// on the Keeper side even though the socket is still healthy. Output alone
	// does NOT count as activity — a `tail -f` left running is exactly the case
	// this is for. 0 disables it.
	DefaultIdleTimeout = 30 * time.Minute
)

// The frame plane — queue depth, socket deadlines, inbound frame cap — is NOT
// here. It belongs to the WebSocket implementation in internal/api, which is
// the only thing that reads it; this package owns operator policy, above. A
// second copy lived here until NIM-255, referenced by nothing, and both ways it
// could go wrong were silent: editing the dead copy did nothing, and editing
// the live one drifted from the comment that called them mirrors. limits_test.go
// keeps the split.

// Limits is the resolved operator-facing envelope, wired from keeper.yml.
// Zero fields resolve to the defaults above, so the zero value is usable.
type Limits struct {
	MaxSessionsPerAID int
	MaxSessionsGlobal int
	IdleTimeout       time.Duration
}

// resolve fills zero fields with the defaults. A negative value means the
// caller explicitly disabled the limit and is kept as-is by the checks
// (which only enforce a positive ceiling).
func (l Limits) resolve() Limits {
	if l.MaxSessionsPerAID == 0 {
		l.MaxSessionsPerAID = DefaultMaxSessionsPerAID
	}
	if l.MaxSessionsGlobal == 0 {
		l.MaxSessionsGlobal = DefaultMaxSessionsGlobal
	}
	if l.IdleTimeout == 0 {
		l.IdleTimeout = DefaultIdleTimeout
	}
	return l
}
