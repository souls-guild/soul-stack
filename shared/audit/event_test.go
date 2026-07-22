package audit

import (
	"testing"
	"time"
)

func TestEvent_ZeroValueDefaults(t *testing.T) {
	var ev Event
	if ev.AuditID != "" {
		t.Errorf("zero AuditID = %q, want empty", ev.AuditID)
	}
	if !ev.CreatedAt.IsZero() {
		t.Errorf("zero CreatedAt not zero: %v", ev.CreatedAt)
	}
	if ev.Source != Source("") {
		t.Errorf("zero Source = %q, want empty", ev.Source)
	}
	if ev.Source.Valid() {
		t.Errorf("zero Source.Valid() = true, want false")
	}
}

func TestSource_Valid(t *testing.T) {
	cases := []struct {
		s    Source
		want bool
	}{
		{SourceSignal, true},
		{SourceAPI, true},
		{SourceMCP, true},
		{SourceKeeperInternal, true},
		{SourceSoulGRPC, true},
		{Source(""), false},
		{Source("hax0r"), false},
		{Source("SIGNAL"), false}, // case-sensitive
	}
	for _, c := range cases {
		if got := c.s.Valid(); got != c.want {
			t.Errorf("Source(%q).Valid() = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestEventType_ConstantsStable(t *testing.T) {
	// These strings go into audit_log.event_type → they are stable per the
	// normalization of ADR-021(g) / docs/naming-rules.md.
	if string(EventConfigReloadSucceeded) != "config.reload_succeeded" {
		t.Errorf("EventConfigReloadSucceeded = %q", EventConfigReloadSucceeded)
	}
	if string(EventConfigReloadFailed) != "config.reload_failed" {
		t.Errorf("EventConfigReloadFailed = %q", EventConfigReloadFailed)
	}
	if string(EventOperatorCreated) != "operator.created" {
		t.Errorf("EventOperatorCreated = %q", EventOperatorCreated)
	}
	if string(EventOperatorRevoked) != "operator.revoked" {
		t.Errorf("EventOperatorRevoked = %q", EventOperatorRevoked)
	}
	if string(EventOperatorTokenIssued) != "operator.token-issued" {
		t.Errorf("EventOperatorTokenIssued = %q", EventOperatorTokenIssued)
	}
	if string(EventIncarnationScenarioStarted) != "incarnation.scenario_started" {
		t.Errorf("EventIncarnationScenarioStarted = %q", EventIncarnationScenarioStarted)
	}
	if string(EventIncarnationUnlocked) != "incarnation.unlocked" {
		t.Errorf("EventIncarnationUnlocked = %q", EventIncarnationUnlocked)
	}

	// Voyage area (ADR-043, S5). Names are pinned in docs/naming-rules.md (the
	// Audit-events table, `scenario_run.*` / `command_run.*` blocks). Any deviation
	// from the string below = a breaking change for existing audit-log records.
	voyageCases := map[EventType]string{
		EventScenarioRunStarted:       "scenario_run.started",
		EventCommandRunInvoked:        "command_run.invoked",
		EventScenarioRunCancelled:     "scenario_run.cancelled",
		EventCommandRunCancelled:      "command_run.cancelled",
		EventScenarioRunLegStarted:    "scenario_run.leg_started",
		EventScenarioRunLegCompleted:  "scenario_run.leg_completed",
		EventScenarioRunCompleted:     "scenario_run.completed",
		EventScenarioRunPartialFailed: "scenario_run.partial_failed",
		EventScenarioRunFailed:        "scenario_run.failed",
		EventScenarioRunLeaseLost:     "scenario_run.lease_lost",
		EventCommandRunCompleted:      "command_run.completed",
		EventCommandRunPartialFailed:  "command_run.partial_failed",
		EventCommandRunFailed:         "command_run.failed",
		EventVoyageReclaimed:          "voyage.reclaimed",
		EventCadenceCreated:           "cadence.created",
		EventCadenceUpdated:           "cadence.updated",
		EventCadenceDeleted:           "cadence.deleted",
		EventCadenceSpawned:           "cadence.spawned",
		EventCadenceSkippedOverlap:    "cadence.skipped_overlap",
	}
	for got, want := range voyageCases {
		if string(got) != want {
			t.Errorf("voyage event = %q, want %q", string(got), want)
		}
	}

	// Reaper-recovery area (ADR-027 amend (m)). The name was pinned by the user
	// (propose-and-wait): standalone-orphan reconcile that releases an orphaned
	// applying-lock. A deviation = a breaking change for existing audit-log records.
	if string(EventReconcileOrphanApplyingExecuted) != "reaper.reconcile_orphan_applying.executed" {
		t.Errorf("EventReconcileOrphanApplyingExecuted = %q", EventReconcileOrphanApplyingExecuted)
	}

	// Synod area (ADR-049). Names are pinned in docs/naming-rules.md (the
	// Audit-events table, `synod.*` block). A deviation = a breaking change for
	// existing audit-log records.
	synodCases := map[EventType]string{
		EventSynodCreated:         "synod.created",
		EventSynodDeleted:         "synod.deleted",
		EventSynodOperatorAdded:   "synod.operator-added",
		EventSynodOperatorRemoved: "synod.operator-removed",
		EventSynodRoleGranted:     "synod.role-granted",
		EventSynodRoleRevoked:     "synod.role-revoked",
	}
	for got, want := range synodCases {
		if string(got) != want {
			t.Errorf("synod event = %q, want %q", string(got), want)
		}
	}
}

func TestEvent_CreatedAtUTC(t *testing.T) {
	// Smoke: at INSERT time the write-path implementation converts CreatedAt to
	// UTC; here we check that time.Time values in different locales are equal by
	// instant.
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skip("no Moscow tz")
	}
	now := time.Now().In(loc)
	if now.UTC().Unix() != now.Unix() {
		t.Errorf("UTC conversion misbehaves: %v vs %v", now.UTC(), now)
	}
}
