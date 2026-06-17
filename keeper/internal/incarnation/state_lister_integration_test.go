//go:build integration

// Integration-тест StateLister против живой PG (testcontainers): реальная
// выборка по service/coven (SQL-pushdown) + page-by-page дренаж + проброс
// state-jsonb. Переиспользует integrationPool / resetAll / seedOperator из
// integration_test.go.

package incarnation

import (
	"context"
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
)

func TestIntegration_StateLister_PushdownAndPages(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	// redis-сервис, coven=prod, разные redis_version в state.
	mk := func(name, service string, covens []string, state map[string]any) {
		t.Helper()
		inc := &Incarnation{
			Name: name, Service: service, ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
			Covens: covens, State: state,
		}
		if err := Create(ctx, integrationPool, inc); err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
	}

	mk("redis-prod-a", "redis", []string{"prod"}, map[string]any{"redis_version": "8.0"})
	mk("redis-prod-b", "redis", []string{"prod"}, map[string]any{"redis_version": "7.4"})
	mk("redis-stg", "redis", []string{"staging"}, map[string]any{"redis_version": "8.0"})
	mk("pg-prod", "postgres", []string{"prod"}, map[string]any{"pg_version": "16"})

	l := NewStateLister(integrationPool)

	// pushdown service=redis & coven=prod → ровно redis-prod-a, redis-prod-b.
	got := drain(t, l, statepredicate.BaseFilter{Service: "redis", Coven: "prod"})
	if len(got) != 2 {
		t.Fatalf("service=redis coven=prod: got %d строк %v, want 2", len(got), names(got))
	}
	byName := map[string]map[string]any{}
	for _, s := range got {
		byName[s.Name] = s.State
	}
	if _, ok := byName["redis-prod-a"]; !ok {
		t.Errorf("redis-prod-a отсутствует в выборке")
	}
	if _, ok := byName["redis-prod-b"]; !ok {
		t.Errorf("redis-prod-b отсутствует в выборке")
	}
	// state-jsonb пробросился (фундамент под CEL-eval).
	if byName["redis-prod-a"]["redis_version"] != "8.0" {
		t.Errorf("state не пробросился: %+v", byName["redis-prod-a"])
	}

	// pushdown только service=postgres → ровно pg-prod.
	gotPg := drain(t, l, statepredicate.BaseFilter{Service: "postgres"})
	if len(gotPg) != 1 || gotPg[0].Name != "pg-prod" {
		t.Fatalf("service=postgres: got %v, want [pg-prod]", names(gotPg))
	}
}

// Многостраничный дренаж против живой PG: создаём > statePageSize инкарнаций,
// проверяем что offset/limit-цикл обходит все без потерь и дублей.
func TestIntegration_StateLister_MultiPage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	const n = statePageSize + 25
	for i := 0; i < n; i++ {
		inc := &Incarnation{
			Name: fmt.Sprintf("svc-inc-%05d", i), Service: "bulk", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
			State: map[string]any{"idx": i},
		}
		if err := Create(ctx, integrationPool, inc); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}

	l := NewStateLister(integrationPool)
	got := drain(t, l, statepredicate.BaseFilter{Service: "bulk"})

	if len(got) != n {
		t.Fatalf("multi-page: got %d строк, want %d (offset/limit-цикл потерял строки)", len(got), n)
	}
	seen := make(map[string]bool, n)
	for _, s := range got {
		if seen[s.Name] {
			t.Fatalf("дубликат %q — страница перечитана", s.Name)
		}
		seen[s.Name] = true
	}
}

func drain(t *testing.T, l *StateLister, base statepredicate.BaseFilter) []statepredicate.Stated {
	t.Helper()
	var all []statepredicate.Stated
	if err := l.ListStatePages(context.Background(), base, func(page []statepredicate.Stated) error {
		all = append(all, page...)
		return nil
	}); err != nil {
		t.Fatalf("ListStatePages: %v", err)
	}
	return all
}

func names(ss []statepredicate.Stated) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}
