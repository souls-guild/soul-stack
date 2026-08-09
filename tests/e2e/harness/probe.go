//go:build e2e

package harness

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// probeReady sends a GET to probeURL and returns true on 2xx. Used by
// startKeeperRun to poll the keeper process's /readyz.
//
// A 2xx here says "something on this address is healthy", NOT "our keeper is
// up": /readyz is unauthenticated and carries no identity, so any process that
// happens to hold the port satisfies it. assertOwnKeeper is what closes that
// gap — see its doc comment.
//
// One short-timeout client per call — not shared (test environment, low
// frequency, no need for extra global state).
func probeReady(probeURL string) bool {
	cl := &http.Client{Timeout: 1 * time.Second}
	resp, err := cl.Get(probeURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// assertOwnKeeper verifies that the process answering on KeeperHTTPURL is the
// keeper THIS stack started, and fails the stack's construction if it is not.
//
// Why a separate check (NIM-469). probeReady is satisfied by a 2xx from anyone,
// and the address it polls was reserved by opening port :0 and closing it again
// — so if the port changed hands in between, a stack could come up "green"
// pointed at a neighbouring test's keeper and stay that way for its whole run.
// What that looked like was never a port error: it was a 401 on the first
// operator call, or an incarnation-state assert reading an untouched database,
// hundreds of lines later and in a different family each time.
//
// The token is the identity test. It was minted by this stack's `keeper init`
// against a signing key generated randomly into this stack's own Vault, so it
// verifies against this keeper and no other. A 401 here is worth stopping on,
// and stopping by name, instead of surfacing as a mystery further down.
//
// A 401 is NOT by itself proof that the answering process is foreign, and an
// earlier version of this comment said it was. `invalid token` is the keeper's
// catch-all for every validation failure it does not name — it names exactly
// three, expiry, clock skew and a foreign issuer (keeper/internal/jwt/verifier.go,
// ClassifyVerifyErr) — so the detail string is what separates "wrong process"
// from "right process, wrong clock". This check reads it and only blames the
// address when nothing else fits.
func (s *Stack) assertOwnKeeper() error {
	c := s.opClient(s.t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body, status, err := c.get(ctx, "/v1/souls")
	if err != nil {
		return err
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return nil
	}

	hint, advice := identityFailureCause(string(body))
	return fmt.Errorf(
		"keeper identity check failed on %s: status %d (%s), issuer=%q, body=%s\n%s",
		s.KeeperHTTPURL, status, hint, s.issuer, strings.TrimSpace(string(body)), advice)
}

// identityFailureCause turns the keeper's 401/403 body into what the reader
// should go and look at. Split out of assertOwnKeeper because it is the whole
// judgement — everything around it is one HTTP call — and because it is then
// testable without a stack (probe_test.go).
//
// `invalid token` is the ORDINARY shape for a foreign keeper, and the per-stack
// issuer does not change that: it verifies against a different random signing
// key, and the signature is checked before `iss` is looked at
// (keeper/internal/jwt/verifier.go), so a foreign keeper can only ever answer
// this. It stays the default for the same reason it is the keeper's default —
// it is also what a malformed token and a missing claim produce, and none of
// those readings is better than "check the address" from in here.
//
// `token issuer not trusted` would mean something else and worse — a keeper that
// shares our signing key but not our issuer. Two stacks cannot reach that state,
// because generateHS256Key (vault.go) draws fresh randomness per stack, so it
// points at the harness seeding ONE key into several stacks rather than at an
// address that changed hands.
//
// `token issued in the future` is not about the address at all. Since NIM-621
// the verifier absorbs a minute of clock skew and answers this past it, so on a
// box whose wall clock steps backwards — WSL2 resume, an NTP correction,
// observed twice in one session (NIM-611) — this is our own keeper refusing our
// own token. Before NIM-621 it was indistinguishable from a foreign keeper, and
// the reader was sent after a port collision that never happened.
func identityFailureCause(body string) (hint, advice string) {
	switch {
	case strings.Contains(body, "issuer not trusted"):
		return "the answering keeper shares our signing key but pins another issuer",
			"Suspect the harness seeding one signing key into several stacks, not a port collision"
	case strings.Contains(body, "issued in the future"):
		return "our own keeper, refusing our own token over a clock that moved",
			"The host's wall clock stepped backwards by more than the verifier's leeway between " +
				"`keeper init` minting this token and this request; the address is fine, check the clock"
	default:
		return "this process is not the keeper this stack started",
			"The address was reserved and then bound a moment later; if something else took it " +
				"in between, every request this test makes goes to that process instead"
	}
}
