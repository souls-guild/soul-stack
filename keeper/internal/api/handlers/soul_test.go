package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// discardSlog — logger в /dev/null для middleware.Audit в unit-тестах.
func discardSlog() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// fakeSoulPool — мок [SoulPool] для unit-тестов Soul-handler-а. Диспетчер
// SQL поверх soul.* / bootstraptoken.* CRUD. BeginTx возвращает [soulTx],
// проксирующую обратно. Транзакционная consistency (rollback на сбое) —
// integration-тест; здесь проверяем маппинг ошибок и shape ответа.
type fakeSoulPool struct {
	beginErr error

	// soulExists: SelectBySID отдаёт строку (для issue-token); nil → ErrNoRows.
	existingSoul *soul.Soul
	// soulInsertErr: ошибка soul.Insert (например, unique-violation).
	soulInsertErr error
	// tokenInsertErr: ошибка bootstraptoken.Insert (active-exists).
	tokenInsertErr error
	// activeTokenID: token_id, который вернёт ExpireActiveBySID; "" → нет
	// активного токена (pgx.ErrNoRows из RETURNING).
	activeTokenID string

	// listCount: значение COUNT(*) для List (SelectAll). Считывается из
	// QueryRow при наличии "COUNT(*) FROM souls" в SQL.
	listCount int
	// listSouls: строки, которые отдаст Query (SelectAll). nil → пустой набор.
	listSouls []*soul.Soul
	// listQueryErr: ошибка Query (SelectAll list-pass) для проверки 500-маппинга.
	listQueryErr error
	// lastListWhere: фрагмент SQL последнего list-QueryRow/Query (для проверки
	// что фильтры дошли до SQL без конкатенации значений).
	lastListArgs []any

	expireCalled bool
	tokenInserts int

	commitCalled   bool
	rollbackCalled bool

	// bulk-поля для AssignCoven: listCount служит count-ом (Matched);
	// CTE-чанк отдаёт (bulkScanned, bulkChanged, bulkMaxSID). bulkMaxSID=""
	// и bulkScanned<bulkChunkSize → итерация завершается одним чанком.
	bulkChanged    int
	bulkScanned    int
	bulkMaxSID     string
	bulkChunkCalls int
	lastBulkArgs   []any

	// bulkChunkPlan — per-call сценарий чанков для multi-chunk / partial.
	// Если задан, каждый chunk-QueryRow берёт следующий шаг из плана (по
	// bulkChunkCalls); иначе работает статичный одно-чанковый путь выше.
	// Завершающий пустой чанк (scanned<bulkChunkSize) caller обязан положить
	// сам — fake его не дописывает (моделируем ровно то, что вернёт PG).
	bulkChunkPlan []bulkChunkStep

	// updateSshTargetCalls — счётчик UPDATE souls SET ssh_target. notFound=true
	// → RETURNING вернёт pgx.ErrNoRows (моделирует «SID не существует»).
	updateSshTargetCalls    int
	updateSshTargetNotFound bool
	lastUpdateSshTargetArgs []any

	// scopeEvalAll — весь набор строк ListForScopeEval (S3b-2a keyset-режим),
	// эмулирующий содержимое `souls`. Fake сам применяет keyset-окно по
	// (registered_at, sid)-границе из args (как реальная PG), чтобы курсорный
	// обход не давал дублей/пропусков. Порядок в наборе задавать НЕ обязательно —
	// fake сортирует как SQL (registered_at DESC, sid ASC).
	scopeEvalAll     []soul.ScopeEvalRow
	scopeEvalQueries int // счётчик scope-eval Query-вызовов (cap/добор-проверка).
}

// bulkChunkStep — один шаг плана multi-chunk: что вернёт CTE-чанк
// (scanned/changed/maxSID) либо ошибку (err != nil → errRow, моделирует
// сбой коммита чанка → BulkAssignCoven отдаёт BulkPartial).
type bulkChunkStep struct {
	scanned int
	changed int
	maxSID  string
	err     error
}

// fakeScoper — мок [PurviewResolver] для unit-тестов List/AssignCoven. Поля
// мапятся в [rbac.Purview]: covens → Covens, unrestricted → Unrestricted.
// Доп. поля покрывают scope-ветки List (Empty / regex keyset / soulprint Partial).
type fakeScoper struct {
	covens         []string
	unrestricted   bool
	empty          bool     // Purview{} (fail-closed): ни одного измерения.
	regexes        []string // regex-измерение (keyset-режим, S3b-2a).
	soulprintExprs []string // введённое не-вычисляемое измерение (Partial-ветка).
}

func (s fakeScoper) ResolvePurview(_, _, _ string) rbac.Purview {
	if s.empty {
		return rbac.Purview{}
	}
	return rbac.Purview{
		Covens:         s.covens,
		Unrestricted:   s.unrestricted,
		Regexes:        s.regexes,
		SoulprintExprs: s.soulprintExprs,
	}
}

func (f *fakeSoulPool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return &soulTx{pool: f}, nil
}

func (f *fakeSoulPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fakeSoulPool.Exec: unexpected SQL: " + sql)
}

func (f *fakeSoulPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "WITH chunk AS"):
		// Bulk chunk CTE: возвращает (scanned, changed, max_sid). Один чанк
		// меньше bulkChunkSize → BulkAssignCoven завершает итерацию. Эта ветка
		// ПЕРВОЙ: CTE содержит и `FROM souls`, и `WHERE sid IN(...)`, иначе бы
		// сматчилась soul-select-ветка ниже.
		f.lastBulkArgs = args
		// Multi-chunk / partial: шаг плана по номеру вызова. Ошибка в шаге →
		// errRow (моделирует сбой коммита чанка → BulkPartial).
		if f.bulkChunkPlan != nil {
			idx := f.bulkChunkCalls
			f.bulkChunkCalls++
			if idx >= len(f.bulkChunkPlan) {
				return errRow{err: errors.New("fakeSoulPool: bulkChunkPlan exhausted (нет завершающего пустого чанка?)")}
			}
			step := f.bulkChunkPlan[idx]
			if step.err != nil {
				return errRow{err: step.err}
			}
			var maxSID any = nil
			if step.maxSID != "" {
				maxSID = step.maxSID
			}
			return staticRow{values: []any{step.scanned, int64(step.changed), maxSID}}
		}
		f.bulkChunkCalls++
		var maxSID any = nil
		if f.bulkMaxSID != "" {
			maxSID = f.bulkMaxSID
		}
		return staticRow{values: []any{f.bulkScanned, int64(f.bulkChanged), maxSID}}

	case strings.Contains(sql, "INSERT INTO souls"):
		if f.soulInsertErr != nil {
			return errRow{err: f.soulInsertErr}
		}
		// RETURNING registered_at, requested_at (оба non-NULL; PG
		// проставляет requested_at через COALESCE(..., NOW())).
		now := time.Now().UTC()
		return staticRow{values: []any{now, now}}

	case strings.Contains(sql, "FROM souls") && strings.Contains(sql, "WHERE sid = $1"):
		// SelectBySID: точечный фильтр по PK. Узкий matcher с `= $1` ОТ
		// `sid = ANY($n)` (bulk-selector через sids), иначе COUNT-ветка с
		// sids-предикатом неверно матчилась бы сюда.
		if f.existingSoul == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		s := f.existingSoul
		var lastSeenAt, requestedAt any = nil, nil
		var lastSeenByKID, createdByAID, note any = nil, nil, nil
		if s.CreatedByAID != nil {
			createdByAID = *s.CreatedByAID
		}
		// scanSoul order: sid, transport, status, coven, traits, registered_at,
		// last_seen_at, last_seen_by_kid, created_by_aid, requested_at, note.
		return staticRow{values: []any{
			s.SID, string(s.Transport), string(s.Status), s.Coven,
			[]byte(nil), // traits jsonb (ADR-060): NULL → пустой map в scanSoul
			s.RegisteredAt, lastSeenAt, lastSeenByKID, createdByAID, requestedAt, note,
		}}

	case strings.Contains(sql, "INSERT INTO bootstrap_tokens"):
		if f.tokenInsertErr != nil {
			return errRow{err: f.tokenInsertErr}
		}
		f.tokenInserts++
		// RETURNING token_id, created_at.
		return staticRow{values: []any{"token-uuid", time.Now().UTC()}}

	case strings.Contains(sql, "UPDATE bootstrap_tokens") && strings.Contains(sql, "RETURNING token_id"):
		f.expireCalled = true
		if f.activeTokenID == "" {
			return errRow{err: pgx.ErrNoRows}
		}
		return staticRow{values: []any{f.activeTokenID}}

	case strings.Contains(sql, "COUNT(*) FROM souls"):
		f.lastListArgs = args
		return staticRow{values: []any{f.listCount}}

	case strings.Contains(sql, "UPDATE souls") && strings.Contains(sql, "ssh_target"):
		f.updateSshTargetCalls++
		f.lastUpdateSshTargetArgs = args
		if f.updateSshTargetNotFound {
			return errRow{err: pgx.ErrNoRows}
		}
		// RETURNING sid — отдаём именно тот SID, что прилетел в $1.
		var sid string
		if len(args) > 0 {
			if s, ok := args[0].(string); ok {
				sid = s
			}
		}
		return staticRow{values: []any{sid}}
	}
	return errRow{err: errors.New("fakeSoulPool.QueryRow: unexpected SQL: " + sql)}
}

func (f *fakeSoulPool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	// scope-eval keyset-окно (ListForScopeEval) тянет ПОЛНУЮ карточку (как
	// SelectAll), поэтому различаем по keyset-сортировке `ORDER BY
	// registered_at DESC, sid ASC` БЕЗ `OFFSET` (SelectAll имеет OFFSET).
	if strings.Contains(sql, "ORDER BY registered_at DESC, sid ASC") && !strings.Contains(sql, "OFFSET") {
		if f.listQueryErr != nil {
			return nil, f.listQueryErr
		}
		f.lastListArgs = args
		f.scopeEvalQueries++
		page := f.scopeEvalWindow(sql, args)
		return &scopeEvalRows{rows: page}, nil
	}
	if strings.Contains(sql, "FROM souls") {
		if f.listQueryErr != nil {
			return nil, f.listQueryErr
		}
		f.lastListArgs = args
		return &soulRows{souls: f.listSouls}, nil
	}
	return nil, errors.New("fakeSoulPool.Query: unexpected SQL: " + sql)
}

// scopeEvalWindow воспроизводит keyset-страницу ListForScopeEval над
// scopeEvalAll (как реальная PG): user-filter (status/transport/coven) как
// SQL WHERE, keyset-предикат `registered_at < curAt OR (== curAt AND sid >
// curSid)`, сортировка registered_at DESC, sid ASC, LIMIT pageSize.
//
// Args идут в порядке объявления clauses в [soul.buildScopeEvalSQL]:
// сначала filter-args (status/transport/coven, по наличию в SQL), затем
// keyset-границы (curAt, curSid) при наличии, затем pageSize последним.
// Какие clauses присутствуют — определяем по тексту SQL (как реальная PG
// видит WHERE), а args вычитываем позиционно в том же порядке.
func (f *fakeSoulPool) scopeEvalWindow(sql string, args []any) []soul.ScopeEvalRow {
	sorted := append([]soul.ScopeEvalRow(nil), f.scopeEvalAll...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].RegisteredAt.Equal(sorted[j].RegisteredAt) {
			return sorted[i].RegisteredAt.After(sorted[j].RegisteredAt) // DESC.
		}
		return sorted[i].SID < sorted[j].SID // tie-break sid ASC.
	})

	var (
		statusFilter    string
		transportFilter string
		covenFilter     string
		hasCursor       bool
		curAt           time.Time
		curSID          string
		pageSize        int
	)
	pos := 0
	if strings.Contains(sql, "status = $") {
		statusFilter, _ = args[pos].(string)
		pos++
	}
	if strings.Contains(sql, "transport = $") {
		transportFilter, _ = args[pos].(string)
		pos++
	}
	if strings.Contains(sql, "= ANY(coven)") {
		covenFilter, _ = args[pos].(string)
		pos++
	}
	if strings.Contains(sql, "registered_at < $") {
		hasCursor = true
		curAt, _ = args[pos].(time.Time)
		curSID, _ = args[pos+1].(string)
		pos += 2
	}
	pageSize, _ = args[pos].(int)

	out := make([]soul.ScopeEvalRow, 0, pageSize)
	for _, row := range sorted {
		if statusFilter != "" && string(row.Status) != statusFilter {
			continue
		}
		if transportFilter != "" && string(row.Transport) != transportFilter {
			continue
		}
		if covenFilter != "" && !containsStr(row.Coven, covenFilter) {
			continue
		}
		if hasCursor {
			after := row.RegisteredAt.Before(curAt) ||
				(row.RegisteredAt.Equal(curAt) && row.SID > curSID)
			if !after {
				continue
			}
		}
		out = append(out, row)
		if len(out) == pageSize {
			break
		}
	}
	return out
}

// containsStr — есть ли v среди xs (для coven-ANY-фильтра в fake).
func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// scopeEvalRows — pgx.Rows над []soul.ScopeEvalRow (ПОЛНАЯ карточка souls) для
// keyset-режима List. Порядок Scan совпадает с ListForScopeEval-проекцией:
// sid, transport, status, coven, traits, registered_at, last_seen_at,
// last_seen_by_kid, created_by_aid, requested_at, note.
type scopeEvalRows struct {
	rows []soul.ScopeEvalRow
	idx  int
}

func (r *scopeEvalRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *scopeEvalRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*dest[0].(*string) = row.SID
	*dest[1].(*string) = string(row.Transport)
	*dest[2].(*string) = string(row.Status)
	*dest[3].(*[]string) = row.Coven
	// traits jsonb (ADR-060): nil Traits → nil-bytes (ListForScopeEval → пустой map).
	if len(row.Traits) > 0 {
		b, err := json.Marshal(row.Traits)
		if err != nil {
			return err
		}
		*dest[4].(*[]byte) = b
	} else {
		*dest[4].(*[]byte) = nil
	}
	*dest[5].(*time.Time) = row.RegisteredAt
	*dest[6].(**time.Time) = row.LastSeenAt
	*dest[7].(**string) = row.LastSeenByKID
	*dest[8].(**string) = row.CreatedByAID
	*dest[9].(**time.Time) = row.RequestedAt
	if row.Note == "" {
		*dest[10].(**string) = nil
	} else {
		note := row.Note
		*dest[10].(**string) = &note
	}
	return nil
}

func (r *scopeEvalRows) Err() error                                   { return nil }
func (r *scopeEvalRows) Close()                                       {}
func (r *scopeEvalRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *scopeEvalRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *scopeEvalRows) Values() ([]any, error)                       { return nil, nil }
func (r *scopeEvalRows) RawValues() [][]byte                          { return nil }
func (r *scopeEvalRows) Conn() *pgx.Conn                              { return nil }

// soulRows — pgx.Rows-stub под [soul.SelectAll]: отдаёт преднастроенный
// набор Soul-ов в порядке scanSoul (sid, transport, status, coven, traits,
// registered_at, last_seen_at, last_seen_by_kid, created_by_aid,
// requested_at, note).
type soulRows struct {
	souls []*soul.Soul
	idx   int
}

func (r *soulRows) Next() bool {
	if r.idx >= len(r.souls) {
		return false
	}
	r.idx++
	return true
}

func (r *soulRows) Scan(dest ...any) error {
	s := r.souls[r.idx-1]
	*dest[0].(*string) = s.SID
	*dest[1].(*string) = string(s.Transport)
	*dest[2].(*string) = string(s.Status)
	*dest[3].(*[]string) = s.Coven
	// traits jsonb (ADR-060): nil Traits → nil-bytes (scanSoul → пустой map).
	if len(s.Traits) > 0 {
		b, err := json.Marshal(s.Traits)
		if err != nil {
			return err
		}
		*dest[4].(*[]byte) = b
	} else {
		*dest[4].(*[]byte) = nil
	}
	*dest[5].(*time.Time) = s.RegisteredAt
	*dest[6].(**time.Time) = s.LastSeenAt
	*dest[7].(**string) = s.LastSeenByKID
	*dest[8].(**string) = s.CreatedByAID
	*dest[9].(**time.Time) = s.RequestedAt
	if s.Note == "" {
		*dest[10].(**string) = nil
	} else {
		note := s.Note
		*dest[10].(**string) = &note
	}
	return nil
}

func (r *soulRows) Err() error                                   { return nil }
func (r *soulRows) Close()                                       {}
func (r *soulRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *soulRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *soulRows) Values() ([]any, error)                       { return nil, nil }
func (r *soulRows) RawValues() [][]byte                          { return nil }
func (r *soulRows) Conn() *pgx.Conn                              { return nil }

// soulTx проксирует pgx.Tx-методы на fakeSoulPool; неиспользуемые — panic.
type soulTx struct{ pool *fakeSoulPool }

func (t *soulTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.pool.BeginTx(ctx, pgx.TxOptions{})
}
func (t *soulTx) Commit(_ context.Context) error   { t.pool.commitCalled = true; return nil }
func (t *soulTx) Rollback(_ context.Context) error { t.pool.rollbackCalled = true; return nil }
func (t *soulTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("soulTx.CopyFrom: unexpected")
}
func (t *soulTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("soulTx.SendBatch: unexpected")
}
func (t *soulTx) LargeObjects() pgx.LargeObjects { panic("soulTx.LargeObjects: unexpected") }
func (t *soulTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("soulTx.Prepare: unexpected")
}
func (t *soulTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *soulTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, args...)
}
func (t *soulTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.pool.QueryRow(ctx, sql, args...)
}
func (t *soulTx) Conn() *pgx.Conn { return nil }

// doCreate строго разбирает JSON-тело (DisallowUnknownFields — как прежний (w,r)-роут,
// bad/unknown JSON → 400) и вызывает CreateTyped напрямую (handler-native T5d).
func doCreate(t *testing.T, h *SoulHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls", nil)
	rec := httptest.NewRecorder()

	var raw soulCreateRequest
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		problem.Write(rec, problem.New(problem.TypeMalformedRequest, req.URL.Path, "invalid JSON body: "+err.Error()))
		return rec
	}
	reply, err := h.CreateTyped(req.Context(), claimsFor("archon-alice"), raw)
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusCreated, soulCreateViewJSON(reply.Body), h.logger)
	return rec
}

// soulCreateViewJSON проецирует доменный SoulCreateView в map json-ключей native
// SoulCreateReply (covens всегда present; bootstrap_token/expires_at omitempty).
func soulCreateViewJSON(v SoulCreateView) map[string]any {
	m := map[string]any{
		"sid":            v.SID,
		"transport":      v.Transport,
		"status":         v.Status,
		"covens":         v.Covens,
		"registered_at":  v.RegisteredAt,
		"created_by_aid": v.CreatedByAID,
	}
	if v.BootstrapToken != nil {
		m["bootstrap_token"] = *v.BootstrapToken
	}
	if v.ExpiresAt != nil {
		m["expires_at"] = *v.ExpiresAt
	}
	return m
}

// doIssueToken вызывает IssueTokenTyped напрямую (handler-native T5d), парся ?force=.
func doIssueToken(t *testing.T, h *SoulHandler, sid, query string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v1/souls/" + sid + "/issue-token"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, url, nil)
	rec := httptest.NewRecorder()
	reply, err := h.IssueTokenTyped(req.Context(), claimsFor("archon-alice"), sid, req.URL.Query().Get("force") == "true")
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, map[string]any{
		"sid":             reply.Body.SID,
		"bootstrap_token": reply.Body.BootstrapToken,
		"expires_at":      reply.Body.ExpiresAt,
	}, h.logger)
	return rec
}

// doAssignCoven строго разбирает JSON-тело и вызывает AssignCovenTyped напрямую
// (handler-native T5d), парся ?dry_run=. reply.Body — соль custom MarshalJSON.
func doAssignCoven(t *testing.T, h *SoulHandler, body, query string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v1/souls/coven"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, url, nil)
	rec := httptest.NewRecorder()

	in, perr := decodeCovenAssignBody(body)
	if perr != nil {
		problem.Write(rec, problem.New(problem.TypeMalformedRequest, req.URL.Path, "invalid JSON body: "+perr.Error()))
		return rec
	}
	reply, err := h.AssignCovenTyped(req.Context(), claimsFor("archon-alice"), in, req.URL.Query().Get("dry_run") == "true")
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, reply.Body, h.logger)
	return rec
}

// decodeCovenAssignBody строго (DisallowUnknownFields) разбирает JSON-тело coven-assign в
// native SoulCovenAssignInput (parity прежней (w,r)-strict-декодировки → bad/unknown → 400).
func decodeCovenAssignBody(body string) (SoulCovenAssignInput, error) {
	var raw struct {
		Mode     string   `json:"mode"`
		Label    string   `json:"label"`
		Labels   []string `json:"labels"`
		DryRun   bool     `json:"dry_run"`
		Selector struct {
			All         bool     `json:"all"`
			Sids        []string `json:"sids"`
			Coven       string   `json:"coven"`
			Incarnation string   `json:"incarnation"`
			Status      string   `json:"status"`
		} `json:"selector"`
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return SoulCovenAssignInput{}, err
	}
	return SoulCovenAssignInput{
		Mode:   raw.Mode,
		Label:  raw.Label,
		Labels: raw.Labels,
		DryRun: raw.DryRun,
		Selector: SoulCovenAssignSelectorInput{
			All:         raw.Selector.All,
			SIDs:        raw.Selector.Sids,
			Coven:       raw.Selector.Coven,
			Incarnation: raw.Selector.Incarnation,
			Status:      raw.Selector.Status,
		},
	}, nil
}

func TestAssignCoven_Append_Happy(t *testing.T) {
	pool := &fakeSoulPool{listCount: 3, bulkScanned: 3, bulkChanged: 2}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignCoven(t, h, `{"mode":"append","label":"edge","selector":{"all":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["matched"].(float64) != 3 || out["changed"].(float64) != 2 {
		t.Errorf("matched/changed = %v/%v, want 3/2", out["matched"], out["changed"])
	}
	if out["status"] != "completed" {
		t.Errorf("status = %v, want completed", out["status"])
	}
	if out["dry_run"] != false {
		t.Errorf("dry_run = %v, want false", out["dry_run"])
	}
	if pool.bulkChunkCalls != 1 {
		t.Errorf("bulkChunkCalls = %d, want 1", pool.bulkChunkCalls)
	}
}

func TestAssignCoven_DryRun_NoUpdate(t *testing.T) {
	pool := &fakeSoulPool{listCount: 5}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignCoven(t, h, `{"mode":"append","label":"edge","selector":{"all":true},"dry_run":true}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["matched"].(float64) != 5 {
		t.Errorf("matched = %v, want 5", out["matched"])
	}
	if out["changed"].(float64) != 0 {
		t.Errorf("changed = %v, want 0 for dry_run", out["changed"])
	}
	if out["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", out["dry_run"])
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("bulkChunkCalls = %d, want 0 (dry_run must not UPDATE)", pool.bulkChunkCalls)
	}
}

func TestAssignCoven_DryRun_QueryParam(t *testing.T) {
	pool := &fakeSoulPool{listCount: 2}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignCoven(t, h, `{"mode":"append","label":"edge","selector":{"all":true}}`, "dry_run=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("?dry_run=true should suppress UPDATE, chunkCalls=%d", pool.bulkChunkCalls)
	}
}

// TestAssignCoven_NegativeScope_LabelOutOfScope — КРИТИЧНЫЙ scope-test (гейт b):
// оператор с coven=dev НЕ может навесить prod ни через all, ни через sids.
func TestAssignCoven_NegativeScope_LabelOutOfScope(t *testing.T) {
	for _, sel := range []string{
		`{"all":true}`,
		`{"sids":["h.example.com"]}`,
	} {
		pool := &fakeSoulPool{}
		h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)
		rec := doAssignCoven(t, h, `{"mode":"append","label":"prod","selector":`+sel+`}`, "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("selector %s: status = %d, want 422, body=%s", sel, rec.Code, rec.Body.String())
		}
		// Метка вне scope отвергнута ДО БД — ни count, ни UPDATE.
		if pool.bulkChunkCalls != 0 {
			t.Errorf("selector %s: UPDATE выполнен на out-of-scope метке", sel)
		}
	}
}

// TestAssignCoven_NegativeScope_InScopeLabel_Allowed — та же coven=dev роль
// МОЖЕТ навесить dev (метка в scope): доходит до bulk-слоя.
func TestAssignCoven_NegativeScope_InScopeLabel_Allowed(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"dev","selector":{"all":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if pool.bulkChunkCalls != 1 {
		t.Errorf("in-scope label should reach bulk-layer, chunkCalls=%d", pool.bulkChunkCalls)
	}
}

func TestAssignCoven_BadMode_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"merge","label":"edge","selector":{"all":true}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (unknown mode), body=%s", rec.Code, rec.Body.String())
	}
}

// --- mode=replace handler (3 кейса: успех, label-out-of-scope, host-out-of-scope) ---

// TestAssignCoven_Replace_Happy — replace меняет набор; на admin (unrestricted)
// scope гейт b не срабатывает, BulkReplaceCoven вызывается, ответ содержит
// labels[], не label.
func TestAssignCoven_Replace_Happy(t *testing.T) {
	pool := &fakeSoulPool{listCount: 2, bulkScanned: 2, bulkChanged: 2}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignCoven(t, h, `{"mode":"replace","labels":["prod","edge"],"selector":{"all":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["mode"] != "replace" {
		t.Errorf("mode = %v, want replace", out["mode"])
	}
	labels, ok := out["labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Fatalf("labels = %v, want 2-элементный массив", out["labels"])
	}
	if _, hasLabel := out["label"]; hasLabel {
		t.Errorf("replace-ответ содержит лишнее поле label: %v", out)
	}
	if pool.bulkChunkCalls != 1 {
		t.Errorf("bulkChunkCalls = %d, want 1 (replace дошёл до chunk-UPDATE)", pool.bulkChunkCalls)
	}
}

// TestAssignCoven_Replace_LabelOutOfScope_422 — гейт (b) на replace: набор
// с меткой вне scope → 422 ДО любого обращения к БД.
func TestAssignCoven_Replace_LabelOutOfScope_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"replace","labels":["dev","prod"],"selector":{"all":true}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (prod вне scope), body=%s", rec.Code, rec.Body.String())
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("UPDATE выполнен на out-of-scope replace-наборе")
	}
}

// TestAssignCoven_Replace_HostOutOfScope_0Changed — гейт (a) на replace через
// fake: scope=dev, but selector=sids[prod-host], handler пускает в bulk-слой
// (метки набора в scope), но scope-предикат в WHERE пустит фактически 0 хостов.
// На fakeDB matched=0 заставит вернуть пустой Report без chunk-вызова —
// проверяем 200 + changed=0.
func TestAssignCoven_Replace_HostOutOfScope_0Changed(t *testing.T) {
	pool := &fakeSoulPool{listCount: 0} // scope-фильтр сделал matched=0.
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"replace","labels":["dev"],"selector":{"sids":["prod-host.example.com"]}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["matched"].(float64) != 0 || out["changed"].(float64) != 0 {
		t.Errorf("matched/changed = %v/%v, want 0/0 (host out of scope)", out["matched"], out["changed"])
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("UPDATE выполнен при matched=0")
	}
}

// TestAssignCoven_Replace_RejectsLabelField_422 — XOR-валидация: label+replace.
func TestAssignCoven_Replace_RejectsLabelField_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"replace","label":"prod","selector":{"all":true}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (label запрещён для replace), body=%s", rec.Code, rec.Body.String())
	}
}

// TestAssignCoven_Append_RejectsLabelsField_422 — XOR-валидация: labels+append.
func TestAssignCoven_Append_RejectsLabelsField_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","labels":["prod"],"selector":{"all":true}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (labels запрещены для append), body=%s", rec.Code, rec.Body.String())
	}
}

// TestAssignCoven_Replace_EmptyLabels_OK — пустой labels = «снять все» —
// валидный кейс, доходит до bulk-слоя.
func TestAssignCoven_Replace_EmptyLabels_OK(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"replace","labels":[],"selector":{"all":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (пустой labels — clear-all), body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	labels, ok := out["labels"].([]any)
	if !ok || len(labels) != 0 {
		t.Errorf("labels = %v, want []", out["labels"])
	}
}

// --- selector.incarnation handler (3 кейса: матч, no-match, scope) ---

// TestAssignCoven_Incarnation_Match — incarnation-селектор доходит до bulk-слоя
// (handler не отвергает; fake matched=2, changed=2).
func TestAssignCoven_Incarnation_Match(t *testing.T) {
	pool := &fakeSoulPool{listCount: 2, bulkScanned: 2, bulkChanged: 2}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"patched","selector":{"incarnation":"redis"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["matched"].(float64) != 2 || out["changed"].(float64) != 2 {
		t.Errorf("matched/changed = %v/%v, want 2/2", out["matched"], out["changed"])
	}
}

// TestAssignCoven_Incarnation_NoMatch_0 — incarnation-селектор без матча
// (listCount=0) → 200 + 0/0.
func TestAssignCoven_Incarnation_NoMatch_0(t *testing.T) {
	pool := &fakeSoulPool{listCount: 0}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"patched","selector":{"incarnation":"ghost"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["matched"].(float64) != 0 || out["changed"].(float64) != 0 {
		t.Errorf("matched/changed = %v/%v, want 0/0", out["matched"], out["changed"])
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("UPDATE выполнен при matched=0")
	}
}

// TestAssignCoven_Incarnation_ScopeIntersection — incarnation в комбинации со
// scope: handler доходит до bulk-слоя, аргументы count включают и
// incarnation-имя, и scope-массив.
func TestAssignCoven_Incarnation_ScopeIntersection(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"dev","selector":{"incarnation":"redis"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	// args: $1 = incarnation, $2 = scope []string{"dev"}. Проверяем оба.
	foundIncarnation := false
	foundScope := false
	for _, a := range pool.lastListArgs {
		if s, ok := a.(string); ok && s == "redis" {
			foundIncarnation = true
		}
		if arr, ok := a.([]string); ok && len(arr) == 1 && arr[0] == "dev" {
			foundScope = true
		}
	}
	if !foundIncarnation || !foundScope {
		t.Errorf("count-args не содержат incarnation+scope: %v", pool.lastListArgs)
	}
}

// TestAssignCoven_Incarnation_InvalidName_422 — невалидное имя incarnation →
// 422 ДО БД.
func TestAssignCoven_Incarnation_InvalidName_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"patched","selector":{"incarnation":"BAD_NAME"}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("UPDATE на невалидной incarnation")
	}
}

func TestAssignCoven_BadLabel_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"BAD_LABEL","selector":{"all":true}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAssignCoven_BadStatus_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"edge","selector":{"status":"zombie"}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAssignCoven_BadJSON_400(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAssignCoven_EmptySelector_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"edge","selector":{}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (empty selector), body=%s", rec.Code, rec.Body.String())
	}
}

func TestAssignCoven_NilScoper_500(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, nil, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"edge","selector":{"all":true}}`, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (scoper not configured)", rec.Code)
	}
}

// handlerBulkChunkSize дублирует soul.bulkChunkSize (unexported): multi-chunk
// сценарий fake-плана требует первый чанк со scanned == размеру чанка, чтобы
// BulkAssignCoven не оборвал итерацию. Должно совпадать с soul.bulkChunkSize.
const handlerBulkChunkSize = 2000

// TestAssignCoven_Partial_200 (gap: partial-маппинг ~588-597). chunk K падает
// после закоммиченного чанка K-1 → BulkAssignCoven возвращает BulkPartial+Err.
// Handler обязан отдать 200 + status:partial (НЕ 500), changed < matched.
func TestAssignCoven_Partial_200(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: handlerBulkChunkSize + 50, // matched: 1 полный чанк + хвост.
		bulkChunkPlan: []bulkChunkStep{
			// Чанк 1: полный, закоммичен (changed=chunk, scanned=chunk, есть курсор).
			{scanned: handlerBulkChunkSize, changed: handlerBulkChunkSize, maxSID: "h01999.example.com"},
			// Чанк 2: сбой коммита → BulkPartial, чанк 1 уцелел.
			{err: errors.New("chunk 2 commit boom")},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignCoven(t, h, `{"mode":"append","label":"batch","selector":{"all":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial НЕ 500), body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "partial" {
		t.Errorf("status = %v, want partial", out["status"])
	}
	matched := out["matched"].(float64)
	changed := out["changed"].(float64)
	if matched != float64(handlerBulkChunkSize+50) {
		t.Errorf("matched = %v, want %d", matched, handlerBulkChunkSize+50)
	}
	if changed >= matched {
		t.Errorf("changed (%v) must be < matched (%v) on partial", changed, matched)
	}
	if changed != float64(handlerBulkChunkSize) {
		t.Errorf("changed = %v, want %d (ровно 1 закоммиченный чанк)", changed, handlerBulkChunkSize)
	}
	if out["dry_run"] != false {
		t.Errorf("dry_run = %v, want false", out["dry_run"])
	}
	// Оба чанка дёрнуты (1 ок + 1 сбой), третий не вызывается.
	if pool.bulkChunkCalls != 2 {
		t.Errorf("bulkChunkCalls = %d, want 2", pool.bulkChunkCalls)
	}
}

// TestAssignCoven_MultiChunk_Aggregates (gap: bulkMaxSID никогда не задан в
// тестах). 2 полных чанка + завершающий пустой: matched/changed агрегируются
// корректно через handler-путь.
func TestAssignCoven_MultiChunk_Aggregates(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: handlerBulkChunkSize * 2,
		bulkChunkPlan: []bulkChunkStep{
			{scanned: handlerBulkChunkSize, changed: handlerBulkChunkSize, maxSID: "h01999.example.com"},
			{scanned: handlerBulkChunkSize, changed: handlerBulkChunkSize, maxSID: "h03999.example.com"},
			// Завершающий пустой чанк: scanned < chunk → выход.
			{scanned: 0, changed: 0, maxSID: ""},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignCoven(t, h, `{"mode":"append","label":"batch","selector":{"all":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "completed" {
		t.Errorf("status = %v, want completed", out["status"])
	}
	if out["matched"].(float64) != float64(handlerBulkChunkSize*2) {
		t.Errorf("matched = %v, want %d", out["matched"], handlerBulkChunkSize*2)
	}
	// changed = сумма по двум полным чанкам (пустой финальный 0).
	if out["changed"].(float64) != float64(handlerBulkChunkSize*2) {
		t.Errorf("changed = %v, want %d (агрегат по чанкам)", out["changed"], handlerBulkChunkSize*2)
	}
	if pool.bulkChunkCalls != 3 {
		t.Errorf("bulkChunkCalls = %d, want 3 (2 полных + пустой финальный)", pool.bulkChunkCalls)
	}
}

// auditCapture — audit.Writer-stub на handler-уровне: собирает записанные
// события (паттерн middleware/audit_test.go captureWriter).
type auditCapture struct {
	events []*audit.Event
}

func (c *auditCapture) Write(_ context.Context, ev *audit.Event) error {
	cp := *ev
	c.events = append(c.events, &cp)
	return nil
}

// auditedAssignCoven вызывает AssignCovenTyped напрямую (handler-native T5d) и
// воспроизводит семантику huma-audit-middleware (вариант B): audit-событие
// soul.coven-changed пишется ТОЛЬКО на успехе (2xx), payload — reply.AuditPayload
// (включая source:"api", собранный handler-ом); на раннем отказе (400/422) событие НЕ
// пишется (parity middleware «не-2xx → skip»). Возвращает recorder + captured-события.
func auditedAssignCoven(t *testing.T, h *SoulHandler, body, query string) (*httptest.ResponseRecorder, *auditCapture) {
	t.Helper()
	cap := &auditCapture{}
	url := "/v1/souls/coven"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, url, nil)
	rec := httptest.NewRecorder()

	in, perr := decodeCovenAssignBody(body)
	if perr != nil {
		problem.Write(rec, problem.New(problem.TypeMalformedRequest, req.URL.Path, "invalid JSON body: "+perr.Error()))
		return rec, cap
	}
	reply, err := h.AssignCovenTyped(req.Context(), claimsFor("archon-alice"), in, req.URL.Query().Get("dry_run") == "true")
	if err != nil {
		writeProblemError(rec, req, err)
		return rec, cap
	}
	// success → вариант B: одно событие с payload handler-а (source:"api" внутри).
	_ = cap.Write(req.Context(), &audit.Event{
		EventType: audit.EventSoulCovenChanged,
		Source:    audit.SourceAPI,
		ArchonAID: "archon-alice",
		Payload:   map[string]any(reply.AuditPayload),
	})
	writeJSON(rec, http.StatusOK, reply.Body, h.logger)
	return rec, cap
}

// TestAssignCoven_Audit_PayloadOnSuccess (gap: audit-payload не ассертился).
// Успешная мутация пишет ровно одно событие soul.coven-changed с корректным
// payload (source/mode/label/selector/matched/changed/status/scope_applied/dry_run).
func TestAssignCoven_Audit_PayloadOnSuccess(t *testing.T) {
	pool := &fakeSoulPool{listCount: 3, bulkScanned: 3, bulkChanged: 2}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)

	rec, cap := auditedAssignCoven(t, h,
		`{"mode":"append","label":"dev","selector":{"coven":"stage","status":"connected"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(cap.events) != 1 {
		t.Fatalf("audit events = %d, want ровно 1", len(cap.events))
	}
	ev := cap.events[0]
	if ev.EventType != audit.EventSoulCovenChanged {
		t.Errorf("event type = %q, want %q", ev.EventType, audit.EventSoulCovenChanged)
	}
	if ev.Source != audit.SourceAPI {
		t.Errorf("source = %q, want api", ev.Source)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", ev.ArchonAID)
	}
	p := ev.Payload
	if p["source"] != "api" {
		t.Errorf("payload.source = %v, want api", p["source"])
	}
	if p["mode"] != "append" || p["label"] != "dev" {
		t.Errorf("payload mode/label = %v/%v", p["mode"], p["label"])
	}
	if p["matched"] != 3 || p["changed"] != 2 {
		t.Errorf("payload matched/changed = %v/%v, want 3/2", p["matched"], p["changed"])
	}
	if p["status"] != "completed" {
		t.Errorf("payload.status = %v, want completed", p["status"])
	}
	// coven-scoped оператор (не unrestricted) → scope_applied=true.
	if p["scope_applied"] != true {
		t.Errorf("payload.scope_applied = %v, want true (coven-scoped)", p["scope_applied"])
	}
	if p["dry_run"] != false {
		t.Errorf("payload.dry_run = %v, want false", p["dry_run"])
	}
	// Нормализованный селектор: all=false + coven/status (sids опущен).
	sel, ok := p["selector"].(map[string]any)
	if !ok {
		t.Fatalf("payload.selector type = %T, want map", p["selector"])
	}
	if sel["all"] != false || sel["coven"] != "stage" || sel["status"] != "connected" {
		t.Errorf("normalized selector = %v", sel)
	}
	if _, hasSIDs := sel["sids"]; hasSIDs {
		t.Errorf("normalized selector содержит пустой sids: %v", sel)
	}
}

// TestAssignCoven_Audit_OnDryRun (gap + PM-decision): dry_run=true ВСЁ РАВНО
// пишет audit (след «кто запускал предпросмотр массовой операции») с dry_run:true.
func TestAssignCoven_Audit_OnDryRun(t *testing.T) {
	pool := &fakeSoulPool{listCount: 5}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec, cap := auditedAssignCoven(t, h,
		`{"mode":"append","label":"edge","selector":{"all":true},"dry_run":true}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(cap.events) != 1 {
		t.Fatalf("audit events on dry_run = %d, want 1 (намеренный след предпросмотра)", len(cap.events))
	}
	p := cap.events[0].Payload
	if p["dry_run"] != true {
		t.Errorf("payload.dry_run = %v, want true", p["dry_run"])
	}
	if p["changed"] != 0 {
		t.Errorf("payload.changed = %v, want 0 (dry_run)", p["changed"])
	}
	// unrestricted-scope → scope_applied=false.
	if p["scope_applied"] != false {
		t.Errorf("payload.scope_applied = %v, want false (unrestricted)", p["scope_applied"])
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("dry_run выполнил UPDATE: bulkChunkCalls = %d", pool.bulkChunkCalls)
	}
}

// TestAssignCoven_Audit_NotWrittenOnEarlyReject (gap): ранний отказ (битый JSON
// 400 / невалидный ввод 422 / label-out-of-scope 422) НЕ пишет audit-событие
// мутации — middleware.Audit пропускает не-2xx.
func TestAssignCoven_Audit_NotWrittenOnEarlyReject(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"bad_json_400", `{"mode":`, http.StatusBadRequest},
		{"bad_mode_422", `{"mode":"replace","label":"edge","selector":{"all":true}}`, http.StatusUnprocessableEntity},
		{"empty_selector_422", `{"mode":"append","label":"edge","selector":{}}`, http.StatusUnprocessableEntity},
		{"label_out_of_scope_422", `{"mode":"append","label":"prod","selector":{"all":true}}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// coven=dev scope: 'prod' вне scope → 422 ДО БД (гейт b).
			h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{covens: []string{"dev"}}, nil, nil)
			rec, cap := auditedAssignCoven(t, h, tc.body, "")
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.code, rec.Body.String())
			}
			if len(cap.events) != 0 {
				t.Errorf("audit написан на раннем отказе (%d): %d событий", tc.code, len(cap.events))
			}
		})
	}
}

func TestSoulCreate_Happy_Agent(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent","covens":["prod","dc-eu"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["sid"] != "web-01.example.com" || out["status"] != "pending" {
		t.Errorf("response = %v", out)
	}
	// covens из запроса должны вернуться в ответе (привязка при онбординге).
	covens, _ := out["covens"].([]any)
	if len(covens) != 2 || covens[0] != "prod" || covens[1] != "dc-eu" {
		t.Errorf("covens = %v, want [prod dc-eu]", out["covens"])
	}
	if tok, _ := out["bootstrap_token"].(string); tok == "" {
		t.Errorf("bootstrap_token missing for agent")
	}
	// guard: ключ срока истечения — `expires_at` (не legacy `token_expires_at`).
	// Ловит регрессию переименования wire-ключа (вернуть тег `token_expires_at`
	// → красный).
	if _, ok := out["expires_at"]; !ok {
		t.Errorf("ключ expires_at отсутствует в ответе")
	}
	if _, ok := out["token_expires_at"]; ok {
		t.Errorf("legacy-ключ token_expires_at присутствует — переименование откатилось")
	}
	if out["created_by_aid"] != "archon-alice" {
		t.Errorf("created_by_aid = %v", out["created_by_aid"])
	}
	if pool.tokenInserts != 1 {
		t.Errorf("token inserts = %d, want 1", pool.tokenInserts)
	}
}

func TestSoulCreate_SSH_NoToken(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"ssh-01.example.com","transport":"ssh"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if tok, _ := out["bootstrap_token"].(string); tok != "" {
		t.Errorf("bootstrap_token should be empty for ssh, got %q", tok)
	}
	if pool.tokenInserts != 0 {
		t.Errorf("token inserts = %d, want 0 for ssh", pool.tokenInserts)
	}
}

func TestSoulCreate_Duplicate_409(t *testing.T) {
	pool := &fakeSoulPool{soulInsertErr: soul.ErrSoulAlreadyExists}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeSoulExists)
}

// TestSoulCreate_TokenInsertFails_Rollback (coverage gap 3): сбой token-insert
// после успешного souls-insert → tx.Rollback вызван, Commit нет. На реальной БД
// это гарантирует отсутствие осиротевшей souls-row (insert и token в одной tx).
func TestSoulCreate_TokenInsertFails_Rollback(t *testing.T) {
	pool := &fakeSoulPool{tokenInsertErr: errors.New("token insert boom")}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if pool.commitCalled {
		t.Errorf("Commit вызван, не должен быть (token-insert упал)")
	}
	if !pool.rollbackCalled {
		t.Errorf("Rollback НЕ вызван — осиротевшая souls-row останется в БД")
	}
}

// TestSoulCreate_UnknownCreator_422 (B2 unit): FK-violation на
// souls_created_by_aid_fk маппится в 422, а не в opaque 500.
func TestSoulCreate_UnknownCreator_422(t *testing.T) {
	pool := &fakeSoulPool{soulInsertErr: soul.ErrSoulCreatorNotFound}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeValidationFailed)
}

func TestSoulCreate_InvalidTransport_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	for _, body := range []string{
		`{"sid":"web-01.example.com","transport":"carrier-pigeon"}`,
		`{"sid":"web-01.example.com"}`, // missing transport
	} {
		rec := doCreate(t, h, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %q: status = %d, want 422", body, rec.Code)
		}
		assertProblemType(t, rec, problem.TypeValidationFailed)
	}
}

func TestSoulCreate_InvalidSID_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"WEB_01!","transport":"agent"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
}

// TestSoulCreate_Covens_AcceptedNotUnknownField (GAP #3 regression):
// документированное OpenAPI-поле `covens` НЕ должно отвергаться
// strict-декодером как unknown field. Раньше декодер ждал `coven` →
// 400 "json: unknown field \"covens\"", и не было API-способа привязать
// Soul к coven при онбординге.
func TestSoulCreate_Covens_AcceptedNotUnknownField(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent","covens":["redis-prod"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (covens must be accepted), body=%s", rec.Code, rec.Body.String())
	}
}

// TestSoulCreate_UnknownField_400: strict-декодер по-прежнему отвергает
// действительно неизвестные поля (например, старое имя `coven`).
func TestSoulCreate_UnknownField_400(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent","coven":["prod"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field 'coven', body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeMalformedRequest)
}

// TestSoulCreate_InvalidCoven_422: coven-метка не в kebab-case → 422
// (валидация ADR-008 стабильных тегов на API-границе).
func TestSoulCreate_InvalidCoven_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	for _, body := range []string{
		`{"sid":"web-01.example.com","transport":"agent","covens":["Prod"]}`,    // uppercase
		`{"sid":"web-01.example.com","transport":"agent","covens":["db_main"]}`, // underscore
		`{"sid":"web-01.example.com","transport":"agent","covens":["-edge"]}`,   // leading hyphen
		`{"sid":"web-01.example.com","transport":"agent","covens":[""]}`,        // empty label
		`{"sid":"web-01.example.com","transport":"agent","covens":["a","x y"]}`, // space in second label
	} {
		rec := doCreate(t, h, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %q: status = %d, want 422", body, rec.Code)
		}
		assertProblemType(t, rec, problem.TypeValidationFailed)
	}
}

func TestSoulCreate_MalformedJSON_400(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSoulIssueToken_Happy(t *testing.T) {
	pool := &fakeSoulPool{
		existingSoul: &soul.Soul{
			SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending,
		},
	}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doIssueToken(t, h, "web-01.example.com", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if tok, _ := out["bootstrap_token"].(string); tok == "" {
		t.Errorf("bootstrap_token missing")
	}
	// guard: ключ срока истечения — `expires_at` (не legacy `token_expires_at`).
	if _, ok := out["expires_at"]; !ok {
		t.Errorf("ключ expires_at отсутствует в ответе")
	}
	if _, ok := out["token_expires_at"]; ok {
		t.Errorf("legacy-ключ token_expires_at присутствует — переименование откатилось")
	}
	if pool.expireCalled {
		t.Errorf("expire должен НЕ вызываться без force")
	}
}

func TestSoulIssueToken_ActiveExists_409(t *testing.T) {
	pool := &fakeSoulPool{
		existingSoul: &soul.Soul{
			SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending,
		},
		tokenInsertErr: bootstraptoken.ErrTokenActiveExists,
	}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doIssueToken(t, h, "web-01.example.com", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeBootstrapTokenActive)
}

func TestSoulIssueToken_Force_ExpiresOld(t *testing.T) {
	pool := &fakeSoulPool{
		existingSoul: &soul.Soul{
			SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending,
		},
		activeTokenID: "old-token-uuid",
	}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doIssueToken(t, h, "web-01.example.com", "force=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !pool.expireCalled {
		t.Errorf("expire должен вызываться при force=true")
	}
	if pool.tokenInserts != 1 {
		t.Errorf("token inserts = %d, want 1", pool.tokenInserts)
	}
}

func TestSoulIssueToken_SSH_422(t *testing.T) {
	pool := &fakeSoulPool{
		existingSoul: &soul.Soul{
			SID: "ssh-01.example.com", Transport: soul.TransportSSH, Status: soul.StatusPending,
		},
	}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doIssueToken(t, h, "ssh-01.example.com", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeValidationFailed)
}

func TestSoulIssueToken_NotFound_404(t *testing.T) {
	pool := &fakeSoulPool{existingSoul: nil}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doIssueToken(t, h, "ghost.example.com", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeNotFound)
}

func TestSoulIssueToken_InvalidSID_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doIssueToken(t, h, "BAD_SID!", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
}

// doList выполняет GET /v1/souls (с claims archon-alice) через recordList.
func doList(t *testing.T, h *SoulHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	return recordList(t, h, query, "archon-alice")
}

// recordList разбирает pagination/cursor так же, как прежний (w,r)-роут (offset+cursor
// конфликт → 422, битый cursor / bad pagination → 400), вызывает ListTyped напрямую
// (handler-native T5d) и сериализует результат в recorder. aid="" → fail-closed (no claims).
func recordList(t *testing.T, h *SoulHandler, query, aid string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v1/souls"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	q := req.URL.Query()
	page, cursor, perr := api.ParsePageWithCursor(q)
	if perr != nil {
		var pe *api.PaginationError
		if errors.As(perr, &pe) && pe.IsConflict() {
			problem.Write(rec, problem.New(problem.TypeValidationFailed, req.URL.Path, perr.Error()))
			return rec
		}
		problem.Write(rec, problem.New(problem.TypeMalformedRequest, req.URL.Path, perr.Error()))
		return rec
	}
	var claims *jwt.Claims
	if aid != "" {
		claims = claimsFor(aid)
	}
	reply, err := h.ListTyped(req.Context(), claims, SoulListInput{
		Coven:     q.Get("coven"),
		Status:    q.Get("status"),
		Transport: q.Get("transport"),
		Page:      page,
		Cursor:    cursor,
	})
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, soulListReplyJSON(reply), h.logger)
	return rec
}

// soulListReplyJSON проецирует доменный SoulListReply (PagedResponse[SoulListView]) в map с
// json-ключами native envelope/element (для downstream decode тестов GET /v1/souls).
func soulListReplyJSON(r SoulListReply) map[string]any {
	items := make([]map[string]any, 0, len(r.Items))
	for i := range r.Items {
		items = append(items, soulListViewJSON(r.Items[i]))
	}
	m := map[string]any{
		"items":  items,
		"offset": r.Offset,
		"limit":  r.Limit,
		"total":  r.Total,
	}
	if r.NextCursor != nil {
		m["next_cursor"] = *r.NextCursor
	}
	if r.TotalApproximate {
		m["total_approximate"] = true
	}
	return m
}

func TestSoulList_Happy(t *testing.T) {
	kid := "keeper-eu-01"
	creator := "archon-alice"
	seen := time.Date(2026, 5, 20, 15, 29, 55, 0, time.UTC)
	pool := &fakeSoulPool{
		listCount: 2,
		listSouls: []*soul.Soul{
			{
				SID: "redis-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected,
				Coven: []string{"redis-prod", "dc-eu"}, RegisteredAt: time.Now().UTC(),
				Traits:     map[string]any{"tier": "gold", "rack": "r12"},
				LastSeenAt: &seen, LastSeenByKID: &kid, CreatedByAID: &creator,
			},
			{
				SID: "ssh-02.example.com", Transport: soul.TransportSSH, Status: soul.StatusPending,
				RegisteredAt: time.Now().UTC(),
			},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []struct {
			SID           string         `json:"sid"`
			Transport     string         `json:"transport"`
			Status        string         `json:"status"`
			Covens        []string       `json:"covens"`
			Traits        map[string]any `json:"traits"`
			LastSeenAt    *string        `json:"last_seen_at"`
			LastSeenByKID *string        `json:"last_seen_by_kid"`
			RegisteredAt  string         `json:"registered_at"`
			CreatedByAID  *string        `json:"created_by_aid"`
		} `json:"items"`
		Total  int `json:"total"`
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2", out.Total, len(out.Items))
	}
	if out.Offset != 0 || out.Limit != 50 {
		t.Errorf("pagination meta = offset:%d limit:%d, want 0/50", out.Offset, out.Limit)
	}
	first := out.Items[0]
	if first.SID != "redis-01.example.com" || first.Transport != "agent" || first.Status != "connected" {
		t.Errorf("item[0] = %+v", first)
	}
	if len(first.Covens) != 2 {
		t.Errorf("item[0].covens = %v, want 2", first.Covens)
	}
	if first.CreatedByAID == nil || *first.CreatedByAID != "archon-alice" {
		t.Errorf("item[0].created_by_aid = %v", first.CreatedByAID)
	}
	// traits (ADR-060 read-path): хост с метками отдаёт их как object.
	if first.Traits["tier"] != "gold" || first.Traits["rack"] != "r12" {
		t.Errorf("item[0].traits = %v, want {tier:gold rack:r12}", first.Traits)
	}
	// ssh-host без coven → covens должен быть `[]`, не null (coalesceCoven).
	if out.Items[1].Covens == nil {
		t.Errorf("item[1].covens = null, want [] (coalesceCoven)")
	}
	// bare-soul без traits → `{}`, не null (coalesceTraits): UI рендерит пустой
	// набор без nil-проверки.
	if out.Items[1].Traits == nil {
		t.Errorf("item[1].traits = null, want {} (coalesceTraits)")
	}
	if len(out.Items[1].Traits) != 0 {
		t.Errorf("item[1].traits = %v, want empty", out.Items[1].Traits)
	}
}

// TestSoulList_NoSecretsLeak — fingerprint и любые секреты SoulSeed-а НЕ
// должны попадать в list-response. soulListItem их не объявляет; проверяем
// через raw-map, что таких ключей нет.
func TestSoulList_NoSecretsLeak(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(out.Items))
	}
	for _, leak := range []string{"fingerprint", "soul_seed", "token_hash", "bootstrap_token"} {
		if _, present := out.Items[0][leak]; present {
			t.Errorf("list item leaked sensitive key %q: %v", leak, out.Items[0])
		}
	}
}

func TestSoulList_Empty(t *testing.T) {
	pool := &fakeSoulPool{listCount: 0, listSouls: nil}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 0 {
		t.Errorf("total = %d, want 0", out.Total)
	}
	// items должен сериализоваться как `[]`, не null.
	if out.Items == nil {
		t.Errorf("items = null, want [] (empty slice)")
	}
}

func TestSoulList_Filters_ReachSQL(t *testing.T) {
	pool := &fakeSoulPool{listCount: 0}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doList(t, h, "coven=redis-prod&status=connected&transport=agent")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Все три значения должны прийти как pgx-параметры (не конкатенация).
	want := map[string]bool{"connected": false, "agent": false, "redis-prod": false}
	for _, a := range pool.lastListArgs {
		if s, ok := a.(string); ok {
			if _, tracked := want[s]; tracked {
				want[s] = true
			}
		}
	}
	for v, found := range want {
		if !found {
			t.Errorf("filter value %q не дошёл до SQL-args (%v)", v, pool.lastListArgs)
		}
	}
}

func TestSoulList_InvalidStatus_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doList(t, h, "status=on-fire")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeValidationFailed)
}

func TestSoulList_InvalidTransport_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doList(t, h, "transport=carrier-pigeon")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeValidationFailed)
}

func TestSoulList_InvalidPagination_400(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doList(t, h, "limit=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeMalformedRequest)
}

func TestSoulList_QueryError_500(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, listQueryErr: errors.New("db boom")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeInternalError)
}

// --- ADR-047 S3b: scoped-видимость souls-list по Purview ---

// decodeListItems разбирает {items:[{sid,status}]} ответа List.
func decodeListItems(t *testing.T, body []byte) struct {
	Items []struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	} `json:"items"`
	Total int `json:"total"`
} {
	t.Helper()
	var out struct {
		Items []struct {
			SID    string `json:"sid"`
			Status string `json:"status"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, body)
	}
	return out
}

// TestSoulList_EmptyPurview_FailClosed — ГЛАВНЫЙ security-инвариант (ADR-047):
// оператор с пустым Purview (default-deny, нет coven-измерения) видит ПУСТОЙ
// список, НЕ весь флот. fakeSoulPool отдал бы 1 хост — handler обязан вернуть 0
// и НЕ обратиться к SelectAll. Регресс = оператор видит чужие хосты.
func TestSoulList_EmptyPurview_FailClosed(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "secret-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{empty: true}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := decodeListItems(t, rec.Body.Bytes())
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("empty-purview list total/len = %d/%d, want 0/0 (fail-closed, НЕ весь флот)", out.Total, len(out.Items))
	}
	if pool.lastListArgs != nil {
		t.Errorf("fail-closed обязан НЕ ходить в SelectAll, но lastListArgs=%v", pool.lastListArgs)
	}
}

// TestSoulList_NoClaims_FailClosed — нет claims в контексте (защитный инвариант,
// штатно route под RequireJWT) → пустой список, НЕ весь флот.
func TestSoulList_NoClaims_FailClosed(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "secret-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	// БЕЗ claims — запрос без identity (recordList с aid="" → fail-closed).
	rec := recordList(t, h, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := decodeListItems(t, rec.Body.Bytes())
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("no-claims list total/len = %d/%d, want 0/0 (fail-closed)", out.Total, len(out.Items))
	}
}

// TestSoulList_NilScoper_FailClosed — scoper не сконфигурирован (nil) → пустой
// список (НЕ весь флот). В отличие от prod (Holder всегда есть), это защита от
// мис-wire-up-а: отсутствие резолвера НЕ должно раскрывать флот.
func TestSoulList_NilScoper_FailClosed(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "secret-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := decodeListItems(t, rec.Body.Bytes())
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("nil-scoper list total/len = %d/%d, want 0/0 (fail-closed)", out.Total, len(out.Items))
	}
}

// TestSoulList_Unrestricted_All — `*`/bare-без-default Purview → весь список без
// scope-фильтра (coven-scope-args в SQL не добавляются).
func TestSoulList_Unrestricted_All(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 2,
		listSouls: []*soul.Soul{
			{SID: "a.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
			{SID: "b.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	out := decodeListItems(t, rec.Body.Bytes())
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("unrestricted list total/len = %d/%d, want 2/2", out.Total, len(out.Items))
	}
	// Unrestricted → НЕ должно быть coven-scope-args (`[]string`) в SQL.
	for _, a := range pool.lastListArgs {
		if _, ok := a.([]string); ok {
			t.Errorf("unrestricted scope добавил coven-scope-args в SQL: %v", pool.lastListArgs)
		}
	}
}

// TestSoulList_CovenScope_ReachesSQL — coven-scoped оператор: covens доходят до
// SQL как []string-аргумент scope-pushdown-а (`coven && ARRAY[covens]`).
func TestSoulList_CovenScope_ReachesSQL(t *testing.T) {
	pool := &fakeSoulPool{listCount: 0}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"prod", "staging"}}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var found bool
	for _, a := range pool.lastListArgs {
		if covs, ok := a.([]string); ok {
			if len(covs) == 2 && covs[0] == "prod" && covs[1] == "staging" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("coven-scope [prod staging] не дошёл до SQL-args как []string: %v", pool.lastListArgs)
	}
}

// TestSoulList_ScopeOverridesPresence — scope (fail-closed) и presence-overlay
// (fail-safe) разведены: scope СУЖАЕТ результат ДО presence-overlay-я. Пустой
// Purview → 0 хостов, даже если presence отдал бы их connected. Регресс этого
// теста = presence-паттерн «при сомнении показать» протёк на scope-слой.
func TestSoulList_ScopeOverridesPresence(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "redis-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
		},
	}
	// presence отдал бы redis-01 как connected (live lease) — но scope пуст.
	presence := &fakePresence{alive: aliveSet("redis-01.example.com")}
	h := NewSoulHandler(pool, fakeScoper{empty: true}, presence, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	out := decodeListItems(t, rec.Body.Bytes())
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("scope=empty list total/len = %d/%d, want 0/0 (scope скрывает раньше presence)", out.Total, len(out.Items))
	}
	if len(presence.gotSIDs) != 0 {
		t.Errorf("presence-overlay не должен вызываться при пустом scope (0 items), gotSIDs=%v", presence.gotSIDs)
	}
}

// TestSoulList_PartialScope_AppliesCovenSubset — оператор с coven + не-coven
// измерением (soulprint, ещё не вычисляется в пилоте): пилот применяет coven-
// pushdown (строгое подмножество, никогда НЕ over-show), список не падает в 0.
func TestSoulList_PartialScope_AppliesCovenSubset(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "prod-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, Coven: []string{"prod"}, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{
		covens:         []string{"prod"},
		soulprintExprs: []string{`soulprint.self.os.family == "debian"`},
	}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// coven-pushdown применён (covens дошли до SQL); не-coven измерение пилот
	// игнорирует (S3b-2), но НЕ обнуляет результат.
	var found bool
	for _, a := range pool.lastListArgs {
		if covs, ok := a.([]string); ok && len(covs) == 1 && covs[0] == "prod" {
			found = true
		}
	}
	if !found {
		t.Errorf("partial-scope coven [prod] не дошёл до SQL: %v", pool.lastListArgs)
	}
}

func assertProblemType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var p problem.Details
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body=%s)", err, rec.Body.String())
	}
	if p.Type != want {
		t.Errorf("problem.Type = %q, want %q", p.Type, want)
	}
}
