package api

import "time"

// Re-authorization of the long-lived operator channels (NIM-844).
//
// Every other route on this surface decides access per request, so revocation
// reaches it within one RBAC snapshot refresh ([rbac.DefaultRefreshInterval]) —
// `apimiddleware.RejectRevoked` sits on the chain and refuses the next call.
// Two channels on THIS surface have no next call: the console WebSocket
// (`GET /v1/console`) and the run-events SSE stream
// (`GET /v1/incarnations/{id}/runs/{apply_id}/events`) are authorized when they
// open and then carry traffic for as long as the operator keeps them — unbounded
// for a PTY, up to [sseMaxLifetime] for a stream. A per-request middleware cannot
// reach a request already inside its handler, so the channel has to ask again on
// its own.
//
// There is a THIRD, and it is not fixed here: `GET /mcp/events` on the MCP
// listener (`keeper/internal/mcp/sse.go`) authorizes once in `authorizeSSE` and
// streams for the same 30 minutes, and `RejectRevoked` deliberately does not run
// on that listener at all (middleware/rbac.go, NIM-551). ADR-068 §A3 wrote the
// /v1 stream as a narrow DUPLICATE of that code rather than sharing it, which is
// exactly how a fix reaches one and not the other. Tracked as NIM-858; do not
// read this file as covering the MCP plane.
//
// ADR-014's amendment is titled "JWT immediate revoke", and the console is
// precisely where "immediate" is load-bearing: the channel carries an arbitrary
// root shell.
//
// WHAT IS RE-ASKED, and it is deliberately not the whole opening decision: the
// facts that cannot change are not re-read. The run's initiator and the
// incarnation an apply_id belongs to are fixed once the row exists, so the SSE
// stream keeps them from its first (and only) database read and re-evaluates
// only the RBAC half. The console does re-read the target host's Coven labels,
// because those are mutable and a label moving is one of the ways a scope
// narrows.
const longLivedReauthInterval = 15 * time.Second

// WHAT IS NOT RE-ASKED: the token's own `exp`. Both channels verify it once, at
// the upgrade and at the open, and then never again — so an operator whose JWT
// ages out keeps a channel that every ordinary route would refuse them. That is
// not an oversight of this file but a decision nobody has made: ADR-014 names the
// short TTL as the compensating control for revocation, and closing a terminal
// under a half-typed command is a product call, not a bug fix. Tracked as
// NIM-861. Until it is answered, the re-check below is the only thing bounding an
// established channel, and it is bounded by the RBAC snapshot rather than by the
// credential.

// Why 15s rather than per-event or per-keystroke: the cost of a re-check must
// not scale with traffic. A busy stream delivers thousands of events and a
// terminal a keystroke per character, so putting the check on the data path
// would charge an RBAC evaluation — and, for the console, an indexed SELECT —
// to every one of them. On a timer the cost is per channel per interval
// regardless of how loud the channel is.
//
// The interval is the same order as the snapshot TTL it rides on (10s): the
// answer cannot be fresher than the snapshot, so spending more checks to beat
// it buys nothing. Worst case from `POST /v1/operators/{aid}/revoke` to a dead
// shell is one refresh plus one interval.
