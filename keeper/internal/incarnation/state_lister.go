package incarnation

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
)

// StateLister — production implementation of [statepredicate.IncarnationStateLister]
// on top of [SelectAll]. Two-stage pushdown architecture:
//  1. SQL-pushdown: [statepredicate.BaseFilter] (service/coven) maps to
//     [ListFilter] → WHERE narrows set BEFORE CEL-eval on PG side;
//  2. page-by-page: narrowed set drained in fixed-size pages (offset/limit loop
//     [SelectAll]), each page passed to yield and immediately released —
//     full set doesn't materialize in memory at once.
//
// Located in incarnation package (not statepredicate): otherwise resolver would pull
// direct dependency on incarnation + pgx and lose testability. Adapter
// binds resolver's narrow interface to concrete repository.
type StateLister struct {
	db ExecQueryRower
}

// statePageSize — page size for page-by-page drain. Matches pageSize
// of voyage-resolver (handlers.VoyageScenarioPGResolver): tradeoff round-trip
// ↔ memory on 100k fleet. State-jsonb snapshot of single incarnation small; 1000 rows
// per page keeps resolver's working set bounded regardless of
// service size.
const statePageSize = 1000

// NewStateLister constructs adapter. db mandatory (real *pgxpool.Pool in
// production, fake in unit tests — symmetric with other CRUD operations).
func NewStateLister(db ExecQueryRower) *StateLister {
	return &StateLister{db: db}
}

var _ statepredicate.IncarnationStateLister = (*StateLister)(nil)

// ListStatePages drains incarnations narrowed by base (service/coven) in pages
// and passes each to yield. Resolver runs CEL-Matches per-page (see
// [statepredicate.Resolver.ResolveIncarnations]).
//
// Empty pages not passed to yield (offset/limit loop breaks on
// set exhaustion). Error from yield (e.g., non-bool predicate on full
// state) interrupts drain and propagates — extra pages not fetched from PG.
// [SelectAll] error propagated as-is.
func (l *StateLister) ListStatePages(ctx context.Context, base statepredicate.BaseFilter, yield func(page []statepredicate.Stated) error) error {
	lf := ListFilter{Service: base.Service, Coven: base.Coven}

	// base.Covens (multi-coven, ADDITIVE) → coven∪{name} scope ([ListScope]):
	// label matches both covens[] and name (ADR-008). Empty Covens → Unrestricted
	// scope (state-CEL resolves over entire service-narrowed set; coven
	// narrowing then unnecessary — typical S3b-3 List path where coven and state —
	// independent OR dimensions, combined in outer-SelectAll).
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

		// Last page: offset+len already covered total, or page incomplete.
		if offset+len(items) >= total || len(items) < statePageSize {
			break
		}
	}
	return nil
}
