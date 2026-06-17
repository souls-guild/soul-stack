package incarnation

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
)

// StateLister — production-реализация [statepredicate.IncarnationStateLister]
// поверх [SelectAll]. Двухступенчатый pushdown архитектора:
//  1. SQL-pushdown: [statepredicate.BaseFilter] (service/coven) мапится в
//     [ListFilter] → WHERE сужает множество ДО CEL-eval на стороне PG;
//  2. page-by-page: сужённый набор дренируется страницами фиксированного
//     размера (offset/limit-цикл [SelectAll]), каждая страница отдаётся в yield
//     и сразу отпускается — весь набор разом в память не материализуется.
//
// Лежит в incarnation-пакете (а не в statepredicate): иначе резолвер потянул бы
// прямую зависимость на incarnation + pgx и потерял тестируемость. Адаптер
// связывает узкий интерфейс резолвера с конкретным репозиторием.
type StateLister struct {
	db ExecQueryRower
}

// statePageSize — размер страницы page-by-page-дренажа. Совпадает с pageSize
// voyage-резолвера (handlers.VoyageScenarioPGResolver): компромисс round-trip
// ↔ память на 100k-флоте. State-jsonb-снимок одной инкарнации мал; 1000 строк
// на страницу держат рабочее множество резолвера ограниченным независимо от
// размера сервиса.
const statePageSize = 1000

// NewStateLister конструирует адаптер. db обязателен (реальный *pgxpool.Pool в
// production, fake в unit-тестах — симметрично прочим CRUD-операциям).
func NewStateLister(db ExecQueryRower) *StateLister {
	return &StateLister{db: db}
}

var _ statepredicate.IncarnationStateLister = (*StateLister)(nil)

// ListStatePages дренирует инкарнации, сужённые base (service/coven), страницами
// и отдаёт каждую в yield. Резолвер прогоняет CEL-Matches per-page (см.
// [statepredicate.Resolver.ResolveIncarnations]).
//
// Пустые страницы в yield не отдаются (offset/limit-цикл прерывается на
// исчерпании набора). Ошибка из yield (например, не-bool предикат на полном
// state) прерывает дренаж и пробрасывается наружу — лишние страницы из PG не
// тянутся. Ошибка [SelectAll] пробрасывается как есть.
func (l *StateLister) ListStatePages(ctx context.Context, base statepredicate.BaseFilter, yield func(page []statepredicate.Stated) error) error {
	lf := ListFilter{Service: base.Service, Coven: base.Coven}

	// base.Covens (multi-coven, ADDITIVE) → coven∪{name} scope ([ListScope]):
	// метка матчит и covens[], и name (ADR-008). Пустой Covens → Unrestricted
	// scope (state-CEL резолвится по всему service-сужённому множеству; coven-
	// сужение тогда не нужно — типовой путь S3b-3 List, где coven и state —
	// независимые OR-измерения, объединяемые в outer-SelectAll).
	scope := ListScope{Unrestricted: true}
	if len(base.Covens) > 0 {
		scope = ListScope{Covens: base.Covens}
	}

	for offset := 0; ; offset += statePageSize {
		items, total, err := SelectAll(ctx, l.db, lf, scope, offset, statePageSize)
		if err != nil {
			return fmt.Errorf("incarnation: state-lister page (offset=%d): %w", offset, err)
		}
		if len(items) == 0 {
			break
		}

		page := make([]statepredicate.Stated, len(items))
		for i, inc := range items {
			page[i] = statepredicate.Stated{Name: inc.Name, State: inc.State}
		}
		if err := yield(page); err != nil {
			return err
		}

		// Последняя страница: offset+len уже покрыл total, либо страница неполная.
		if offset+len(items) >= total || len(items) < statePageSize {
			break
		}
	}
	return nil
}
