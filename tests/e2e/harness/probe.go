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
// verifies against this keeper and no other. Any 401 here means the answering
// process is not ours — reported now, by name, instead of as a mystery further
// down.
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

	// `invalid token` is the ORDINARY shape here, not a puzzling one, and the
	// per-stack issuer does not change that: a foreign keeper verifies against a
	// different random signing key, and the signature is checked before `iss` is
	// looked at (keeper/internal/jwt/verifier.go), so a foreign keeper can only
	// ever answer this. The 401 itself is the whole signal.
	//
	// `token issuer not trusted` would mean something else and worse — a keeper
	// that shares our signing key but not our issuer. Two stacks cannot reach
	// that state, because generateHS256Key (vault.go) draws fresh randomness per
	// stack, so it points at the harness seeding ONE key into several stacks
	// rather than at an address that changed hands.
	hint := "this process is not the keeper this stack started"
	if strings.Contains(string(body), "issuer not trusted") {
		hint = "the answering keeper shares our signing key but pins another issuer — " +
			"suspect the harness seeding one key into several stacks, not a port collision"
	}
	return fmt.Errorf(
		"keeper identity check failed on %s: status %d (%s), issuer=%q, body=%s\n"+
			"The address was reserved and then bound a moment later; if something else took it "+
			"in between, every request this test makes goes to that process instead",
		s.KeeperHTTPURL, status, hint, s.issuer, strings.TrimSpace(string(body)))
}
