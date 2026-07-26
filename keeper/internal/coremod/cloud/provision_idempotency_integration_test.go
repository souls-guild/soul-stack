//go:build integration

// Integration guards for provision idempotency (NIM-170, NIM-189) against a real
// Postgres: `core.cloud.created` re-run over an incarnation whose previous
// attempt already registered souls. Live repro (WB stand, 2026-07-22): create →
// deploy fails → error_locked → rerun-last → `insert soul "<name>-N": soul: SID
// already exists (constraint souls_pkey): SQLSTATE 23505`, unblockable only by
// deleting rows from PG by hand.
//
// NIM-170 covered the records that never reached onboarding; NIM-189 covers the
// hosts that did — provisioning converges over them instead of refusing.
//
// The souls PK is the point of the exercise, so these run against PG rather
// than the store fakes in provisioned_test.go. Container/pool/migrations come
// from TestMain in integration_adapter_test.go.

package cloud_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

const (
	provIncarnation = "redis-sa"
	provSuffix      = "ns.vm.example"
	provSID0        = "redis-sa-0." + provSuffix
	provSID1        = "redis-sa-1." + provSuffix
)

// resetProvision clears souls AND the incarnation side (membership is the
// ownership signal here, resetCloud doesn't touch incarnation rows).
func resetProvision(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, incarnation_membership,
		 incarnation, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedIncarnation(t *testing.T, name string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation (name, service, service_version, status)
		 VALUES ($1, 'redis', 'main', 'ready')`, name)
	if err != nil {
		t.Fatalf("seed incarnation %q: %v", name, err)
	}
}

func seedProvisionMembership(t *testing.T, incarnation, sid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_membership (incarnation_name, sid) VALUES ($1, $2)`,
		incarnation, sid)
	if err != nil {
		t.Fatalf("seed membership %s/%s: %v", incarnation, sid, err)
	}
}

func seedSoul(t *testing.T, sid string, status keepersoul.Status) {
	t.Helper()
	s := &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: status}
	if err := keepersoul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seed soul %q: %v", sid, err)
	}
}

func soulsCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(), `SELECT count(*) FROM souls`).Scan(&n); err != nil {
		t.Fatalf("count souls: %v", err)
	}
	return n
}

func soulStatus(t *testing.T, sid string) keepersoul.Status {
	t.Helper()
	got, err := keepersoul.SelectBySID(context.Background(), integrationPool, sid)
	if err != nil {
		t.Fatalf("select soul %q: %v", sid, err)
	}
	return got.Status
}

func soulLastSeen(t *testing.T, sid string) *time.Time {
	t.Helper()
	got, err := keepersoul.SelectBySID(context.Background(), integrationPool, sid)
	if err != nil {
		t.Fatalf("select soul %q: %v", sid, err)
	}
	return got.LastSeenAt
}

// activeTokens counts the still-redeemable bootstrap tokens of one SID
// (`used_at IS NULL`, the partial unique index caps it at one).
func activeTokens(t *testing.T, sid string) int {
	t.Helper()
	var n int
	err := integrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM bootstrap_tokens WHERE sid = $1 AND used_at IS NULL`, sid).Scan(&n)
	if err != nil {
		t.Fatalf("count active tokens of %q: %v", sid, err)
	}
	return n
}

// onboardSoul brings a seeded record to the state a host reaches once it has
// actually onboarded: a live status plus the presence stamp that a pass-through
// must leave alone. PG timestamptz keeps microseconds, so does the expectation.
func onboardSoul(t *testing.T, sid string, status keepersoul.Status) time.Time {
	t.Helper()
	ctx := context.Background()
	if err := keepersoul.UpdateStatus(ctx, integrationPool, sid, status, nil); err != nil {
		t.Fatalf("set status %q on %q: %v", status, sid, err)
	}
	seen := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if err := keepersoul.UpdateLastSeen(ctx, integrationPool, sid, "kid-test", seen); err != nil {
		t.Fatalf("stamp last_seen on %q: %v", sid, err)
	}
	return seen
}

// createdHosts unpacks `output.hosts` by SID.
func createdHosts(t *testing.T, ev *pluginv1.ApplyEvent) map[string]map[string]any {
	t.Helper()
	raw, ok := ev.GetOutput().AsMap()["hosts"].([]any)
	if !ok {
		t.Fatalf("output carries no hosts list: %v", ev.GetOutput().AsMap())
	}
	out := make(map[string]map[string]any, len(raw))
	for i, h := range raw {
		hm, ok := h.(map[string]any)
		if !ok {
			t.Fatalf("hosts[%d] is %T, want an object", i, h)
		}
		sid, _ := hm["sid"].(string)
		out[sid] = hm
	}
	return out
}

// createdVMIDs unpacks `output.vm_ids` — what covenant.yml writes into
// incarnation.state.provisioned_vm_ids.
func createdVMIDs(t *testing.T, ev *pluginv1.ApplyEvent) []string {
	t.Helper()
	raw, ok := ev.GetOutput().AsMap()["vm_ids"].([]any)
	if !ok {
		t.Fatalf("output carries no vm_ids list: %v", ev.GetOutput().AsMap())
	}
	out := make([]string, 0, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("vm_ids[%d] is %T, want a string", i, v)
		}
		out = append(out, s)
	}
	return out
}

// provisionModule wires the module over the real PG stores; the cloud driver
// and userdata renderer stay fakes (the VM side is not what's under test).
func provisionModule(fp *fakePlugins) *cloud.Module {
	fr := &fakeResolver{driver: "example", fqdnSuffix: provSuffix}
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n"}
	return cloud.New(fp, fr,
		cloud.NewSoulPG(integrationPool),
		cloud.NewTokenPG(integrationPool, time.Hour),
		nil, &fakeAudit{},
	).WithUserdata(fu)
}

// runCreate applies state=created with self_onboard for `count` VMs on behalf
// of `incarnation`, and returns the terminal event.
func runCreate(t *testing.T, m *cloud.Module, incarnation string, count int) *pluginv1.ApplyEvent {
	t.Helper()
	stream := internaltest.NewApplyStreamCtx(util.WithIncarnation(context.Background(), incarnation))
	req := &pluginv1.ApplyRequest{
		State: "created",
		Params: mustStructIT(t, map[string]any{
			"provider":     "example-prod",
			"name":         provIncarnation,
			"count":        float64(count),
			"self_onboard": true,
		}),
	}
	if err := m.Apply(req, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil {
		t.Fatal("module sent no terminal event")
	}
	return last
}

func createdVMs() []*pluginv1.VmInfo {
	return []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: provSID0, PrimaryIp: "10.0.0.1"},
		{VmId: "i-bbb", Fqdn: provSID1, PrimaryIp: "10.0.0.2"},
	}
}

// TestIntegration_Created_RerunOverPendingSouls_NoDuplicateSID — THE guard
// (NIM-170): the exact live sequence, twice through the module against one
// registry. Before the fix the second run died on souls_pkey; now it takes the
// pending records over and the registry still holds exactly two rows.
func TestIntegration_Created_RerunOverPendingSouls_NoDuplicateSID(t *testing.T) {
	resetProvision(t)
	seedIncarnation(t, provIncarnation)
	m := provisionModule(&fakePlugins{createResp: createdVMs()})

	if ev := runCreate(t, m, provIncarnation, 2); ev.GetFailed() {
		t.Fatalf("first create failed: %s", ev.GetMessage())
	}
	// The first run's souls are members of the incarnation by the time the
	// scenario reaches its deploy tasks (core.soul.registered binds them).
	seedProvisionMembership(t, provIncarnation, provSID0)
	seedProvisionMembership(t, provIncarnation, provSID1)

	ev := runCreate(t, m, provIncarnation, 2)
	if ev.GetFailed() {
		t.Fatalf("re-run failed: %s — provisioning is still not idempotent", ev.GetMessage())
	}
	if n := soulsCount(t); n != 2 {
		t.Errorf("souls rows = %d, want 2 (re-run must not add rows)", n)
	}
	if got := ev.GetOutput().AsMap()["reused"]; got != float64(2) {
		t.Errorf("output[reused] = %v, want 2", got)
	}
	for _, sid := range []string{provSID0, provSID1} {
		if st := soulStatus(t, sid); st != keepersoul.StatusPending {
			t.Errorf("soul %q status = %q, want pending (re-armed for onboarding)", sid, st)
		}
	}
}

// TestIntegration_Created_RerunAfterDestroy_ReusesTombstone — `destroy` leaves
// the souls rows behind as `destroyed` (ADR-017 cascade). Re-creating the same
// incarnation must reuse those tombstones instead of colliding with them.
func TestIntegration_Created_RerunAfterDestroy_ReusesTombstone(t *testing.T) {
	resetProvision(t)
	seedIncarnation(t, provIncarnation)
	seedSoul(t, provSID0, keepersoul.StatusPending)
	seedProvisionMembership(t, provIncarnation, provSID0)
	if err := keepersoul.UpdateStatus(context.Background(), integrationPool,
		provSID0, keepersoul.StatusDestroyed, nil); err != nil {
		t.Fatalf("cascade-destroy the soul: %v", err)
	}

	m := provisionModule(&fakePlugins{createResp: createdVMs()[:1]})
	ev := runCreate(t, m, provIncarnation, 1)
	if ev.GetFailed() {
		t.Fatalf("create over a destroyed tombstone failed: %s", ev.GetMessage())
	}
	if n := soulsCount(t); n != 1 {
		t.Errorf("souls rows = %d, want 1", n)
	}
	if st := soulStatus(t, provSID0); st != keepersoul.StatusPending {
		t.Errorf("soul status = %q, want pending (tombstone re-armed)", st)
	}
}

// TestIntegration_Created_RerunOverLiveSoul_PassesThrough — THE guard of
// NIM-189, the inversion of the NIM-170 boundary: a SID carrying a live
// registration of THIS incarnation no longer stops the run. Provisioning
// converges over it — the row is left exactly as it is, no bootstrap token is
// minted, and the run goes on to its remaining steps.
func TestIntegration_Created_RerunOverLiveSoul_PassesThrough(t *testing.T) {
	resetProvision(t)
	seedIncarnation(t, provIncarnation)
	seedSoul(t, provSID0, keepersoul.StatusPending)
	seen := onboardSoul(t, provSID0, keepersoul.StatusConnected)
	seedProvisionMembership(t, provIncarnation, provSID0)

	fp := &fakePlugins{createResp: createdVMs()[:1]}
	ev := runCreate(t, provisionModule(fp), provIncarnation, 1)
	if ev.GetFailed() {
		t.Fatalf("create over a live host of its own incarnation failed: %s — a re-create must be a no-op, not a refusal", ev.GetMessage())
	}
	if fp.lastCount != 1 {
		t.Errorf("driver Create called with count=%d, want 1 (the driver is what reuses the live VM)", fp.lastCount)
	}
	if st := soulStatus(t, provSID0); st != keepersoul.StatusConnected {
		t.Errorf("soul status = %q, want connected (a live host must not be re-armed to pending)", st)
	}
	if got := soulLastSeen(t, provSID0); got == nil || !got.Equal(seen) {
		t.Errorf("last_seen_at = %v, want %v (presence must survive a re-run)", got, seen)
	}
	if n := activeTokens(t, provSID0); n != 0 {
		t.Errorf("active bootstrap tokens = %d, want 0 (an onboarded host holds an identity; a token would be a capability with nothing to redeem)", n)
	}
	out := ev.GetOutput().AsMap()
	if out["existing"] != float64(1) || out["reused"] != float64(0) {
		t.Errorf("output existing=%v reused=%v, want 1/0", out["existing"], out["reused"])
	}
	host := createdHosts(t, ev)[provSID0]
	if host == nil {
		t.Fatalf("output.hosts is missing %q", provSID0)
	}
	if ob, _ := host["onboarded"].(bool); !ob {
		t.Errorf("hosts[%q][onboarded] = %v, want true — core.bootstrap.delivered skips on this flag, without it the delivery step dies on the missing token",
			provSID0, host["onboarded"])
	}
}

// TestIntegration_Created_RerunMixedRoster_StateStaysComplete — the day-2
// regression guard. covenant.yml writes `output.vm_ids` and `output.hosts[].sid`
// into incarnation.state (provisioned_vm_ids / provisioned_sids) unconditionally
// whenever provision is enabled, so a re-run that reported only the hosts it
// touched would strand the rest at the provider (billing orphans) and lose the
// SIDs cascade-destroy needs. Both lists must stay whole across a mixed roster.
func TestIntegration_Created_RerunMixedRoster_StateStaysComplete(t *testing.T) {
	resetProvision(t)
	seedIncarnation(t, provIncarnation)
	// -0 already onboarded, -1 a leftover of the attempt that never came up.
	seedSoul(t, provSID0, keepersoul.StatusPending)
	onboardSoul(t, provSID0, keepersoul.StatusConnected)
	seedSoul(t, provSID1, keepersoul.StatusPending)
	seedProvisionMembership(t, provIncarnation, provSID0)
	seedProvisionMembership(t, provIncarnation, provSID1)

	ev := runCreate(t, provisionModule(&fakePlugins{createResp: createdVMs()}), provIncarnation, 2)
	if ev.GetFailed() {
		t.Fatalf("re-run over a partly live roster failed: %s", ev.GetMessage())
	}
	out := ev.GetOutput().AsMap()
	if out["existing"] != float64(1) || out["reused"] != float64(1) {
		t.Errorf("output existing=%v reused=%v, want 1/1", out["existing"], out["reused"])
	}
	if got := createdVMIDs(t, ev); !slices.Equal(got, []string{"i-aaa", "i-bbb"}) {
		t.Errorf("output[vm_ids] = %v, want both VMs — day-2 destroy reads this into provisioned_vm_ids", got)
	}
	hosts := createdHosts(t, ev)
	if len(hosts) != 2 || hosts[provSID0] == nil || hosts[provSID1] == nil {
		t.Fatalf("output.hosts covers %d SID(s), want both — cascade-destroy reads these into provisioned_sids", len(hosts))
	}
	if n := soulsCount(t); n != 2 {
		t.Errorf("souls rows = %d, want 2 (a re-run must not add rows)", n)
	}
	// The live host keeps its registration; the leftover is re-armed for onboarding.
	if st := soulStatus(t, provSID0); st != keepersoul.StatusConnected {
		t.Errorf("live soul status = %q, want connected", st)
	}
	if st := soulStatus(t, provSID1); st != keepersoul.StatusPending {
		t.Errorf("leftover soul status = %q, want pending (re-armed)", st)
	}
	if n := activeTokens(t, provSID0); n != 0 {
		t.Errorf("live host holds %d active token(s), want 0", n)
	}
	if n := activeTokens(t, provSID1); n != 1 {
		t.Errorf("re-armed host holds %d active token(s), want 1", n)
	}
}

// TestIntegration_Created_ForeignLiveSoulNotTakenOver — the security boundary
// NIM-189 keeps: converging in place is only for hosts of THIS incarnation. A
// live host that belongs to another one is still refused — passing through it
// would fold somebody else's machine into this incarnation's roster and deploy
// onto it.
func TestIntegration_Created_ForeignLiveSoulNotTakenOver(t *testing.T) {
	resetProvision(t)
	seedIncarnation(t, provIncarnation)
	seedIncarnation(t, "redis-other")
	seedSoul(t, provSID0, keepersoul.StatusPending)
	seen := onboardSoul(t, provSID0, keepersoul.StatusConnected)
	seedProvisionMembership(t, "redis-other", provSID0)

	fp := &fakePlugins{createResp: createdVMs()[:1]}
	ev := runCreate(t, provisionModule(fp), provIncarnation, 1)
	if !ev.GetFailed() {
		t.Fatal("create took over a live host of another incarnation")
	}
	if st := soulStatus(t, provSID0); st != keepersoul.StatusConnected {
		t.Errorf("soul status = %q, want connected (untouched)", st)
	}
	if got := soulLastSeen(t, provSID0); got == nil || !got.Equal(seen) {
		t.Errorf("last_seen_at = %v, want %v (a refused provision must not touch the row)", got, seen)
	}
	if fp.lastCount != 0 {
		t.Errorf("driver Create called (count=%d) although the SID was refused", fp.lastCount)
	}
}

// TestIntegration_Created_ForeignIncarnationNotTakenOver — the ownership guard:
// a pending record bound to ANOTHER incarnation is not this run's leftover, so
// it is refused (hijacking it would hand this run a bootstrap token for someone
// else's host). The symmetric case — same incarnation — is reused.
func TestIntegration_Created_ForeignIncarnationNotTakenOver(t *testing.T) {
	resetProvision(t)
	seedIncarnation(t, provIncarnation)
	seedIncarnation(t, "redis-other")
	seedSoul(t, provSID0, keepersoul.StatusPending)
	seedProvisionMembership(t, "redis-other", provSID0)

	fp := &fakePlugins{createResp: createdVMs()[:1]}
	ev := runCreate(t, provisionModule(fp), provIncarnation, 1)
	if !ev.GetFailed() {
		t.Fatal("create took over a pending record owned by another incarnation")
	}
	if fp.lastCount != 0 {
		t.Errorf("driver Create called (count=%d) although the SID was refused", fp.lastCount)
	}

	// Same record, now also a member of THIS incarnation → reusable.
	seedProvisionMembership(t, provIncarnation, provSID0)
	if ev := runCreate(t, provisionModule(&fakePlugins{createResp: createdVMs()[:1]}), provIncarnation, 1); ev.GetFailed() {
		t.Fatalf("create over own pending record failed: %s", ev.GetMessage())
	}
	if n := soulsCount(t); n != 1 {
		t.Errorf("souls rows = %d, want 1", n)
	}
}

// TestIntegration_EnsureProvisionable_StatusMatrix pins the CRUD contract the
// module relies on: which registry states a provision run re-arms, which it
// converges over in place, and which it must refuse.
//
//   - reused      — never completed an onboarding (`pending`) or is a cascade
//     tombstone (`destroyed`): re-armed for a fresh onboarding (NIM-170).
//   - passthrough — the host onboarded and its identity stands (`connected` /
//     `disconnected`): the row is read back and left alone (NIM-189).
//   - refused     — `revoked` (an operator cut the host off deliberately) and
//     `expired` (a pending record the Reaper timed out): neither a leftover this
//     run may re-arm nor a host it may adopt.
func TestIntegration_EnsureProvisionable_StatusMatrix(t *testing.T) {
	want := map[keepersoul.Status]keepersoul.ProvisionOutcome{
		keepersoul.StatusPending:      keepersoul.ProvisionReused,
		keepersoul.StatusDestroyed:    keepersoul.ProvisionReused,
		keepersoul.StatusConnected:    keepersoul.ProvisionExisting,
		keepersoul.StatusDisconnected: keepersoul.ProvisionExisting,
		keepersoul.StatusRevoked:      "",
		keepersoul.StatusExpired:      "",
	}
	ctx := context.Background()

	for _, st := range []keepersoul.Status{
		keepersoul.StatusPending, keepersoul.StatusDestroyed,
		keepersoul.StatusConnected, keepersoul.StatusDisconnected,
		keepersoul.StatusRevoked, keepersoul.StatusExpired,
	} {
		t.Run(string(st), func(t *testing.T) {
			resetProvision(t)
			seedSoul(t, provSID0, keepersoul.StatusPending)
			if st != keepersoul.StatusPending {
				if err := keepersoul.UpdateStatus(ctx, integrationPool, provSID0, st, nil); err != nil {
					t.Fatalf("set status %q: %v", st, err)
				}
			}

			s := &keepersoul.Soul{SID: provSID0, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
			got, err := keepersoul.EnsureProvisionable(ctx, integrationPool, s, provIncarnation)

			switch expect := want[st]; {
			case expect == "":
				if !errors.Is(err, keepersoul.ErrSoulNotProvisionable) {
					t.Fatalf("status %q: err = %v, want ErrSoulNotProvisionable", st, err)
				}
			case err != nil:
				t.Fatalf("status %q: err = %v, want outcome %q", st, err, expect)
			case got != expect:
				t.Errorf("status %q: outcome = %q, want %q", st, got, expect)
			}

			// Only a re-arm may rewrite the row; refusal and pass-through leave it.
			if want[st] != keepersoul.ProvisionReused && soulStatus(t, provSID0) != st {
				t.Errorf("status %q was modified by a provision that must not write", st)
			}
			if want[st] == keepersoul.ProvisionExisting && s.Status != st {
				t.Errorf("pass-through returned soul.Status = %q, want the row's own %q", s.Status, st)
			}
			if n := soulsCount(t); n != 1 {
				t.Errorf("souls rows = %d, want 1", n)
			}
		})
	}
}

// TestIntegration_EnsureProvisionable_FreeSID inserts, as plain Insert does.
func TestIntegration_EnsureProvisionable_FreeSID(t *testing.T) {
	resetProvision(t)
	s := &keepersoul.Soul{SID: provSID0, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
	got, err := keepersoul.EnsureProvisionable(context.Background(), integrationPool, s, provIncarnation)
	if err != nil {
		t.Fatalf("EnsureProvisionable on a free SID: %v", err)
	}
	if got != keepersoul.ProvisionInserted {
		t.Errorf("outcome = %q on a free SID, want %q", got, keepersoul.ProvisionInserted)
	}
	if soulStatus(t, provSID0) != keepersoul.StatusPending {
		t.Error("inserted record is not pending")
	}
}
