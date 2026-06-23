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
// Зачем seed нужен на L3b: некоторые сервисы имеют create-сценарий, неприменимый
// офлайн (cloud-spawn / declared-primary / probe на ещё-не-запущенном демоне),
// поэтому incarnation засевается напрямую с baseline state, а тестируется
// мутирующий сценарий поверх живого демона, поднятого отдельно.

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

// SpecHostDecl — declared-запись `incarnation.spec.hosts[]` для
// [Stack.SeedIncarnationForCreate]. Форма зеркалит парсер
// topology.parseDeclaredRoles (`{sid, role}`, ADR-008); role kebab-case
// (`primary`/`replica`/...), пустая допустима (хост вне declared-роли).
type SpecHostDecl struct {
	SID  string
	Role string
}

// SeedIncarnationForCreate вставляет incarnation в status='ready' с ПУСТЫМ state
// (`{}`) и declared `spec.hosts[]` (роли host-0/host-1/...). Отличие от
// [Stack.SeedIncarnationReady]: state пуст (create наполнит его сам через
// state_changes), а spec несёт declared-роли — их читает topology.parseDeclaredRoles
// при резолве `soulprint.hosts.where("role == 'primary'")` в create-scenario.
//
// Зачем отдельный helper: POST /v1/incarnations НЕ принимает declared spec.hosts
// (ADR-008, ровно как поясняет ТЗ), а bootstrap-create-сценарии, таргетящие
// primary по `soulprint.hosts.where("role == 'primary'")[0]`, этим declared-ролям
// обязаны. Прямой SQL-seed spec.hosts ДО RunScenario(create) закрывает разрыв
// «declared-роль недоступна офлайн».
//
// status='ready' → штатный RunScenario(create) проходит lock-gate (lockRun
// стартует обычный прогон из ready, run.go), без FromLocked-хака.
func (s *Stack) SeedIncarnationForCreate(t *testing.T, name, service, serviceVersion string, hosts []SpecHostDecl) {
	t.Helper()
	specHosts := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		obj := map[string]any{"sid": h.SID}
		if h.Role != "" {
			obj["role"] = h.Role
		}
		specHosts = append(specHosts, obj)
	}
	specJSON, err := json.Marshal(map[string]any{"hosts": specHosts})
	if err != nil {
		t.Fatalf("SeedIncarnationForCreate(%s): marshal spec: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation (name, service, service_version, spec, state, status)
		VALUES ($1, $2, $3, $4::jsonb, '{}'::jsonb, 'ready')
	`, name, service, serviceVersion, string(specJSON)); err != nil {
		t.Fatalf("SeedIncarnationForCreate(%s): %v", name, err)
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
