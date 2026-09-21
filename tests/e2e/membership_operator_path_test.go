//go:build e2e

// L3a contract-e2e: membership is bound through the OPERATOR route (NIM-231).
//
// WHY THIS EXISTS. NIM-209 gave membership an operator path — `POST
// /v1/incarnations/{name}/members` — because until then a host could only be
// bound from inside a scenario run, and a create over a ready roster was
// unreachable through the API. The harness had grown a direct
// `INSERT INTO incarnation_membership` to work around exactly that gap, and it
// kept using it afterwards. Two things followed:
//
//   - the route had unit and integration guards but nothing exercising the real
//     HTTP path, so a regression in middleware, the huma schema or the SID dedup
//     would not have been caught anywhere;
//   - every suite bound hosts along a path an operator cannot take. A direct
//     INSERT skips both authorization gates and the `connected` status rule, so
//     tests went green in situations where a live operator gets 403 or 422.
//     A harness that is more permissive than production tests the wrong system.
//
// Stack.AddMember now calls the route, so every existing suite exercises it as a
// side effect. This file pins the two things that side effect cannot state on
// its own: that the route really wrote the row, and that it still REFUSES what
// an operator would be refused.
package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EMembership_BoundThroughOperatorRoute(t *testing.T) {
	const (
		serviceName = "service-noop"
		examplePath = "examples/service/noop"
		incName     = "membership-op-path"
	)

	stack := harness.NewStack(t, harness.Config{ExamplePath: examplePath, Souls: 2})
	defer stack.Cleanup()

	stack.RegisterService(t, serviceName, examplePath)
	stack.ConnectSoulStub(t, 0)
	stack.ConnectSoulStub(t, 1)
	stack.SeedIncarnationReady(t, incName, serviceName, "main", map[string]any{})

	// ── The route writes the row ────────────────────────────────────────────
	// The assertion is not "AddMember did not fatal": it is that the membership
	// relation gained exactly this SID, through the HTTP surface, with the
	// gates applied.
	stack.AddMember(t, 0, incName)
	if got := memberSIDs(t, stack, incName); len(got) != 1 || got[0] != stack.SoulSID(0) {
		t.Fatalf("after the operator bind, members = %v, want [%s]", got, stack.SoulSID(0))
	}

	// Idempotent, and legibly so: a re-bind is a 200 that reports the SID as
	// already a member rather than binding it twice (ADR-008 amendment/NIM-209
	// item 5). The harness must not turn that into a failure.
	stack.AddMember(t, 0, incName)
	if got := memberSIDs(t, stack, incName); len(got) != 1 {
		t.Fatalf("re-binding the same SID changed the roster: %v", got)
	}

	// ── The route still refuses what an operator would be refused ───────────
	// Soul 1 is taken out of `connected`. An operator binding it gets 422: a
	// disconnected host would produce a roster that looks right and fails at
	// dispatch (NIM-209 item 4). The OLD harness would have inserted the row
	// regardless — that is the behaviour this guards against, and the reason
	// the switch is worth more than tidiness.
	setSoulStatus(t, stack, stack.SoulSID(1), "disconnected")
	body, status := stack.AddMemberRaw(t, 1, incName)
	if status != 422 {
		t.Fatalf("binding a non-connected host: status %d, want 422 — the harness must refuse what the operator is refused (body=%s)",
			status, string(body))
	}
	if !strings.Contains(string(body), stack.SoulSID(1)) {
		t.Errorf("422 body does not name the offending SID: %s", string(body))
	}
	if got := memberSIDs(t, stack, incName); len(got) != 1 {
		t.Errorf("a refused bind still changed the roster: %v — the reject must be all-or-nothing", got)
	}

	// ── The refusal is about status, not about the route being broken ───────
	// Put the host back and the same call succeeds. Without this, the check
	// above would also pass if the endpoint were rejecting everything.
	setSoulStatus(t, stack, stack.SoulSID(1), "connected")
	stack.AddMember(t, 1, incName)
	if got := memberSIDs(t, stack, incName); len(got) != 2 {
		t.Fatalf("after reconnecting and binding, members = %v, want 2", got)
	}
}

// memberSIDs reads the roster straight from the relation — deliberately NOT
// through the API, so the assertion is independent of the surface under test.
func memberSIDs(t *testing.T, stack *harness.Stack, incName string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := stack.DB().Query(ctx,
		`SELECT sid FROM incarnation_membership WHERE incarnation_name = $1 ORDER BY sid`, incName)
	if err != nil {
		t.Fatalf("read membership(%s): %v", incName, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			t.Fatalf("scan membership(%s): %v", incName, err)
		}
		out = append(out, sid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate membership(%s): %v", incName, err)
	}
	return out
}

// setSoulStatus forces a host's lifecycle status. Direct SQL on purpose: there
// is no operator route that puts a host into `disconnected`, and the point of
// the test is the SERVER's reaction to that state, not how it was reached.
func setSoulStatus(t *testing.T, stack *harness.Stack, sid, status string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := stack.DB().Exec(ctx, `UPDATE souls SET status = $2 WHERE sid = $1`, sid, status); err != nil {
		t.Fatalf("setSoulStatus(%s, %s): %v", sid, status, err)
	}
}
