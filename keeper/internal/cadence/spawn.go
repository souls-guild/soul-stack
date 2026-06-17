package cadence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// TargetRecipe — declarative-target рецепта Cadence, хранится в jsonb-колонке
// `target` как есть (та же форма, что POST /v1/voyages target — ADR-046 §3).
// Резолвится в snapshot единиц при спавне (Reaper-правило spawn_due_cadence).
//
// Поля по kind: scenario читает Incarnations/Service/Coven; command —
// SIDs/Where/Coven. Нерелевантные для kind поля игнорируются резолвером.
type TargetRecipe struct {
	// scenario-режим:
	Incarnations []string `json:"incarnations,omitempty"`
	Service      string   `json:"service,omitempty"`
	// command-режим:
	SIDs  []string `json:"sids,omitempty"`
	Where string   `json:"where,omitempty"`
	// общий env-тег incarnation / coven-метка хоста:
	Coven []string `json:"coven,omitempty"`
}

// ScenarioResolver — резолв scenario-target Cadence → snapshot ИМЁН инкарнаций
// (parity handlers.VoyageScenarioResolver). Спавн-путь пере-резолвит target
// just-in-time (snapshot фиксируется на момент спавна, не создания Cadence).
type ScenarioResolver interface {
	ResolveIncarnations(ctx context.Context, incarnations []string, service, coven string) ([]string, error)
}

// CommandResolver — резолв command-target Cadence → snapshot SID-ов (parity
// handlers.VoyageCommandResolver). requireAlive — presence-фильтр живых.
type CommandResolver interface {
	ResolveSIDs(ctx context.Context, sids, covens []string, where string, requireAlive bool) ([]string, error)
}

// effectiveBatchSize резолвит эффективный размер пачки (parity handler
// effectiveBatchSize, ADR-043 amendment §2): batch_percent → ceil(scope*pct/100),
// clamp [1, scope]; иначе batch_size как есть (nil = весь прогон одним Leg).
func effectiveBatchSize(batchSize, batchPercent *int, scope int) *int {
	if batchPercent == nil || scope <= 0 {
		return batchSize
	}
	eff := (scope*(*batchPercent) + 99) / 100
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	return &eff
}

// effectiveFailThreshold резолвит порог провалов на spawn-scope (ADR-043 amendment
// 2026-06-09, Cadence-recipe S3). Зеркало effectiveBatchSize: percent →
// ceil(scope*pct/100), clamp [1, scope]; иначе абсолютный threshold как есть (nil ⇒
// без порога). Резолв происходит ЗДЕСЬ, а не на create-time Cadence: scope Cadence
// (len(resolved)) известен лишь при спавне — отличие от Voyage, где
// resolveMaxFailuresPercent резолвит percent в абсолют ещё на create (там scope
// уже резолвнут). Спавнящийся Voyage получает уже абсолютный FailThreshold
// (у Voyage нет колонки percent).
func effectiveFailThreshold(failThreshold, failThresholdPercent *int, scope int) *int {
	if failThresholdPercent == nil || scope <= 0 {
		return failThreshold
	}
	eff := (scope*(*failThresholdPercent) + 99) / 100
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	return &eff
}

// batchIndexFor — 0-based индекс Leg-а i-й единицы (chunk по batch_size). window →
// 0 у всех (плоский прогон, нет Leg-ов); barrier с nil/<=0 batch_size → один Leg.
func batchIndexFor(i int, batchSize *int, mode voyage.BatchMode) int {
	if mode == voyage.BatchModeWindow || batchSize == nil || *batchSize <= 0 {
		return 0
	}
	return i / *batchSize
}

// totalBatches — число Leg-ов. window → 1 (одна волна-окно); barrier без
// batch_size → 1 Leg = весь прогон; barrier с batch_size → ceil(n/bs).
func totalBatches(n int, batchSize *int, mode voyage.BatchMode) int {
	if n == 0 {
		return 0
	}
	if mode == voyage.BatchModeWindow || batchSize == nil || *batchSize <= 0 {
		return 1
	}
	bs := *batchSize
	return (n + bs - 1) / bs
}

// BuildVoyage собирает Voyage-row + targets-snapshot из рецепта Cadence и
// резолвнутого набора единиц (имена инкарнаций для scenario / SID-ы для command).
// voyageID — caller-сгенерированный ULID; cadenceID проставляется как back-link
// (ADR-046 §2). batch_mode пишется только при window (barrier ⇒ NULL,
// forward-compat, parity handler.buildVoyageRow).
//
// startedByAID = created_by_aid Cadence (спавн от имени создателя, ADR-046 §7).
func BuildVoyage(c *Cadence, voyageID string, resolved []string) (*voyage.Voyage, []voyage.VoyageTarget) {
	mode := voyage.BatchMode(voyage.BatchModeBarrier)
	if c.BatchMode != nil {
		mode = voyage.BatchMode(*c.BatchMode)
	}
	effBatch := effectiveBatchSize(c.BatchSize, c.BatchPercent, len(resolved))
	effFailThreshold := effectiveFailThreshold(c.FailThreshold, c.FailThresholdPercent, len(resolved))

	targetKind := voyage.TargetKindIncarnation
	if c.Kind == KindCommand {
		targetKind = voyage.TargetKindSID
	}
	targets := make([]voyage.VoyageTarget, len(resolved))
	for i, id := range resolved {
		targets[i] = voyage.VoyageTarget{
			TargetKind: targetKind,
			TargetID:   id,
			BatchIndex: batchIndexFor(i, effBatch, mode),
			Status:     voyage.TargetStatusAwaiting,
		}
	}

	resolvedJSON, _ := json.Marshal(resolved) // []string всегда сериализуется

	v := &voyage.Voyage{
		VoyageID:       voyageID,
		Kind:           voyage.Kind(c.Kind),
		ScenarioName:   c.ScenarioName,
		Module:         c.Module,
		Input:          c.Input,
		TargetResolved: resolvedJSON,
		TargetOrigin:   json.RawMessage(c.Target),
		Concurrency:    c.Concurrency,
		TotalBatches:   totalBatches(len(resolved), effBatch, mode),
		StartedByAID:   c.CreatedByAID,
		FailThreshold:  effFailThreshold,
		RequireAlive:   c.RequireAlive,
		CadenceID:      &c.ID,
	}
	if c.OnFailure != nil {
		of := voyage.OnFailure(*c.OnFailure)
		v.OnFailure = &of
	}
	if mode == voyage.BatchModeWindow {
		bm := mode
		v.BatchMode = &bm
		// window: batch_size/batch_percent не используются (ширина окна =
		// concurrency), inter_unit_interval — per-unit пауза.
		v.InterUnitInterval = c.InterUnitInterval
	} else {
		v.BatchSize = effBatch
		v.BatchPercent = c.BatchPercent
		v.InterBatchInterval = c.InterBatchInterval
	}
	return v, targets
}

// ResolveScope резолвит declarative-target рецепта Cadence в snapshot единиц
// (имена инкарнаций для scenario / SID-ы для command) через переданные
// resolver-ы. Пустой результат — не ошибка (caller трактует как «нет целей,
// спавнить нечего», parity handler voyage_empty_target — но в spawn-пути это
// мягкий skip, не 422).
func ResolveScope(ctx context.Context, c *Cadence, sr ScenarioResolver, cr CommandResolver) ([]string, error) {
	var recipe TargetRecipe
	if len(c.Target) > 0 {
		if err := json.Unmarshal(c.Target, &recipe); err != nil {
			return nil, fmt.Errorf("cadence: parse target recipe: %w", err)
		}
	}
	switch c.Kind {
	case KindScenario:
		coven := ""
		if len(recipe.Coven) > 0 {
			coven = recipe.Coven[0]
		}
		return sr.ResolveIncarnations(ctx, recipe.Incarnations, recipe.Service, coven)
	case KindCommand:
		return cr.ResolveSIDs(ctx, recipe.SIDs, recipe.Coven, recipe.Where, deref(c.RequireAlive))
	default:
		return nil, fmt.Errorf("cadence: invalid kind %q", c.Kind)
	}
}

func deref(b *bool) bool { return b != nil && *b }

// NewVoyageID генерит ULID для спавнутого Voyage (parity handler.buildVoyageRow).
// Var (не функция-обёртка) для подмены в unit-тестах spawn-правила.
var NewVoyageID = audit.NewULID
