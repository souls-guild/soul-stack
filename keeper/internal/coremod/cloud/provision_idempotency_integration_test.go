//go:build integration

// Integration guards for provision idempotency (NIM-170) against a real
// Postgres: `core.cloud.created` re-run over an incarnation whose previous
// attempt already registered souls. Live repro (WB stand, 2026-07-22): create →
// deploy fails → error_locked → rerun-last → `insert soul "<name>-N": soul: SID
// already exists (constraint souls_pkey): SQLSTATE 23505`, unblockable only by
// deleting rows from PG by hand.
//
// The souls PK is the point of the exercise, so these run against PG rather
// than the store fakes in provisioned_test.go. Container/pool/migrations come
// from TestMain in integration_adapter_test.go.

package cloud_test

import (
	"context"
	"errors"
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

// TestIntegration_Created_LiveSoulNotTakenOver — a SID that carries a LIVE
// registration is never re-provisioned: the run fails with a reason an operator
// can act on, and the registration keeps its status.
func TestIntegration_Created_LiveSoulNotTakenOver(t *testing.T) {
	resetProvision(t)
	seedIncarnation(t, provIncarnation)
	seedSoul(t, provSID0, keepersoul.StatusPending)
	if err := keepersoul.UpdateStatus(context.Background(), integrationPool,
		provSID0, keepersoul.StatusConnected, nil); err != nil {
		t.Fatalf("connect the soul: %v", err)
	}
	seedProvisionMembership(t, provIncarnation, provSID0)

	fp := &fakePlugins{createResp: createdVMs()[:1]}
	ev := runCreate(t, provisionModule(fp), provIncarnation, 1)
	if !ev.GetFailed() {
		t.Fatal("create took over a connected host — a live registration must never be re-provisioned")
	}
	if st := soulStatus(t, provSID0); st != keepersoul.StatusConnected {
		t.Errorf("soul status = %q, want connected (untouched)", st)
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
// module relies on: which registry states a provision run may re-arm, and which
// it must refuse.
func TestIntegration_EnsureProvisionable_StatusMatrix(t *testing.T) {
	reusable := []keepersoul.Status{keepersoul.StatusPending, keepersoul.StatusDestroyed}
	refused := []keepersoul.Status{
		keepersoul.StatusConnected, keepersoul.StatusDisconnected,
		keepersoul.StatusRevoked, keepersoul.StatusExpired,
	}
	ctx := context.Background()

	for _, st := range append(append([]keepersoul.Status{}, reusable...), refused...) {
		t.Run(string(st), func(t *testing.T) {
			resetProvision(t)
			seedSoul(t, provSID0, keepersoul.StatusPending)
			if st != keepersoul.StatusPending {
				if err := keepersoul.UpdateStatus(ctx, integrationPool, provSID0, st, nil); err != nil {
					t.Fatalf("set status %q: %v", st, err)
				}
			}

			s := &keepersoul.Soul{SID: provSID0, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
			reused, err := keepersoul.EnsureProvisionable(ctx, integrationPool, s, provIncarnation)

			wantReuse := st == keepersoul.StatusPending || st == keepersoul.StatusDestroyed
			switch {
			case wantReuse && err != nil:
				t.Fatalf("status %q: err = %v, want reuse", st, err)
			case wantReuse && !reused:
				t.Errorf("status %q: reused = false, want true", st)
			case !wantReuse && !errors.Is(err, keepersoul.ErrSoulNotProvisionable):
				t.Fatalf("status %q: err = %v, want ErrSoulNotProvisionable", st, err)
			}
			if !wantReuse && soulStatus(t, provSID0) != st {
				t.Errorf("status %q was modified by a refused provision", st)
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
	reused, err := keepersoul.EnsureProvisionable(context.Background(), integrationPool, s, provIncarnation)
	if err != nil {
		t.Fatalf("EnsureProvisionable on a free SID: %v", err)
	}
	if reused {
		t.Error("reused = true on a free SID, want false (freshly inserted)")
	}
	if soulStatus(t, provSID0) != keepersoul.StatusPending {
		t.Error("inserted record is not pending")
	}
}
