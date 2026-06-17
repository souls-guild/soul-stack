package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// TestClaim_HostTaskFilter — фильтр задач прогона по SID заклеймленного хоста
// (claim.execute переиспользует groupByHost). on:/where:-отфильтрованный хост
// (пустой TargetSIDs) → нет задач → no-op no_match (FINDING-01 вариант (б));
// иначе только его задачи.
func TestClaim_HostTaskFilter(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "t0", Module: "core.exec.run"},
		{Index: 1, Name: "t1", Module: "core.file.present"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"host-a", "host-b"}},
		{TaskIndex: 1, TargetSIDs: []string{"host-b"}},
	}
	perHost := groupByHost(tasks, plans)

	if got := perHost["host-a"]; len(got) != 1 || got[0].Name != "t0" {
		t.Errorf("host-a tasks = %+v, want [t0]", got)
	}
	if got := perHost["host-b"]; len(got) != 2 {
		t.Errorf("host-b tasks = %d, want 2", len(got))
	}
	// on:/where: отфильтровал всё на host-c → нет задач → claim закроет no-op
	// терминалом no_match (FINDING-01 вариант (б)), не success.
	if got := perHost["host-c"]; len(got) != 0 {
		t.Errorf("host-c tasks = %d, want 0 (no-op no_match)", len(got))
	}
}

// TestClaim_AbortedGuard — drain-guard (graceful-drain пула Acolyte, ADR-027 Phase 2):
// execute считает задание прерванным drain-ом ровно тогда, когда claim-ctx
// отменён. На отменённом ctx render/SendApply-ошибка НЕ ведёт в markFailed —
// Ward остаётся в БД (claimed) для recovery; на живом ctx доменная ошибка
// штатно ведёт в failed.
func TestClaim_AbortedGuard(t *testing.T) {
	c := &ClaimRunner{}

	live := context.Background()
	if c.aborted(live) {
		t.Error("живой ctx не должен считаться drain-прерванным")
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !c.aborted(cctx) {
		t.Error("отменённый claim-ctx должен считаться drain-прерванным")
	}
}

// TestClaim_FailedSummaryMasksSecret — инвариант A: failed-summary, собираемый
// из render-ошибки (maskErrText), НЕ несёт ни раскрытого секрета, ни голого
// vault-ref. Так провал claim-задания не утекает секрет в operator-facing
// status_details / error_summary.
func TestClaim_FailedSummaryMasksSecret(t *testing.T) {
	// render-ошибка, транзитом несущая vault-ref в тексте.
	err := errors.New("scenario: RenderForHost: render redis-prod/create: vault:secret/db-creds#password missing")
	summary := maskErrText(err)

	if strings.Contains(summary, "vault:secret/db-creds") {
		t.Errorf("summary несёт голый vault-ref: %q", summary)
	}
	if strings.Contains(summary, "***MASKED***") == false {
		t.Errorf("summary не замаскирован: %q", summary)
	}
}

// TestClaim_RecipeCarriesVaultRefAsIs — инвариант A на уровне рецепта: Input
// рецепта несёт vault-ref СТРОКОЙ (КАК ЕСТЬ), секрет НЕ раскрыт. Это то, что
// dispatchPlanned кладёт в planned-строку и что Acolyte получает при claim до
// ResolveInputValuesVault.
func TestClaim_RecipeCarriesVaultRefAsIs(t *testing.T) {
	recipe := &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "redis", Git: "https://example.test/redis.git", Ref: "main"},
		ScenarioName: "create",
		Input:        map[string]any{"db_password": "vault:secret/db-creds#password"},
	}
	b, err := applyrun.MarshalRecipe(recipe)
	if err != nil {
		t.Fatalf("MarshalRecipe: %v", err)
	}
	// В persisted-рецепте — именно vault-ref-строка, не раскрытое значение.
	if !strings.Contains(string(b), "vault:secret/db-creds#password") {
		t.Errorf("рецепт не несёт vault-ref как есть: %s", b)
	}

	back, err := applyrun.UnmarshalRecipe(b)
	if err != nil {
		t.Fatalf("UnmarshalRecipe: %v", err)
	}
	if back.Input["db_password"] != "vault:secret/db-creds#password" {
		t.Errorf("round-trip потерял vault-ref: %v", back.Input["db_password"])
	}
}
