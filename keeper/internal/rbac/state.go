package rbac

import (
	"fmt"
	"sync"

	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
)

// maxStateExprLen — верхняя граница длины state-CEL-предиката селектора
// (ADR-047 S2c). Параллель [maxSoulprintExprLen]/[maxRegexLen]: cap длины —
// дешёвая страховка против раздутых выражений в снимке (compile-cost/память на
// load). CEL-go не подвержен catastrophic backtracking, ограничение — про объём.
const maxStateExprLen = 512

// stateResolver — общий валидатор/eval state-предикатов RBAC-селектора (ADR-047
// S2c). НЕ дублирует CEL-движок: делегирует keeper/internal/statepredicate
// (migration-sandbox корень `state`, запрещены vault/now/register/soulprint/
// input/essence — state-предикат = чистая функция от incarnation.state). Один
// резолвер на процесс (потокобезопасен, общий compile-cache shared/cel) — как
// soulprintEngine.
//
// Собирается лениво один раз: конструктор statepredicate.New не зависит от
// рантайма (ошибка только при программной несовместимости cel-go), но строить
// его в init() значит платить на каждый импорт пакета; ленивая сборка под
// sync.Once дешевле.
var (
	stateResolverOnce sync.Once
	stateResolverInst statepredicate.Resolver
	stateResolverErr  error
)

func stateResolver() (statepredicate.Resolver, error) {
	stateResolverOnce.Do(func() {
		stateResolverInst, stateResolverErr = statepredicate.New()
	})
	return stateResolverInst, stateResolverErr
}

// validateStateExpr компилирует state-CEL на load снимка (ADR-047 S2c) через
// statepredicate.Compile: синтаксис + sandbox (запрещённый корень/функция) →
// load-fail. Runtime-no-such-key на реальной инкарнации НЕ ошибка (Compile это
// уже учитывает: валидация против пустого state). Симметрично
// [validateSoulprintExpr], но движок — statepredicate.
func validateStateExpr(expr string) error {
	r, err := stateResolver()
	if err != nil {
		return fmt.Errorf("state CEL engine: %w", err)
	}
	return r.Compile(expr)
}

// EvalStateExpr вычисляет state-предикат против incarnation.state. Готов под S3b
// (видимость/резолв инкарнаций по state): резолвер list/target подаёт реальный
// state инкарнации. Тонкая обёртка над statepredicate.Matches — единый источник
// семантики (no-such-key → fail-closed no-match, не-bool → ошибка автора).
//
// Возврат:
//   - (true, nil)  — предикат истинен (инкарнация в scope);
//   - (false, nil) — ложен ЛИБО нужный state-факт отсутствует (no-such-key →
//     fail-closed: «не в scope», не ошибка);
//   - (false, err) — compile-ошибка (битый предикат; в норме отсеян на load)
//     либо не-bool результат предиката.
func EvalStateExpr(expr string, state map[string]any) (bool, error) {
	r, err := stateResolver()
	if err != nil {
		return false, fmt.Errorf("state CEL engine: %w", err)
	}
	return r.Matches(expr, state)
}
