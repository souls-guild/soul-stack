package rbac

import (
	"errors"
	"fmt"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// maxSoulprintExprLen — верхняя граница длины soulprint-CEL-предиката селектора
// (ADR-047 S2b). Параллель [maxRegexLen]: cap длины — дешёвая страховка против
// раздутых выражений в снимке (compile-cost/память на load). CEL-go не
// подвержен catastrophic backtracking, ограничение — про объём, не про ReDoS.
const maxSoulprintExprLen = 512

// soulprintEngine — общий валидатор/eval-движок soulprint-предикатов RBAC-
// селектора. Sandbox-режим [cel.NewFlowControl]: объявлен `soulprint.self.*`
// (ADR-018 каноническая форма), запрещены vault()/now()/register/state
// (scope-предикат = чистая функция от фактов хоста), а soulprint.hosts/
// soulprint.where отсекаются изоляцией allowHosts=false. Движок потокобезопасен
// (compile-cache под RWMutex) и переиспользуется всеми Check/load — НЕ
// дублируем CEL-движок проекта.
//
// Собирается лениво один раз: конструктор [cel.NewFlowControl] не зависит от
// рантайма (ошибка только при программной несовместимости cel-go), но строить
// его в init() значит платить на каждый импорт пакета; ленивая сборка под
// sync.Once дешевле.
var (
	soulprintEngineOnce sync.Once
	soulprintEngineInst *cel.Engine
	soulprintEngineErr  error
)

func soulprintEngine() (*cel.Engine, error) {
	soulprintEngineOnce.Do(func() {
		soulprintEngineInst, soulprintEngineErr = cel.NewFlowControl()
	})
	return soulprintEngineInst, soulprintEngineErr
}

// validateSoulprintExpr компилирует soulprint-CEL на load снимка (ADR-047 S2b).
// Fatal — только compile-фаза: синтаксис, неизвестный корень (register/state/
// vault/now), host-аксессор (soulprint.hosts → isolation-error). Runtime-no-such-
// key (предикат ссылается на факт, которого нет в ПУСТЫХ фактах валидации) —
// НЕ ошибка load (на реальном хосте факт будет): eval против пустого
// SoulprintSelf даёт [cel.ErrEval], его проглатываем. Так битый CEL фейлит load
// (как битый regex / unknown-permission), а валидный — нет, без подачи фейковых
// фактов.
func validateSoulprintExpr(expr string) error {
	e, err := soulprintEngine()
	if err != nil {
		return fmt.Errorf("soulprint CEL engine: %w", err)
	}
	_, evalErr := e.EvalPredicate(expr, cel.Vars{SoulprintSelf: map[string]any{}})
	if evalErr == nil {
		return nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return evalErr
	}
	// [cel.ErrEval] на пустых фактах (no-such-key, не-bool на отсутствующем
	// ключе) — ожидаемо, выражение синтаксически валидно.
	var ee *cel.ErrEval
	if errors.As(evalErr, &ee) {
		return nil
	}
	// Прочее (теоретически недостижимо) — fail-closed: load фейлится.
	return evalErr
}

// EvalSoulprintExpr вычисляет soulprint-предикат против фактов хоста
// (`soulprint.self.*`, ADR-018). Готов под слайсы S3/S4 (list-видимость/target):
// резолвер list/target подаёт реальные SoulprintFacts хоста.
//
// Возврат:
//   - (true, nil)  — предикат истинен (хост в scope);
//   - (false, nil) — ложен ЛИБО факт отсутствует (no-such-key → default-deny:
//     отсутствие нужного факта = «не в scope», не ошибка);
//   - (false, err) — compile-ошибка (битый предикат; в норме отсеян на load).
//
// Семантика «runtime-no-match = (false, nil)» симметрична oracle.WhereEvaluator
// и flow-control-предикатам: недоверенный/неполный facts-снимок не должен
// ронять резолвер, отсутствие факта трактуется как «не сматчило».
func EvalSoulprintExpr(expr string, facts map[string]any) (bool, error) {
	e, err := soulprintEngine()
	if err != nil {
		return false, fmt.Errorf("soulprint CEL engine: %w", err)
	}
	ok, evalErr := e.EvalPredicate(expr, cel.Vars{SoulprintSelf: facts})
	if evalErr == nil {
		return ok, nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return false, evalErr
	}
	// Runtime (no-such-key / non-bool) → no-match (default-deny).
	return false, nil
}
