//go:build e2e_live

package harness

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Direct-seed helpers L3b: запись incarnation/soulprint напрямую в Postgres,
// минуя Operator API. Дословный (адаптированный под SoulContainers) порт
// L3a-harness (tests/e2e/harness/stack.go::SeedIncarnationReady,
// cert.go::SeedSoulprint). Дублирование санкционировано architect-вердиктом
// `a0af3d90ec118aafd`: L3a/L3b — независимые test-frequencies (stub vs real soul),
// общий harness недоступен через module-границу.
//
// Зачем seed нужен на L3b: сервисы вроде examples/service/redis-cluster имеют
// create-сценарий, неприменимый офлайн (cloud-spawn / declared-primary / probe на
// ещё-не-запущенном redis), поэтому incarnation засевается напрямую с baseline
// state, а тестируется мутирующий сценарий (update_acl) поверх живого redis,
// поднятого отдельно.

// SeedIncarnationReady вставляет готовую (status='ready') incarnation с baseline
// state напрямую в Postgres. spec — пустой `{}` (для мутирующих сценариев spec
// не читается). Используется, когда штатный create-flow на L3b недоступен.
func (s *Stack) SeedIncarnationReady(t *testing.T, name, service, serviceVersion string, state map[string]any) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("SeedIncarnationReady(%s): marshal state: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation (name, service, service_version, spec, state, status)
		VALUES ($1, $2, $3, '{}'::jsonb, $4::jsonb, 'ready')
	`, name, service, serviceVersion, string(stateJSON)); err != nil {
		t.Fatalf("SeedIncarnationReady(%s): %v", name, err)
	}
}

// SeedSoulprint записывает soulprint-факты i-го soul-контейнера напрямую в
// `souls.soulprint_facts` (форма SoulprintFacts-JSON, CEL `soulprint.self.<path>`,
// ADR-018). На L3b реальный soul шлёт собственный SoulprintReport при установке
// сессии (см. WaitSoulprintReported) — seed нужен лишь когда тесту требуется
// детерминированный факт (например стабильный primary_ip), не зависящий от
// сетевого адреса контейнера. SID берётся из SoulContainers[soulIndex].
func (s *Stack) SeedSoulprint(t *testing.T, soulIndex int, facts map[string]any) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("SeedSoulprint(%d): out of range (создано %d soul-контейнеров)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		t.Fatalf("SeedSoulprint(%s): marshal facts: %v", sid, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		UPDATE souls
		SET soulprint_facts = $2::jsonb,
		    soulprint_collected_at = NOW(),
		    soulprint_received_at = NOW()
		WHERE sid = $1
	`, sid, string(factsJSON)); err != nil {
		t.Fatalf("SeedSoulprint(%s): %v", sid, err)
	}
}
