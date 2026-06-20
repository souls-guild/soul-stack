//go:build e2e_live

package harness

import (
	"context"
	"testing"
	"time"
)

// AddSoulToCoven добавляет Coven-метку в `souls.coven` i-го soul-контейнера.
//
// Зачем: roster прогона scenario резолвится по Coven-членству
// (`WHERE <incarnation.name> = ANY(coven)`, ADR-008 — incarnation.name есть
// корневая Coven-метка; keeper/internal/topology/resolver.go::rosterSQL). Без
// этого incarnation «не имеет connected-хостов» → run.go abort `no_hosts` ДО
// dispatch-фазы → НИ ОДНОЙ строки apply_runs (run.go §3) → WaitApplySuccess
// крутится до timeout. Симметрично L3a-harness (tests/e2e/harness/cert.go::
// AddSoulToCoven).
//
// IssueBootstrapToken создаёт строку `souls` с пустым coven, а Bootstrap-flow
// апгрейдит только status — coven остаётся пустым. Этот шаг закрывает разрыв
// «connected, но не в roster-е incarnation».
//
// Идемпотентно (array_append только если метки ещё нет). Fatal при ошибке.
func (s *Stack) AddSoulToCoven(t *testing.T, soulIndex int, coven string) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("AddSoulToCoven(%d): out of range (создано %d soul-контейнеров)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		UPDATE souls
		SET coven = array_append(coalesce(coven, '{}'), $2)
		WHERE sid = $1 AND NOT ($2 = ANY(coalesce(coven, '{}')))
	`, sid, coven); err != nil {
		t.Fatalf("AddSoulToCoven(%s, %s): %v", sid, coven, err)
	}
}

// WaitSoulprintReported блокируется до появления непустого souls.soulprint_facts
// у i-го soul-контейнера.
//
// Зачем: сервисы с keeper-side soulprint-резолвом (redis-create читает
// soulprint.self.os.arch при рендере URL release-tarball-ов redis-exporter/
// node-exporter, ADR-018) требуют, чтобы факты хоста уже были в БД к render-фазе
// create-прогона. Реальный soul шлёт первый SoulprintReport СРАЗУ при установке
// сессии (soul/cmd/soul/main.go::handleSession), но это отдельное сообщение ПОСЛЕ
// status='connected' — между connected и обработкой SoulprintReport есть окно.
// Без ожидания первый CreateIncarnation может попасть в render с пустым
// soulprint_facts → «no such key: arch». Ждём непустые факты ДО Create.
func (s *Stack) WaitSoulprintReported(t *testing.T, soulIndex int, timeoutSec int) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("WaitSoulprintReported(%d): out of range (создано %d soul-контейнеров)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		var facts []byte
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.db.QueryRow(ctx,
			"SELECT soulprint_facts FROM souls WHERE sid = $1", sid).Scan(&facts)
		cancel()
		if err != nil {
			t.Fatalf("WaitSoulprintReported(%s): query: %v", sid, err)
		}
		if len(facts) > 0 && string(facts) != "null" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("WaitSoulprintReported(%s): soulprint_facts не заполнен за %ds", sid, timeoutSec)
}
