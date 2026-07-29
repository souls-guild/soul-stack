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

// Frame-plane constants. Unlike the values above these are not operator policy
// but flow-control tuning, so they stay in code (same split as the Soul side,
// where chunk size and queue depth are code constants in consolerunner/limits.go).
const (
	// outQueueDepth is how many frames may wait for one WebSocket writer.
	// Sized per SOCKET, not per session: a WebSocket has a single writer, so
	// this is the real queue. 256 frames of a 32 KiB pty chunk is ~8 MiB
	// worst-case per operator — the point past which we drop rather than let a
	// browser's backlog become Keeper's memory leak.
	outQueueDepth = 256

	// pongWait is how long the peer may stay silent before we consider the
	// socket dead. TCP alone will not tell us: a laptop that slept keeps a
	// half-open connection for many minutes, and every session behind it is a
	// live root shell.
	pongWait = 60 * time.Second

	// writeWait bounds one WebSocket write, on the same budget as pongWait.
	// Backpressure fills the socket buffer by construction, so a write parks
	// for as long as the operator takes to drain; a shorter budget would be a
	// second, stricter liveness rule that kills a merely slow browser and every
	// pty behind it (NIM-242).
	writeWait = pongWait

	// pingPeriod must be shorter than pongWait, or we would time out a healthy
	// peer between our own pings.
	pingPeriod = (pongWait * 9) / 10

	// maxClientFrameSize caps one inbound JSON frame. Keystrokes and resizes are
	// tiny; a paste is the large case, and 1 MiB is far above any of them.
	maxClientFrameSize = 1 << 20
)

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
