package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// augurFakeDB — реализует augurDB (augur.ExecQueryRower + soul.ExecQueryRower).
// Маршрутизация по SQL: SELECT ... FROM omens → omen-строка; FROM souls →
// soul-строка (covens); FROM rites → rite-набор.
type augurFakeDB struct {
	omenRow   func() pgx.Row // SelectOmenByName
	soulRow   func() pgx.Row // SelectBySID (covens)
	riteRows  func() (pgx.Rows, error)
	queryRows int
}

func (f *augurFakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("augurFakeDB: Exec not used")
}

func (f *augurFakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM omens"):
		if f.omenRow != nil {
			return f.omenRow()
		}
	case strings.Contains(sql, "FROM souls"):
		if f.soulRow != nil {
			return f.soulRow()
		}
	}
	return augurErrRow{err: pgx.ErrNoRows}
}

func (f *augurFakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.queryRows++
	if strings.Contains(sql, "FROM rites") && f.riteRows != nil {
		return f.riteRows()
	}
	return &augurEmptyRows{}, nil
}

type augurErrRow struct{ err error }

func (r augurErrRow) Scan(_ ...any) error { return r.err }

// augurOmenRowVals — значения в порядке omenColumns:
// name, source_type, endpoint, auth_ref, created_by_aid, created_at.
type augurValRow struct{ vals []any }

func (r augurValRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return errors.New("augurValRow: len mismatch")
	}
	for i, d := range dest {
		augurAssign(d, r.vals[i])
	}
	return nil
}

func augurAssign(dest, src any) {
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *int64:
		*d = src.(int64)
	case *bool:
		*d = src.(bool)
	case *time.Time:
		*d = src.(time.Time)
	case *[]byte:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	case *[]string:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]string)
		}
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case **int:
		if src == nil {
			*d = nil
		} else {
			n := src.(int)
			*d = &n
		}
	case **time.Time:
		if src == nil {
			*d = nil
		} else {
			tm := src.(time.Time)
			*d = &tm
		}
	default:
		panic("augurAssign: unsupported dest")
	}
}

type augurEmptyRows struct{}

func (r *augurEmptyRows) Next() bool                                   { return false }
func (r *augurEmptyRows) Scan(...any) error                            { return nil }
func (r *augurEmptyRows) Err() error                                   { return nil }
func (r *augurEmptyRows) Close()                                       {}
func (r *augurEmptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *augurEmptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *augurEmptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *augurEmptyRows) RawValues() [][]byte                          { return nil }
func (r *augurEmptyRows) Conn() *pgx.Conn                              { return nil }

// augurRiteRows — отдаёт набор rite-строк в порядке riteColumns:
// id, omen, coven, sid, allow, delegate, token_ttl, token_num_uses,
// created_by_aid, created_at.
type augurRiteRows struct {
	rows [][]any
	idx  int
}

func (r *augurRiteRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}
func (r *augurRiteRows) Scan(dest ...any) error {
	return augurValRow{vals: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *augurRiteRows) Err() error                                   { return nil }
func (r *augurRiteRows) Close()                                       {}
func (r *augurRiteRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *augurRiteRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *augurRiteRows) Values() ([]any, error)                       { return nil, nil }
func (r *augurRiteRows) RawValues() [][]byte                          { return nil }
func (r *augurRiteRows) Conn() *pgx.Conn                              { return nil }

var augurTestNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func augurAllowPaths(paths ...string) []byte {
	b, _ := json.Marshal(map[string][]string{"paths": paths})
	return b
}

// augurOmenRowVault — omen-строка vault-типа. nil = NULL-колонка.
func augurOmenRowVault(name string) pgx.Row {
	return augurValRow{vals: []any{name, "vault", "https://vault:8200", "vault:secret/keeper/augur/" + name, nil, augurTestNow}}
}

// augurSoulRow — soul-строка с заданными covens (11 колонок selectBySIDSQL:
// sid, transport, status, coven, traits, registered_at, last_seen_at,
// last_seen_by_kid, created_by_aid, requested_at, note). nil = NULL-колонка
// (traits NULL → пустой map в scanSoul, ADR-060).
func augurSoulRow(sid string, covens []string) pgx.Row {
	return augurValRow{vals: []any{
		sid, "agent", "connected", covens, nil, augurTestNow,
		nil, nil, nil, nil, nil,
	}}
}

// augurRiteRow — rite-строка (coven-субъект) с allow.paths. nil = NULL-колонка.
func augurRiteRow(id int, omen, coven string, paths ...string) []any {
	return []any{
		int64(id), omen, coven, nil, augurAllowPaths(paths...),
		false, nil, nil, nil, augurTestNow,
	}
}

// stubKV — fake augur.KVReader.
type stubKV struct {
	data    map[string]any
	err     error
	gotPath string
}

func (s *stubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	s.gotPath = path
	if s.err != nil {
		return nil, s.err
	}
	return s.data, nil
}

// newAugurHandler собирает handler с Augur-deps + StreamManager + Outbound и
// возвращает outCh, из которого читается отправленный AugurReply (тот же стрим).
func newAugurHandler(t *testing.T, db augurDB, kv augur.KVReader, aw audit.Writer, sid string) (*eventStreamHandler, <-chan *keeperv1.FromKeeper) {
	t.Helper()
	mgr := NewStreamManager(discardLogger(t))
	outCh := mgr.Register(sid)
	out, err := NewOutbound(OutboundDeps{Manager: mgr, AuditWriter: aw, Logger: discardLogger(t)})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		Manager:     mgr,
		Augur: &AugurDeps{
			DB:          db,
			Vault:       kv,
			Egress:      stubDoer{},
			AuditWriter: aw,
			Outbound:    out,
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), outCh
}

// stubDoer — заглушка augur.HTTPDoer для vault-round-trip-тестов (prom/elk не
// дёргаются). prom/elk-специфичные тесты передают собственный doer через
// newAugurHandlerEgress.
type stubDoer struct {
	resp func() (*http.Response, error)
}

func (d stubDoer) Do(*http.Request) (*http.Response, error) {
	if d.resp != nil {
		return d.resp()
	}
	return nil, errors.New("stubDoer: not configured")
}

// newAugurHandlerEgress — как newAugurHandler, но с явным egress-doer и опц.
// лимитом параллелизма (0 → default). Для prom/elk и семафор-тестов.
func newAugurHandlerEgress(t *testing.T, db augurDB, kv augur.KVReader, doer augur.HTTPDoer, aw audit.Writer, sid string, concurrency int) (*eventStreamHandler, <-chan *keeperv1.FromKeeper) {
	t.Helper()
	mgr := NewStreamManager(discardLogger(t))
	outCh := mgr.Register(sid)
	out, err := NewOutbound(OutboundDeps{Manager: mgr, AuditWriter: aw, Logger: discardLogger(t)})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	deps := EventStreamDeps{
		SeedDB:           &fakeSeedDB{},
		AuditWriter:      aw,
		KID:              "kid-test",
		Manager:          mgr,
		AugurConcurrency: concurrency,
		Augur: &AugurDeps{
			DB:          db,
			Vault:       kv,
			Egress:      doer,
			AuditWriter: aw,
			Outbound:    out,
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), outCh
}

func recvReply(t *testing.T, outCh <-chan *keeperv1.FromKeeper) *keeperv1.AugurReply {
	t.Helper()
	select {
	case msg := <-outCh:
		reply := msg.GetAugurReply()
		if reply == nil {
			t.Fatalf("FromKeeper is not AugurReply: %T", msg.GetPayload())
		}
		return reply
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AugurReply")
		return nil
	}
}

func TestAugur_RoundTrip_OK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRow(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"username": "svc", "password": "s3cr3t"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-1", ApplyId: "apply-1", OmenName: "vault-prod", Query: "secret/keeper/db",
	})

	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK (error=%q)", reply.GetStatus(), reply.GetError())
	}
	if reply.GetRequestId() != "req-1" {
		t.Errorf("request_id echo = %q, want req-1", reply.GetRequestId())
	}
	inline := reply.GetInlineData()
	if inline == nil {
		t.Fatalf("inline_data nil")
	}
	if inline.GetFields()["username"].GetStringValue() != "svc" {
		t.Errorf("inline username missing")
	}
	if kv.gotPath != "secret/keeper/db" {
		t.Errorf("ReadKV path = %q, want secret/keeper/db", kv.gotPath)
	}

	// audit: augur.fetch_brokered, без значения секрета.
	evs := aw.snapshot()
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.EventType != audit.EventAugurFetchBrokered {
		t.Errorf("event_type = %q, want augur.fetch_brokered", ev.EventType)
	}
	if ev.Source != audit.SourceSoulGRPC {
		t.Errorf("source = %q, want soul_grpc", ev.Source)
	}
	if ev.CorrelationID != "apply-1" {
		t.Errorf("correlation_id = %q, want apply-1", ev.CorrelationID)
	}
	assertNoSecretInPayload(t, ev.Payload)
	if ev.Payload["omen"] != "vault-prod" || ev.Payload["query"] != "secret/keeper/db" {
		t.Errorf("payload omen/query missing: %v", ev.Payload)
	}
}

func TestAugur_RoundTrip_Denied_NoRite(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: nil}, nil // нет Rite-ов
		},
	}
	kv := &stubKV{}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-2", ApplyId: "apply-2", OmenName: "vault-prod", Query: "secret/keeper/db",
	})

	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED", reply.GetStatus())
	}
	if reply.GetInlineData() != nil {
		t.Errorf("denied reply must not carry inline_data")
	}
	if kv.gotPath != "" {
		t.Errorf("ReadKV must NOT be called on denied, got path %q", kv.gotPath)
	}
	evs := aw.snapshot()
	if len(evs) != 1 || evs[0].EventType != audit.EventAugurAccessDenied {
		t.Fatalf("expected one augur.access_denied event, got %+v", evs)
	}
}

func TestAugur_Denied_QueryNotInAllow(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRow(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"x": "y"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-3", OmenName: "vault-prod", Query: "secret/keeper/other",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED", reply.GetStatus())
	}
	if kv.gotPath != "" {
		t.Errorf("ReadKV must not be called when query not in allow")
	}
}

// TestAugur_SIDFromMTLS — handler берёт SID из аргумента (mTLS peer cert),
// а covens резолвятся по ЭТОМУ SID, игнорируя любой sid внутри AugurRequest
// (его в proto и нет). Проверяем: covens-резолв идёт по authoritative SID.
func TestAugur_SIDFromMTLS(t *testing.T) {
	const authoritativeSID = "host.example.com"
	var soulQueriedSID string
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row {
			soulQueriedSID = authoritativeSID // fake возвращает covens только для него
			return augurSoulRow(authoritativeSID, []string{"prod"})
		},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRow(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"k": "v"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, authoritativeSID)

	h.processAugurRequest(context.Background(), authoritativeSID, "sess", &keeperv1.AugurRequest{
		RequestId: "req-4", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK", reply.GetStatus())
	}
	if soulQueriedSID != authoritativeSID {
		t.Errorf("covens resolved for %q, want authoritative %q", soulQueriedSID, authoritativeSID)
	}
	// audit фиксирует authoritative SID.
	if aw.snapshot()[0].Payload["sid"] != authoritativeSID {
		t.Errorf("audit sid = %v, want %q", aw.snapshot()[0].Payload["sid"], authoritativeSID)
	}
}

func TestAugur_VaultReadFail_Error(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRow(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{err: errors.New("vault down")}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-5", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_ERROR {
		t.Fatalf("status = %v, want ERROR", reply.GetStatus())
	}
	// На ERROR (доступ разрешён, но fetch не состоялся) audit-события нет.
	if len(aw.snapshot()) != 0 {
		t.Errorf("expected no audit on fetch error, got %d", len(aw.snapshot()))
	}
}

// TestAugur_GoroutinePath — handleAugurRequest запускает обработку в горутине;
// проверяем, что reply всё равно приходит (полный путь dispatch → goroutine).
func TestAugur_GoroutinePath(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRow(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"k": "v"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-6", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK", reply.GetStatus())
	}
}

func TestAugur_NilDeps_NoPanic(t *testing.T) {
	deps := EventStreamDeps{SeedDB: &fakeSeedDB{}, AuditWriter: &recordingAudit{}, KID: "kid"}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	h := newEventStreamHandler(deps, discardLogger(t))
	// Augur=nil → warn + no-op, без паники.
	h.handleAugurRequest(context.Background(), "host", "sess", &keeperv1.AugurRequest{OmenName: "x"})
}

// assertNoSecretInPayload — payload не содержит значений секрета (s3cr3t / svc).
func assertNoSecretInPayload(t *testing.T, payload map[string]any) {
	t.Helper()
	b, _ := json.Marshal(payload)
	for _, leak := range []string{"s3cr3t", "svc", "password", "username"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("audit payload leaks secret material %q: %s", leak, b)
		}
	}
}

// --- prom / elk round-trip + семафор ----------------------------------

func augurOmenRowProm(name string) pgx.Row {
	return augurValRow{vals: []any{name, "prometheus", "https://prom.example.com:9090", "vault:secret/keeper/" + name, nil, augurTestNow}}
}

func augurOmenRowELK(name string) pgx.Row {
	return augurValRow{vals: []any{name, "elk", "https://elk.example.com:9200", "vault:secret/keeper/" + name, nil, augurTestNow}}
}

func augurRiteRowQueries(id int, omen, coven string, queries ...string) []any {
	b, _ := json.Marshal(map[string][]string{"queries": queries})
	return []any{int64(id), omen, coven, nil, b, false, nil, nil, nil, augurTestNow}
}

func augurRiteRowIndices(id int, omen, coven string, indices ...string) []any {
	b, _ := json.Marshal(map[string][]string{"indices": indices})
	return []any{int64(id), omen, coven, nil, b, false, nil, nil, nil, augurTestNow}
}

func jsonRespDoer(body string) augur.HTTPDoer {
	return stubDoer{resp: func() (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}}
}

func TestAugur_Prometheus_RoundTrip_OK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowProm("prom-main") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"token": "tkn"}}
	doer := jsonRespDoer(`{"status":"success","data":{"result":[]}}`)
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, kv, doer, aw, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "p-1", ApplyId: "apply-p", OmenName: "prom-main", Query: "up",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK (error=%q)", reply.GetStatus(), reply.GetError())
	}
	if reply.GetInlineData().GetFields()["status"].GetStringValue() != "success" {
		t.Errorf("inline_data not carried: %v", reply.GetInlineData().AsMap())
	}
	evs := aw.snapshot()
	if len(evs) != 1 || evs[0].EventType != audit.EventAugurFetchBrokered {
		t.Fatalf("expected augur.fetch_brokered, got %+v", evs)
	}
}

func TestAugur_Prometheus_Denied_QueryNotInAllow(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowProm("prom-main") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	doer := jsonRespDoer(`{}`)
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, &stubKV{}, doer, aw, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "p-2", OmenName: "prom-main", Query: "node_load1",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED", reply.GetStatus())
	}
}

func TestAugur_ELK_RoundTrip_OK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowELK("elk-logs") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowIndices(1, "elk-logs", "prod", "logs-app")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"api_key": "ak"}}
	doer := jsonRespDoer(`{"took":1,"hits":{"hits":[]}}`)
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, kv, doer, aw, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "e-1", ApplyId: "apply-e", OmenName: "elk-logs", Query: "logs-app",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK (error=%q)", reply.GetStatus(), reply.GetError())
	}
	if int(reply.GetInlineData().GetFields()["took"].GetNumberValue()) != 1 {
		t.Errorf("inline_data not carried: %v", reply.GetInlineData().AsMap())
	}
}

// TestAugur_Semaphore_Overflow — при заполненном семафоре новый AugurRequest
// получает ERROR без спавна обработки. Лимит=1, первый запрос блокируется в
// fetch-е (doer ждёт сигнал) → второй упирается в полный семафор.
func TestAugur_Semaphore_Overflow(t *testing.T) {
	const sid = "host.example.com"
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var once sync.Once
	blockingDoer := stubDoer{resp: func() (*http.Response, error) {
		once.Do(func() { started <- struct{}{} })
		<-release // держим слот занятым
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	}}
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowProm("prom-main") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, &stubKV{data: map[string]any{}}, blockingDoer, aw, sid, 1)

	// Первый запрос — займёт единственный слот и зависнет в fetch-е.
	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "s-1", OmenName: "prom-main", Query: "up",
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("первый запрос не дошёл до fetch (семафор не занят)")
	}

	// Второй — семафор полон → немедленный ERROR без спавна.
	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "s-2", OmenName: "prom-main", Query: "up",
	})
	reply := recvReply(t, outCh)
	if reply.GetRequestId() != "s-2" {
		t.Fatalf("ожидался reply на s-2 (overflow), got %q", reply.GetRequestId())
	}
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_ERROR {
		t.Fatalf("overflow status = %v, want ERROR", reply.GetStatus())
	}
	if !strings.Contains(reply.GetError(), "concurrency") && !strings.Contains(reply.GetError(), "busy") {
		t.Errorf("overflow error = %q, want concurrency/busy", reply.GetError())
	}

	// Отпускаем первый — он должен завершиться OK, слот освободиться.
	close(release)
	r1 := recvReply(t, outCh)
	if r1.GetRequestId() != "s-1" || r1.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("первый запрос: id=%q status=%v, want s-1/OK", r1.GetRequestId(), r1.GetStatus())
	}
}

// --- keeper_augur_* metrics wire-up ----------------------------------

// newAugurHandlerWithMetrics — как newAugurHandler, но с зарегистрированным
// keeper_augur_*-дескриптором в AugurDeps.Metrics; возвращает Registry для
// скрейпа. concurrency=0 → default.
func newAugurHandlerWithMetrics(t *testing.T, db augurDB, kv augur.KVReader, doer augur.HTTPDoer, aw audit.Writer, sid string, concurrency int) (*eventStreamHandler, <-chan *keeperv1.FromKeeper, *obs.Registry) {
	t.Helper()
	mgr := NewStreamManager(discardLogger(t))
	outCh := mgr.Register(sid)
	out, err := NewOutbound(OutboundDeps{Manager: mgr, AuditWriter: aw, Logger: discardLogger(t)})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	reg := obs.NewRegistry()
	deps := EventStreamDeps{
		SeedDB:           &fakeSeedDB{},
		AuditWriter:      aw,
		KID:              "kid-test",
		Manager:          mgr,
		AugurConcurrency: concurrency,
		Augur: &AugurDeps{
			DB:          db,
			Vault:       kv,
			Egress:      doer,
			AuditWriter: aw,
			Outbound:    out,
			Metrics:     augur.RegisterBrokerMetrics(reg),
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), outCh, reg
}

// TestAugurMetrics_FetchOK — успешный брокер инкрементирует
// fetch_total{source=vault,decision=ok}.
func TestAugurMetrics_FetchOK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRow(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"k": "v"}}
	h, outCh, reg := newAugurHandlerWithMetrics(t, db, kv, stubDoer{}, &recordingAudit{}, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "m-1", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	if recvReply(t, outCh).GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatal("expected OK reply")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_augur_fetch_total{decision="ok",source="vault"} 1`) {
		t.Errorf("fetch_total ok/vault mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_augur_fetch_duration_seconds_count{source="vault"} 1`) {
		t.Errorf("fetch_duration vault count mismatch; got=\n%s", body)
	}
	// Секрет/omen/query не должны утечь в метрики.
	for _, leak := range []string{"omen=", "query=", "sid=", "secret/keeper/db", "vault-prod"} {
		if strings.Contains(body, leak) {
			t.Errorf("augur metrics leak %q; got=\n%s", leak, body)
		}
	}
}

// TestAugurMetrics_Denied — denied резолв инкрементирует
// fetch_total{decision=denied}.
func TestAugurMetrics_Denied(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowVault("vault-prod") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: nil}, nil // нет Rite-ов → denied
		},
	}
	h, outCh, reg := newAugurHandlerWithMetrics(t, db, &stubKV{}, stubDoer{}, &recordingAudit{}, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "m-2", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	if recvReply(t, outCh).GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatal("expected DENIED reply")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `decision="denied"`) {
		t.Errorf("fetch_total denied missing; got=\n%s", body)
	}
}

// TestAugurMetrics_SemaphoreOverflow — отбой по concurrency-limit учитывается как
// fetch_total{source=unknown,decision=error}.
func TestAugurMetrics_SemaphoreOverflow(t *testing.T) {
	const sid = "host.example.com"
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var once sync.Once
	blockingDoer := stubDoer{resp: func() (*http.Response, error) {
		once.Do(func() { started <- struct{}{} })
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	}}
	db := &augurFakeDB{
		omenRow: func() pgx.Row { return augurOmenRowProm("prom-main") },
		soulRow: func() pgx.Row { return augurSoulRow(sid, []string{"prod"}) },
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	h, outCh, reg := newAugurHandlerWithMetrics(t, db, &stubKV{data: map[string]any{}}, blockingDoer, &recordingAudit{}, sid, 1)

	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "ms-1", OmenName: "prom-main", Query: "up",
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("первый запрос не дошёл до fetch")
	}
	// Второй — отбой семафором.
	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "ms-2", OmenName: "prom-main", Query: "up",
	})
	if recvReply(t, outCh).GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_ERROR {
		t.Fatal("expected ERROR on overflow")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_augur_fetch_total{decision="error",source="unknown"} 1`) {
		t.Errorf("overflow error/unknown count mismatch; got=\n%s", body)
	}

	close(release)
	_ = recvReply(t, outCh) // дренаж завершения первого запроса
}
