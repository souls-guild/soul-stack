package topology

import (
	"context"
	"strings"
	"testing"
)

// The scenario roster is two disjoint queries since NIM-880: [rosterSQL] for
// agent members, [rosterPushSQL] for `transport: ssh` members. The split exists
// because `pending` means opposite things on the two transports — "onboarding,
// no identity yet" for an agent, "the only status it will ever hold" for a push
// host — and one predicate could not say both.
//
// These guard the three properties that split is worth having: a push member is
// visible, an onboarding agent still is not, and no host is counted twice.

// TestLoadIncarnationHosts_PushMemberIsInTheRoster — the whole point of
// NIM-880's roster half. Before it, an ssh host bound to an incarnation was
// invisible to every scenario run, so the dispatcher's push branch could not be
// reached by any production configuration.
func TestLoadIncarnationHosts_PushMemberIsInTheRoster(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "agent.example.com", status: "connected", transport: "agent"},
		{sid: "push.example.com", status: "pending", transport: "ssh"},
	}}

	hosts, err := newResolver(pool, nil).LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if !equalSIDs(sids(hosts), []string{"agent.example.com", "push.example.com"}) {
		t.Fatalf("got %v, want both hosts: a pending ssh member is targetable, "+
			"its presence is answered by the dial at dispatch", sids(hosts))
	}
}

// TestLoadIncarnationHosts_PendingAgentStaysExcluded — the exemption is for the
// ssh transport and nothing else. A pending AGENT has a bootstrap token issued
// and no identity yet; targeting it is meaningless whatever the dispatcher can
// do, and a predicate widened by transport rather than by status would have
// admitted it.
func TestLoadIncarnationHosts_PendingAgentStaysExcluded(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "onboarding.example.com", status: "pending", transport: "agent"},
		{sid: "push.example.com", status: "pending", transport: "ssh"},
	}}

	hosts, err := newResolver(pool, nil).LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if !equalSIDs(sids(hosts), []string{"push.example.com"}) {
		t.Fatalf("got %v, want only the ssh host: a pending agent has no identity yet", sids(hosts))
	}
}

// TestLoadIncarnationHosts_ConnectedSSHHostIsNotCountedTwice — the one row the
// old single predicate got wrong in the other direction. An ssh host left at
// `status='connected'` (the agent→ssh migration nothing writes yet) used to pass
// the agent filter and reach a stream dispatch it cannot answer. It must now
// appear exactly once, on the push side.
func TestLoadIncarnationHosts_ConnectedSSHHostIsNotCountedTwice(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "migrated.example.com", status: "connected", transport: "ssh"},
	}}

	hosts, err := newResolver(pool, nil).LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("len(hosts) = %d, want 1 - the two roster queries must be disjoint: %v", len(hosts), sids(hosts))
	}
	if hosts[0].Transport != transportSSH {
		t.Fatalf("transport = %q, want ssh", hosts[0].Transport)
	}
}

// TestLoadIncarnationHosts_MergedRosterStaysSIDOrdered — a serial wave is a
// prefix of this slice (orchestration.md §2.2.1), so ordering is not cosmetic:
// a roster returned grouped by transport would roll a rolling deploy in a
// different order than the same roster with one host retyped.
func TestLoadIncarnationHosts_MergedRosterStaysSIDOrdered(t *testing.T) {
	pool := &fakePool{rosterRows: []rosterRow{
		{sid: "a-agent.example.com", status: "connected", transport: "agent"},
		{sid: "b-push.example.com", status: "pending", transport: "ssh"},
		{sid: "c-agent.example.com", status: "connected", transport: "agent"},
		{sid: "d-push.example.com", status: "pending", transport: "ssh"},
	}}

	hosts, err := newResolver(pool, nil).LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	want := []string{"a-agent.example.com", "b-push.example.com", "c-agent.example.com", "d-push.example.com"}
	if got := sids(hosts); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("roster order = %v, want %v", got, want)
	}
}

// TestRosterQueriesAreDisjointByTransport — the predicates themselves, read as
// text. The disjointness is what lets [mergeBySID] skip de-duplication, so it
// has to be a property of the SQL rather than of a loop somebody could delete.
func TestRosterQueriesAreDisjointByTransport(t *testing.T) {
	if !strings.Contains(rosterSQL, "s.transport <> 'ssh'") {
		t.Error("rosterSQL no longer excludes ssh hosts - it would overlap rosterPushSQL and double-count a connected ssh member")
	}
	if !strings.Contains(rosterPushSQL, "s.transport = 'ssh'") {
		t.Error("rosterPushSQL no longer restricts itself to ssh hosts")
	}
	if strings.Contains(rosterPushSQL, "'pending'") {
		t.Error("rosterPushSQL excludes pending - that is the only status a push host ever holds, so it would return nothing")
	}
	if !strings.Contains(rosterSQL, "'pending'") {
		t.Error("rosterSQL no longer excludes pending agents - an onboarding host without an identity would be targeted")
	}
}
