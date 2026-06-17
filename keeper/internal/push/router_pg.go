package push

// router_pg.go — production-implementation [PGRouterReader] поверх pgxpool.Pool
// (ADR-032 amendment 2026-05-27, P2 W-3 Multi-provider routing).
//
// Separate type-обёртка над pgPoolTargetReader (см. target_pg.go) добавляет
// чтение `souls.coven[]` для Level 2 резолва. Изолировано от target-reader-а:
// последний широко используется в PGFallbackTargetResolver (hot path SendApply,
// где coven читать не нужно), смешивать ради одного типа неоправданно.

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// pgPoolRouterReader реализует [PGRouterReader] над soul.ExecQueryRower.
type pgPoolRouterReader struct {
	db soul.ExecQueryRower
}

// NewPGRouterReader адаптирует pgxpool.Pool (или любой soul.ExecQueryRower)
// под [PGRouterReader]. Используется setupPushDispatchers в daemon-wire-up.
func NewPGRouterReader(db soul.ExecQueryRower) PGRouterReader {
	return &pgPoolRouterReader{db: db}
}

func (r *pgPoolRouterReader) SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error) {
	return soul.SelectSshTarget(ctx, r.db, sid)
}

func (r *pgPoolRouterReader) SelectCovens(ctx context.Context, sid string) ([]string, error) {
	// SelectBySID — единственный CRUD-метод, возвращающий полный Soul вместе с
	// coven[]. router-у нужны только метки, но дополнительный SQL под router
	// усложнит инвалидацию схемы; стоимость лишних 5 полей в Soul-row
	// мизерная против отдельного round-trip-а либо отдельного SELECT-а.
	s, err := soul.SelectBySID(ctx, r.db, sid)
	if err != nil {
		return nil, err
	}
	return s.Coven, nil
}
