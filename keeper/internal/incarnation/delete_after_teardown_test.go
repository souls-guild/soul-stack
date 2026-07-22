package incarnation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// deleteTx builds a fakeTx for DeleteAfterTeardown: FOR UPDATE isn't needed
// here (DeleteAfterTeardown doesn't SELECT — the single-winner guard lives in
// the DELETE's own WHERE status='destroying'). execTags set RowsAffected for
// the DELETE (third Exec, idx=2).
func deleteTx(deleteTag pgconn.CommandTag) *fakeTx {
	return &fakeTx{
		execErrAt: -1,
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("INSERT 0 1"), // archive incarnation
			pgconn.NewCommandTag("INSERT 0 3"), // archive state_history
			deleteTag,                          // DELETE incarnation
		},
	}
}

// TestDeleteAfterTeardown_HappyWinner — the single-winner DELETE removed the
// row (RowsAffected==1): the archive is filled BEFORE DELETE (Exec order),
// the transaction commits, Deleted=true, an audit destroy_completed is written.
func TestDeleteAfterTeardown_HappyWinner(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if !res.Deleted {
		t.Error("Deleted = false, want true (RowsAffected==1)")
	}
	if !tx.committed {
		t.Error("tx not committed on winning DELETE")
	}
	// Note: fakeTx.Rollback always sets rolled=true (defer Rollback after Commit
	// is a standard pattern, real pgx ignores such a rollback). So we check
	// committed, not !rolled (as in the Unlock/Upgrade tests).

	// Order: archive incarnation → archive state_history → DELETE.
	if tx.execN != 3 {
		t.Fatalf("Exec calls = %d, want 3 (archive×2 + DELETE)", tx.execN)
	}
	if !strings.Contains(tx.execSQLs[0], "INSERT INTO incarnation_archive") {
		t.Errorf("Exec[0] not incarnation_archive insert: %q", tx.execSQLs[0])
	}
	if !strings.Contains(tx.execSQLs[1], "INSERT INTO state_history_archive") {
		t.Errorf("Exec[1] not state_history_archive insert: %q", tx.execSQLs[1])
	}
	if !strings.Contains(tx.execSQLs[2], "DELETE FROM incarnation") {
		t.Errorf("Exec[2] not DELETE: %q", tx.execSQLs[2])
	}
	// The archive is written BEFORE DELETE (indexes 0,1 < 2).
	if !strings.Contains(tx.execSQLs[2], "status = 'destroying'") {
		t.Errorf("DELETE missing single-winner guard status='destroying': %q", tx.execSQLs[2])
	}
	// The archive incarnation SELECT is also under the destroying guard.
	if !strings.Contains(tx.execSQLs[0], "status = 'destroying'") {
		t.Errorf("archive incarnation missing destroying guard: %q", tx.execSQLs[0])
	}
}

// TestDeleteAfterTeardown_NoOpLoser — RowsAffected==0 (someone already removed
// the row / the status changed): the single-winner lost → Deleted=false, the
// transaction does NOT commit (rollback discards the written archive), no
// audit is written. No error — an idempotent no-op.
func TestDeleteAfterTeardown_NoOpLoser(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 0"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown no-op must not error: %v", err)
	}
	if res.Deleted {
		t.Error("Deleted = true, want false (RowsAffected==0)")
	}
	if tx.committed {
		t.Error("no-op tx must NOT commit (rollback discards archive)")
	}
	if !tx.rolled {
		t.Error("no-op tx must rollback")
	}
	if len(aw.events) != 0 {
		t.Errorf("no-op wrote %d audit events, want 0", len(aw.events))
	}
}

// TestDeleteAfterTeardown_AuditCompleted — on a successful DELETE,
// incarnation.destroy_completed is written: source=keeper_internal, payload
// {name, force}, no secrets.
func TestDeleteAfterTeardown_AuditCompleted(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	if _, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil); err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationDestroyCompleted {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventIncarnationDestroyCompleted)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Errorf("Source = %q, want keeper_internal", ev.Source)
	}
	if ev.ArchonAID != "" {
		t.Errorf("ArchonAID = %q, want empty (keeper_internal, NULL column)", ev.ArchonAID)
	}
	if ev.Payload["name"] != "redis-prod" {
		t.Errorf("payload.name = %v, want redis-prod", ev.Payload["name"])
	}
	if ev.Payload["force"] != true {
		t.Errorf("payload.force = %v, want true", ev.Payload["force"])
	}
}

// TestDeleteAfterTeardown_AuditFailureDoesNotFail — an audit-write failure does
// NOT fail destroy (the row is already gone, the tx is committed).
func TestDeleteAfterTeardown_AuditFailureDoesNotFail(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{writeErr: errors.New("audit down")}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("must not fail on audit error: %v", err)
	}
	if !res.Deleted {
		t.Error("Deleted = false, want true")
	}
	if !tx.committed {
		t.Error("tx must commit despite audit write failure")
	}
}

// TestDeleteAfterTeardown_NilAuditWriter — w == nil doesn't panic.
func TestDeleteAfterTeardown_NilAuditWriter(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := DeleteAfterTeardown(context.Background(), pool, nil, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown with nil writer: %v", err)
	}
	if !res.Deleted {
		t.Error("Deleted = false, want true")
	}
	if !tx.committed {
		t.Error("tx not committed")
	}
}

// TestDeleteAfterTeardown_RejectsBadName — an invalid name is rejected before
// the round trip (no transaction at all).
func TestDeleteAfterTeardown_RejectsBadName(t *testing.T) {
	pool := &fakePool{txs: []*fakeTx{deleteTx(pgconn.NewCommandTag("DELETE 1"))}}
	_, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "BAD_NAME", false, nil)
	if err == nil {
		t.Fatal("invalid name returned nil err")
	}
	if pool.beginN != 0 {
		t.Errorf("BeginTx called %d times, want 0 (validation before round-trip)", pool.beginN)
	}
}

// TestDeleteAfterTeardown_ArchiveErrorAborts — an archive-INSERT failure rolls
// back the transaction and never reaches DELETE (archive+DELETE are atomic).
func TestDeleteAfterTeardown_ArchiveErrorAborts(t *testing.T) {
	tx := &fakeTx{
		execErrAt: 0, // fail on the first Exec (archive incarnation)
		execErr:   errors.New("archive boom"),
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", false, nil)
	if err == nil {
		t.Fatal("archive failure returned nil err")
	}
	if tx.committed {
		t.Error("tx must not commit on archive failure")
	}
	if !tx.rolled {
		t.Error("tx must rollback on archive failure")
	}
	// Never reached DELETE (failed on the first Exec).
	if tx.execN != 1 {
		t.Errorf("Exec calls = %d, want 1 (aborted at archive incarnation)", tx.execN)
	}
}
