package incarnation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeAuditWriter — collects recorded audit-events. nil-safe not needed:
// tests pass concrete instance or explicitly nil-interface.
type fakeAuditWriter struct {
	events   []*audit.Event
	writeErr error
}

func (f *fakeAuditWriter) Write(_ context.Context, e *audit.Event) error {
	f.events = append(f.events, e)
	return f.writeErr
}

// destroyPool assembles fakePool with single transaction whose FOR UPDATE response
// returns (state, status).
func destroyPool(status string) (*fakePool, *fakeTx) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"k":"v"}`), status}},
	}
	return &fakePool{txs: []*fakeTx{tx}}, tx
}

// TestDestroy_AllowedStatuses — destroy allowed from ready / error_locked /
// migration_failed: status transitions to destroying, state_history and
// audit-event written.
func TestDestroy_AllowedStatuses(t *testing.T) {
	for _, from := range []string{"ready", "error_locked", "migration_failed"} {
		t.Run(from, func(t *testing.T) {
			pool, tx := destroyPool(from)
			aw := &fakeAuditWriter{}

			res, err := Destroy(context.Background(), pool, aw, "redis-prod", false,
				audit.SourceAPI, "archon-alice", "01HISTORYID0000000000000000", nil)
			if err != nil {
				t.Fatalf("Destroy from %s: %v", from, err)
			}
			if res.PreviousStatus != Status(from) {
				t.Errorf("PreviousStatus = %q, want %q", res.PreviousStatus, from)
			}
			if !tx.committed {
				t.Error("destroy tx not committed")
			}

			// Exec[0] — state_history insert, Exec[1] — UPDATE status.
			if tx.execN != 2 {
				t.Fatalf("Exec calls = %d, want 2 (history + status update)", tx.execN)
			}
			if !strings.Contains(tx.execSQLs[0], "state_history") {
				t.Errorf("Exec[0] not state_history insert: %q", tx.execSQLs[0])
			}
			if !strings.Contains(tx.execSQLs[1], "UPDATE incarnation") {
				t.Errorf("Exec[1] not incarnation UPDATE: %q", tx.execSQLs[1])
			}
			// status-argument of UPDATE — destroying.
			if got := tx.execArgs[1][1]; got != string(StatusDestroying) {
				t.Errorf("UPDATE status arg = %v, want destroying", got)
			}
		})
	}
}

// TestDestroy_RejectedStatuses — destroy rejected from applying / destroying
// (and unknown status): ErrIncarnationNotDestroyable, tx rolls back, doesn't
// transition to destroying, audit NOT written.
func TestDestroy_RejectedStatuses(t *testing.T) {
	for _, from := range []string{"applying", "destroying", "weird"} {
		t.Run(from, func(t *testing.T) {
			pool, tx := destroyPool(from)
			aw := &fakeAuditWriter{}

			_, err := Destroy(context.Background(), pool, aw, "redis-prod", false,
				audit.SourceAPI, "archon-alice", "01HISTORYID0000000000000000", nil)
			if !errors.Is(err, ErrIncarnationNotDestroyable) {
				t.Fatalf("Destroy from %s: err = %v, want ErrIncarnationNotDestroyable", from, err)
			}
			if tx.committed {
				t.Error("rejected destroy must not commit")
			}
			if !tx.rolled {
				t.Error("rejected destroy must rollback")
			}
			// Never reached UPDATE (guard triggered after FOR UPDATE).
			if tx.execN != 0 {
				t.Errorf("Exec calls = %d, want 0 (rejected before any write)", tx.execN)
			}
			if len(aw.events) != 0 {
				t.Errorf("rejected destroy wrote %d audit events, want 0", len(aw.events))
			}
		})
	}
}

// TestDestroy_NotFound — ErrIncarnationNotFound when row absent.
func TestDestroy_NotFound(t *testing.T) {
	tx := &fakeTx{execErrAt: -1, selectRow: scriptedRow{err: pgx.ErrNoRows}}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := Destroy(context.Background(), pool, &fakeAuditWriter{}, "absent", false,
		audit.SourceAPI, "archon-alice", "01HISTORYID0000000000000000", nil)
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
}

// TestDestroy_AuditEvent — after successful destroy
// incarnation.destroy_started written with correct source / AID / payload.
func TestDestroy_AuditEvent(t *testing.T) {
	pool, _ := destroyPool("ready")
	aw := &fakeAuditWriter{}

	if _, err := Destroy(context.Background(), pool, aw, "redis-prod", true,
		audit.SourceMCP, "archon-bob", "01HISTORYID0000000000000000", nil); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationDestroyStarted {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventIncarnationDestroyStarted)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("Source = %q, want mcp", ev.Source)
	}
	if ev.ArchonAID != "archon-bob" {
		t.Errorf("ArchonAID = %q, want archon-bob", ev.ArchonAID)
	}
	if ev.Payload["name"] != "redis-prod" {
		t.Errorf("payload.name = %v, want redis-prod", ev.Payload["name"])
	}
	if ev.Payload["previous_status"] != "ready" {
		t.Errorf("payload.previous_status = %v, want ready", ev.Payload["previous_status"])
	}
	if ev.Payload["force"] != true {
		t.Errorf("payload.force = %v, want true", ev.Payload["force"])
	}
}

// TestDestroy_ForcePersistedInStatusDetails — force intent persisted in
// status_details (for S-D3), UPDATE goes JSON with "force".
func TestDestroy_ForcePersistedInStatusDetails(t *testing.T) {
	for _, force := range []bool{true, false} {
		pool, tx := destroyPool("ready")
		if _, err := Destroy(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", force,
			audit.SourceAPI, "archon-alice", "01HISTORYID0000000000000000", nil); err != nil {
			t.Fatalf("Destroy force=%v: %v", force, err)
		}
		// UPDATE — second Exec; status_details — third argument ($3).
		detailsArg, ok := tx.execArgs[1][2].([]byte)
		if !ok {
			t.Fatalf("status_details arg type = %T, want []byte", tx.execArgs[1][2])
		}
		want := `"force":` + boolStr(force)
		if !strings.Contains(string(detailsArg), want) {
			t.Errorf("status_details = %q, want substring %q", detailsArg, want)
		}
	}
}

// TestDestroy_AuditFailureDoesNotFailDestroy — audit-write failure does NOT fail
// destroy (transition already committed).
func TestDestroy_AuditFailureDoesNotFailDestroy(t *testing.T) {
	pool, tx := destroyPool("ready")
	aw := &fakeAuditWriter{writeErr: errors.New("audit down")}

	if _, err := Destroy(context.Background(), pool, aw, "redis-prod", false,
		audit.SourceAPI, "archon-alice", "01HISTORYID0000000000000000", nil); err != nil {
		t.Fatalf("Destroy must not fail on audit error: %v", err)
	}
	if !tx.committed {
		t.Error("destroy tx must commit despite audit write failure")
	}
}

// TestDestroy_NilAuditWriter — w == nil doesn't panic, destroy passes.
func TestDestroy_NilAuditWriter(t *testing.T) {
	pool, tx := destroyPool("ready")
	if _, err := Destroy(context.Background(), pool, nil, "redis-prod", false,
		audit.SourceAPI, "archon-alice", "01HISTORYID0000000000000000", nil); err != nil {
		t.Fatalf("Destroy with nil writer: %v", err)
	}
	if !tx.committed {
		t.Error("destroy tx not committed")
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
