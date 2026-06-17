// Package statepredicate — единый резолвер инкарнаций по CEL-предикату над
// incarnation.state. Фундамент для трёх потребителей (НЕ дублировать механизм):
//   - фильтр инкарнаций → Run late-binding (target = state-предикат, резолв на
//     старте прогона);
//   - RBAC Purview S2c state-селектор (Purview.StateExprs);
//   - Cadence late-binding по state.
//
// Compile (валидация + кэш program) + Matches (single-incarnation проверка
// против state-map) — фундамент. ResolveIncarnations (list + CEL-фильтр на
// сужённом множестве) добавлен поверх: резолвер сам SQL НЕ знает — list-доступ
// инкапсулирован в [IncarnationStateLister], pushdown ([BaseFilter] по
// service/coven) живёт в реализации lister-а у потребителя, резолвер лишь
// прогоняет [Matches] по уже-сужённому набору. DB-coupling в пакет не тянется,
// lister тривиально мокается в тестах.
//
// CEL-движок НЕ дублируется: переиспользуется shared/cel в migration-режиме
// ([cel.NewMigration]) — единственная песочница проекта с корнем `state`
// (ADR-019). Семантически state-предикат = чистая функция от state, что в
// точности совпадает с migration-CEL sandbox: объявлен только `state.<path>`,
// прочие корни (register/soulprint/essence/input/incarnation/vars) необъявлены
// (compile-ошибка undeclared reference), vault()/now() отсекаются guard-ами.
// Тот же приём, что rbac.soulprint (S2b) с [cel.NewFlowControl].
package statepredicate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// Resolver компилирует и вычисляет state-предикаты. Потокобезопасен
// (compile-cache внутри shared/cel.Engine под RWMutex).
type Resolver interface {
	// Compile валидирует предикат (синтаксис + sandbox) и прогревает кэш
	// program. Зовётся на load (фильтр/RBAC-селектор/Cadence-target проверяют
	// предикат заранее, до прогона). Пустой/blank предикат отвергается.
	//
	// Ошибки — compile-фаза: битый CEL, обращение к запрещённому корню/функции
	// (vault/now/register/soulprint/input/incarnation/essence). Реальное
	// отсутствие state-факта на runtime ошибкой Compile НЕ считается (на
	// конкретной инкарнации факт может быть).
	Compile(predicate string) error

	// Matches проверяет одну инкарнацию: истинен ли predicate для её state.
	//
	// Возврат:
	//   - (true, nil)  — предикат истинен (инкарнация в выборке);
	//   - (false, nil) — ложен ЛИБО нужный state-факт отсутствует (no-such-key
	//     → fail-closed: «не сматчило», не ошибка);
	//   - (false, err) — compile-ошибка (битый/sandbox-предикат; в норме отсеян
	//     на Compile) либо не-bool результат предиката.
	//
	// Семантика «runtime-no-match = (false, nil)» симметрична
	// rbac.EvalSoulprintExpr и oracle.WhereEvaluator: неполный state-снимок не
	// должен ронять резолвер.
	Matches(predicate string, state map[string]any) (bool, error)

	// ResolveIncarnations возвращает имена инкарнаций, чей state удовлетворяет
	// predicate. Перф-стратегия — двухступенчатый pushdown:
	//   1. SQL-pushdown: base ([BaseFilter] service/coven) сужает множество на
	//      стороне lister-а (SQL WHERE) ДО CEL-eval (100k флот → подмножество
	//      сервиса/ковена);
	//   2. page-by-page: lister стримит сужённый набор страницами (см.
	//      [IncarnationStateLister.ListStatePages]), резолвер прогоняет [Matches]
	//      per-incarnation на КАЖДОЙ странице и сразу её отпускает. Весь набор в
	//      память разом не материализуется (важно для крупного сервиса, чьё
	//      подмножество всё ещё велико).
	// Резолвер сам SQL не выполняет — list-доступ и пагинация даёт lister.
	//
	// predicate компилируется один раз (Compile-семантика): пустой/битый/sandbox/
	// не-bool — ошибка ДО обхода набора. Per-incarnation no-such-key — fail-closed
	// (инкарнация просто не в выборке, см. [Matches]). Ошибка lister пробрасывается.
	ResolveIncarnations(ctx context.Context, predicate string, base BaseFilter, lister IncarnationStateLister) ([]string, error)
}

// BaseFilter — pre-фильтр SQL-pushdown (service/coven) для сужения множества
// инкарнаций ДО CEL-eval. Пустые поля — «не фильтровать». Намеренно НЕ
// импортирует incarnation.ListFilter: резолвер не знает о репозитории, адаптер
// потребителя сам мапит эти поля в incarnation.ListFilter (см. [IncarnationStateLister]).
//
// Coven (single) и Covens (multi) — два пути одного измерения «coven-сужение»:
//   - Coven — exact any-of по ОДНОЙ метке (`$n = ANY(covens)`); прежний путь
//     Run/Cadence late-binding.
//   - Covens — multi-coven any-of В РЕЖИМЕ coven∪{name} (ADR-008 amendment a):
//     метка матчит и `covens[] && ARRAY[Covens]`, и `name = ANY(Covens)` (имя
//     incarnation = корневая Coven-метка). Введён ADDITIVE для S3b-3 RBAC-scope
//     резолва: scope-coven `redis-prod` обязан матчить incarnation как с
//     covens[]⊇{redis-prod}, так и с name=redis-prod. Пустой — не фильтровать.
//
// Оба поля additive; при заданных обоих адаптер AND-комбинирует их (на практике
// потребитель использует одно). Старый единственный путь (Coven) не тронут.
type BaseFilter struct {
	Service string
	Coven   string
	Covens  []string
}

// Stated — пара «имя инкарнации + её state» из lister-а. Минимальная проекция,
// нужная резолверу: остальные поля incarnation-строки для CEL-фильтра по state
// не требуются.
type Stated struct {
	Name  string
	State map[string]any
}

// IncarnationStateLister — узкий list-доступ, отвязывающий резолвер от DB.
// Реализация-адаптер переиспользует incarnation.SelectAll для SQL-pushdown по
// base (service/coven) и отдаёт уже-сужённый набор СТРАНИЦАМИ через callback —
// весь набор разом в память не материализуется (page-by-page стратегия
// architect: подмножество крупного сервиса может само быть велико).
//
// ListStatePages вызывает yield на каждую страницу до исчерпания набора. Контракт:
//   - страницы непустые и в стабильном порядке (пагинация не «прыгает»);
//   - ошибка из yield пробрасывается наружу и прерывает обход (резолвер так
//     возвращает не-bool / прочую eval-ошибку);
//   - ошибка чтения страницы из БД пробрасывается наружу.
//
// Реализация ([incarnation.StateLister]) живёт у потребителя/в incarnation-
// пакете, а не здесь: иначе statepredicate потянул бы прямую зависимость на
// incarnation + pgx, что ломает тестируемость (тут lister мокается без PG).
type IncarnationStateLister interface {
	ListStatePages(ctx context.Context, base BaseFilter, yield func(page []Stated) error) error
}

// resolver — реализация Resolver поверх sandbox-Engine shared/cel (migration-
// режим, корень `state`).
type resolver struct {
	engine *cel.Engine
}

// stateEngine — общий sandbox-движок state-предикатов. Собирается лениво один
// раз процессом (конструктор не зависит от рантайма; строить в init() значит
// платить на каждый импорт пакета). Потокобезопасен, переиспользуется всеми
// Resolver-ами — единый compile-cache на процесс.
var (
	stateEngineOnce sync.Once
	stateEngineInst *cel.Engine
	stateEngineErr  error
)

func stateEngine() (*cel.Engine, error) {
	stateEngineOnce.Do(func() {
		stateEngineInst, stateEngineErr = cel.NewMigration()
	})
	return stateEngineInst, stateEngineErr
}

// New создаёт Resolver. Ошибка возможна лишь при программной несовместимости
// cel-go (не пользовательская).
func New() (Resolver, error) {
	e, err := stateEngine()
	if err != nil {
		return nil, fmt.Errorf("state-predicate CEL engine: %w", err)
	}
	return &resolver{engine: e}, nil
}

func (r *resolver) Compile(predicate string) error {
	if strings.TrimSpace(predicate) == "" {
		return errors.New("пустой state-предикат (ожидается CEL-выражение по state.*; для выборки «все» резолвер не вызывается)")
	}
	// Валидация = eval против ПУСТОГО state. compile-ошибки (синтаксис,
	// запрещённый корень, vault/now) поднимаем; runtime-no-such-key на пустом
	// state — НЕ ошибка load (на реальной инкарнации факт будет). Тот же приём,
	// что rbac.validateSoulprintExpr.
	_, evalErr := r.engine.EvalPredicate(predicate, cel.Vars{State: map[string]any{}})
	if evalErr == nil {
		return nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return evalErr
	}
	// ErrEval на пустом state (no-such-key / не-bool на отсутствующем ключе) —
	// синтаксически валидное выражение, load не фейлим.
	var ee *cel.ErrEval
	if errors.As(evalErr, &ee) {
		return nil
	}
	// Прочее (теоретически недостижимо) — fail-closed.
	return evalErr
}

func (r *resolver) Matches(predicate string, state map[string]any) (bool, error) {
	if strings.TrimSpace(predicate) == "" {
		return false, errors.New("пустой state-предикат")
	}
	ok, evalErr := r.engine.EvalPredicate(predicate, cel.Vars{State: state})
	if evalErr == nil {
		return ok, nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return false, evalErr
	}
	// Не-bool результат и runtime-no-such-key — оба ErrEval. Различаем по
	// типизированному признаку shared/cel (sentinel ErrPredicateNotBool,
	// обёрнут внутрь ErrEval) — устойчиво к смене текста сообщения, в отличие
	// от прежнего strings.Contains.
	//   - не-bool → ошибка автора предиката (предикат обязан быть булевым),
	//     возвращаем;
	//   - прочий runtime (no-such-key и т.п.) → fail-closed (false, nil).
	var ee *cel.ErrEval
	if errors.As(evalErr, &ee) {
		if errors.Is(evalErr, cel.ErrPredicateNotBool) {
			return false, evalErr
		}
		return false, nil
	}
	return false, evalErr
}

func (r *resolver) ResolveIncarnations(ctx context.Context, predicate string, base BaseFilter, lister IncarnationStateLister) ([]string, error) {
	// Валидируем predicate ОДИН раз до обхода набора: пустой/битый/sandbox/
	// не-bool отсекаются здесь, чтобы не платить eval-ом по каждой инкарнации и
	// не возвращать частичную выборку по битому выражению. Compile прогревает
	// общий program-кэш Engine — последующие Matches переиспользуют program.
	if err := r.Compile(predicate); err != nil {
		return nil, err
	}

	var out []string
	pageErr := lister.ListStatePages(ctx, base, func(page []Stated) error {
		for i := range page {
			ok, err := r.Matches(predicate, page[i].State)
			if err != nil {
				// Compile (против пустого state) не ловит не-bool, если на полном
				// state предикат даёт не-булев результат (no-such-key на пустом
				// state маскирует его) — поэтому not-bool может всплыть только тут.
				// Fail-closed: не глотаем, обход прерывается (битый автор-предикат,
				// а не данные инкарнации).
				return fmt.Errorf("state-predicate eval %q: %w", page[i].Name, err)
			}
			if ok {
				out = append(out, page[i].Name)
			}
		}
		return nil
	})
	if pageErr != nil {
		return nil, fmt.Errorf("state-predicate list: %w", pageErr)
	}
	return out, nil
}
