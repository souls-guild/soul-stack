package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- fakes ---

// fakeVoyageStore is a mock [VoyageStore]. A minimal SQL router by substrings
// (parity fakeErrandRunStore). Counts Insert/InsertTargets/cancel-UPDATE calls.
type fakeVoyageStore struct {
	mu               sync.Mutex
	insertCalls      int
	insertTargets    int   // rows read by CopyFrom (InsertTargets, S-med-3)
	insertArgs       []any // last INSERT INTO voyages args (for batch_mode checks)
	copyBatchIndexes []int // batch_index of each inserted unit (col #3 CopyFrom)
	committed        bool  // tx.Commit called (cap-reject must not commit)
	cancelUpdateRows int64 // RowsAffected returned by cancel-UPDATE (default 1)
	insertErr        error
	selectByID       func(id string) pgx.Row
	listRows         func() (pgx.Rows, error)
	listCount        int
	targetsRows      func() (pgx.Rows, error)
	// notify path (ephemeral Tiding, ADR-052(g)): heraldExists makes the
	// SelectHeraldByName existence-check "green" (otherwise notify fails 422 before
	// insert); insertTidings counts inserted ephemeral rules;
	// insertTidingErr simulates an INSERT INTO tidings failure (rollback invariant).
	heraldExists    bool
	insertTidings   int
	insertTidingErr error
}

func (f *fakeVoyageStore) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "INSERT INTO voyage_targets"):
		f.mu.Lock()
		f.insertTargets++
		f.mu.Unlock()
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "UPDATE voyages") && strings.Contains(sql, "status      = 'cancelled'"):
		n := f.cancelUpdateRows
		if n == 0 {
			n = 1
		}
		return pgconn.NewCommandTag("UPDATE " + itoa64(n)), nil
	}
	return pgconn.CommandTag{}, errors.New("fakeVoyageStore.Exec: unexpected SQL: " + sql)
}

func (f *fakeVoyageStore) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO voyages"):
		f.mu.Lock()
		f.insertCalls++
		f.insertArgs = args
		f.mu.Unlock()
		if f.insertErr != nil {
			return voyageErrRow{err: f.insertErr}
		}
		return voyageScalarRow{vals: []any{time.Now().UTC()}}
	case strings.Contains(sql, "FROM voyages\nWHERE voyage_id = $1"):
		if f.selectByID != nil {
			return f.selectByID(args[0].(string))
		}
		return voyageErrRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT COUNT(*) FROM voyages"):
		return voyageScalarRow{vals: []any{f.listCount}}
	case strings.Contains(sql, "FROM heralds\nWHERE name = $1"):
		// notify existence-check: heralds(name,type,config,secret_ref,enabled,
		// created_at,updated_at,created_by_aid). heraldExists=false → ErrNoRows
		// (→ 422 in prepareNotify).
		if !f.heraldExists {
			return voyageErrRow{err: pgx.ErrNoRows}
		}
		return voyageFullRow{vals: []any{
			args[0].(string), "webhook", []byte(`{}`), nil, true,
			time.Now().UTC(), time.Now().UTC(), nil,
		}}
	case strings.Contains(sql, "INSERT INTO tidings"):
		f.mu.Lock()
		f.insertTidings++
		f.mu.Unlock()
		if f.insertTidingErr != nil {
			return voyageErrRow{err: f.insertTidingErr}
		}
		return voyageScalarRow{vals: []any{time.Now().UTC(), time.Now().UTC()}}
	}
	return voyageErrRow{err: errors.New("fakeVoyageStore.QueryRow: unexpected SQL: " + sql)}
}

func (f *fakeVoyageStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM voyage_targets"):
		if f.targetsRows != nil {
			return f.targetsRows()
		}
		return &emptyRows{}, nil
	case strings.Contains(sql, "FROM voyages"):
		if f.listRows != nil {
			return f.listRows()
		}
		return &emptyRows{}, nil
	}
	return &emptyRows{}, nil
}

// CopyFrom is not called on the store itself (InsertTargets goes through tx,
// fakeVoyageTx.CopyFrom), but is required to satisfy the VoyageStore interface
// (ExecQueryRower now carries CopyFrom, S-med-3).
func (f *fakeVoyageStore) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("fakeVoyageStore.CopyFrom: unexpected (targets go through tx)")
}

func (f *fakeVoyageStore) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeVoyageTx{store: f}, nil
}

// fakeVoyageTx is a pgx.Tx wrapper routing Exec/QueryRow back into the store.
type fakeVoyageTx struct{ store *fakeVoyageStore }

func (t *fakeVoyageTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t *fakeVoyageTx) BeginFunc(ctx context.Context, fn func(pgx.Tx) error) error {
	return fn(t)
}
func (t *fakeVoyageTx) Commit(context.Context) error {
	t.store.mu.Lock()
	t.store.committed = true
	t.store.mu.Unlock()
	return nil
}
func (t *fakeVoyageTx) Rollback(context.Context) error { return nil }

// CopyFrom intercepts the batch insert of voyage_targets (InsertTargets, S-med-3):
// walks the source, counts rows into store.insertTargets, and simulates
// store.insertErr as a COPY failure.
func (t *fakeVoyageTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	n := 0
	var batchIndexes []int
	for src.Next() {
		vals, err := src.Values()
		if err != nil {
			return int64(n), err
		}
		// voyageTargetsColumns: voyage_id, target_kind, target_id, batch_index, status.
		if len(vals) >= 4 {
			if bi, ok := vals[3].(int); ok {
				batchIndexes = append(batchIndexes, bi)
			}
		}
		n++
	}
	if err := src.Err(); err != nil {
		return int64(n), err
	}
	t.store.mu.Lock()
	t.store.insertTargets += n
	t.store.copyBatchIndexes = batchIndexes
	t.store.mu.Unlock()
	return int64(n), nil
}
func (t *fakeVoyageTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { panic("unexpected") }
func (t *fakeVoyageTx) LargeObjects() pgx.LargeObjects                         { panic("unexpected") }
func (t *fakeVoyageTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected")
}
func (t *fakeVoyageTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.store.Exec(ctx, sql, args...)
}
func (t *fakeVoyageTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.store.Query(ctx, sql, args...)
}
func (t *fakeVoyageTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.store.QueryRow(ctx, sql, args...)
}
func (t *fakeVoyageTx) Conn() *pgx.Conn { return nil }

// voyageErrRow / voyageScalarRow are minimal Row implementations.
type voyageErrRow struct{ err error }

func (r voyageErrRow) Scan(...any) error { return r.err }

type voyageScalarRow struct{ vals []any }

func (r voyageScalarRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch p := d.(type) {
		case *time.Time:
			*p = r.vals[i].(time.Time)
		case *int:
			*p = r.vals[i].(int)
		}
	}
	return nil
}

// voyageFullRow is a Row for voyage.scanVoyage (25 columns strictly in
// selectColumns order). nil values in vals → NULL.
type voyageFullRow struct {
	vals []any
}

func (r voyageFullRow) Scan(dest ...any) error {
	for i, d := range dest {
		v := r.vals[i]
		switch p := d.(type) {
		case *string:
			*p = v.(string)
		case **string:
			if v == nil {
				*p = nil
			} else {
				s := v.(string)
				*p = &s
			}
		case *[]byte:
			if v == nil {
				*p = nil
			} else {
				*p = v.([]byte)
			}
		case **int:
			if v == nil {
				*p = nil
			} else {
				n := v.(int)
				*p = &n
			}
		case *int:
			*p = v.(int)
		case *bool:
			*p = v.(bool)
		case **bool:
			if v == nil {
				*p = nil
			} else {
				b := v.(bool)
				*p = &b
			}
		case **time.Time:
			if v == nil {
				*p = nil
			} else {
				t := v.(time.Time)
				*p = &t
			}
		case *time.Time:
			*p = v.(time.Time)
		case **float64:
			if v == nil {
				*p = nil
			} else {
				f := v.(float64)
				*p = &f
			}
		}
	}
	return nil
}

// voyageRowVals assembles the 31-column set for scanVoyage given kind/status
// (26 base + 4 S-W3/S-W4: batch_percent/fail_threshold/inter_unit/require_alive
// + cadence_id back-link ADR-046).
func voyageRowVals(id string, kind voyage.Kind, status voyage.Status) []any {
	var scenario, module any
	switch kind {
	case voyage.KindScenario:
		scenario = "deploy"
	case voyage.KindCommand:
		module = "core.cmd.shell"
	}
	resolved := []byte(`["host-a"]`)
	if kind == voyage.KindScenario {
		resolved = []byte(`["inc-a"]`)
	}
	return []any{
		id,               // voyage_id
		string(kind),     // kind
		scenario,         // scenario_name **string
		module,           // module **string
		[]byte(`{}`),     // input
		resolved,         // target_resolved
		[]byte(`{}`),     // target_origin
		nil,              // batch_size **int
		50,               // concurrency (as **int via *int? scanVoyage uses *int via &batchSize) -> see below
		nil,              // batch_mode **string
		false,            // dry_run
		nil,              // schedule_at **time.Time
		nil,              // inter_batch_interval **float64
		"continue",       // on_failure **string
		1,                // total_batches *int
		0,                // current_batch_index *int
		string(status),   // status
		nil,              // claimed_by_kid **string
		nil,              // last_renewed_at **time.Time
		nil,              // claim_expires_at **time.Time
		0,                // attempt *int
		"archon-alice",   // started_by_aid
		time.Now().UTC(), // created_at
		nil,              // started_at **time.Time
		nil,              // finished_at **time.Time
		[]byte(nil),      // summary
		nil,              // batch_percent **int
		nil,              // fail_threshold **int
		nil,              // inter_unit_interval **float64
		nil,              // require_alive **bool
		nil,              // cadence_id **string (back-link ADR-046; manual run = NULL)
	}
}

// fakeVoyageScenarioResolver / fakeVoyageCommandResolver.
type fakeVoyageScenarioResolver struct {
	out []string
	err error
}

func (f *fakeVoyageScenarioResolver) ResolveIncarnations(context.Context, VoyageScenarioFilter) ([]string, error) {
	return f.out, f.err
}

type fakeVoyageCommandResolver struct {
	out []string
	err error
}

func (f *fakeVoyageCommandResolver) ResolveSIDs(context.Context, VoyageCommandFilter) ([]string, error) {
	return f.out, f.err
}

// ResolveSIDsInScope — the scoper=nil createCommand path is not used here (helpers
// pass a nil scoper → cluster-wide ResolveSIDs). Implementation only to satisfy the
// interface: returns out as-is (scope is not applied).
func (f *fakeVoyageCommandResolver) ResolveSIDsInScope(context.Context, VoyageCommandFilter, soulpurview.Scope) (ScopedSIDs, error) {
	return ScopedSIDs{SIDs: f.out}, f.err
}

// captureCommandResolver records the last filter passed (checks that
// require_alive is propagated, S-W4).
type captureCommandResolver struct {
	out        []string
	err        error
	lastFilter VoyageCommandFilter
}

func (f *captureCommandResolver) ResolveSIDs(_ context.Context, filter VoyageCommandFilter) ([]string, error) {
	f.lastFilter = filter
	return f.out, f.err
}

func (f *captureCommandResolver) ResolveSIDsInScope(_ context.Context, filter VoyageCommandFilter, _ soulpurview.Scope) (ScopedSIDs, error) {
	f.lastFilter = filter
	return ScopedSIDs{SIDs: f.out}, f.err
}

// scopedCommandResolver is a fake ScopedSIDs resolver for guard tests of the ADR-047
// S4 hybrid semantics (403-explicit-foreign / trimming / 422-empty). lastScope
// records the scope passed; scoped is the fixed result of the InScope resolve.
type scopedCommandResolver struct {
	scoped     ScopedSIDs
	err        error
	lastFilter VoyageCommandFilter
	lastScope  soulpurview.Scope
	calledIn   bool // ResolveSIDsInScope was called (not cluster-wide ResolveSIDs)
}

func (f *scopedCommandResolver) ResolveSIDs(_ context.Context, filter VoyageCommandFilter) ([]string, error) {
	f.lastFilter = filter
	return f.scoped.SIDs, f.err
}

func (f *scopedCommandResolver) ResolveSIDsInScope(_ context.Context, filter VoyageCommandFilter, scope soulpurview.Scope) (ScopedSIDs, error) {
	f.calledIn = true
	f.lastFilter = filter
	f.lastScope = scope
	return f.scoped, f.err
}

// recordingScoper is a [PurviewResolver] fake that records the (resource, action)
// for which Purview was resolved (guards the resolve point — soul.list, not errand.run).
// fakeScoper is already declared in soul_test.go (coven/regex/unrestricted form).
type recordingScoper struct {
	pv               rbac.Purview
	resource, action string
}

func (s *recordingScoper) ResolvePurview(_, resource, action string) rbac.Purview {
	s.resource, s.action = resource, action
	return s.pv
}

// fakeVoyageEnforcer is a mock [middleware.PermissionChecker]. allow is the set of
// "<resource>.<action>" allowed for archon-alice; everything else → deny.
// revoked → Check always returns [rbac.ErrOperatorRevoked] (parity with a
// revoked Archon: no permission holds in any scope).
type fakeVoyageEnforcer struct {
	allow   map[string]bool
	revoked bool
}

func (e *fakeVoyageEnforcer) Check(_ string, resource, action string, _ map[string]string) error {
	if e.revoked {
		return rbac.ErrOperatorRevoked
	}
	if e.allow[resource+"."+action] {
		return nil
	}
	return rbac.ErrPermissionDenied
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- helpers ---

func newVoyageHandler(store *fakeVoyageStore, sc VoyageScenarioResolver, cmd VoyageCommandResolver, enf apimiddleware.PermissionChecker) *VoyageHandler {
	// maxScope=0 / maxBatchSize=0 → unlimited: existing tests do not hit the cap.
	// scoper=nil → cluster-wide command resolve (backcompat of existing tests).
	return NewVoyageHandler(store, sc, cmd, nil /*incReader=nil → bare-check only*/, enf, nil /*scoper*/, nil, nil /*tidingInvalidator*/, 0, 0, nil)
}

// newVoyageHandlerScoped is the variant with a scoper (ADR-047 S4 hybrid guard tests).
// Command resolve goes through ResolveSIDsInScope (target ∩ Purview).
func newVoyageHandlerScoped(store *fakeVoyageStore, cmd VoyageCommandResolver, enf apimiddleware.PermissionChecker, scoper PurviewResolver) *VoyageHandler {
	return NewVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, nil /*incReader*/, enf, scoper, nil, nil /*tidingInvalidator*/, 0, 0, nil)
}

// newVoyageHandlerCap is the variant with an explicit maxScope (DoS-guard S-med-3 tests).
// maxScope=0 → unlimited; >0 → cap, exceeding it yields 422 voyage_scope_too_large.
func newVoyageHandlerCap(store *fakeVoyageStore, sc VoyageScenarioResolver, cmd VoyageCommandResolver, enf apimiddleware.PermissionChecker, maxScope int) *VoyageHandler {
	return NewVoyageHandler(store, sc, cmd, nil /*incReader=nil → bare-check only*/, enf, nil /*scoper*/, nil, nil /*tidingInvalidator*/, maxScope, 0, nil)
}

// newVoyageHandlerBatchCap is the variant with an explicit maxBatchSize (DoS-guard S-W4 tests).
// maxBatchSize=0 → no limit; >0 → cap, exceeding it yields 422
// voyage_batch_size_too_large. maxScope=0 (does not interfere with batch-cap tests).
func newVoyageHandlerBatchCap(store *fakeVoyageStore, sc VoyageScenarioResolver, cmd VoyageCommandResolver, enf apimiddleware.PermissionChecker, maxBatchSize int) *VoyageHandler {
	return NewVoyageHandler(store, sc, cmd, nil /*incReader=nil → bare-check only*/, enf, nil /*scoper*/, nil, nil /*tidingInvalidator*/, 0, maxBatchSize, nil)
}

func voyageReq(method, url, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, url, http.NoBody)
	} else {
		r = httptest.NewRequest(method, url, strings.NewReader(body))
	}
	return r.WithContext(apimiddleware.InjectClaimsForTest(r.Context(), &keeperjwt.Claims{Subject: "archon-alice"}))
}

func voyageReqID(method, url, id, body string) *http.Request {
	r := voyageReq(method, url, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func allowAll() *fakeVoyageEnforcer {
	return &fakeVoyageEnforcer{allow: map[string]bool{
		"incarnation.run":     true,
		"errand.run":          true,
		"incarnation.history": true,
	}}
}

// --- tests: create ---

func TestVoyageCreate_Scenario_OK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a", "inc-b"}}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply voyageCreateReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Kind != "scenario" || reply.ScopeSize != 2 || reply.Status != "pending" {
		t.Errorf("reply = %+v, want kind=scenario scope=2 status=pending", reply)
	}
	if store.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", store.insertCalls)
	}
	if store.insertTargets != 2 {
		t.Errorf("insertTargets = %d, want 2 (per resolved incarnation)", store.insertTargets)
	}
}

func TestVoyageCreate_Command_OK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a", "host-b", "host-c"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply voyageCreateReply
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.Kind != "command" || reply.ScopeSize != 3 {
		t.Errorf("reply = %+v, want kind=command scope=3", reply)
	}
	if store.insertCalls != 1 || store.insertTargets != 3 {
		t.Errorf("insertCalls=%d insertTargets=%d, want 1/3", store.insertCalls, store.insertTargets)
	}
}

func TestVoyageCreate_ScenarioRBACDenied(t *testing.T) {
	store := &fakeVoyageStore{}
	// errand.run allowed, incarnation.run not.
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"errand.run": true}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, enf)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (denied before insert)", store.insertCalls)
	}
}

func TestVoyageCreate_CommandRBACDenied(t *testing.T) {
	store := &fakeVoyageStore{}
	// incarnation.run allowed, errand.run not.
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"incarnation.run": true}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a"}}, enf)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["host-a"]}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

func TestVoyageCreate_EmptyTarget422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: nil}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_empty_target") {
		t.Errorf("body should mention voyage_empty_target: %s", rec.Body.String())
	}
}

// S-med-1: a command target given ONLY via where (no sids/coven) silently
// resolved to all hosts (where is not evaluated in the MVP). Now — 422 before resolve.
func TestVoyageCreate_CommandWhereOnly422(t *testing.T) {
	store := &fakeVoyageStore{}
	// The resolver would return all hosts — but the guard must fire BEFORE resolve/insert.
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a", "host-b"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"where":"soulprint.self.os.family == 'debian'"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_where_not_evaluated") {
		t.Errorf("body should mention voyage_where_not_evaluated: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (rejected before insert)", store.insertCalls)
	}
}

// command with sids passes (scope narrowed by explicit SIDs).
func TestVoyageCreate_CommandSIDsOK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["host-a"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// command with coven passes (scope narrowed by a coven label).
func TestVoyageCreate_CommandCovenOK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a", "host-b"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// command with coven+where passes: where is additive, scope is already narrowed by coven.
func TestVoyageCreate_CommandCovenAndWhereOK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"],"where":"soulprint.self.os.family == 'debian'"}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// --- ADR-047 S4: hybrid target ∩ Purview for the command path ---

// Branch 1 (anti-escalation): a scoped Archon named an explicit foreign SID (DeniedExplicit
// non-empty) → 403, run NOT created.
func TestVoyageCreate_CommandScoped_ExplicitForeignSID_403(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{
		SIDs:           []string{"web-01.example.com"},
		DeniedExplicit: []string{"db-01.example.com"},
	}}
	scoper := fakeScoper{covens: []string{"prod"}}
	h := newVoyageHandlerScoped(store, cmd, allowAll(), scoper)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["web-01.example.com","db-01.example.com"]}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (явный чужой хост); body=%s", rec.Code, rec.Body.String())
	}
	if !cmd.calledIn {
		t.Error("ResolveSIDsInScope не вызван — scope не применён")
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (403 не создаёт прогон)", store.insertCalls)
	}
}

// Branch 2 (trimming): a wide target coven=[A,B], scope coven=A → only the A
// subset resolves (NOT the whole A∪B), 202, no rejection.
func TestVoyageCreate_CommandScoped_WideTarget_Trimmed202(t *testing.T) {
	store := &fakeVoyageStore{}
	// The resolver returned the trimmed subset (only A hosts), DeniedExplicit empty.
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{SIDs: []string{"a1.example.com", "a2.example.com"}}}
	scoper := fakeScoper{covens: []string{"coven-a"}}
	h := newVoyageHandlerScoped(store, cmd, allowAll(), scoper)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-a","coven-b"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (урезание без отказа); body=%s", rec.Code, rec.Body.String())
	}
	var reply voyageCreateReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.ScopeSize != 2 {
		t.Errorf("scope_size = %d, want 2 (только подмножество A, не A∪B)", reply.ScopeSize)
	}
	if store.insertTargets != 2 {
		t.Errorf("insertTargets = %d, want 2 (урезанный snapshot)", store.insertTargets)
	}
}

// Branch 3 (empty intersection): a wide target trimmed to zero → 422
// voyage_empty_target (not 403 — this is not escalation but a valid request with no target).
func TestVoyageCreate_CommandScoped_EmptyIntersection_422(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{}} // neither SIDs nor DeniedExplicit
	scoper := fakeScoper{covens: []string{"coven-a"}}
	h := newVoyageHandlerScoped(store, cmd, allowAll(), scoper)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-b"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (пустое пересечение); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_empty_target") {
		t.Errorf("detail не содержит voyage_empty_target: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// Backcompat: Unrestricted scope (cluster-admin) → full resolve through
// ResolveSIDsInScope (scope.Unrestricted=true), 202, nothing trimmed.
func TestVoyageCreate_CommandScoped_Unrestricted_FullResolve202(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{SIDs: []string{"h1", "h2", "h3"}}}
	scoper := fakeScoper{unrestricted: true}
	h := newVoyageHandlerScoped(store, cmd, allowAll(), scoper)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (Unrestricted backcompat); body=%s", rec.Code, rec.Body.String())
	}
	if !cmd.lastScope.Unrestricted {
		t.Error("scope, переданный в резолвер, не Unrestricted (cluster-admin backcompat сломан)")
	}
	var reply voyageCreateReply
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.ScopeSize != 3 {
		t.Errorf("scope_size = %d, want 3 (полный резолв)", reply.ScopeSize)
	}
}

// The scoper resolves Purview for (errand, run) — the command target scope comes
// from the selector of the errand.run permission itself (ADR-047 S4 delegation), not soul.list.
func TestVoyageCreate_CommandScoped_ResolvesErrandRunPurview(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{SIDs: []string{"h1"}}}
	rec := &recordingScoper{pv: rbac.Purview{Covens: []string{"prod"}}}
	h := newVoyageHandlerScoped(store, cmd, allowAll(), rec)

	w := httptest.NewRecorder()
	h.Create(w, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if rec.resource != "errand" || rec.action != "run" {
		t.Errorf("Purview резолвнут для (%q,%q), want (errand,run)", rec.resource, rec.action)
	}
}

// Existence-gate: the scoper returned an empty Purview (no errand.run) → 403 before resolve
// (a scoped role with a permission for something else does not launch a command).
func TestVoyageCreate_CommandScoped_NoErrandRun_403(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{SIDs: []string{"h1"}}}
	scoper := fakeScoper{empty: true} // Purview{} → Scope.Empty
	h := newVoyageHandlerScoped(store, cmd, allowAll(), scoper)

	w := httptest.NewRecorder()
	h.Create(w, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (нет errand.run); body=%s", w.Code, w.Body.String())
	}
	if cmd.calledIn {
		t.Error("ResolveSIDsInScope вызван при Empty-scope — existence-gate не сработал")
	}
}

// Guard (ADR-047 S4 parity of revoked semantics): a REVOKED Archon with a scoped role
// takes the command path (scoper!=nil). ResolvePurview yields Empty (revoke merges with
// no-perm), but the classifier inside the Empty branch must return
// TypeOperatorRevokedToken (401), NOT a generic 403 — parity with the scenario path and
// the scoper==nil branch. Mutating the fix to "always 403" is caught by the problem-type
// check.
func TestVoyageCreate_CommandScoped_Revoked_RevokedTokenNotGeneric403(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{SIDs: []string{"h1"}}}
	scoper := fakeScoper{empty: true}         // revoked → Purview{} → Scope.Empty
	enf := &fakeVoyageEnforcer{revoked: true} // Check → ErrOperatorRevoked
	h := newVoyageHandlerScoped(store, cmd, enf, scoper)

	w := httptest.NewRecorder()
	h.Create(w, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (revoked); body=%s", w.Code, w.Body.String())
	}
	if ct := w.Body.String(); !strings.Contains(ct, problem.TypeOperatorRevokedToken) {
		t.Fatalf("body не содержит %s (вернулся generic 403?); body=%s",
			problem.TypeOperatorRevokedToken, ct)
	}
	if cmd.calledIn {
		t.Error("ResolveSIDsInScope вызван при Empty-scope — existence-gate не сработал")
	}
}

func TestVoyageCreate_InvalidKind422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"frobnicate","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVoyageCreate_ScenarioMissingName422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVoyageCreate_BadJSON400(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages", `{"kind":}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: batch_mode (S-W1) ---

// batch_mode=window for kind=command: 202, batch_mode='window' goes into Insert,
// batch_index=0 for all units (flat run, ADR-043 amendment §7).
func TestVoyageCreate_Command_WindowOK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a", "host-b", "host-c"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_mode":"window","concurrency":2,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	// batch_mode arg (10th positional, index 9 in insertSQL) = 'window'.
	store.mu.Lock()
	bm := store.insertArgs[9]
	idxs := append([]int(nil), store.copyBatchIndexes...)
	store.mu.Unlock()
	if bm != string(voyage.BatchModeWindow) {
		t.Errorf("batch_mode arg = %v, want %q", bm, voyage.BatchModeWindow)
	}
	if len(idxs) != 3 {
		t.Fatalf("captured batch_indexes = %v, want 3 единицы", idxs)
	}
	for i, bi := range idxs {
		if bi != 0 {
			t.Errorf("batch_index[%d] = %d, want 0 (window — плоский прогон)", i, bi)
		}
	}
}

// batch_mode=window + explicit batch_size → 422 (ADR-043 amendment §1: batch_size is
// not used in window).
func TestVoyageCreate_WindowWithBatchSize422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_mode":"window","batch_size":2,"target":{"sids":["host-a"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (отвергнут до Insert)", store.insertCalls)
	}
}

// kind=scenario + batch_mode=window → 202 (S-W2: guard снят, появился
// scenario-исполнитель скользящего окна; единица окна = инкарнация). batch_mode=
// 'window' уходит в Insert, batch_index=0 у всех инкарнаций (плоский прогон,
// ADR-043 amendment §7).
func TestVoyageCreate_ScenarioWindowOK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a", "inc-b", "inc-c"}}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","batch_mode":"window","concurrency":2,"target":{"service":"web"}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bm := store.insertArgs[9]
	idxs := append([]int(nil), store.copyBatchIndexes...)
	store.mu.Unlock()
	if bm != string(voyage.BatchModeWindow) {
		t.Errorf("batch_mode arg = %v, want %q", bm, voyage.BatchModeWindow)
	}
	if len(idxs) != 3 {
		t.Fatalf("captured batch_indexes = %v, want 3 инкарнации", idxs)
	}
	for i, bi := range idxs {
		if bi != 0 {
			t.Errorf("batch_index[%d] = %d, want 0 (window — плоский прогон)", i, bi)
		}
	}
}

// batch_mode вне {barrier, window} → 422.
func TestVoyageCreate_InvalidBatchMode422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_mode":"bogus","target":{"sids":["host-a"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// batch_mode=barrier (явный) — текущее поведение, batch_mode NULL не пишется как
// 'barrier' (forward-compat: «не задано» = barrier, см. buildVoyageRow).
func TestVoyageCreate_ExplicitBarrier_NotPersistedAsValue(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_mode":"barrier","target":{"sids":["host-a"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bm := store.insertArgs[9]
	store.mu.Unlock()
	if bm != nil {
		t.Errorf("batch_mode arg = %v, want nil (barrier → NULL, forward-compat)", bm)
	}
}

// --- tests: batch_percent (S-W3) ---

// batch_percent резолвится в эффективный batch_size = ceil(scope * pct/100):
// scope=5 хостов, pct=50 → ceil(2.5)=3 пишется в insertSQL batch_size (index 7),
// а batch_percent — в index 17.
func TestVoyageCreate_BatchPercent_ResolvesEffectiveBatchSize(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3", "h4", "h5"}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_percent":50,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bs := store.insertArgs[7]  // batch_size
	bp := store.insertArgs[17] // batch_percent
	idxs := append([]int(nil), store.copyBatchIndexes...)
	store.mu.Unlock()
	if bs != 3 {
		t.Errorf("effective batch_size arg = %v, want 3 (ceil(5*50/100))", bs)
	}
	if bp != 50 {
		t.Errorf("batch_percent arg = %v, want 50", bp)
	}
	// batch_index по эффективному размеру 3: [0,0,0,1,1].
	want := []int{0, 0, 0, 1, 1}
	if len(idxs) != len(want) {
		t.Fatalf("batch_indexes = %v, want %v", idxs, want)
	}
	for i := range want {
		if idxs[i] != want[i] {
			t.Errorf("batch_index[%d] = %d, want %d", i, idxs[i], want[i])
		}
	}
}

// batch_size + batch_percent одновременно → 422 (взаимоисключение, §2).
func TestVoyageCreate_BatchSizeAndPercent_MutuallyExclusive422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_size":2,"batch_percent":50,"target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (отвергнут до Insert)", store.insertCalls)
	}
}

// batch_percent + batch_mode=window → 422 (percent не используется в window, §2).
func TestVoyageCreate_BatchPercentWithWindow422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_mode":"window","batch_percent":50,"target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// batch_percent вне [1,100] → 422.
func TestVoyageCreate_BatchPercentOutOfRange422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_percent":0,"target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: fail_threshold (S-W3, handler-валидация) ---

// fail_threshold пишется в Insert (index 18); <=0 → 422.
func TestVoyageCreate_FailThreshold_Persisted(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1", "h2"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","fail_threshold":3,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	ft := store.insertArgs[18]
	store.mu.Unlock()
	if ft != 3 {
		t.Errorf("fail_threshold arg = %v, want 3", ft)
	}
}

func TestVoyageCreate_FailThresholdNonPositive422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","fail_threshold":0,"target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: max_batch_size (S-W4) ---

// barrier: эффективный batch_size > max_batch_size → 422 voyage_batch_size_too_large.
func TestVoyageCreate_BatchSizeExceedsCap422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerBatchCap(store, &fakeVoyageScenarioResolver{},
		&fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3"}}, allowAll(), 2)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_size":5,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_size_too_large") {
		t.Errorf("body lacks voyage_batch_size_too_large: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (отвергнут до Insert)", store.insertCalls)
	}
}

// window: concurrency > max_batch_size → 422 voyage_batch_size_too_large.
func TestVoyageCreate_WindowConcurrencyExceedsCap422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerBatchCap(store, &fakeVoyageScenarioResolver{},
		&fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3"}}, allowAll(), 2)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_mode":"window","concurrency":10,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_size_too_large") {
		t.Errorf("body lacks voyage_batch_size_too_large: %s", rec.Body.String())
	}
}

// batch_size == max_batch_size → ok (граница включительна).
func TestVoyageCreate_BatchSizeAtCap_OK(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerBatchCap(store, &fakeVoyageScenarioResolver{},
		&fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3", "h4"}}, allowAll(), 2)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_size":2,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// barrier: batch_percent даёт эффективный batch_size > max_batch_size → 422
// voyage_batch_size_too_large. scope=4 хоста, pct=75 → ceil(3)=3 > cap 2.
func TestVoyageCreate_BatchPercentEffectiveExceedsCap422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerBatchCap(store, &fakeVoyageScenarioResolver{},
		&fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3", "h4"}}, allowAll(), 2)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_percent":75,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_size_too_large") {
		t.Errorf("body lacks voyage_batch_size_too_large: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (отвергнут до Insert)", store.insertCalls)
	}
}

// --- tests: require_alive (S-W4, проброс в command-фильтр) ---

// require_alive=true прокидывается в VoyageCommandFilter.RequireAlive и пишется
// в Insert (index 20).
func TestVoyageCreate_RequireAlive_PropagatedAndPersisted(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &captureCommandResolver{out: []string{"h1"}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","require_alive":true,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if !cmd.lastFilter.RequireAlive {
		t.Errorf("filter.RequireAlive = false, want true (проброс §5)")
	}
	store.mu.Lock()
	ra := store.insertArgs[20]
	store.mu.Unlock()
	if ra != true {
		t.Errorf("require_alive arg = %v, want true", ra)
	}
}

// --- tests: read ---

func TestVoyageGet_OK(t *testing.T) {
	id := audit.NewULID()
	store := &fakeVoyageStore{
		selectByID: func(gotID string) pgx.Row {
			return voyageFullRow{vals: voyageRowVals(gotID, voyage.KindScenario, voyage.StatusRunning)}
		},
	}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Get(rec, voyageReqID(http.MethodGet, "/v1/voyages/"+id, id, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto voyageDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	if dto.VoyageID != id || dto.Kind != "scenario" || dto.Status != "running" {
		t.Errorf("dto = %+v, want id=%s scenario running", dto, id)
	}
}

func TestVoyageGet_NotFound(t *testing.T) {
	id := audit.NewULID()
	store := &fakeVoyageStore{selectByID: func(string) pgx.Row { return voyageErrRow{err: pgx.ErrNoRows} }}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Get(rec, voyageReqID(http.MethodGet, "/v1/voyages/"+id, id, ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVoyageGet_BadULID422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Get(rec, voyageReqID(http.MethodGet, "/v1/voyages/not-a-ulid", "not-a-ulid", ""))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVoyageList_OK(t *testing.T) {
	store := &fakeVoyageStore{
		listCount: 1,
		listRows: func() (pgx.Rows, error) {
			return &voyageRowsIter{rows: [][]any{voyageRowVals(audit.NewULID(), voyage.KindCommand, voyage.StatusPending)}}, nil
		},
	}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.List(rec, voyageReq(http.MethodGet, "/v1/voyages?kind=command", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Items []voyageDTO `json:"items"`
		Total int         `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.Total != 1 || len(reply.Items) != 1 || reply.Items[0].Kind != "command" {
		t.Errorf("reply = %+v, want total=1 one command item", reply)
	}
}

func TestVoyageList_BadKind422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.List(rec, voyageReq(http.MethodGet, "/v1/voyages?kind=bogus", ""))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVoyageTargets_OK(t *testing.T) {
	id := audit.NewULID()
	store := &fakeVoyageStore{
		selectByID: func(gotID string) pgx.Row {
			return voyageFullRow{vals: voyageRowVals(gotID, voyage.KindScenario, voyage.StatusRunning)}
		},
		targetsRows: func() (pgx.Rows, error) {
			return &voyageTargetRowsIter{rows: [][]any{
				{id, "incarnation", "inc-a", 0, "running", "apply-1", nil, nil},
			}}, nil
		},
	}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Targets(rec, voyageReqID(http.MethodGet, "/v1/voyages/"+id+"/targets", id, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Targets []voyageTargetEntryDTO `json:"targets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if len(reply.Targets) != 1 || reply.Targets[0].TargetID != "inc-a" || reply.Targets[0].Status != "running" {
		t.Errorf("targets = %+v, want one running inc-a", reply.Targets)
	}
}

// --- tests: cancel ---

func TestVoyageCancel_Pending(t *testing.T) {
	id := audit.NewULID()
	store := &fakeVoyageStore{
		cancelUpdateRows: 1,
		selectByID: func(gotID string) pgx.Row {
			return voyageFullRow{vals: voyageRowVals(gotID, voyage.KindScenario, voyage.StatusPending)}
		},
	}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Cancel(rec, voyageReqID(http.MethodDelete, "/v1/voyages/"+id, id, ""))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cancelled") {
		t.Errorf("body should report cancelled: %s", rec.Body.String())
	}
}

func TestVoyageCancel_RunningUnsupported409(t *testing.T) {
	id := audit.NewULID()
	store := &fakeVoyageStore{
		selectByID: func(gotID string) pgx.Row {
			return voyageFullRow{vals: voyageRowVals(gotID, voyage.KindCommand, voyage.StatusRunning)}
		},
	}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Cancel(rec, voyageReqID(http.MethodDelete, "/v1/voyages/"+id, id, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVoyageCancel_TerminalConflict(t *testing.T) {
	id := audit.NewULID()
	store := &fakeVoyageStore{
		selectByID: func(gotID string) pgx.Row {
			return voyageFullRow{vals: voyageRowVals(gotID, voyage.KindScenario, voyage.StatusSucceeded)}
		},
	}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Cancel(rec, voyageReqID(http.MethodDelete, "/v1/voyages/"+id, id, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVoyageCancel_RBACDenied(t *testing.T) {
	id := audit.NewULID()
	store := &fakeVoyageStore{
		selectByID: func(gotID string) pgx.Row {
			return voyageFullRow{vals: voyageRowVals(gotID, voyage.KindScenario, voyage.StatusPending)}
		},
	}
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"errand.run": true}} // нет incarnation.run
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, enf)

	rec := httptest.NewRecorder()
	h.Cancel(rec, voyageReqID(http.MethodDelete, "/v1/voyages/"+id, id, ""))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// --- row iterators ---

type voyageRowsIter struct {
	rows [][]any
	idx  int
}

func (r *voyageRowsIter) Next() bool {
	r.idx++
	return r.idx <= len(r.rows)
}
func (r *voyageRowsIter) Scan(dest ...any) error {
	return voyageFullRow{vals: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *voyageRowsIter) Err() error                                   { return nil }
func (r *voyageRowsIter) Close()                                       {}
func (r *voyageRowsIter) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *voyageRowsIter) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *voyageRowsIter) Values() ([]any, error)                       { return nil, nil }
func (r *voyageRowsIter) RawValues() [][]byte                          { return nil }
func (r *voyageRowsIter) Conn() *pgx.Conn                              { return nil }

// voyageTargetRowsIter — для SelectTargets (8 колонок).
type voyageTargetRowsIter struct {
	rows [][]any
	idx  int
}

func (r *voyageTargetRowsIter) Next() bool {
	r.idx++
	return r.idx <= len(r.rows)
}
func (r *voyageTargetRowsIter) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		v := row[i]
		switch p := d.(type) {
		case *string:
			*p = v.(string)
		case *int:
			*p = v.(int)
		case **string:
			if v == nil {
				*p = nil
			} else {
				s := v.(string)
				*p = &s
			}
		case **time.Time:
			if v == nil {
				*p = nil
			} else {
				tt := v.(time.Time)
				*p = &tt
			}
		}
	}
	return nil
}
func (r *voyageTargetRowsIter) Err() error                                   { return nil }
func (r *voyageTargetRowsIter) Close()                                       {}
func (r *voyageTargetRowsIter) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *voyageTargetRowsIter) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *voyageTargetRowsIter) Values() ([]any, error)                       { return nil, nil }
func (r *voyageTargetRowsIter) RawValues() [][]byte                          { return nil }
func (r *voyageTargetRowsIter) Conn() *pgx.Conn                              { return nil }

// --- S-med-3: scope cap (voyage_scope_too_large) ---

// voyageCapSIDs генерирует n валидных SID-ов для cap-тестов command-ветки.
func voyageCapSIDs(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = "host-" + strconv.Itoa(i) + ".example.com"
	}
	return out
}

// (а) scope > cap → 422 voyage_scope_too_large (command-ветка); ни COPY, ни commit.
func TestVoyageCreate_Command_ScopeTooLarge(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(5)}
	h := newVoyageHandlerCap(store, &fakeVoyageScenarioResolver{}, cmd, allowAll(), 3)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_scope_too_large") {
		t.Fatalf("body lacks voyage_scope_too_large: %s", rec.Body.String())
	}
	if store.insertTargets != 0 {
		t.Fatalf("insertTargets=%d, want 0 (cap reject before COPY)", store.insertTargets)
	}
	if store.committed {
		t.Fatalf("tx must not commit when scope exceeds cap")
	}
}

// (а') scope > cap → 422 (scenario-ветка).
func TestVoyageCreate_Scenario_ScopeTooLarge(t *testing.T) {
	store := &fakeVoyageStore{}
	sc := &fakeVoyageScenarioResolver{out: []string{"inc-a", "inc-b", "inc-c", "inc-d"}}
	h := newVoyageHandlerCap(store, sc, &fakeVoyageCommandResolver{}, allowAll(), 2)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"incarnations":["inc-a","inc-b","inc-c","inc-d"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_scope_too_large") {
		t.Fatalf("body lacks voyage_scope_too_large: %s", rec.Body.String())
	}
}

// (б) scope == cap → ok (граница включительна); CopyFrom вставил ровно cap строк.
func TestVoyageCreate_Command_ScopeAtCap(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(3)}
	h := newVoyageHandlerCap(store, &fakeVoyageScenarioResolver{}, cmd, allowAll(), 3)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertTargets != 3 {
		t.Fatalf("insertTargets=%d, want 3 (CopyFrom row count)", store.insertTargets)
	}
}

// (в) cap=0 → безлимит: большой scope проходит, CopyFrom вставляет все строки.
func TestVoyageCreate_Command_CapUnlimited(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(50)}
	h := newVoyageHandlerCap(store, &fakeVoyageScenarioResolver{}, cmd, allowAll(), 0)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202 (cap=0 = unlimited); body=%s", rec.Code, rec.Body.String())
	}
	if store.insertTargets != 50 {
		t.Fatalf("insertTargets=%d, want 50", store.insertTargets)
	}
}

// --- tests: batch (строковое поле, S1) ---

// batch:"5" эквивалентен batch_size:5 по эффекту: тот же batch_size в Insert
// (index 7) и тот же batch_index-разбиение.
func TestVoyageCreate_BatchString_HostsEqualsBatchSize(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3", "h4", "h5", "h6"}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch":"5","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bs := store.insertArgs[7]
	idxs := append([]int(nil), store.copyBatchIndexes...)
	store.mu.Unlock()
	if bs != 5 {
		t.Errorf("batch_size arg = %v, want 5 (batch:\"5\" == batch_size:5)", bs)
	}
	// 6 единиц по 5 → Leg-и [0,0,0,0,0,1].
	want := []int{0, 0, 0, 0, 0, 1}
	if len(idxs) != len(want) {
		t.Fatalf("batch_indexes = %v, want %v", idxs, want)
	}
	for i := range want {
		if idxs[i] != want[i] {
			t.Errorf("batch_index[%d] = %d, want %d", i, idxs[i], want[i])
		}
	}
}

// batch:"25%" при scope=10 → effective batch_size = ceil(10*25/100) = 3 (как
// batch_percent:25). batch_size (index 7) = 3, batch_percent (index 17) = 25.
func TestVoyageCreate_BatchString_PercentResolvesEffectiveBatchSize(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(10)}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch":"25%","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bs := store.insertArgs[7]
	bp := store.insertArgs[17]
	store.mu.Unlock()
	if bs != 3 {
		t.Errorf("effective batch_size arg = %v, want 3 (ceil(10*25/100))", bs)
	}
	if bp != 25 {
		t.Errorf("batch_percent arg = %v, want 25", bp)
	}
}

// fail-closed: malformed batch → 422 с человекочитаемым detail, до Insert.
func TestVoyageCreate_BatchString_Malformed422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch":"5.5","target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "field 'batch' must be") {
		t.Errorf("body lacks human-readable batch detail: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (отвергнут до Insert)", store.insertCalls)
	}
}

// percent вне диапазона → 422 (parity batch_percent).
func TestVoyageCreate_BatchString_PercentOutOfRange422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch":"150%","target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// conflict: batch + batch_size одновременно → 422 voyage_batch_spec_conflict.
func TestVoyageCreate_BatchString_ConflictWithBatchSize422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch":"5","batch_size":2,"target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_spec_conflict") {
		t.Errorf("body lacks voyage_batch_spec_conflict: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// conflict: batch + batch_percent одновременно → 422 voyage_batch_spec_conflict.
func TestVoyageCreate_BatchString_ConflictWithBatchPercent422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch":"20%","batch_percent":50,"target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_spec_conflict") {
		t.Errorf("body lacks voyage_batch_spec_conflict: %s", rec.Body.String())
	}
}

// window + batch → 422 (parity существующего window-guard для batch_size/percent).
func TestVoyageCreate_BatchString_WithWindow422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_mode":"window","batch":"5","target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// batch:"" (явная пустая строка) → трактуется как «не задано»: весь scope одним
// Leg, 202, batch_size NULL (не 422).
func TestVoyageCreate_BatchString_EmptyTreatedAsUnset(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3"}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch":"","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (empty batch = not set); body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bs := store.insertArgs[7]
	idxs := append([]int(nil), store.copyBatchIndexes...)
	store.mu.Unlock()
	if bs != nil {
		t.Errorf("batch_size arg = %v, want nil (весь scope одним Leg)", bs)
	}
	for i, bi := range idxs {
		if bi != 0 {
			t.Errorf("batch_index[%d] = %d, want 0 (один Leg)", i, bi)
		}
	}
}

// backcompat: старый batch_size:5 без batch работает (не сломан добавлением batch).
func TestVoyageCreate_Backcompat_BatchSizeWithoutBatch(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3"}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_size":5,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bs := store.insertArgs[7]
	store.mu.Unlock()
	if bs != 5 {
		t.Errorf("batch_size arg = %v, want 5 (backcompat)", bs)
	}
}

// backcompat: старый batch_percent:25 без batch работает.
func TestVoyageCreate_Backcompat_BatchPercentWithoutBatch(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(10)}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","batch_percent":25,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	bp := store.insertArgs[17]
	store.mu.Unlock()
	if bp != 25 {
		t.Errorf("batch_percent arg = %v, want 25 (backcompat)", bp)
	}
}

// --- tests: max_failures (S2 строковых batch-полей, ADR-043 amendment 2026-06-09) ---

// max_failures:"3" → абсолютный fail_threshold = 3 (как fail_threshold:3).
// fail_threshold пишется в Insert (index 18).
func TestVoyageCreate_MaxFailures_HostsEqualsFailThreshold(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3", "h4", "h5"}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","max_failures":"3","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	ft := store.insertArgs[18]
	store.mu.Unlock()
	if ft != 3 {
		t.Errorf("fail_threshold arg = %v, want 3 (max_failures:\"3\" == fail_threshold:3)", ft)
	}
}

// max_failures:"25%" при scope=8 хостов → ceil(8*25/100) = 2.
func TestVoyageCreate_MaxFailures_Percent_Command_ScopeEight(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(8)}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","max_failures":"25%","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	ft := store.insertArgs[18]
	store.mu.Unlock()
	if ft != 2 {
		t.Errorf("fail_threshold arg = %v, want 2 (ceil(8*25/100))", ft)
	}
}

// max_failures:"50%" при scope=4 инкарнации (scenario) → ceil(4*50/100) = 2.
// Подтверждает: единица прогона scenario = инкарнация, та же база, что effBatchSize.
func TestVoyageCreate_MaxFailures_Percent_Scenario_ScopeFour(t *testing.T) {
	store := &fakeVoyageStore{}
	sc := &fakeVoyageScenarioResolver{out: []string{"inc-a", "inc-b", "inc-c", "inc-d"}}
	h := newVoyageHandler(store, sc, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","max_failures":"50%","target":{"service":"web"}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	ft := store.insertArgs[18]
	store.mu.Unlock()
	if ft != 2 {
		t.Errorf("fail_threshold arg = %v, want 2 (ceil(4*50/100), scope=incarnations)", ft)
	}
}

// max_failures:"100%" при scope=5 → весь scope (clamp [1,scope]): ceil(5*100/100)=5.
func TestVoyageCreate_MaxFailures_PercentHundred_ClampsToScope(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(5)}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","max_failures":"100%","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	ft := store.insertArgs[18]
	store.mu.Unlock()
	if ft != 5 {
		t.Errorf("fail_threshold arg = %v, want 5 (ceil(5*100/100), clamp to scope)", ft)
	}
}

// fail-closed: malformed max_failures → 422 с человекочитаемым detail, до Insert.
func TestVoyageCreate_MaxFailures_Malformed422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","max_failures":"3.5","target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "field 'max_failures' must be") {
		t.Errorf("body lacks human-readable max_failures detail: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (отвергнут до Insert)", store.insertCalls)
	}
}

// percent вне диапазона → 422 (parity batch_percent), до Insert.
func TestVoyageCreate_MaxFailures_PercentOutOfRange422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","max_failures":"150%","target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// conflict: max_failures + fail_threshold одновременно → 422 voyage_batch_spec_conflict.
func TestVoyageCreate_MaxFailures_ConflictWithFailThreshold422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","max_failures":"3","fail_threshold":2,"target":{"sids":["h1"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_spec_conflict") {
		t.Errorf("body lacks voyage_batch_spec_conflict: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// empty max_failures:"" → «не задано» (no-op, не 422): fail_threshold остаётся NULL.
func TestVoyageCreate_MaxFailures_EmptyTreatedAsUnset(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: []string{"h1", "h2"}}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","max_failures":"","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	ft := store.insertArgs[18]
	store.mu.Unlock()
	if ft != nil {
		t.Errorf("fail_threshold arg = %v, want nil (empty max_failures == unset)", ft)
	}
}

// backcompat: старый fail_threshold:3 без max_failures работает (уже покрыт
// TestVoyageCreate_FailThreshold_Persisted — здесь проверяем явное сосуществование
// в одном раунде: max_failures отсутствует ⇒ fail_threshold int проходит как раньше).
func TestVoyageCreate_Backcompat_FailThresholdWithoutMaxFailures(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &fakeVoyageCommandResolver{out: voyageCapSIDs(10)}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, cmd, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","fail_threshold":3,"target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	ft := store.insertArgs[18]
	store.mu.Unlock()
	if ft != 3 {
		t.Errorf("fail_threshold arg = %v, want 3 (backcompat)", ft)
	}
}

// --- tests: preview (POST /v1/voyages/preview, ADR-043 amendment §4) ---

// decodePreviewBody — разбор voyagePreviewReply из rec.Body.
func decodePreviewBody(t *testing.T, body []byte) voyagePreviewReply {
	t.Helper()
	var r voyagePreviewReply
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode preview reply: %v; body=%s", err, body)
	}
	return r
}

// Preview не персистит и не раскрывает SID-список: barrier scenario, batch=2 на
// 3 инкарнации → scope_size=3, effective_batch_size=2, total_batches=2. Store не
// тронут (insertCalls=0), сырой JSON не содержит SID/incarnation-полей.
func TestVoyagePreview_Scenario_NoPersist_NoDisclosure(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a", "inc-b", "inc-c"}}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"},"batch":"2"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rep := decodePreviewBody(t, rec.Body.Bytes())
	if rep.Kind != "scenario" || rep.ScopeSize != 3 || rep.BatchMode != "barrier" {
		t.Errorf("reply = %+v, want kind=scenario scope_size=3 batch_mode=barrier", rep)
	}
	if rep.EffectiveBatchSize == nil || *rep.EffectiveBatchSize != 2 {
		t.Errorf("effective_batch_size = %v, want 2", rep.EffectiveBatchSize)
	}
	if rep.TotalBatches != 2 {
		t.Errorf("total_batches = %d, want 2 (ceil 3/2)", rep.TotalBatches)
	}
	// No-persist: ни Insert, ни InsertTargets, ни commit.
	if store.insertCalls != 0 || store.insertTargets != 0 || store.committed {
		t.Errorf("preview персистил: insertCalls=%d insertTargets=%d committed=%v, want 0/0/false",
			store.insertCalls, store.insertTargets, store.committed)
	}
	// No-disclosure: сырой JSON не содержит имён единиц / target-полей.
	for _, forbidden := range []string{"inc-a", "inc-b", "inc-c", "\"sids\"", "\"incarnations\"", "\"hosts\"", "target_resolved"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("preview-ответ раскрывает единицы (нашёл %q): %s", forbidden, rec.Body.String())
		}
	}
}

// barrier batch=N% → ceil(scope*pct/100). 4 хоста, batch=25% → eff=1,
// total_batches=4.
func TestVoyagePreview_Command_BarrierPercent(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3", "h4"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]},"batch":"25%"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rep := decodePreviewBody(t, rec.Body.Bytes())
	if rep.ScopeSize != 4 {
		t.Errorf("scope_size = %d, want 4", rep.ScopeSize)
	}
	if rep.EffectiveBatchSize == nil || *rep.EffectiveBatchSize != 1 {
		t.Errorf("effective_batch_size = %v, want 1 (ceil 4*25%%)", rep.EffectiveBatchSize)
	}
	if rep.TotalBatches != 4 {
		t.Errorf("total_batches = %d, want 4", rep.TotalBatches)
	}
}

// window-режим: effective_batch_size опущен (omitempty), total_batches=1,
// batch_mode=window. Без null-мусора в сыром JSON.
func TestVoyagePreview_Command_Window_NoNullJunk(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"h1", "h2", "h3"}}, allowAll())

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]},"batch_mode":"window","concurrency":2}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rep := decodePreviewBody(t, rec.Body.Bytes())
	if rep.BatchMode != "window" || rep.ScopeSize != 3 || rep.TotalBatches != 1 {
		t.Errorf("reply = %+v, want batch_mode=window scope_size=3 total_batches=1", rep)
	}
	if rep.EffectiveBatchSize != nil {
		t.Errorf("effective_batch_size = %v, want отсутствие (window)", *rep.EffectiveBatchSize)
	}
	if strings.Contains(rec.Body.String(), "effective_batch_size") {
		t.Errorf("window-ответ несёт ключ effective_batch_size (должен быть опущен): %s", rec.Body.String())
	}
}

// max_scope превышен → 422 voyage_scope_too_large (как Create), без persist.
func TestVoyagePreview_MaxScopeExceeded_422(t *testing.T) {
	store := &fakeVoyageStore{}
	// maxScope=2; резолвер вернул 3 → cap-reject.
	h := newVoyageHandlerCap(store, &fakeVoyageScenarioResolver{out: []string{"inc-a", "inc-b", "inc-c"}}, &fakeVoyageCommandResolver{}, allowAll(), 2)

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_scope_too_large") {
		t.Errorf("detail не содержит voyage_scope_too_large: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// RBAC-by-kind deny parity: scenario без incarnation.run → 403 (тот же гейт, что
// Create).
func TestVoyagePreview_ScenarioRBACDenied_403(t *testing.T) {
	store := &fakeVoyageStore{}
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"errand.run": true}} // incarnation.run нет
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, enf)

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// Empty resolve → 422 voyage_empty_target (как Create).
func TestVoyagePreview_EmptyTarget_422(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{out: nil}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_empty_target") {
		t.Errorf("detail не содержит voyage_empty_target: %s", rec.Body.String())
	}
}

// scoped command preview parity (ADR-047 S4): явный чужой SID (DeniedExplicit) →
// 403, без persist.
func TestVoyagePreview_CommandScoped_ExplicitForeignSID_403(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{
		SIDs:           []string{"web-01.example.com"},
		DeniedExplicit: []string{"db-01.example.com"},
	}}
	scoper := fakeScoper{covens: []string{"prod"}}
	h := newVoyageHandlerScoped(store, cmd, allowAll(), scoper)

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["web-01.example.com","db-01.example.com"]}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !cmd.calledIn {
		t.Error("ResolveSIDsInScope не вызван — scope не применён")
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// scoped command preview: широкий target урезан до Purview → scope_size =
// подмножество (наследует ResolveSIDsInScope), 200, без persist.
func TestVoyagePreview_CommandScoped_WideTargetTrimmed_200(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := &scopedCommandResolver{scoped: ScopedSIDs{SIDs: []string{"a1.example.com", "a2.example.com"}}}
	scoper := fakeScoper{covens: []string{"coven-a"}}
	h := newVoyageHandlerScoped(store, cmd, allowAll(), scoper)

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview",
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-a","coven-b"]}}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rep := decodePreviewBody(t, rec.Body.Bytes())
	if rep.ScopeSize != 2 {
		t.Errorf("scope_size = %d, want 2 (подмножество A, не A∪B)", rep.ScopeSize)
	}
	if store.insertCalls != 0 || store.insertTargets != 0 {
		t.Errorf("preview персистил: insertCalls=%d insertTargets=%d", store.insertCalls, store.insertTargets)
	}
}

// Malformed JSON → 400 (как Create).
func TestVoyagePreview_BadJSON_400(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandler(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll())

	rec := httptest.NewRecorder()
	h.Preview(rec, voyageReq(http.MethodPost, "/v1/voyages/preview", `{"kind":`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- guard: wire-equivalence миграции на oapi-типы (ADR-051) ---

// toVoyageDTO мигрирован на pointer-optional поля [Voyage] (scenario_name/
// module/batch_mode/on_failure — `*string`/`*enum` вместо `string ...,omitempty`;
// summary.no_match — `*int` вместо `int ...,omitempty`). Guard фиксирует
// байт-в-байт: nil/пустое → ключ ОПУЩЕН (не `null`, не `0`), non-nil → present.
// Ловит регресс, если кто-то заменит alias-конвертер и сломает omitempty-семантику.
func TestToVoyageDTO_PointerOptional_WireEquivalence(t *testing.T) {
	noMatch := 0
	bm := voyage.BatchModeBarrier
	of := voyage.OnFailureContinue
	mod := "core.cmd.shell"

	// command-voyage: scenario_name nil → ключ опущен; module present; batch_mode
	// nil (barrier → NULL) → опущен; on_failure present; no_match=0 → опущен.
	v := &voyage.Voyage{
		VoyageID:       "01J0000000000000000000000A",
		Kind:           voyage.KindCommand,
		Module:         &mod,
		TargetResolved: json.RawMessage(`["host-a"]`),
		Status:         voyage.StatusRunning,
		OnFailure:      &of,
		StartedByAID:   "archon-alice",
		Summary:        &voyage.Summary{Total: 1, Succeeded: 1, NoMatch: noMatch},
	}
	raw, err := json.Marshal(toVoyageDTO(v))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["scenario_name"]; ok {
		t.Errorf("scenario_name присутствует при nil; want опущен (байт-в-байт со старым omitempty)")
	}
	if _, ok := got["batch_mode"]; ok {
		t.Errorf("batch_mode присутствует при nil (barrier→NULL); want опущен")
	}
	if string(got["module"]) != `"core.cmd.shell"` {
		t.Errorf("module = %s, want \"core.cmd.shell\"", got["module"])
	}
	if string(got["on_failure"]) != `"continue"` {
		t.Errorf("on_failure = %s, want \"continue\"", got["on_failure"])
	}
	// summary.no_match=0 → ключ опущен внутри summary.
	var summ map[string]json.RawMessage
	if err := json.Unmarshal(got["summary"], &summ); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if _, ok := summ["no_match"]; ok {
		t.Errorf("summary.no_match присутствует при 0; want опущен")
	}

	// non-zero ветка: scenario_name/batch_mode/no_match присутствуют и несут значение.
	sc := "deploy"
	vv := &voyage.Voyage{
		VoyageID:       "01J0000000000000000000000B",
		Kind:           voyage.KindScenario,
		ScenarioName:   &sc,
		BatchMode:      &bm,
		TargetResolved: json.RawMessage(`["inc-a"]`),
		Status:         voyage.StatusRunning,
		StartedByAID:   "archon-alice",
		Summary:        &voyage.Summary{Total: 2, Succeeded: 1, NoMatch: 3},
	}
	raw2, _ := json.Marshal(toVoyageDTO(vv))
	var got2 map[string]json.RawMessage
	_ = json.Unmarshal(raw2, &got2)
	if string(got2["scenario_name"]) != `"deploy"` {
		t.Errorf("scenario_name = %s, want \"deploy\"", got2["scenario_name"])
	}
	if string(got2["batch_mode"]) != `"barrier"` {
		t.Errorf("batch_mode = %s, want \"barrier\"", got2["batch_mode"])
	}
	var summ2 map[string]json.RawMessage
	_ = json.Unmarshal(got2["summary"], &summ2)
	if string(summ2["no_match"]) != `3` {
		t.Errorf("summary.no_match = %s, want 3", summ2["no_match"])
	}
}

// TestToVoyageDTO_DateTime_NanosecondWire — guard wire-формы date-time полей
// detail/list-ответа Voyage (created_at/started_at/finished_at/schedule_at).
//
// Domain Voyage несёт голый time.Time, toVoyageDTO присваивает его как есть БЕЗ
// .UTC()/Truncate (см. комментарий проектора), поэтому домен Voyage —
// НАНОСЕКУНДНЫЙ (RFC3339Nano): время с ненулевыми наносекундами обязано
// появиться на wire с дробной частью секунд, а исходный таймзон-offset —
// сохраниться (отсутствие .UTC()-нормализации). Тест фиксирует эту форму, чтобы
// Фаза 2 (strict-server) не превратила её молча в секундную (Truncate) — что
// было бы wire-change для уже выпущенного контракта.
func TestToVoyageDTO_DateTime_NanosecondWire(t *testing.T) {
	// Не-UTC зона с фиксированным offset: ловит и наносекунды, и .UTC()-снос.
	zone := time.FixedZone("MSK", 3*60*60)
	created := time.Date(2026, 6, 10, 12, 0, 1, 123456789, zone)
	started := time.Date(2026, 6, 10, 12, 0, 5, 987654321, zone)
	finished := time.Date(2026, 6, 10, 12, 0, 9, 500000000, zone)
	schedule := time.Date(2026, 6, 10, 13, 0, 0, 1, zone)

	mod := "core.cmd.shell"
	v := &voyage.Voyage{
		VoyageID:       "01J0000000000000000000000C",
		Kind:           voyage.KindCommand,
		Module:         &mod,
		TargetResolved: json.RawMessage(`["host-a"]`),
		Status:         voyage.StatusSucceeded,
		StartedByAID:   "archon-alice",
		CreatedAt:      created,
		StartedAt:      &started,
		FinishedAt:     &finished,
		ScheduleAt:     &schedule,
	}
	raw, err := json.Marshal(toVoyageDTO(v))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// RFC3339Nano голого time.Time: дробная часть секунд + сохранённый +03:00.
	wantWire := map[string]string{
		"created_at":  `"2026-06-10T12:00:01.123456789+03:00"`,
		"started_at":  `"2026-06-10T12:00:05.987654321+03:00"`,
		"finished_at": `"2026-06-10T12:00:09.5+03:00"`,
		"schedule_at": `"2026-06-10T13:00:00.000000001+03:00"`,
	}
	for key, want := range wantWire {
		if string(got[key]) != want {
			t.Errorf("%s = %s, want %s (наносекундный wire, БЕЗ .UTC()/Truncate)", key, got[key], want)
		}
	}
}

// TestVoyageTargetEntry_FinishedAt_NanosecondWire — guard wire-формы finished_at
// в drill-ответе GET /v1/voyages/{id}/targets. Handler собирает
// voyageTargetEntryDTO напрямую (присваивая t.FinishedAt как есть), поэтому
// finished_at единицы прогона — тоже НАНОСЕКУНДНЫЙ голый time.Time. Тест строит
// DTO ровно так же, как Targets, и фиксирует наносекундную форму.
func TestVoyageTargetEntry_FinishedAt_NanosecondWire(t *testing.T) {
	zone := time.FixedZone("MSK", 3*60*60)
	finished := time.Date(2026, 6, 10, 12, 0, 9, 250000000, zone)
	applyID := "01J0000000000000000000000D"

	entry := &voyage.VoyageTarget{
		VoyageID:   "01J0000000000000000000000E",
		TargetKind: voyage.TargetKindSID,
		TargetID:   "host-a",
		BatchIndex: 0,
		Status:     voyage.TargetStatusSucceeded,
		ApplyID:    &applyID,
		FinishedAt: &finished,
	}
	dto := voyageTargetEntryDTO{
		TargetKind: string(entry.TargetKind),
		TargetID:   entry.TargetID,
		BatchIndex: entry.BatchIndex,
		Status:     string(entry.Status),
		ApplyID:    entry.ApplyID,
		ErrandID:   entry.ErrandID,
		FinishedAt: entry.FinishedAt,
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if want := `"2026-06-10T12:00:09.25+03:00"`; string(got["finished_at"]) != want {
		t.Errorf("finished_at = %s, want %s (наносекундный wire, БЕЗ .UTC()/Truncate)", got["finished_at"], want)
	}
}
