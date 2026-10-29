//go:build integration

// The start gate of a scenario Voyage through the real router and Postgres
// (NIM-1074, ADR-0093 §3–§5): a run during a freeze needs consent and the
// refusal commits nothing, a window run waits as a scheduled Voyage, the claim
// asks the gate again and defers a row a later freeze or a moved window holds
// back, a cancelled deferred run never starts, and targets whose windows never
// meet are refused.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

const (
	gateIncA = "redis-gate-a"
	gateIncB = "redis-gate-b"
	gateOp   = "archon-gate"
	gateKID  = "keeper-gate-test"
)

type gateVoyageView struct {
	VoyageID            string     `json:"voyage_id"`
	Status              string     `json:"status"`
	ScheduleAt          *time.Time `json:"schedule_at"`
	InMaintenanceWindow bool       `json:"in_maintenance_window"`
	ConfirmedFreezeIDs  []string   `json:"confirmed_freeze_ids"`
}

type gateRow struct {
	status     string
	scheduleAt *time.Time
	startedAt  *time.Time
	attempt    int
}

func gateReadRow(t *testing.T, id string) gateRow {
	t.Helper()
	var r gateRow
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT status, schedule_at, started_at, attempt FROM voyages WHERE voyage_id = $1`, id).
		Scan(&r.status, &r.scheduleAt, &r.startedAt, &r.attempt); err != nil {
		t.Fatalf("read voyage %s: %v", id, err)
	}
	return r
}

// gateMakeDue moves a scheduled row's time into the past: what the clock does
// to it, without waiting.
func gateMakeDue(t *testing.T, id string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE voyages SET schedule_at = NOW() - interval '1 second' WHERE voyage_id = $1`, id); err != nil {
		t.Fatalf("make %s due: %v", id, err)
	}
}

func gateClaim(t *testing.T) (*voyage.Voyage, []voyage.GateOutcome) {
	t.Helper()
	v, held, err := voyage.ClaimNext(context.Background(), integrationPool, gateKID, 30*time.Second, voyage.MaintenanceGate{})
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	return v, held
}

func TestIntegration_RunGate_VoyageEndToEnd(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, gateOp, "")
	seedIncarnationFull(t, gateIncA, "redis", gateOp, nil, map[string]any{})
	seedIncarnationFull(t, gateIncB, "redis", gateOp, nil, map[string]any{})
	ctx := context.Background()

	base, stop := startServer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "gate-runner", Operators: []string{gateOp}, Permissions: []string{"incarnation.run", "incarnation.history", "incarnation.get"}},
	}})
	defer stop()
	tok := newValidTokenFor(t, gateOp, []string{"gate-runner"})
	create := func(body map[string]any) (int, []byte) {
		t.Helper()
		body["kind"] = "scenario"
		body["scenario_name"] = "restart"
		return maintDo(t, http.MethodPost, base+"/v1/voyages", tok, body)
	}
	get := func(id string) gateVoyageView {
		t.Helper()
		code, body := maintDo(t, http.MethodGet, base+"/v1/voyages/"+id, tok, nil)
		if code != http.StatusOK {
			t.Fatalf("GET voyage %s: %d %s", id, code, body)
		}
		var v gateVoyageView
		_ = json.Unmarshal(body, &v)
		return v
	}
	cancel := func(id string) {
		t.Helper()
		if code, body := maintDo(t, http.MethodDelete, base+"/v1/voyages/"+id, tok, nil); code != http.StatusAccepted {
			t.Fatalf("cancel %s: %d %s", id, code, body)
		}
	}
	targets := func(ids ...string) map[string]any { return map[string]any{"incarnations": ids} }

	// Open: no windows, no freezes — the run is claimable at once.
	code, body := create(map[string]any{"target": targets(gateIncA)})
	if code != http.StatusAccepted {
		t.Fatalf("open run: %d %s, want 202", code, body)
	}
	open := voyageIDFromReply(t, string(body))
	if v := get(open); v.Status != string(voyage.StatusPending) || v.ScheduleAt != nil {
		t.Fatalf("open run = %+v, want pending with no schedule_at", v)
	}
	cancel(open)

	// A freeze on one of two targets refuses the whole request without consent.
	now := time.Now().UTC()
	sale, err := maintenance.InsertFreeze(ctx, integrationPool, gateIncB, maintenance.FreezeSpec{
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(24 * time.Hour), Comment: "Распродажа",
	})
	if err != nil {
		t.Fatalf("InsertFreeze: %v", err)
	}
	before := countVoyages(t)
	code, body = create(map[string]any{"target": targets(gateIncA, gateIncB)})
	if code != http.StatusConflict || !strings.Contains(string(body), "freeze-active") ||
		!strings.Contains(string(body), gateIncB) || !strings.Contains(string(body), "Распродажа") ||
		strings.Contains(string(body), gateIncA+" ") {
		t.Fatalf("run during a freeze without consent: %d %s, want 409 freeze-active naming %s and the comment", code, body, gateIncB)
	}
	if after := countVoyages(t); after != before {
		t.Fatalf("a refused run left %d Voyage(s) behind", after-before)
	}

	// With consent the run is accepted and records which freeze it covers.
	code, body = create(map[string]any{"target": targets(gateIncA, gateIncB), "confirm_freeze": true})
	if code != http.StatusAccepted {
		t.Fatalf("run with consent: %d %s, want 202", code, body)
	}
	consented := voyageIDFromReply(t, string(body))
	if v := get(consented); v.Status != string(voyage.StatusPending) || len(v.ConfirmedFreezeIDs) != 1 || v.ConfirmedFreezeIDs[0] != sale.ID {
		t.Fatalf("consented run = %+v, want pending with confirmed_freeze_ids=[%s]", v, sale.ID)
	}

	// Closed: a daily one-hour slot six hours ahead — the run waits for it.
	slot := now.Add(6 * time.Hour).Truncate(time.Minute)
	spec := maintenance.WindowSpec{Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		StartTime: slot.Format("15:04"), DurationMinutes: 60, Timezone: "UTC"}
	if _, err := maintenance.InsertWindow(ctx, integrationPool, gateIncA, spec); err != nil {
		t.Fatalf("InsertWindow: %v", err)
	}
	code, body = create(map[string]any{"target": targets(gateIncA), "in_maintenance_window": true})
	if code != http.StatusAccepted {
		t.Fatalf("window run: %d %s, want 202", code, body)
	}
	deferred := voyageIDFromReply(t, string(body))
	if v := get(deferred); v.Status != string(voyage.StatusScheduled) || !v.InMaintenanceWindow ||
		v.ScheduleAt == nil || !v.ScheduleAt.Equal(slot) {
		t.Fatalf("window run = %+v, want scheduled at the next slot %s", v, slot)
	}

	// A freeze added after the consented run was accepted: nobody consented to
	// it, so the claim defers the run past it, unstarted.
	incident, err := maintenance.InsertFreeze(ctx, integrationPool, gateIncB, maintenance.FreezeSpec{
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(2 * time.Hour).Truncate(time.Second), Comment: "incident",
	})
	if err != nil {
		t.Fatalf("InsertFreeze: %v", err)
	}
	claimed, held := gateClaim(t)
	if claimed != nil {
		t.Fatalf("claimed %s while every claimable row is held back", claimed.VoyageID)
	}
	if len(held) != 1 || held[0].Voyage.VoyageID != consented || !held[0].Verdict.DeferTo.Equal(incident.EndsAt) {
		t.Fatalf("held = %+v, want %s deferred to the incident's end %s", held, consented, incident.EndsAt)
	}
	if r := gateReadRow(t, consented); r.status != string(voyage.StatusScheduled) || r.startedAt != nil || r.attempt != 0 ||
		r.scheduleAt == nil || !r.scheduleAt.Equal(incident.EndsAt) {
		t.Fatalf("deferred row = %+v, want scheduled at %s, never started, no attempt spent", r, incident.EndsAt)
	}

	// The window run's time comes while its window is closed (the slot moved):
	// asked again, it is deferred again.
	gateMakeDue(t, deferred)
	if claimed, held = gateClaim(t); claimed != nil || len(held) != 1 || held[0].Voyage.VoyageID != deferred {
		t.Fatalf("claim of a due window run in a closed window = %v / %+v, want it deferred", claimed, held)
	}
	if r := gateReadRow(t, deferred); r.status != string(voyage.StatusScheduled) || r.startedAt != nil || r.attempt != 0 ||
		r.scheduleAt == nil || !r.scheduleAt.Equal(slot) {
		t.Fatalf("re-deferred window run = %+v, want scheduled at %s, unstarted", r, slot)
	}

	// A cancelled deferred run never starts, even once its time has come and
	// nothing holds it back.
	cancel(deferred)
	if _, err := integrationPool.Exec(ctx, `DELETE FROM incarnation_maintenance_windows WHERE incarnation_id = $1`, gateIncA); err != nil {
		t.Fatalf("delete windows: %v", err)
	}
	gateMakeDue(t, deferred)
	if claimed, _ = gateClaim(t); claimed != nil && claimed.VoyageID == deferred {
		t.Fatal("a cancelled deferred run was claimed")
	}
	if r := gateReadRow(t, deferred); r.status != string(voyage.StatusCancelled) || r.startedAt != nil {
		t.Fatalf("cancelled run = %+v, want cancelled and never started", r)
	}

	// The unconsented freeze gone, the consented one alone does not hold the run.
	if _, err := maintenance.DeleteFreeze(ctx, integrationPool, gateIncB, incident.ID); err != nil {
		t.Fatalf("DeleteFreeze: %v", err)
	}
	gateMakeDue(t, consented)
	claimed, held = gateClaim(t)
	if claimed == nil || claimed.VoyageID != consented || len(held) != 0 {
		t.Fatalf("claim under the consented freeze = %v / %+v, want %s started", claimed, held, consented)
	}
	if r := gateReadRow(t, consented); r.status != string(voyage.StatusRunning) || r.startedAt == nil || r.attempt != 1 {
		t.Fatalf("started row = %+v, want running, started, attempt 1", r)
	}

	// Disjoint windows: no moment both targets may start — refused at request.
	if _, err := maintenance.InsertWindow(ctx, integrationPool, gateIncA, spec); err != nil {
		t.Fatalf("InsertWindow: %v", err)
	}
	other := spec
	other.StartTime = now.Add(12 * time.Hour).Truncate(time.Minute).Format("15:04")
	if _, err := maintenance.InsertWindow(ctx, integrationPool, gateIncB, other); err != nil {
		t.Fatalf("InsertWindow: %v", err)
	}
	if code, body = create(map[string]any{"target": targets(gateIncA, gateIncB), "in_maintenance_window": true}); code != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), "in_maintenance_window") {
		t.Fatalf("window run over disjoint windows: %d %s, want 422", code, body)
	}

	// A host has no window.
	code, body = maintDo(t, http.MethodPost, base+"/v1/voyages", tok, map[string]any{
		"kind": "command", "module": "core.cmd.shell", "target": map[string]any{"sids": []string{"h1.example.com"}},
		"in_maintenance_window": true,
	})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "kind=scenario only") {
		t.Fatalf("command run with in_maintenance_window: %d %s, want 422", code, body)
	}

	// An incarnation's runs are listed by its id, and a run of another
	// incarnation in the same status is not.
	code, body = create(map[string]any{"target": targets(gateIncB), "confirm_freeze": true})
	if code != http.StatusAccepted {
		t.Fatalf("run on %s: %d %s", gateIncB, code, body)
	}
	otherInc := voyageIDFromReply(t, string(body))
	cancel(otherInc)
	code, body = maintDo(t, http.MethodGet, base+"/v1/voyages?incarnation="+gateIncA+"&status=cancelled", tok, nil)
	if code != http.StatusOK || !strings.Contains(string(body), deferred) || !strings.Contains(string(body), open) ||
		strings.Contains(string(body), otherInc) {
		t.Fatalf("list by incarnation: %d %s, want %s's cancelled runs (%s, %s) and not %s's (%s)",
			code, body, gateIncA, open, deferred, gateIncB, otherInc)
	}
	if code, body = maintDo(t, http.MethodGet, base+"/v1/voyages?incarnation=Bad_Id", tok, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("list by a malformed incarnation id: %d %s, want 422", code, body)
	}
}

// TestIntegration_RunGate_NoCommonStartFailsAtTheClaim — windows edited after a
// window run was accepted so that its targets never meet: the claim fails the
// row with no_common_window on every target instead of deferring it forever.
func TestIntegration_RunGate_NoCommonStartFailsAtTheClaim(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, gateOp, "")
	seedIncarnationFull(t, gateIncA, "redis", gateOp, nil, map[string]any{})
	seedIncarnationFull(t, gateIncB, "redis", gateOp, nil, map[string]any{})
	ctx := context.Background()

	id := "01J000000000000000NOSTART0"
	resolved, _ := json.Marshal([]string{gateIncA, gateIncB})
	scn := "restart"
	due := time.Now().Add(-time.Second)
	if err := voyage.Insert(ctx, integrationPool, &voyage.Voyage{
		VoyageID: id, Kind: voyage.KindScenario, ScenarioName: &scn, TargetResolved: resolved,
		TotalBatches: 1, Status: voyage.StatusScheduled, ScheduleAt: &due, StartedByAID: gateOp,
		InMaintenanceWindow: true,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := voyage.InsertTargets(ctx, integrationPool, id, []voyage.VoyageTarget{
		{TargetKind: voyage.TargetKindIncarnation, TargetID: gateIncA},
		{TargetKind: voyage.TargetKindIncarnation, TargetID: gateIncB},
	}); err != nil {
		t.Fatalf("InsertTargets: %v", err)
	}
	for inc, day := range map[string]string{gateIncA: "mon", gateIncB: "sat"} {
		if _, err := maintenance.InsertWindow(ctx, integrationPool, inc, maintenance.WindowSpec{
			Days: []string{day}, StartTime: "02:00", DurationMinutes: 60, Timezone: "UTC"}); err != nil {
			t.Fatalf("InsertWindow: %v", err)
		}
	}

	claimed, held := gateClaim(t)
	if claimed != nil || len(held) != 1 || !held[0].Verdict.NoStart {
		t.Fatalf("claim = %v / %+v, want the row failed for no common start", claimed, held)
	}
	if r := gateReadRow(t, id); r.status != string(voyage.StatusFailed) || r.startedAt != nil || r.attempt != 0 {
		t.Fatalf("row = %+v, want failed, never started", r)
	}
	targets, err := voyage.SelectTargets(ctx, integrationPool, id)
	if err != nil {
		t.Fatalf("SelectTargets: %v", err)
	}
	for _, tg := range targets {
		if tg.Status != voyage.TargetStatusCancelled || tg.ErrorCode == nil || *tg.ErrorCode != voyage.TargetErrorNoCommonWindow {
			t.Errorf("target %s = %s/%v, want cancelled with %s", tg.TargetID, tg.Status, tg.ErrorCode, voyage.TargetErrorNoCommonWindow)
		}
	}
}
