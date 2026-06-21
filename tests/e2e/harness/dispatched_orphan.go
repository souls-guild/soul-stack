//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

// ConnectSoulStubReconnect открывает live EventStream i-го pre-auth Soul-а к
// primary keeper-у (как ConnectSoulStub), но дополнительно заряжает стаб
// fallback-списком ВСЕХ keeper-gRPC-адресов кластера и включает авто-reconnect+
// WardRoster (зеркало реального soul/cmd/soul reconnectLoop→handleSession). После
// смерти keeper-холдера стрима стаб сам переподключится к живому keeper-у и
// объявит ему свой набор ведомых apply_id через WardRoster.
//
// holdApply=true дополнительно переводит стаб в режим «держать ApplyRequest»: на
// ApplyRequest он НЕ шлёт RunResult (строка apply_runs остаётся `dispatched`), а
// регистрирует apply_id в activeWard. Это воспроизводит dispatched-orphan: задание
// отдано, RunResult не пришёл, keeper-холдер убит.
//
// Возвращает стаб (с уже подтверждённым HelloReply на primary).
func (s *Stack) ConnectSoulStubReconnect(t *testing.T, soulIndex int, holdApply bool) *soulstub.Stub {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("ConnectSoulStubReconnect(%d): out of range (создано %d soul-ов)", soulIndex, len(s.souls))
	}
	id := s.souls[soulIndex]

	stub := soulstub.New(id.SID, s.KeeperGRPCAddr, id.Cert, id.Key, s.caBundle)
	stub.SetEndpoints(s.AllKeeperGRPCAddrs())
	stub.EnableReconnect(true)
	stub.SetHoldApply(holdApply)

	ctx, cancel := context.WithCancel(context.Background())
	if err := stub.Open(ctx); err != nil {
		cancel()
		t.Fatalf("ConnectSoulStubReconnect(%s): open stream: %v", id.SID, err)
	}
	s.cleanups = append(s.cleanups, func() {
		_ = stub.Close()
		cancel()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range stub.Messages() {
			if m.Kind == "HelloReply" {
				return stub
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ConnectSoulStubReconnect(%s): HelloReply не получен за 10s (lease/handshake не завершён)", id.SID)
	return nil
}

// ApplyRunStatusForSID читает status строки apply_runs (applyID, sid) из PG.
// Пустая строка, если строки ещё нет (TaskEvent/Insert не дошли). Узкий
// read-helper для поллинга dispatched→orphaned перехода.
func (s *Stack) ApplyRunStatusForSID(t *testing.T, applyID, sid string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	err := s.db.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2 ORDER BY passage ASC LIMIT 1`,
		applyID, sid).Scan(&status)
	if err != nil {
		return ""
	}
	return status
}

// WaitApplyRunStatusForSID поллит status строки apply_runs (applyID, sid) до тех
// пор, пока он не войдёт в один из want-статусов, либо до таймаута. Возвращает
// достигнутый статус. Fatal при таймауте с последним наблюдённым статусом.
func (s *Stack) WaitApplyRunStatusForSID(t *testing.T, applyID, sid string, want []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.ApplyRunStatusForSID(t, applyID, sid)
		for _, w := range want {
			if last == w {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WaitApplyRunStatusForSID(%s,%s): статус %v не достигнут за %s (последний=%q)",
		applyID, sid, want, timeout, last)
	return ""
}

// StreamHolderKID возвращает KID keeper-а, на чей EventStream подключён soul-stub
// (= keeper по primary-адресу s.KeeperGRPCAddr, к которому Open дозвонился
// напрямую). Это keeper-холдер стрима: SendApply маршрутизируется в него по
// SID-lease, и его смерть оставляет dispatched-строку осиротевшей. Детерминирован
// без чтения Redis: стаб всегда открывает initial-стрим к primary.
func (s *Stack) StreamHolderKID(t *testing.T) string {
	t.Helper()
	kid := s.KeeperKIDForGRPCAddr(s.KeeperGRPCAddr)
	if kid == "" {
		t.Fatalf("StreamHolderKID: не удалось сопоставить primary-адрес %q ни одному KID", s.KeeperGRPCAddr)
	}
	return kid
}
