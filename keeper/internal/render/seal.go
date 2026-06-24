package render

import (
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// seal / sealed-paths ([ADR-010] §7.4) — render-time provenance/taint. Pipeline
// помечает путь ячейки params SEALED, когда её СЫРОЕ (до vault-resolve+CEL)
// значение — строка с `${ … }`-выражением, читающим secret-источник: secret-
// input активной схемы прохода, vault(), транзитивно sealed vars/compute.
// Детекцию по AST делает [cel.Engine.DetectSealed] (shared/cel/seal.go).
//
// SealedSet аккумулирует найденные пути (dot/idx-форма, БИТ-В-БИТ как
// renderValue path) ЗА ОДИН Render-прогон. Caller (scenario.run) создаёт его,
// кладёт в [RenderInput.Sealed] и после Render использует пути для seal-aware
// маскинга (audit.MaskSecretsSealed) на write-точках (error_summary/status_details).
// nil → коллекция выключена (push/trial/Acolyte без seal-нужды — БИТ-В-БИТ).
type SealedSet struct {
	paths map[string]bool
}

// NewSealedSet — пустой аккумулятор sealed-путей одного Render-прогона.
func NewSealedSet() *SealedSet { return &SealedSet{paths: map[string]bool{}} }

// add помечает путь sealed. nil-получатель — no-op (коллекция выключена).
func (s *SealedSet) add(path string) {
	if s == nil {
		return
	}
	s.paths[path] = true
}

// Paths возвращает набор sealed-путей для audit.MaskSecretsSealed (map копируется
// — caller не мутирует внутреннее состояние). nil-получатель → nil.
func (s *SealedSet) Paths() map[string]bool {
	if s == nil || len(s.paths) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.paths))
	for k := range s.paths {
		out[k] = true
	}
	return out
}

// scenarioSealSources строит [cel.SealSources] для scenario-прохода: secret-input
// активной scenario-схемы. vars/compute транзитивность в пилоте не вычисляется
// предварительно (vars резолвятся per-task; secret-провенанс через vars ловится
// тем, что само значение vars прошло DetectSealed — это расширение). nil-схема →
// пустой набор (детектор ловит только vault()).
func scenarioSealSources(in RenderInput) cel.SealSources {
	return cel.SealSources{SecretInputs: secretInputNames(in.Scenario)}
}

// secretInputNames — имена input-параметров, объявленных secret:true в схеме
// прохода (scenario.Input / destiny.Input — обе config.InputSchemaMap). nil →
// пустой набор.
func secretInputNames(scn *config.ScenarioManifest) map[string]bool {
	if scn == nil || len(scn.Input) == 0 {
		return nil
	}
	out := make(map[string]bool, len(scn.Input))
	for name, s := range scn.Input {
		if s != nil && s.Secret {
			out[name] = true
		}
	}
	return out
}

// collectSealed обходит СЫРЫЕ params (до vault-resolve+CEL) тем же path-обходом,
// что renderValue, и помечает в set путь каждой строковой ячейки, чьё `${ … }`-
// выражение читает secret-источник (engine.DetectSealed). set nil → no-op.
// Вызывается per-task ОДИН раз (не per-host): провенанс ячейки host-инвариантен
// (secret-источник один и тот же на всех хостах).
func collectSealed(engine *cel.Engine, set *SealedSet, params map[string]any, sources cel.SealSources, base string) {
	if set == nil {
		return
	}
	walkSealed(engine, set, params, sources, base)
}

func walkSealed(engine *cel.Engine, set *SealedSet, v any, sources cel.SealSources, path string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			walkSealed(engine, set, val, sources, joinKey(path, k))
		}
	case []any:
		for i, val := range t {
			walkSealed(engine, set, val, sources, joinIdx(path, i))
		}
	case string:
		if engine.DetectSealed(t, sources) {
			set.add(path)
		}
	}
}
