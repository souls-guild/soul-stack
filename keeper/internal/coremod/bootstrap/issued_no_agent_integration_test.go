//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
)

// presenceDown is a lease source that cannot be read.
type presenceDown struct{}

func (presenceDown) SoulsStreamAlive(context.Context, []string) (map[string]struct{}, error) {
	return nil, errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")
}

// seedIdentity writes a Soul that holds an identity: the row, its last stream
// (nil = no stream on record) and an active seed. fingerprint must be unique across
// the test — the seed index on it is global.
func seedIdentity(t *testing.T, sid string, status keepersoul.Status, lastSeenAt *time.Time, fingerprint string) {
	t.Helper()
	ctx := context.Background()
	if err := keepersoul.Insert(ctx, issuedIntegrationPool, &keepersoul.Soul{
		SID: sid, Transport: keepersoul.TransportAgent, Status: status,
	}); err != nil {
		t.Fatalf("insert Soul %s: %v", sid, err)
	}
	if _, err := issuedIntegrationPool.Exec(ctx,
		`UPDATE souls SET last_seen_at = $2 WHERE sid = $1`, sid, lastSeenAt); err != nil {
		t.Fatalf("set last_seen_at %s: %v", sid, err)
	}
	kid := "keeper-1"
	if err := soulseed.Insert(ctx, issuedIntegrationPool, &soulseed.SoulSeed{
		SID:          sid,
		Fingerprint:  fingerprint,
		SerialNumber: "serial-" + sid,
		ExpiresAt:    time.Now().UTC().Add(720 * time.Hour),
		IssuedByKID:  &kid,
		Status:       soulseed.StatusActive,
	}); err != nil {
		t.Fatalf("insert seed %s: %v", sid, err)
	}
}

func countRows(t *testing.T, table, sid string) int {
	t.Helper()
	var n int
	if err := issuedIntegrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE sid = $1`, sid).Scan(&n); err != nil {
		t.Fatalf("count %s for %s: %v", table, sid, err)
	}
	return n
}

// ★★ NIM-886 — the subject. A machine re-created under a SID the registry
// already holds an identity for used to be passed through as `onboarded`: the
// install step skipped it, reported success, and the run failed fifteen minutes
// later at the barrier, pointing at the Keeper's own Redis. The registry cannot
// tell that machine from one whose agent is merely stopped — both are a
// `disconnected` row with an active seed — and only a live stream proves the
// identity is still held. So without one the step refuses, at once, naming the
// host and the way out.
//
// Under BOTH values of `reissue`: the flag decides what happens to a token and
// must not reach the identity arm, in either direction.
func TestIntegration_IssuedBatch_IdentityWithoutAgentIsRefused(t *testing.T) {
	for _, reissue := range []bool{false, true} {
		t.Run(fmt.Sprintf("reissue=%t", reissue), func(t *testing.T) {
			resetIssuedIntegration(t)
			ctx := context.Background()
			const stale, fresh = "redis-a-s1-k3f9q.example.com", "redis-a-s1-p2m8x.example.com"
			seenAt := time.Date(2026, 9, 12, 10, 1, 22, 0, time.UTC)
			seedIdentity(t, stale, keepersoul.StatusDisconnected, &seenAt,
				"4444444444444444444444444444444444444444444444444444444444444444")

			_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, noAgents, time.Hour).
				IssueBatch(ctx, []string{stale, fresh}, runIncarnation, reissue)
			var noAgent *coremodbootstrap.NoAgentError
			if !errors.As(err, &noAgent) {
				t.Fatalf("IssueBatch = %v, want a NoAgentError — a record nobody is connected under was passed through", err)
			}
			if len(noAgent.Hosts) != 1 {
				t.Fatalf("refused hosts = %+v, want only %s", noAgent.Hosts, stale)
			}
			h := noAgent.Hosts[0]
			if h.SID != stale || h.Status != string(keepersoul.StatusDisconnected) || h.LastSeenAt == nil || !h.LastSeenAt.Equal(seenAt) {
				t.Errorf("refused host = %+v, want %s / disconnected / %s", h, stale, seenAt)
			}
			for _, want := range []string{
				fmt.Sprintf("%q (status disconnected, last stream on record 2026-09-12T10:01:22Z)", stale),
				"DELETE /v1/souls/{sid}",
				"bring its agent up and repeat",
				"leaves it unable to onboard",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal lacks %q:\n%s", want, err)
				}
			}

			// The whole batch rolled back: the neighbour that WOULD have been minted
			// for has no row and no token, so the refusal leaves nothing half-done.
			if n := countRows(t, "souls", fresh); n != 0 {
				t.Errorf("fresh neighbour has %d souls rows after the refusal, want 0", n)
			}
			if n := countRows(t, "bootstrap_tokens", fresh); n != 0 {
				t.Errorf("fresh neighbour has %d tokens after the refusal, want 0", n)
			}
			// And the refused record is left as it was: deciding what it is belongs
			// to the operator, not to issuance.
			if n := countRows(t, "bootstrap_tokens", stale); n != 0 {
				t.Errorf("refused host got %d token rows, want 0", n)
			}
			got, gerr := keepersoul.SelectBySID(ctx, issuedIntegrationPool, stale)
			if gerr != nil {
				t.Fatalf("SelectBySID: %v", gerr)
			}
			if got.Status != keepersoul.StatusDisconnected {
				t.Errorf("refused host status = %q, want disconnected (untouched)", got.Status)
			}
		})
	}
}

// One refusal names every such host of the batch, in batch order, and only
// those: a group re-created under the same names is repaired in one pass rather
// than one rerun per machine, and a neighbour whose agent IS connected is still
// passed through rather than listed. The never-streamed shape — a Bootstrap
// that committed and no stream after it (NIM-865) — is refused the same way and
// says so.
func TestIntegration_IssuedBatch_IdentityWithoutAgent_EveryHostNamedAtOnce(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	const live, staleA, never = "live.example.com", "stale-a.example.com", "never.example.com"
	seenAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seedIdentity(t, live, keepersoul.StatusConnected, &seenAt,
		"5555555555555555555555555555555555555555555555555555555555555555")
	seedIdentity(t, staleA, keepersoul.StatusDisconnected, &seenAt,
		"6666666666666666666666666666666666666666666666666666666666666666")
	seedIdentity(t, never, keepersoul.StatusPending, nil,
		"7777777777777777777777777777777777777777777777777777777777777777")

	_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, agentsOnline{live: {}}, time.Hour).
		IssueBatch(ctx, []string{staleA, live, never}, runIncarnation, true)
	var noAgent *coremodbootstrap.NoAgentError
	if !errors.As(err, &noAgent) {
		t.Fatalf("IssueBatch = %v, want a NoAgentError", err)
	}
	var got []string
	for _, h := range noAgent.Hosts {
		got = append(got, h.SID)
	}
	if strings.Join(got, ",") != staleA+","+never {
		t.Errorf("refused hosts = %v, want [%s %s] — every agentless identity, in order, and not the live one", got, staleA, never)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q (status pending, no stream on record)", never)) {
		t.Errorf("refusal does not say the never-streamed host has no stream on record:\n%s", err)
	}
	if !strings.HasPrefix(err.Error(), "2 host(s)") {
		t.Errorf("refusal does not count the hosts it names:\n%s", err)
	}
}

// A lease that cannot be read is not "no agents": the step cannot tell a live
// host from a dead one, so it refuses and says why, rolling the batch back.
func TestIntegration_IssuedBatch_UnreadablePresenceRefusesTheBatch(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	const onboarded, fresh = "onboarded.example.com", "fresh.example.com"
	seenAt := time.Now().UTC().Add(-time.Minute)
	seedIdentity(t, onboarded, keepersoul.StatusConnected, &seenAt,
		"8888888888888888888888888888888888888888888888888888888888888888")

	_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, presenceDown{}, time.Hour).
		IssueBatch(ctx, []string{onboarded, fresh}, runIncarnation, true)
	if err == nil || !strings.Contains(err.Error(), "check agent presence") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("IssueBatch = %v, want a presence-read refusal naming the cause", err)
	}
	if n := countRows(t, "souls", fresh); n != 0 {
		t.Errorf("fresh neighbour has %d souls rows after the refusal, want 0", n)
	}
}

// presenceHangs never answers until its caller gives up.
type presenceHangs struct{}

func (presenceHangs) SoulsStreamAlive(ctx context.Context, _ []string) (map[string]struct{}, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The lease is read while the batch holds FOR UPDATE on its Soul rows, so a
// Redis that hangs must be given up on by the issuer itself — the run's context
// may have no deadline at all, and a concurrent Bootstrap of those hosts waits on
// the locks for as long as the read does.
func TestIntegration_IssuedBatch_HangingPresenceIsGivenUpOn(t *testing.T) {
	resetIssuedIntegration(t)
	const onboarded = "onboarded.example.com"
	seenAt := time.Now().UTC().Add(-time.Minute)
	seedIdentity(t, onboarded, keepersoul.StatusConnected, &seenAt,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	// The caller's own deadline is far past the issuer's bound, so the call
	// returning early proves the bound; and it still ends the transaction if the
	// bound is gone, so a regression fails here instead of leaving a FOR UPDATE
	// lock that hangs every later test in the package.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, presenceHangs{}, time.Hour).
		IssueBatch(ctx, []string{onboarded}, runIncarnation, true)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("IssueBatch waited %s on a presence source that never answers", took)
	}
	if err == nil || !strings.Contains(err.Error(), "check agent presence") {
		t.Fatalf("IssueBatch = %v, want a presence-read refusal", err)
	}
}

// Without a presence source the onboarded arm fails closed — passing the host
// through would be the very guess this ticket removes — while a batch that holds
// no identity never needs the lease and still issues. The check is asked for
// only when there is something to ask about.
func TestIntegration_IssuedBatch_NoPresenceSource(t *testing.T) {
	ctx := context.Background()

	t.Run("a host holding an identity is refused", func(t *testing.T) {
		resetIssuedIntegration(t)
		const sid = "onboarded.example.com"
		seenAt := time.Now().UTC()
		seedIdentity(t, sid, keepersoul.StatusConnected, &seenAt,
			"9999999999999999999999999999999999999999999999999999999999999999")
		_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, nil, time.Hour).
			IssueBatch(ctx, []string{sid}, runIncarnation, true)
		if err == nil || !strings.Contains(err.Error(), "no presence checker") {
			t.Fatalf("IssueBatch = %v, want a refusal naming the missing presence checker", err)
		}
	})

	t.Run("a batch of fresh hosts still issues", func(t *testing.T) {
		resetIssuedIntegration(t)
		hosts, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, nil, time.Hour).
			IssueBatch(ctx, []string{"fresh.example.com"}, runIncarnation, true)
		if err != nil {
			t.Fatalf("IssueBatch: %v", err)
		}
		if len(hosts) != 1 || hosts[0].Token.Reveal() == "" {
			t.Fatalf("hosts = %+v, want one freshly issued token", hosts)
		}
	})
}
