package topology

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// The presence phase asks Redis "does this host hold an EventStream lease".
// A transport=ssh host never holds one and never will — nothing writes it but
// the stream handler — so before NIM-869 the phase returned the empty set for
// every push inventory and `no_live_hosts` was the outcome of every push run.
// These are the guards on the exemption.

// TestTransportSSHConstantMatchesRegistry — the literal in resolver.go against
// the registry enum it copies. topology does not import `soul` in production
// (it is a read-only projection over the same table and the dependency would
// invert), so the equality is only true because something checks it.
func TestTransportSSHConstantMatchesRegistry(t *testing.T) {
	if transportSSH != string(soul.TransportSSH) {
		t.Fatalf("transportSSH = %q, registry says %q", transportSSH, soul.TransportSSH)
	}
}

// TestLoadByInventory_SSHHostSurvivesWithoutLease — an ssh host is
// targetable while the lease checker reports nothing alive. The agent beside
// it is dropped, so a filter that passed everything would not satisfy this.
func TestLoadByInventory_SSHHostSurvivesWithoutLease(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "agent.example.com", status: "connected", transport: "agent"},
		{sid: "push.example.com", status: "pending", transport: "ssh"},
	}}
	lease := &fakeLease{alive: map[string]struct{}{}}

	hosts, err := newResolverWithLease(pool, lease, nil).LoadByInventory(context.Background(), []string{"probe"})
	if err != nil {
		t.Fatalf("LoadByInventory: %v", err)
	}
	if !equalSIDs(sids(hosts), []string{"push.example.com"}) {
		t.Fatalf("got %v, want [push.example.com]: the ssh host is reached by dialling it, "+
			"the agent without a lease is offline", sids(hosts))
	}
}

// TestFilterAlive_LeaseIsNotAskedAboutSSHHosts — the exemption is taken before
// the round-trip, not after it. Asking would widen a Redis query by hosts whose
// answer is discarded, and would read as "we check presence for push" to the
// next reader.
func TestFilterAlive_LeaseIsNotAskedAboutSSHHosts(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "agent.example.com", status: "connected", transport: "agent"},
		{sid: "push.example.com", status: "pending", transport: "ssh"},
	}}
	lease := &fakeLease{alive: map[string]struct{}{"agent.example.com": {}}}

	if _, err := newResolverWithLease(pool, lease, nil).LoadByInventory(context.Background(), []string{"probe"}); err != nil {
		t.Fatalf("LoadByInventory: %v", err)
	}
	if !equalSIDs(lease.gotSIDs, []string{"agent.example.com"}) {
		t.Errorf("lease was asked about %v, want only the agent host", lease.gotSIDs)
	}
}

// TestFilterAlive_AllSSHRosterSkipsTheLeaseEntirely — a push-only inventory
// makes no Redis call at all. The three-way branch in filterAlive has a
// "nothing to ask" arm; without this it is only reached by accident.
func TestFilterAlive_AllSSHRosterSkipsTheLeaseEntirely(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "push-a.example.com", status: "pending", transport: "ssh"},
		{sid: "push-b.example.com", status: "pending", transport: "ssh"},
	}}
	lease := &fakeLease{alive: map[string]struct{}{}}

	hosts, err := newResolverWithLease(pool, lease, nil).LoadByInventory(context.Background(), []string{"probe"})
	if err != nil {
		t.Fatalf("LoadByInventory: %v", err)
	}
	if len(hosts) != 2 {
		t.Errorf("got %v, want both push hosts", sids(hosts))
	}
	if lease.gotCalls != 0 {
		t.Errorf("lease called %d times, want 0 — there was nothing to ask about", lease.gotCalls)
	}
}

// TestFilterAlive_SSHHostSurvivesTheSQLSnapshotFallback — the other arm. With
// no lease checker (single-instance dev) presence degrades to
// `status='connected'`, which an ssh host never reaches: the fallback was the
// second, independent reason push found no hosts.
func TestFilterAlive_SSHHostSurvivesTheSQLSnapshotFallback(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "agent.example.com", status: "disconnected", transport: "agent"},
		{sid: "push.example.com", status: "pending", transport: "ssh"},
	}}

	hosts, err := newResolver(pool, nil).LoadByInventory(context.Background(), []string{"probe"})
	if err != nil {
		t.Fatalf("LoadByInventory: %v", err)
	}
	if !equalSIDs(sids(hosts), []string{"push.example.com"}) {
		t.Fatalf("got %v, want [push.example.com]", sids(hosts))
	}
}

// TestFilterAlive_RedisErrorStillKeepsTheSSHHost — the fail-safe arm. A Redis
// outage degrades the agents to the SQL snapshot; it must not take push down
// with it, since push never depended on Redis in the first place.
func TestFilterAlive_RedisErrorStillKeepsTheSSHHost(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "agent.example.com", status: "connected", transport: "agent"},
		{sid: "push.example.com", status: "pending", transport: "ssh"},
	}}
	lease := &fakeLease{err: context.DeadlineExceeded}

	hosts, err := newResolverWithLease(pool, lease, nil).LoadByInventory(context.Background(), []string{"probe"})
	if err != nil {
		t.Fatalf("LoadByInventory: %v", err)
	}
	if !equalSIDs(sids(hosts), []string{"agent.example.com", "push.example.com"}) {
		t.Fatalf("got %v, want both (agent by snapshot, push by construction)", sids(hosts))
	}
}

// TestFilterAlive_KeepsTheSQLOrder — `ORDER BY sid` is what makes a serial wave
// reproducible, so the presence phase must filter in place. An implementation
// that partitioned into "reachable then streamed" would pass every assertion
// above and reorder every mixed roster.
func TestFilterAlive_KeepsTheSQLOrder(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "a-agent.example.com", status: "connected", transport: "agent"},
		{sid: "b-push.example.com", status: "pending", transport: "ssh"},
		{sid: "c-agent.example.com", status: "connected", transport: "agent"},
	}}
	lease := &fakeLease{alive: map[string]struct{}{
		"a-agent.example.com": {},
		"c-agent.example.com": {},
	}}

	hosts, err := newResolverWithLease(pool, lease, nil).LoadByInventory(context.Background(), []string{"probe"})
	if err != nil {
		t.Fatalf("LoadByInventory: %v", err)
	}
	want := []string{"a-agent.example.com", "b-push.example.com", "c-agent.example.com"}
	if !equalSIDs(sids(hosts), want) {
		t.Fatalf("got %v, want %v in SQL order", sids(hosts), want)
	}
}

// TestInventorySQL_PendingCarveOutIsTransportScoped — the SQL half of the same
// decision, which no fake pool can reach.
//
// ★ A TEXT assertion, and therefore the weaker witness: it keys on exact
// substrings, so re-spacing the clause passes it while changing nothing, and a
// rewrite that keeps the words and inverts the logic passes it while changing
// everything. The behavioural witness is the integration lane
// (TestIntegration_LoadByInventory_* against a real Postgres); this exists so a
// reversion is loud in the docker-free gate too, not so it can replace that.
func TestInventorySQL_PendingCarveOutIsTransportScoped(t *testing.T) {
	if strings.Contains(inventorySQL, "'pending', 'revoked'") {
		t.Error("inventorySQL still excludes pending unconditionally — a push host is pending for its whole life")
	}
	if !strings.Contains(inventorySQL, "transport = 'ssh'") {
		t.Error("inventorySQL has no transport carve-out for pending")
	}
	for _, terminal := range []string{"'revoked'", "'expired'", "'destroyed'"} {
		if !strings.Contains(inventorySQL, terminal) {
			t.Errorf("inventorySQL no longer excludes %s — the carve-out must not widen past pending", terminal)
		}
	}
}

// TestRosterSQL_HasNoTransportCarveOut — the boundary, in the direction that is
// easy to erase by symmetry. A scenario run dispatches over the gRPC stream and
// has no push branch, so an ssh host admitted to the incarnation roster would
// reach dispatch and fail `soul_not_connected` instead of staying invisible.
// Copying the carve-out here is what NIM-870 gets to do, WITH the dispatch.
//
// Same caveat as above — text, not behaviour. The behavioural twin is
// TestIntegration_LoadIncarnationHosts_SSHMemberStaysOutOfTheScenarioRoster.
func TestRosterSQL_HasNoTransportCarveOut(t *testing.T) {
	if strings.Contains(rosterSQL, "transport = 'ssh'") {
		t.Error("rosterSQL grew the push carve-out; the scenario dispatcher cannot reach an ssh host")
	}
	if !strings.Contains(rosterSQL, "'pending', 'revoked'") {
		t.Error("rosterSQL no longer excludes pending outright")
	}
}
