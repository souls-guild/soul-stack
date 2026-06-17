package incarnation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// deleteTx собирает fakeTx под DeleteAfterTeardown: FOR UPDATE здесь не нужен
// (DeleteAfterTeardown не делает SELECT — single-winner guard живёт в WHERE
// status='destroying' самого DELETE). execTags задают RowsAffected для DELETE
// (третий Exec, idx=2).
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

// TestDeleteAfterTeardown_HappyWinner — single-winner DELETE удалил строку
// (RowsAffected==1): archive заполняется ДО DELETE (порядок Exec-ов),
// транзакция коммитится, Deleted=true, пишется audit destroy_completed.
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
	// Замечание: fakeTx.Rollback всегда выставляет rolled=true (defer Rollback
	// после Commit — стандартный паттерн, реальный pgx такой rollback игнорирует).
	// Поэтому проверяем committed, а не !rolled (как в Unlock/Upgrade-тестах).

	// Порядок: archive incarnation → archive state_history → DELETE.
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
	// Архив пишется ДО DELETE (индексы 0,1 < 2).
	if !strings.Contains(tx.execSQLs[2], "status = 'destroying'") {
		t.Errorf("DELETE missing single-winner guard status='destroying': %q", tx.execSQLs[2])
	}
	// Архивный SELECT incarnation тоже под guard-ом destroying.
	if !strings.Contains(tx.execSQLs[0], "status = 'destroying'") {
		t.Errorf("archive incarnation missing destroying guard: %q", tx.execSQLs[0])
	}
}

// TestDeleteAfterTeardown_NoOpLoser — RowsAffected==0 (кто-то уже снёс строку /
// статус сменился): single-winner проиграл → Deleted=false, транзакция НЕ
// коммитится (rollback откатывает записанный архив), audit НЕ пишется. Никакой
// ошибки — идемпотентный no-op.
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

// TestDeleteAfterTeardown_AuditCompleted — на успешном DELETE пишется
// incarnation.destroy_completed: source=keeper_internal, payload {name, force},
// без секретов.
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

// TestDeleteAfterTeardown_AuditFailureDoesNotFail — фейл audit-write НЕ валит
// destroy (строка уже снесена, tx закоммичена).
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

// TestDeleteAfterTeardown_NilAuditWriter — w == nil не паникует.
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

// TestDeleteAfterTeardown_RejectsBadName — невалидное имя отвергается до
// round-trip-а (никакой транзакции).
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

// TestDeleteAfterTeardown_ArchiveErrorAborts — фейл архив-INSERT-а откатывает
// транзакцию и не доходит до DELETE (архив+DELETE атомарны).
func TestDeleteAfterTeardown_ArchiveErrorAborts(t *testing.T) {
	tx := &fakeTx{
		execErrAt: 0, // фейл на первом Exec (archive incarnation)
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
	// До DELETE дело не дошло (упали на первом Exec).
	if tx.execN != 1 {
		t.Errorf("Exec calls = %d, want 1 (aborted at archive incarnation)", tx.execN)
	}
}
