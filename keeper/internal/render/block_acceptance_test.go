package render

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// redisResolver — фикстурный DestinyResolver для apply: destiny: redis в
// acceptance-сценарии restart. Возвращает минимальную destiny (один module-шаг),
// чтобы apply-задачи прогона разворачивались без снапшота.
type redisResolver struct{}

func (redisResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	if name != "redis" {
		return nil, errors.New("unknown destiny " + name)
	}
	return &ResolvedDestiny{
		Name: "redis",
		Tasks: []config.Task{
			{Name: "redis-step", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
		},
		Input: config.InputSchemaMap{"action": {Type: "string"}},
	}, nil
}

// TestAcceptance_RestartBlockFanOut — ★ ПРИЁМКА ТЗ: реальный потребитель
// examples/service/redis-cluster/scenario/restart/main.yml (раньше падал
// ErrUnsupportedDSL на block) рендерится корректно. Рендерим Passage 1 (где живёт
// block) с per-host register probe (Passage 0): хост a — master, b/c — slave.
//
// Доказывает:
//   - block fan-out: 2 потомка блока (Restart + Wait) разворачиваются в 2
//     RenderedTask со сквозными Index;
//   - унаследованный block.where (register.redis_role.stdout == 'slave'):
//     потомки таргетят ТОЛЬКО slave-хосты (b, c), не master (a);
//   - serial:1 наследуется: каждый потомок несёт SerialWidth=1.
func TestAcceptance_RestartBlockFanOut(t *testing.T) {
	path := filepath.FromSlash("../../../examples/service/redis-cluster/scenario/restart/main.yml")
	m, _, diags, err := config.LoadScenarioManifest(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostic (%s): %s", d.Code, d.Message)
		}
	}

	plan, err := Stratify(m.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "redis-prod", Service: "redis"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"redis-prod"}, nil),
			host("b.example.com", []string{"redis-prod"}, nil),
			host("c.example.com", []string{"redis-prod"}, nil),
		},
		TaskPassage:   plan.TaskPassage,
		ActivePassage: 1, // Passage 1 — где живёт block (см. passage_plan [0 1 1 1 2]).
		// Per-host register Passage 0 (probe redis_role): a=master, b/c=slave.
		RegisterByHost: map[string]map[string]any{
			"a.example.com": {"redis_role": map[string]any{"stdout": "master"}},
			"b.example.com": {"redis_role": map[string]any{"stdout": "slave"}},
			"c.example.com": {"redis_role": map[string]any{"stdout": "slave"}},
		},
		Destiny: redisResolver{},
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (приёмка restart/main.yml): %v", err)
	}

	// Собираем block-потомков по имени: 2 шага блока (Restart + Wait).
	blockChildren := map[string]*RenderedTask{}
	blockPlans := map[string]*DispatchPlan{}
	byIndex := map[int]*RenderedTask{}
	for _, tk := range tasks {
		byIndex[tk.Index] = tk
	}
	for i := range plans {
		tk := byIndex[plans[i].TaskIndex]
		if tk == nil {
			continue
		}
		switch tk.Name {
		case "Restart redis-server", "Wait until replica is healthy again":
			blockChildren[tk.Name] = tk
			blockPlans[tk.Name] = &plans[i]
		}
	}
	if len(blockChildren) != 2 {
		t.Fatalf("block fan-out дал %d потомков, want 2 (Restart + Wait)", len(blockChildren))
	}

	for name, pl := range blockPlans {
		// Унаследованный block.where (slave) → таргет ТОЛЬКО slave-хосты b,c.
		if len(pl.TargetSIDs) != 2 {
			t.Errorf("block-потомок %q таргетит %v, want [b c] (унаследованный where: slave)", name, pl.TargetSIDs)
			continue
		}
		for _, sid := range pl.TargetSIDs {
			if sid == "a.example.com" {
				t.Errorf("block-потомок %q таргетит master a — унаследованный where: slave не применился", name)
			}
		}
		// serial:1 наследуется всеми потомками.
		if pl.SerialWidth != 1 {
			t.Errorf("block-потомок %q SerialWidth = %d, want 1 (унаследован block.serial:1)", name, pl.SerialWidth)
		}
	}
}
