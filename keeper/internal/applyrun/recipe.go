package applyrun

import (
	"encoding/json"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// Recipe — render-инструкция для just-in-time-рендера задания одного хоста
// Acolyte-ом при claim (ADR-027(c)(f)). Сохраняется в jsonb-колонке
// `apply_runs.recipe` (миграция 029) при dispatch-е planned-задания (Phase
// 1.4.2) и читается Acolyte-ом при claim (Phase 1.4.3), чтобы воспроизвести
// путь vault-resolve → input-validation → CEL-render → text/template-render →
// ApplyRequest БЕЗ опоры на память run-goroutine (которая живёт на одном
// инстансе и не переживает cross-Keeper-роутинг / рестарт).
//
// Recipe — это персистентная форма [scenario.RunSpec] минус ApplyID/
// IncarnationName (они — отдельные колонки apply_runs): несёт ровно то, что
// нужно Acolyte-у для повторения render-шагов.
//
// Инвариант A (ADR-027): рецепт несёт vault-ref КАК ЕСТЬ — Input хранит
// `incarnation.spec.input` оператора со строковыми `vault:`-ссылками, секреты
// НЕ раскрыты. essence / RenderedTask / ApplyRequest в рецепт НЕ кладутся —
// Acolyte резолвит их в RAM при claim и отдаёт Soul-у; в PG раскрытые секреты и
// готовый рендер не оседают. StartedByAID нужен для audit-ctx при резолве vault
// на claim (от чьего имени читается секрет).
type Recipe struct {
	// ServiceRef — git-координаты Service-репо для загрузки артефакта при claim
	// (тот же тип, что несёт RunSpec; переиспользуем, не зеркалим).
	ServiceRef artifact.ServiceRef `json:"service_ref"`
	// ScenarioName — имя сценария (snake_case), точка входа
	// `scenario/<name>/main.yml`.
	ScenarioName string `json:"scenario_name"`
	// Input — `incarnation.spec.input` оператора КАК ЕСТЬ: vault-ref строками,
	// БЕЗ резолва (инвариант A). nil допустим (сценарий без input).
	Input map[string]any `json:"input,omitempty"`
	// StartedByAID — AID инициатора прогона для audit-ctx при резолве vault на
	// claim. NULL для прогонов без identity Архонта (Soul-инициированные /
	// system).
	StartedByAID *string `json:"started_by_aid,omitempty"`
	// DryRun — Scry-флаг (ADR-031): Acolyte построит `ApplyRequest{dry_run:true}`
	// для этого задания, Soul зовёт `mod.Plan` вместо `mod.Apply` (pure-read,
	// read-safe-capability обязательна). Поле omitempty/false для старых
	// рецептов forward-compat — отсутствие в jsonb эквивалентно false (обычный
	// apply). Заполняется только check-drift-путём (Runner.CheckDrift), обычный
	// run/destroy-путь не трогает.
	DryRun bool `json:"dry_run,omitempty"`
	// FromUpgrade — грузить сценарий из upgrade/<slug>/, а не scenario/<slug>/
	// (ADR-0068): Acolyte при claim re-render-ит upgrade-прогон тем же путём, что
	// run-goroutine. omitempty/false для forward-compat — отсутствие в jsonb ==
	// обычный scenario/-путь. Заполняется found-веткой автозапуска (RunSpec.FromUpgrade).
	FromUpgrade bool `json:"from_upgrade,omitempty"`
}

// MarshalRecipe сериализует рецепт в jsonb-форму колонки apply_runs.recipe.
// nil-рецепт → (nil, nil): старый путь Insert(running) рецепт не несёт, в
// колонку пишется SQL NULL.
func MarshalRecipe(r *Recipe) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("applyrun: marshal recipe: %w", err)
	}
	return b, nil
}

// UnmarshalRecipe восстанавливает рецепт из jsonb-байтов колонки. Пустой вход
// (nil / len 0 — SQL NULL у строк старого пути) → (nil, nil): отсутствие
// рецепта не ошибка на уровне типа (non-NULL для claim-пути — инвариант
// claim-логики, не парсера).
func UnmarshalRecipe(b []byte) (*Recipe, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var r Recipe
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("applyrun: unmarshal recipe: %w", err)
	}
	return &r, nil
}
