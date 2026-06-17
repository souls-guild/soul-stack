package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// VoyageScenarioResolver — резолв target Voyage `kind=scenario` → snapshot ИМЁН
// инкарнаций (ADR-043 §4, B1: единица батча = инкарнация). Out-of-incarnation
// резолв: набор фиксируется на старте (snapshot-scope, parity Tide
// `target_resolved_souls`).
//
// Приём (любой непустой → OR-merge, затем дедуп):
//   - явный incarnations[] (exact-match по имени, проверка существования);
//   - фильтр service= / coven= → резолв в имена (parity GET /v1/incarnations
//     с фильтром, [incarnation.SelectAll] / ListFilter).
//
// Пустой результат — не ошибка (handler решает: 422 voyage_empty_target).
type VoyageScenarioResolver interface {
	ResolveIncarnations(ctx context.Context, filter VoyageScenarioFilter) ([]string, error)
}

// VoyageScenarioFilter — резолвленный target для [VoyageScenarioResolver].
// Все поля опциональны; непустые объединяются по OR (как у list /v1/incarnations:
// явные имена ∪ результат фильтра). AND-семантика была бы сужением,
// несовместимым с use-case «возьми эти + всё из этого окружения».
type VoyageScenarioFilter struct {
	Incarnations []string // exact-match имена; невалидное/несуществующее — ошибка.
	Service      string   // фильтр incarnation.service (exact).
	Coven        string   // env-тег incarnation.covens[] (any-of, ADR-008 amendment a).
}

// VoyageCommandResolver — резолв target Voyage `kind=command` → snapshot SID-ов
// (ADR-043 §4: единица батча = хост). AND-merge sids/coven/where (security
// invariant ADR-040 → ADR-043 §5): invocation сужает scope, не расширяет.
//
// ResolveSIDsInScope (ADR-047 S4) дополнительно пересекает резолвнутый target с
// Purview оператора (верхняя граница, как list-видимость `GET /v1/souls`):
// command-путь не должен раскрывать хосты вне scope Архонта. Preview и прочие
// будущие потребители наследуют пересечение через тот же резолвер.
type VoyageCommandResolver interface {
	ResolveSIDs(ctx context.Context, filter VoyageCommandFilter) ([]string, error)
	ResolveSIDsInScope(ctx context.Context, filter VoyageCommandFilter, scope soulpurview.Scope) (ScopedSIDs, error)
}

// ScopedSIDs — результат [VoyageCommandResolver.ResolveSIDsInScope]: резолвнутый
// target ∩ Purview оператора + явно-указанные чужие хосты (ADR-047 S4).
//
// Гибрид-семантика (выбор пользователя 2026-06-09, ADR-047 §S4): handler решает
// по полям —
//   - DeniedExplicit непуст → 403 (явный чужой хост в sids[] = попытка эскалации,
//     parity per-incarnation scope-check scenario-пути);
//   - иначе SIDs пуст → 422 voyage_empty_target (широкий target урезан в ноль);
//   - иначе SIDs → snapshot прогона (урезанное подмножество, без отказа).
type ScopedSIDs struct {
	// SIDs — резолвнутый target ∩ Purview, отсортирован/дедуплицирован (snapshot
	// прогона). Для Unrestricted = полный резолв (backcompat cluster-admin).
	SIDs []string
	// DeniedExplicit — явно перечисленные в filter.SIDs хосты, существующие
	// (прошли SQL-presence), но выпавшие из Purview оператора. Непустой список →
	// 403 (anti-escalation): оператор назвал конкретный чужой SID. Отсортирован.
	DeniedExplicit []string
}

// VoyageCommandFilter — резолвленный target для [VoyageCommandResolver]
// (kind=command). Все поля опциональны; AND-merge на стороне резолвера
// (security invariant ADR-040: invocation сужает scope, не расширяет). where —
// пока сохраняется в target_origin, NOT evaluated в MVP.
type VoyageCommandFilter struct {
	SIDs   []string
	Covens []string
	Where  string
	// RequireAlive — presence-фильтр (ADR-043 amendment §5): при true резолв
	// дополнительно отсекает Soul-ы без живого presence-lease (SoulPresence).
	// Снимок после фильтра фиксируется в target_resolved как обычно (snapshot не
	// «дрожит» — фильтр на резолве). Применяется поверх SQL-presence (status IN
	// connected/dormant).
	RequireAlive bool
}

// --- production-реализации поверх pgxpool.Pool ---

// voyageResolverDB — узкая поверхность над PG для production-резолверов.
// Реальный *pgxpool.Pool удовлетворяет.
type voyageResolverDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// VoyageScenarioPGResolver — production-реализация [VoyageScenarioResolver].
type VoyageScenarioPGResolver struct {
	db voyageResolverDB
}

// NewVoyageScenarioPGResolver конструирует resolver. db обязателен.
func NewVoyageScenarioPGResolver(db voyageResolverDB) *VoyageScenarioPGResolver {
	return &VoyageScenarioPGResolver{db: db}
}

// ResolveIncarnations резолвит target → отсортированный дедуплицированный slice
// имён инкарнаций (детерминизм для audit/snapshot).
//
// Алгоритм:
//  1. Явные incarnations[] — каждое проверяется через [incarnation.SelectByName]
//     (несуществующее → ошибка ErrIncarnationNotFound, handler маппит в 422:
//     нельзя стартовать прогон на отсутствующей цели).
//  2. Фильтр service= / coven= → [incarnation.SelectAll] с ListFilter (тот же
//     резолв, что GET /v1/incarnations). Дренаж всех страниц через бескрайний
//     limit-цикл по offset.
//  3. Объединение (set), сортировка.
//
// Empty result — не ошибка.
func (r *VoyageScenarioPGResolver) ResolveIncarnations(ctx context.Context, filter VoyageScenarioFilter) ([]string, error) {
	set := make(map[string]struct{})

	for _, name := range filter.Incarnations {
		if !incarnation.ValidName(name) {
			return nil, fmt.Errorf("voyage resolver: invalid incarnation name %q", name)
		}
		if _, err := incarnation.SelectByName(ctx, r.db, name); err != nil {
			if errors.Is(err, incarnation.ErrIncarnationNotFound) {
				return nil, fmt.Errorf("%w: %s", incarnation.ErrIncarnationNotFound, name)
			}
			return nil, fmt.Errorf("voyage resolver: select incarnation %q: %w", name, err)
		}
		set[name] = struct{}{}
	}

	if filter.Service != "" || filter.Coven != "" {
		lf := incarnation.ListFilter{Service: filter.Service, Coven: filter.Coven}
		const pageSize = 1000
		for offset := 0; ; offset += pageSize {
			// scope Unrestricted: voyage-target резолв оперирует полным
			// множеством incarnation-ов (RBAC проверяется на уровне voyage-
			// permission, не scoped-видимостью List). Поведение без изменений.
			items, total, err := incarnation.SelectAll(ctx, r.db, lf, incarnation.ListScope{Unrestricted: true}, offset, pageSize)
			if err != nil {
				return nil, fmt.Errorf("voyage resolver: list incarnations: %w", err)
			}
			for _, inc := range items {
				set[inc.Name] = struct{}{}
			}
			if offset+pageSize >= total || len(items) == 0 {
				break
			}
		}
	}

	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// VoyageCommandPGResolver — production-реализация [VoyageCommandResolver] поверх
// `souls`-таблицы (cluster-wide резолв target → SID[]-snapshot, parity
// [ErrandRunSoulsPGResolver]). AND-merge sids/coven; where в MVP сохраняется
// в target_origin, не evaluate-ится (CEL-evaluator — отдельный slice).
//
// presence — опциональный presence-lease-чекер (ADR-006(a), [SoulPresence]):
// нужен только при filter.RequireAlive=true. nil → require_alive деградирует на
// SQL-presence (status IN connected/dormant), без отдельной lease-проверки
// (single-instance dev без Redis), симметрично topology-резолверу и `GET /v1/souls`.
type VoyageCommandPGResolver struct {
	db       voyageResolverDB
	presence SoulPresence
}

// NewVoyageCommandPGResolver конструирует resolver без presence-чекера
// (require_alive деградирует на SQL-presence). db обязателен.
func NewVoyageCommandPGResolver(db voyageResolverDB) *VoyageCommandPGResolver {
	return &VoyageCommandPGResolver{db: db}
}

// NewVoyageCommandPGResolverWithPresence конструирует resolver с presence-lease-
// чекером (ADR-043 amendment §5): при filter.RequireAlive=true резолв отсекает
// Soul-ы без живого presence-lease. db обязателен; presence nil → деградация на
// SQL-presence (как [NewVoyageCommandPGResolver]).
func NewVoyageCommandPGResolverWithPresence(db voyageResolverDB, presence SoulPresence) *VoyageCommandPGResolver {
	return &VoyageCommandPGResolver{db: db, presence: presence}
}

// ResolveSIDs резолвит target → отсортированный slice SID-ов. AND-merge: все
// непустые фильтры — пересечение (security invariant: сужение, не расширение).
// При filter.RequireAlive — дополнительно presence-фильтр живых через
// [SoulPresence] (ADR-043 amendment §5).
//
// Cluster-wide (без Purview-пересечения): используется keeper-side потребителями,
// которым scope-граница оператора не применима (например Cadence-спавн, где RBAC
// проверен на уровне рецепта). HTTP-путь createCommand зовёт [ResolveSIDsInScope].
func (r *VoyageCommandPGResolver) ResolveSIDs(ctx context.Context, filter VoyageCommandFilter) ([]string, error) {
	pairs, err := r.resolvePairs(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(pairs))
	for i := range pairs {
		out[i] = pairs[i].sid
	}
	return out, nil
}

// ResolveSIDsInScope резолвит target ∩ Purview оператора (ADR-047 S4). Тот же
// AND-merge sids/coven + require_alive, что [ResolveSIDs], затем пересечение с
// scope (верхняя граница видимости оператора):
//   - Unrestricted → полный резолв (backcompat cluster-admin, поле SIDs без
//     урезания, DeniedExplicit пуст);
//   - Empty (fail-closed: оператору не положено ни одного хоста) → SIDs пуст,
//     DeniedExplicit = все явно-указанные существующие SID-ы (→ 403, если их
//     назвали; иначе 422 на пустом SIDs);
//   - coven/regex-scope → видимость хоста = covenMatch OR regexMatch
//     ([soulpurview.CompiledScope.Visible]); резолвнутый набор урезается до
//     видимого. Явно-указанный (filter.SIDs) невидимый хост попадает в
//     DeniedExplicit (anti-escalation), широкий (coven/where) — молча урезается.
//
// soulprint/state-измерения (scope.Partial) НЕ вычисляются (S3b-2b отложен) —
// под-показ (fail-closed: оператор скорее недосчитается своего хоста, доступного
// ТОЛЬКО по soulprint, чем увидит чужой). coven/regex работают полноценно.
//
// Битый/слишком длинный regex в Purview ([soulpurview.CompileScope] error) →
// fail-closed: пустой набор + все явные SID-ы в DeniedExplicit (скрыть, не 500).
func (r *VoyageCommandPGResolver) ResolveSIDsInScope(ctx context.Context, filter VoyageCommandFilter, scope soulpurview.Scope) (ScopedSIDs, error) {
	pairs, err := r.resolvePairs(ctx, filter)
	if err != nil {
		return ScopedSIDs{}, err
	}

	if scope.Unrestricted {
		out := make([]string, len(pairs))
		for i := range pairs {
			out[i] = pairs[i].sid
		}
		return ScopedSIDs{SIDs: out}, nil
	}

	explicit := make(map[string]struct{}, len(filter.SIDs))
	for _, sid := range filter.SIDs {
		explicit[sid] = struct{}{}
	}

	// Empty (fail-closed) или scope-eval-error → ни одного видимого хоста. Явно-
	// указанные существующие SID-ы становятся DeniedExplicit (handler → 403).
	if scope.Empty {
		return scopedFromPairs(pairs, explicit, func(string, []string) bool { return false }), nil
	}
	compiled, cerr := soulpurview.CompileScope(scope)
	if cerr != nil {
		return scopedFromPairs(pairs, explicit, func(string, []string) bool { return false }), nil
	}
	return scopedFromPairs(pairs, explicit, compiled.Visible), nil
}

// scopedFromPairs делит резолвнутые (sid, covens)-пары по предикату видимости
// visible: видимые → SIDs (отсортированы — pairs уже ORDER BY sid ASC), невидимые
// и явно-указанные (∈ explicit) → DeniedExplicit. Невидимые широкие (coven/where,
// ∉ explicit) молча отбрасываются (урезание без отказа, ADR-047 S4 ветка 2).
func scopedFromPairs(pairs []soulPair, explicit map[string]struct{}, visible func(sid string, covens []string) bool) ScopedSIDs {
	var res ScopedSIDs
	for i := range pairs {
		p := &pairs[i]
		if visible(p.sid, p.covens) {
			res.SIDs = append(res.SIDs, p.sid)
			continue
		}
		if _, ok := explicit[p.sid]; ok {
			res.DeniedExplicit = append(res.DeniedExplicit, p.sid)
		}
	}
	return res
}

// soulPair — (sid, covens) одного хоста для scope-eval. covens нужны только в
// regex/coven-режиме [soulpurview.CompiledScope.Visible].
type soulPair struct {
	sid    string
	covens []string
}

// resolvePairs — общий резолв target → отсортированные (sid, covens)-пары
// (AND-merge sids/coven + require_alive). База и для cluster-wide [ResolveSIDs]
// (отбрасывает covens), и для [ResolveSIDsInScope] (scope-eval по covens).
func (r *VoyageCommandPGResolver) resolvePairs(ctx context.Context, filter VoyageCommandFilter) ([]soulPair, error) {
	for _, sid := range filter.SIDs {
		if !soul.ValidSID(sid) {
			return nil, fmt.Errorf("voyage resolver: invalid SID %q", sid)
		}
	}
	for _, c := range filter.Covens {
		if !soul.ValidCoven(c) {
			return nil, fmt.Errorf("voyage resolver: invalid coven label %q", c)
		}
	}

	const baseSQL = `SELECT sid, coven FROM souls
WHERE status IN ('connected','dormant')
  AND ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR sid = ANY($1::text[]))
  AND ($2::text[] IS NULL OR cardinality($2::text[]) = 0 OR coven @> $2::text[])
ORDER BY sid ASC
`
	var sidsArg, covensArg any
	if len(filter.SIDs) > 0 {
		sidsArg = filter.SIDs
	}
	if len(filter.Covens) > 0 {
		covensArg = filter.Covens
	}

	rows, err := r.db.Query(ctx, baseSQL, sidsArg, covensArg)
	if err != nil {
		return nil, fmt.Errorf("voyage resolver: query souls: %w", err)
	}
	defer rows.Close()
	var out []soulPair
	for rows.Next() {
		var sid string
		var covens []string
		if err := rows.Scan(&sid, &covens); err != nil {
			return nil, fmt.Errorf("voyage resolver: scan: %w", err)
		}
		out = append(out, soulPair{sid: sid, covens: covens})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("voyage resolver: iter: %w", err)
	}

	// require_alive (ADR-043 amendment §5): отсечь хосты без живого presence-lease.
	// presence=nil → деградация на уже применённую SQL-presence (status IN
	// connected/dormant) — без дополнительной фильтрации. Порядок SID-ов сохранён
	// (фильтр-подмножество отсортированного среза).
	if filter.RequireAlive && r.presence != nil && len(out) > 0 {
		sids := make([]string, len(out))
		for i := range out {
			sids[i] = out[i].sid
		}
		alive, perr := r.presence.SoulsStreamAlive(ctx, sids)
		if perr != nil {
			return nil, fmt.Errorf("voyage resolver: presence-фильтр (require_alive): %w", perr)
		}
		filtered := out[:0]
		for i := range out {
			if _, ok := alive[out[i].sid]; ok {
				filtered = append(filtered, out[i])
			}
		}
		out = filtered
	}

	return out, nil
}
