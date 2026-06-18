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
