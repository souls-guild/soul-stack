//go:build e2e_live

package harness

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// AddMember binds the i-th soul container to incarnation `incName` in
// incarnation_membership.
//
// Why: membership is a first-class M:N relation (ADR-008 amendment
// 2026-07-17/NIM-124), no longer the derived fact
// `incarnation.name ∈ souls.coven[]`. The scenario run's roster resolves
// members via incarnation_membership
// (keeper/internal/topology/resolver.go::rosterSQL). Without this the
// incarnation "has no connected hosts" -> run.go aborts with `no_hosts`
// BEFORE the dispatch phase -> zero apply_runs rows (run.go §3) ->
// WaitApplySuccess spins until timeout. Symmetric with the L3a harness
// (tests/e2e/harness/cert.go::AddMember).
//
// IssueBootstrapToken creates a `souls` row and the Bootstrap flow only
// upgrades status — no membership is bound. This step closes the gap of
// "connected, but not in the incarnation's roster".
//
// It goes through the OPERATOR path — `POST /v1/incarnations/{name}/members`
// (ADR-008 amendment 2026-07-28/NIM-209). It used to INSERT into
// incarnation_membership directly, because until NIM-209 there was no route to
// call; the harness was working around a genuine product gap. Now the direct
// INSERT would be worse than redundant (NIM-231): it bypasses BOTH
// authorization gates (the incarnation selector and the caller's soul purview)
// and the `connected` status rule, so the live suite would go green along a
// path no operator can take.
//
// A consequence worth stating: this now REQUIRES the soul to be `connected`,
// exactly as the operator's call does. On L3b that is the normal state by the
// time the incarnation exists — the containers onboard during NewStack.
//
// ORDER: the incarnation must already exist. Still true, but no longer
// diagnosed by an FK violation — the route resolves `{name}` before writing
// anything and answers 404, which this turns into the instruction. For
// bootstrapping a NEW incarnation use [Stack.CreateIncarnationOnRoster], which
// owns the whole order in one place.
//
// Idempotent server-side (ON CONFLICT DO NOTHING; the reply splits `bound` from
// `already_member`). Fatal on any non-200 — see [Stack.AddMemberRaw] when the
// status itself is the subject of the test.
func (s *Stack) AddMember(t *testing.T, soulIndex int, incName string) {
	t.Helper()
	body, status := s.AddMemberRaw(t, soulIndex, incName)
	if status == http.StatusNotFound {
		t.Fatalf("AddMember(%s, soul %d): 404 — the incarnation does not exist yet; membership is bound AFTER "+
			"the incarnation is created. Bootstrap a new incarnation with Stack.CreateIncarnationOnRoster. body=%s",
			incName, soulIndex, string(body))
	}
	if status != http.StatusOK {
		t.Fatalf("AddMember(%s, soul %d): status %d, body=%s", incName, soulIndex, status, string(body))
	}
}

// AddMemberRaw — low-level `POST /v1/incarnations/{name}/members`: returns
// (responseBody, statusCode) without checking it. For tests where the code IS
// the subject — binding a host that is not `connected` must be refused the same
// way it is refused an operator (422), not silently accepted as the old direct
// INSERT did (NIM-231).
func (s *Stack) AddMemberRaw(t *testing.T, soulIndex int, incName string) ([]byte, int) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("AddMemberRaw(%d): out of range (%d soul containers created)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
	c := s.opClient(t)
	path := fmt.Sprintf("/v1/incarnations/%s/members", incName)
	resp, status, err := c.post(context.Background(), path, map[string]any{"sids": []string{sid}})
	if err != nil {
		t.Fatalf("AddMemberRaw(%s, %s): http: %v", incName, sid, err)
	}
	return resp, status
}

// WaitSoulprintReported blocks until souls.soulprint_facts becomes non-empty
// for the i-th soul container.
//
// Why: services with keeper-side soulprint resolution (redis-create reads
// soulprint.self.os.arch when rendering the redis-exporter/node-exporter
// release-tarball URLs, ADR-018) require host facts to already be in the DB
// by render time of the create run. A real soul sends its first
// SoulprintReport IMMEDIATELY on session establishment
// (soul/cmd/soul/main.go::handleSession), but that's a separate message
// AFTER status='connected' — there's a window between connected and
// SoulprintReport being processed. Without waiting, the first
// CreateIncarnation could hit render with empty soulprint_facts -> "no such
// key: arch". We wait for non-empty facts BEFORE Create.
func (s *Stack) WaitSoulprintReported(t *testing.T, soulIndex int, timeoutSec int) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("WaitSoulprintReported(%d): out of range (%d soul containers created)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		var facts []byte
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.db.QueryRow(ctx,
			"SELECT soulprint_facts FROM souls WHERE sid = $1", sid).Scan(&facts)
		cancel()
		if err != nil {
			t.Fatalf("WaitSoulprintReported(%s): query: %v", sid, err)
		}
		if len(facts) > 0 && string(facts) != "null" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("WaitSoulprintReported(%s): soulprint_facts not populated within %ds", sid, timeoutSec)
}
