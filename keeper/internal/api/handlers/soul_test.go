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

// discardSlog — a logger into /dev/null for middleware.Audit in unit tests.
func discardSlog() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// fakeSoulPool — mock [SoulPool] for unit tests of the Soul handler. Dispatches
// SQL across soul.* / bootstraptoken.* CRUD. BeginTx returns [soulTx],
// which proxies back. Transactional consistency (rollback on failure) is
// covered by the integration test; here we check error mapping and response shape.
type fakeSoulPool struct {
	beginErr error

	// soulExists: SelectBySID returns a row (for issue-token); nil → ErrNoRows.
	existingSoul *soul.Soul
	// soulInsertErr: error from soul.Insert (e.g. a unique-violation).
	soulInsertErr error
	// tokenInsertErr: error from bootstraptoken.Insert (active-exists).
	tokenInsertErr error
	// activeTokenID: the token_id that ExpireActiveBySID will return; "" → no
	// active token (pgx.ErrNoRows from RETURNING).
	activeTokenID string

	// listCount: the COUNT(*) value for List (SelectAll). Read from
	// QueryRow when the SQL contains "COUNT(*) FROM souls".
	listCount int
	// listSouls: the rows that Query (SelectAll) will return. nil → an empty set.
	listSouls []*soul.Soul
	// listQueryErr: error from Query (SelectAll list-pass), for checking 500 mapping.
	listQueryErr error
	// lastListWhere: the SQL fragment from the last list-QueryRow/Query (to check
	// that filters reached the SQL without value concatenation).
	lastListArgs []any

	expireCalled bool
	tokenInserts int

	commitCalled   bool
	rollbackCalled bool

	// bulk fields for AssignCoven: listCount serves as the count (Matched);
	// the CTE chunk returns (bulkScanned, bulkChanged, bulkMaxSID). bulkMaxSID=""
	// and bulkScanned<bulkChunkSize → the iteration finishes in a single chunk.
	bulkChanged    int
	bulkScanned    int
	bulkMaxSID     string
	bulkChunkCalls int
	lastBulkArgs   []any

	// bulkChunkPlan — per-call chunk scenario for multi-chunk / partial cases.
	// If set, each chunk-QueryRow takes the next step from the plan (indexed by
	// bulkChunkCalls); otherwise the static single-chunk path above runs.
	// The caller must supply the trailing empty chunk (scanned<bulkChunkSize)
	// itself — the fake doesn't append it (we model exactly what PG would return).
	bulkChunkPlan []bulkChunkStep

	// updateSshTargetCalls — counter of UPDATE souls SET ssh_target calls. notFound=true
	// → RETURNING returns pgx.ErrNoRows (models "the SID doesn't exist").
	updateSshTargetCalls    int
	updateSshTargetNotFound bool
	lastUpdateSshTargetArgs []any

	// scopeEvalAll — the full set of ListForScopeEval rows (S3b-2a keyset mode),
	// emulating the contents of `souls`. The fake applies the keyset window itself over
	// the (registered_at, sid) boundary from args (like real PG does), so cursor
	// traversal produces no duplicates/gaps. The set's order doesn't need to be set up —
	// the fake sorts it like the SQL does (registered_at DESC, sid ASC).
	scopeEvalAll     []soul.ScopeEvalRow
	scopeEvalQueries int // counter of scope-eval Query calls (cap/backfill check).
}

// bulkChunkStep — one step of a multi-chunk plan: what the CTE chunk
// returns (scanned/changed/maxSID) or an error (err != nil → errRow, models
// a chunk-commit failure → BulkAssignCoven returns BulkPartial).
type bulkChunkStep struct {
	scanned int
	changed int
	maxSID  string
	err     error
}

// fakeScoper — mock [PurviewResolver] for unit tests of List/AssignCoven. Fields
// map to [rbac.Purview]: covens → Covens, unrestricted → Unrestricted.
// The extra fields cover List's scope branches (Empty / regex keyset / soulprint Partial).
type fakeScoper struct {
	covens         []string
	unrestricted   bool
	empty          bool     // Purview{} (fail-closed): no dimension at all.
	regexes        []string // regex dimension (keyset mode, S3b-2a).
	soulprintExprs []string // an introduced non-evaluable dimension (Partial branch).
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
		// Bulk chunk CTE: returns (scanned, changed, max_sid). One chunk
		// smaller than bulkChunkSize → BulkAssignCoven finishes the iteration. This branch
		// comes FIRST: the CTE contains both `FROM souls` and `WHERE sid IN(...)`, otherwise
		// the soul-select branch below would match it instead.
		f.lastBulkArgs = args
		// Multi-chunk / partial: the plan step indexed by call number. An error in the step →
		// errRow (models a chunk-commit failure → BulkPartial).
		if f.bulkChunkPlan != nil {
			idx := f.bulkChunkCalls
			f.bulkChunkCalls++
			if idx >= len(f.bulkChunkPlan) {
				return errRow{err: errors.New("fakeSoulPool: bulkChunkPlan exhausted (missing terminating empty chunk?)")}
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
		// RETURNING registered_at, requested_at (both non-NULL; PG
		// fills in requested_at via COALESCE(..., NOW())).
		now := time.Now().UTC()
		return staticRow{values: []any{now, now}}

	case strings.Contains(sql, "FROM souls") && strings.Contains(sql, "WHERE sid = $1"):
		// SelectBySID: a point filter on PK. A narrow matcher with `= $1` to distinguish it FROM
		// `sid = ANY($n)` (the bulk selector via sids), otherwise the COUNT branch with
		// a sids predicate would incorrectly match here.
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
			[]byte(nil), // traits jsonb (ADR-060): NULL → an empty map in scanSoul
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
		// RETURNING sid — return exactly the SID that arrived in $1.
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
	// The scope-eval keyset window (ListForScopeEval) pulls the FULL record (like
	// SelectAll), so we distinguish by the keyset ordering `ORDER BY
	// registered_at DESC, sid ASC` WITHOUT `OFFSET` (SelectAll has OFFSET).
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

// scopeEvalWindow reproduces the ListForScopeEval keyset page over
// scopeEvalAll (like real PG does): the user filter (status/transport/coven) as
// SQL WHERE, the keyset predicate `registered_at < curAt OR (== curAt AND sid >
// curSid)`, ordering by registered_at DESC, sid ASC, LIMIT pageSize.
//
// Args arrive in the order clauses are declared in [soul.buildScopeEvalSQL]:
// filter-args first (status/transport/coven, when present in the SQL), then
// the keyset boundary (curAt, curSid) if present, then pageSize last.
// Which clauses are present is determined from the SQL text (the way real PG
// sees WHERE), and args are read positionally in the same order.
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

// containsStr — reports whether v is among xs (for the coven-ANY filter in the fake).
func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// scopeEvalRows — pgx.Rows over []soul.ScopeEvalRow (the FULL souls record) for
// List's keyset mode. The Scan order matches the ListForScopeEval projection:
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
	// traits jsonb (ADR-060): nil Traits → nil bytes (ListForScopeEval → an empty map).
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

// soulRows — a pgx.Rows stub for [soul.SelectAll]: returns a preconfigured
// set of Souls in scanSoul order (sid, transport, status, coven, traits,
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
	// traits jsonb (ADR-060): nil Traits → nil bytes (scanSoul → an empty map).
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

// soulTx proxies pgx.Tx methods onto fakeSoulPool; unused ones panic.
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

// doCreate strictly decodes the JSON body (DisallowUnknownFields — like the former (w,r)-route,
// bad/unknown JSON → 400) and calls CreateTyped directly (handler-native T5d).
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

// soulCreateViewJSON projects the domain SoulCreateView into a map of the native
// SoulCreateReply's json keys (covens always present; bootstrap_token/expires_at omitempty).
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

// doIssueToken calls IssueTokenTyped directly (handler-native T5d), parsing ?force=.
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

// doAssignCoven strictly decodes the JSON body and calls AssignCovenTyped directly
// (handler-native T5d), parsing ?dry_run=. reply.Body relies on a custom MarshalJSON.
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

// decodeCovenAssignBody strictly (DisallowUnknownFields) decodes the coven-assign JSON body into
// the native SoulCovenAssignInput (parity with the former (w,r) strict decoding → bad/unknown → 400).
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

// TestAssignCoven_NegativeScope_LabelOutOfScope — CRITICAL scope test (gate b):
// an operator with coven=dev CANNOT attach prod, neither via all nor via sids.
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
		// A label outside scope is rejected BEFORE the DB — neither count nor UPDATE.
		if pool.bulkChunkCalls != 0 {
			t.Errorf("selector %s: UPDATE выполнен на out-of-scope метке", sel)
		}
	}
}

// TestAssignCoven_NegativeScope_InScopeLabel_Allowed — the same coven=dev role
// CAN attach dev (label in scope): reaches the bulk layer.
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

// --- mode=replace handler (3 cases: success, label-out-of-scope, host-out-of-scope) ---

// TestAssignCoven_Replace_Happy — replace changes the set; on admin (unrestricted)
// scope gate b doesn't trigger, BulkReplaceCoven is called, the response contains
// labels[], not label.
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

// TestAssignCoven_Replace_LabelOutOfScope_422 — gate (b) on replace: a set
// with a label outside scope → 422 BEFORE any DB access.
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

// TestAssignCoven_Replace_HostOutOfScope_0Changed — gate (a) on replace via
// the fake: scope=dev, but selector=sids[prod-host], the handler lets it into the bulk layer
// (the set's labels are in scope), but the scope predicate in WHERE actually lets through 0 hosts.
// On fakeDB matched=0 forces an empty Report to be returned without a chunk call —
// we check 200 + changed=0.
func TestAssignCoven_Replace_HostOutOfScope_0Changed(t *testing.T) {
	pool := &fakeSoulPool{listCount: 0} // the scope filter made matched=0.
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

// TestAssignCoven_Replace_RejectsLabelField_422 — XOR validation: label+replace.
func TestAssignCoven_Replace_RejectsLabelField_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"replace","label":"prod","selector":{"all":true}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (label запрещён для replace), body=%s", rec.Code, rec.Body.String())
	}
}

// TestAssignCoven_Append_RejectsLabelsField_422 — XOR validation: labels+append.
func TestAssignCoven_Append_RejectsLabelsField_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","labels":["prod"],"selector":{"all":true}}`, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (labels запрещены для append), body=%s", rec.Code, rec.Body.String())
	}
}

// TestAssignCoven_Replace_EmptyLabels_OK — empty labels = "remove all" —
// a valid case, reaches the bulk layer.
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

// --- selector.incarnation handler (3 cases: match, no-match, scope) ---

// TestAssignCoven_Incarnation_Match — the incarnation selector reaches the bulk layer
// (the handler doesn't reject; fake matched=2, changed=2).
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

// TestAssignCoven_Incarnation_NoMatch_0 — an incarnation selector with no match
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

// TestAssignCoven_Incarnation_ScopeIntersection — incarnation combined with
// scope: the handler reaches the bulk layer, the count arguments include both
// the incarnation name and the scope array.
func TestAssignCoven_Incarnation_ScopeIntersection(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)
	rec := doAssignCoven(t, h, `{"mode":"append","label":"dev","selector":{"incarnation":"redis"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	// args: $1 = incarnation, $2 = scope []string{"dev"}. Verify both.
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

// TestAssignCoven_Incarnation_InvalidName_422 — an invalid incarnation name →
// 422 BEFORE the DB.
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

// handlerBulkChunkSize duplicates soul.bulkChunkSize (unexported): multi-chunk
// scenario of fake-plan requires first chunk with scanned == chunk size to prevent
// BulkAssignCoven from terminating iteration. Must match soul.bulkChunkSize.
const handlerBulkChunkSize = 2000

// TestAssignCoven_Partial_200 (gap: partial-mapping ~588-597). chunk K fails
// after committed chunk K-1 → BulkAssignCoven returns BulkPartial+Err.
// Handler MUST return 200 + status:partial (NOT 500), changed < matched.
func TestAssignCoven_Partial_200(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: handlerBulkChunkSize + 50, // matched: 1 full chunk + remainder.
		bulkChunkPlan: []bulkChunkStep{
			// Chunk 1: full, committed (changed=chunk, scanned=chunk, has cursor).
			{scanned: handlerBulkChunkSize, changed: handlerBulkChunkSize, maxSID: "h01999.example.com"},
			// Chunk 2: commit failure → BulkPartial, chunk 1 survived.
			{err: errors.New("chunk 2 commit boom")},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignCoven(t, h, `{"mode":"append","label":"batch","selector":{"all":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial NOT 500), body=%s", rec.Code, rec.Body.String())
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
		t.Errorf("changed = %v, want %d (exactly 1 committed chunk)", changed, handlerBulkChunkSize)
	}
	if out["dry_run"] != false {
		t.Errorf("dry_run = %v, want false", out["dry_run"])
	}
	// Both chunks invoked (1 ok + 1 fail), third not called.
	if pool.bulkChunkCalls != 2 {
		t.Errorf("bulkChunkCalls = %d, want 2", pool.bulkChunkCalls)
	}
}

// TestAssignCoven_MultiChunk_Aggregates (gap: bulkMaxSID never set in
// tests). 2 full chunks + terminating empty: matched/changed aggregate
// correctly via handler-path.
func TestAssignCoven_MultiChunk_Aggregates(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: handlerBulkChunkSize * 2,
		bulkChunkPlan: []bulkChunkStep{
			{scanned: handlerBulkChunkSize, changed: handlerBulkChunkSize, maxSID: "h01999.example.com"},
			{scanned: handlerBulkChunkSize, changed: handlerBulkChunkSize, maxSID: "h03999.example.com"},
			// Terminating empty chunk: scanned < chunk → exit.
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
	// changed = sum over two full chunks (empty final 0).
	if out["changed"].(float64) != float64(handlerBulkChunkSize*2) {
		t.Errorf("changed = %v, want %d (агрегат по чанкам)", out["changed"], handlerBulkChunkSize*2)
	}
	if pool.bulkChunkCalls != 3 {
		t.Errorf("bulkChunkCalls = %d, want 3 (2 полных + пустой финальный)", pool.bulkChunkCalls)
	}
}

// auditCapture — audit.Writer-stub at handler-level: collects written
// events (pattern from middleware/audit_test.go captureWriter).
type auditCapture struct {
	events []*audit.Event
}

func (c *auditCapture) Write(_ context.Context, ev *audit.Event) error {
	cp := *ev
	c.events = append(c.events, &cp)
	return nil
}

// auditedAssignCoven calls AssignCovenTyped directly (handler-native T5d) and
// reproduces huma-audit-middleware semantics (variant B): audit event
// soul.coven-changed written ONLY on success (2xx), payload = reply.AuditPayload
// (including source:"api" collected by handler); on early reject (400/422) event is NOT
// written (parity middleware "not-2xx → skip"). Returns recorder + captured-events.
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
	// success → variant B: one event with handler's payload (source:"api" inside).
	_ = cap.Write(req.Context(), &audit.Event{
		EventType: audit.EventSoulCovenChanged,
		Source:    audit.SourceAPI,
		ArchonAID: "archon-alice",
		Payload:   map[string]any(reply.AuditPayload),
	})
	writeJSON(rec, http.StatusOK, reply.Body, h.logger)
	return rec, cap
}

// TestAssignCoven_Audit_PayloadOnSuccess (gap: audit-payload not asserted).
// Successful mutation writes exactly one soul.coven-changed event with correct
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
	// coven-scoped operator (not unrestricted) → scope_applied=true.
	if p["scope_applied"] != true {
		t.Errorf("payload.scope_applied = %v, want true (coven-scoped)", p["scope_applied"])
	}
	if p["dry_run"] != false {
		t.Errorf("payload.dry_run = %v, want false", p["dry_run"])
	}
	// Normalized selector: all=false + coven/status (sids omitted).
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

// TestAssignCoven_Audit_OnDryRun (gap + PM-decision): dry_run=true STILL
// writes audit (trail of "who ran bulk operation preview") with dry_run:true.
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

// TestAssignCoven_Audit_NotWrittenOnEarlyReject (gap): early reject (malformed JSON
// 400 / invalid input 422 / label-out-of-scope 422) does NOT write mutation audit-event
// — middleware.Audit skips non-2xx.
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
			// coven=dev scope: 'prod' outside scope → 422 BEFORE DB (gate b).
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
	// covens from request must be returned in response (binding during onboarding).
	covens, _ := out["covens"].([]any)
	if len(covens) != 2 || covens[0] != "prod" || covens[1] != "dc-eu" {
		t.Errorf("covens = %v, want [prod dc-eu]", out["covens"])
	}
	if tok, _ := out["bootstrap_token"].(string); tok == "" {
		t.Errorf("bootstrap_token missing for agent")
	}
	// guard: expiration key is `expires_at` (not legacy `token_expires_at`).
	// Catches regression of wire-key rename (restore tag `token_expires_at`
	// → red).
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

// TestSoulCreate_TokenInsertFails_Rollback (coverage gap 3): token-insert failure
// after successful souls-insert → tx.Rollback called, no Commit. On real DB
// this guarantees no orphaned souls-row (insert and token in one tx).
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

// TestSoulCreate_UnknownCreator_422 (B2 unit): FK-violation on
// souls_created_by_aid_fk maps to 422, not opaque 500.
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
// documented OpenAPI field `covens` MUST NOT be rejected
// by strict-decoder as unknown field. Previously decoder expected `coven` →
// 400 "json: unknown field \"covens\"", and there was no API way to bind
// Soul to coven during onboarding.
func TestSoulCreate_Covens_AcceptedNotUnknownField(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent","covens":["redis-prod"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (covens must be accepted), body=%s", rec.Code, rec.Body.String())
	}
}

// TestSoulCreate_UnknownField_400: strict-decoder still rejects
// truly unknown fields (e.g., old name `coven`).
func TestSoulCreate_UnknownField_400(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, nil, nil, nil)

	rec := doCreate(t, h, `{"sid":"web-01.example.com","transport":"agent","coven":["prod"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field 'coven', body=%s", rec.Code, rec.Body.String())
	}
	assertProblemType(t, rec, problem.TypeMalformedRequest)
}

// TestSoulCreate_InvalidCoven_422: coven-tag not in kebab-case → 422
// (validation of ADR-008 stable tags at API boundary).
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
	// guard: expiration key is `expires_at` (not legacy `token_expires_at`).
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

// doList performs GET /v1/souls (with claims archon-alice) via recordList.
func doList(t *testing.T, h *SoulHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	return recordList(t, h, query, "archon-alice")
}

// recordList parses pagination/cursor the same way as the former (w,r)-route (offset+cursor
// conflict → 422, malformed cursor / bad pagination → 400), calls ListTyped directly
// (handler-native T5d) and serializes the result in recorder. aid="" → fail-closed (no claims).
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

// soulListReplyJSON projects the domain SoulListReply (PagedResponse[SoulListView]) into a map with
// json keys native envelope/element (for downstream decode tests of GET /v1/souls).
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
	// traits (ADR-060 read-path): host with traits returns them as object.
	if first.Traits["tier"] != "gold" || first.Traits["rack"] != "r12" {
		t.Errorf("item[0].traits = %v, want {tier:gold rack:r12}", first.Traits)
	}
	// ssh-host without coven → covens should be `[]`, not null (coalesceCoven).
	if out.Items[1].Covens == nil {
		t.Errorf("item[1].covens = null, want [] (coalesceCoven)")
	}
	// bare-soul without traits → `{}`, not null (coalesceTraits): UI renders empty
	// set without nil-check.
	if out.Items[1].Traits == nil {
		t.Errorf("item[1].traits = null, want {} (coalesceTraits)")
	}
	if len(out.Items[1].Traits) != 0 {
		t.Errorf("item[1].traits = %v, want empty", out.Items[1].Traits)
	}
}

// TestSoulList_NoSecretsLeak — fingerprint and any SoulSeed secrets MUST NOT
// appear in list-response. soulListItem does not declare them; we verify
// via raw-map that such keys are absent.
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
	// items should serialize as `[]`, not null.
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
	// All three values should arrive as pgx-parameters (not concatenation).
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

// --- ADR-047 S3b: scoped visibility of souls-list by Purview ---

// decodeListItems parses {items:[{sid,status}]} from List response.
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

// TestSoulList_EmptyPurview_FailClosed — CRITICAL security invariant (ADR-047):
// an operator with empty Purview (default-deny, no coven dimension) sees an EMPTY
// list, NOT the entire fleet. fakeSoulPool would return 1 host — handler MUST return 0
// and NOT call SelectAll. Regression = operator sees others' hosts.
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

// TestSoulList_NoClaims_FailClosed — no claims in context (defensive invariant,
// normally route under RequireJWT) → empty list, NOT the entire fleet.
func TestSoulList_NoClaims_FailClosed(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "secret-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: time.Now().UTC()},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	// WITHOUT claims — request without identity (recordList with aid="" → fail-closed).
	rec := recordList(t, h, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := decodeListItems(t, rec.Body.Bytes())
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("no-claims list total/len = %d/%d, want 0/0 (fail-closed)", out.Total, len(out.Items))
	}
}

// TestSoulList_NilScoper_FailClosed — scoper not configured (nil) → empty
// list (NOT the entire fleet). Unlike prod (Holder always exists), this guards against
// mis-wire-up: absence of resolver MUST NOT expose the fleet.
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

// TestSoulList_Unrestricted_All — `*`/bare-without-default Purview → entire list without
// scope-filter (coven-scope-args not added to SQL).
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
	// Unrestricted → MUST NOT have coven-scope-args (`[]string`) in SQL.
	for _, a := range pool.lastListArgs {
		if _, ok := a.([]string); ok {
			t.Errorf("unrestricted scope добавил coven-scope-args в SQL: %v", pool.lastListArgs)
		}
	}
}

// TestSoulList_CovenScope_ReachesSQL — coven-scoped operator: covens reach
// SQL as []string argument of scope-pushdown (`coven && ARRAY[covens]`).
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

// TestSoulList_ScopeOverridesPresence — scope (fail-closed) and presence-overlay
// (fail-safe) are separate: scope NARROWS result BEFORE presence-overlay. Empty
// Purview → 0 hosts, even if presence would return them as connected. Regression of this
// test = presence pattern "show when in doubt" leaked into scope layer.
func TestSoulList_ScopeOverridesPresence(t *testing.T) {
	pool := &fakeSoulPool{
		listCount: 1,
		listSouls: []*soul.Soul{
			{SID: "redis-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusDisconnected, RegisteredAt: time.Now().UTC()},
		},
	}
	// presence would return redis-01 as connected (live lease) — but scope is empty.
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

// TestSoulList_PartialScope_AppliesCovenSubset — operator with coven + non-coven
// dimension (soulprint, not yet evaluated in pilot): pilot applies coven-
// pushdown (strict subset, never over-show), list does not drop to 0.
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
	// coven-pushdown applied (covens reached SQL); non-coven dimension pilot
	// ignores (S3b-2), but does NOT zero out result.
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
