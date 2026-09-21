package middleware

import "time"

// Re-authorization of the long-lived operator channels (NIM-844, NIM-858).
//
// Every other route on an operator surface decides access per request, so
// revocation reaches it within one RBAC snapshot refresh
// ([rbac.DefaultRefreshInterval]) — [RejectRevoked] sits on the chain and refuses
// the next call. Three channels have no next call:
//
//   - the console WebSocket, `GET /v1/console`;
//   - the run-events SSE stream,
//     `GET /v1/incarnations/{id}/runs/{apply_id}/events`;
//   - the MCP apply-event stream, `GET /mcp/events`, which is on a separate
//     listener that [RejectRevoked] deliberately does not run on at all (NIM-551).
//
// Each is authorized when it opens and then carries traffic for as long as the
// operator keeps it — unbounded for a PTY, up to the stream's max lifetime
// otherwise. A per-request middleware cannot reach a request already inside its
// handler, so the channel has to ask again on its own.
//
// THIS FILE IS THE MECHANISM, and it lives here rather than beside any one of
// them because there were three of them and the fix reached one at a time. ADR-068
// §A3 wrote the /v1 stream as a narrow DUPLICATE of the MCP one rather than
// sharing it, and NIM-844 then fixed the duplicate and not the original. A guard
// over the inventory is in longlived_guard_test.go: a fourth channel is red on the
// day it is written.
//
// ADR-014's amendment is titled "JWT immediate revoke", and the console is
// precisely where "immediate" is load-bearing: the channel carries an arbitrary
// root shell.
//
// WHAT IS RE-ASKED, and it is deliberately not the whole opening decision: the
// facts that cannot change are not re-read. The run's initiator and the
// incarnation an apply_id belongs to are fixed once the row exists, so both
// streams keep them from their first (and only) database read and re-evaluate only
// the RBAC half. The console does re-read the target host's Coven labels, because
// those are mutable and a label moving is one of the ways a scope narrows.
//
// WHAT IS NOT RE-ASKED: the token's own `exp`. All three verify it once, at the
// upgrade and at the open, and then never again — so an operator whose JWT ages
// out keeps a channel that every ordinary route would refuse them. That is not an
// oversight of this file but a decision nobody has made: ADR-014 names the short
// TTL as the compensating control for revocation, and closing a terminal under a
// half-typed command is a product call, not a bug fix. Tracked as NIM-861. Until
// it is answered, the re-check below is the only thing bounding an established
// channel, and it is bounded by the RBAC snapshot rather than by the credential.
//
// Why 15s rather than per-event or per-keystroke: the cost of a re-check must not
// scale with traffic. A busy stream delivers thousands of events and a terminal a
// keystroke per character, so putting the check on the data path would charge an
// RBAC evaluation — and, for the console, an indexed SELECT — to every one of
// them. On a timer the cost is per channel per interval regardless of how loud the
// channel is.
//
// The interval is the same order as the snapshot TTL it rides on (10s): the answer
// cannot be fresher than the snapshot, so spending more checks to beat it buys
// nothing. Worst case from `POST /v1/operators/{aid}/revoke` to a dead shell is one
// refresh plus one interval.
const LongLivedReauthInterval = 15 * time.Second

// ReauthTicker is the ticker an established channel re-decides on. The caller
// stops it.
//
// override is for tests only, and exists because the production interval is too
// long to wait out: a test that wanted the SECOND check would otherwise sleep 30
// seconds. A non-positive override takes [LongLivedReauthInterval], so a channel
// that simply leaves the field alone is on the production cadence.
//
// A trivial wrapper over time.NewTicker, and deliberately so: it is the symbol the
// inventory guard looks for. A channel that re-checks by hand-rolling its own
// ticker is a channel whose cadence can drift from this file's reasoning, and the
// guard reads this call, per file, as the evidence that it has not. Per file and
// not per channel — see that guard's own limits.
func ReauthTicker(override time.Duration) *time.Ticker {
	if override <= 0 {
		override = LongLivedReauthInterval
	}
	return time.NewTicker(override)
}
