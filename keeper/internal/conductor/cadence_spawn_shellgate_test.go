package conductor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// The console gate on the background choke-point (ADR-0074 amendment, NIM-197).
// This is the surface the deprecation window was built for: a Cadence recipe
// written under the old rule keeps firing on a timer with nobody watching, so
// enforcement must (a) not silently stop producing runs and (b) not stall every
// other schedule in the tick.

type gateChecker struct{ consoleHosts map[string]bool }

func (g gateChecker) Check(_, resource, action string, ctx map[string]string) error {
	if resource != "soul" || action != "console" {
		return nil
	}
	if g.consoleHosts[ctx["host"]] {
		return nil
	}
	return errors.New("permission denied")
}

// commandCadence is a due kind=command recipe naming module.
func commandCadence(module string) *cadence.Cadence {
	sec := 300
	next := time.Now().Add(-time.Minute) // due
	m := module
	return &cadence.Cadence{
		ID:              "01H000000000000000000CAD01",
		Name:            "hourly-shell",
		Enabled:         true,
		ScheduleKind:    cadence.ScheduleKindInterval,
		IntervalSeconds: &sec,
		OverlapPolicy:   cadence.OverlapPolicyParallel,
		Kind:            cadence.KindCommand,
		Module:          &m,
		Target:          json.RawMessage(`{"coven":["prod"]}`),
		NextRunAt:       &next,
		CreatedByAID:    "archon-alice",
	}
}

func gatedSpawner(tx *spawnFakeTx, hosts []string, mode shellgate.Mode, console map[string]bool, ad audit.Writer) *CadenceSpawner {
	s := newSpawnerFor(tx, stubResolver{out: hosts}, ad)
	s.enforcer = gateChecker{consoleHosts: console}
	s.gate = shellgate.New(mode, nil, nil)
	return s
}

func TestSpawn_ShellGate_WindowStillSpawns(t *testing.T) {
	tx := &spawnFakeTx{}
	s := gatedSpawner(tx, []string{"host-a"}, shellgate.ModeWarn, nil, nil)

	_, spawned, err := s.processOne(context.Background(), tx, commandCadence("core.cmd.shell"), time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !spawned {
		t.Fatal("the window must not break an existing schedule")
	}
}

func TestSpawn_ShellGate_EnforceSkipsButAdvancesAndRecords(t *testing.T) {
	tx := &spawnFakeTx{}
	ad := &spawnAudit{}
	s := gatedSpawner(tx, []string{"host-a"}, shellgate.ModeEnforce, nil, ad)

	rec, spawned, err := s.processOne(context.Background(), tx, commandCadence("core.cmd.shell"), time.Now())
	// An RBAC refusal must NEVER surface as an error: processOne's error rolls
	// back the whole tick and stalls every other due schedule.
	if err != nil {
		t.Fatalf("processOne returned an error; a refusal must be a recorded skip: %v", err)
	}
	if spawned {
		t.Fatal("enforce must not spawn without soul.console")
	}
	if tx.insertCalls != 0 {
		t.Errorf("no voyage should be inserted; insert=%d", tx.insertCalls)
	}
	if tx.execCalls != 1 {
		t.Errorf("the schedule must still advance so the series does not wedge; exec=%d, want 1", tx.execCalls)
	}
	if rec == nil || !rec.skipped || rec.skipReason != skipReasonConsoleRequired {
		t.Fatalf("record = %+v, want a console_required skip", rec)
	}

	// The audit event is the whole reason this is not a silent stop.
	s.emit(context.Background(), rec)
	if len(ad.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(ad.events))
	}
	ev := ad.events[0]
	if ev.EventType != audit.EventCadenceSkippedForbidden {
		t.Errorf("event = %q, want %q", ev.EventType, audit.EventCadenceSkippedForbidden)
	}
	if ev.Payload["module"] != "core.cmd.shell" {
		t.Errorf("payload.module = %v, want core.cmd.shell (the actionable half)", ev.Payload["module"])
	}
	if ev.Payload["reason"] != skipReasonConsoleRequired {
		t.Errorf("payload.reason = %v, want %q", ev.Payload["reason"], skipReasonConsoleRequired)
	}
}

func TestSpawn_ShellGate_EnforceWithConsoleSpawns(t *testing.T) {
	tx := &spawnFakeTx{}
	s := gatedSpawner(tx, []string{"host-a", "host-b"}, shellgate.ModeEnforce,
		map[string]bool{"host-a": true, "host-b": true}, nil)

	_, spawned, err := s.processOne(context.Background(), tx, commandCadence("core.cmd.shell"), time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !spawned {
		t.Fatal("a creator holding soul.console on every resolved host must keep spawning")
	}
}

// TestSpawn_ShellGate_EnforcePartialConsoleSkips — all-or-nothing: covering some
// of the resolved hosts must not quietly shrink the recipe's blast radius.
func TestSpawn_ShellGate_EnforcePartialConsoleSkips(t *testing.T) {
	tx := &spawnFakeTx{}
	s := gatedSpawner(tx, []string{"host-a", "host-b"}, shellgate.ModeEnforce,
		map[string]bool{"host-a": true}, nil)

	_, spawned, err := s.processOne(context.Background(), tx, commandCadence("core.cmd.shell"), time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if spawned {
		t.Fatal("partial console coverage must not spawn a trimmed run")
	}
}

// TestSpawn_ShellGate_ReadSafeModuleUnaffected — an ordinary scheduled Errand
// keeps running without any console right, in either mode.
func TestSpawn_ShellGate_ReadSafeModuleUnaffected(t *testing.T) {
	tx := &spawnFakeTx{}
	s := gatedSpawner(tx, []string{"host-a"}, shellgate.ModeEnforce, nil, nil)

	_, spawned, err := s.processOne(context.Background(), tx, commandCadence("core.http.probe"), time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !spawned {
		t.Fatal("a read-safe scheduled Errand must not require soul.console")
	}
}
