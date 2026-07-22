//go:build integration

// Integration test for StateLister against a live PG (testcontainers): real
// service/coven filtering (SQL pushdown) + page-by-page draining + state-jsonb
// passthrough. Reuses integrationPool / resetAll / seedOperator from
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

	// redis service, coven=prod, different redis_version in state.
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

	// pushdown service=redis & coven=prod → exactly redis-prod-a, redis-prod-b.
	got := drain(t, l, statepredicate.BaseFilter{Service: "redis", Coven: "prod"})
	if len(got) != 2 {
		t.Fatalf("service=redis coven=prod: got %d rows %v, want 2", len(got), names(got))
	}
	byName := map[string]map[string]any{}
	for _, s := range got {
		byName[s.Name] = s.State
	}
	if _, ok := byName["redis-prod-a"]; !ok {
		t.Errorf("redis-prod-a missing from the result set")
	}
	if _, ok := byName["redis-prod-b"]; !ok {
		t.Errorf("redis-prod-b missing from the result set")
	}
	// state-jsonb passed through (foundation for CEL-eval).
	if byName["redis-prod-a"]["redis_version"] != "8.0" {
		t.Errorf("state was not propagated: %+v", byName["redis-prod-a"])
	}

	// pushdown service=postgres only → exactly pg-prod.
	gotPg := drain(t, l, statepredicate.BaseFilter{Service: "postgres"})
	if len(gotPg) != 1 || gotPg[0].Name != "pg-prod" {
		t.Fatalf("service=postgres: got %v, want [pg-prod]", names(gotPg))
	}
}

// Multi-page drain against a live PG: create > statePageSize incarnations,
// verify the offset/limit loop visits all of them without loss or duplicates.
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
		t.Fatalf("multi-page: got %d rows, want %d (offset/limit loop lost rows)", len(got), n)
	}
	seen := make(map[string]bool, n)
	for _, s := range got {
		if seen[s.Name] {
			t.Fatalf("duplicate %q - page re-read", s.Name)
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
