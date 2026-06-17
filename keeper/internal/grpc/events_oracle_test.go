package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// --- fakes -----------------------------------------------------------------

// oracleFakeDB реализует oracleDB. Маршрутизация по SQL:
//   - Query "FROM decrees"      → набор Decree;
//   - QueryRow "FROM souls"     → covens субъекта;
//   - QueryRow "FROM oracle_fires" → cooldown-state (last fired);
//   - Exec "oracle_fires"       → record fire (фиксируется).
type oracleFakeDB struct {
	decreeRows  func() (pgx.Rows, error)
	soulCoven   []string
	soulErr     error
	lastFired   *time.Time
	recordedFnc func(args []any)

	// circuit-breaker (ADR-030(a), S4). bumpReturns — fire_count, который вернёт
	// BumpCircuit (RETURNING). bumpCalled — фиксирует, что BumpCircuit вызывался
	// (для проверки breaker-off → не зовётся). tripWins — выиграл ли TripDecree
	// (RowsAffected==1, single-winner); false → RowsAffected==0.
	bumpReturns int
	bumpCalled  bool
	tripWins    bool
}

func (f *oracleFakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "oracle_fires") && f.recordedFnc != nil {
		f.recordedFnc(args)
	}
	// TripDecree: UPDATE decrees SET enabled=false … single-winner по RowsAffected.
	if strings.Contains(sql, "UPDATE decrees") {
		if f.tripWins {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *oracleFakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM souls"):
		if f.soulErr != nil {
			return oracleErrRow{err: f.soulErr}
		}
		// Порядок selectBySIDSQL: sid, transport, status, coven, registered_at,
		// last_seen_at, last_seen_by_kid, created_by_aid, requested_at, note.
		return oracleValRow{vals: []any{
			"host-a.example.com", "agent", "connected", f.soulCoven,
			time.Now(), nil, nil, nil, nil, nil,
		}}
	case strings.Contains(sql, "FROM oracle_fires"):
		if f.lastFired == nil {
			return oracleErrRow{err: pgx.ErrNoRows}
		}
		return oracleValRow{vals: []any{*f.lastFired}}
	case strings.Contains(sql, "INTO oracle_circuit"):
		// BumpCircuit RETURNING fire_count.
		f.bumpCalled = true
		return oracleValRow{vals: []any{f.bumpReturns}}
	}
	return oracleErrRow{err: pgx.ErrNoRows}
}

func (f *oracleFakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM decrees") && f.decreeRows != nil {
		return f.decreeRows()
	}
	return &oracleEmptyRows{}, nil
}

type oracleErrRow struct{ err error }

func (r oracleErrRow) Scan(_ ...any) error { return r.err }

type oracleValRow struct{ vals []any }

func (r oracleValRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return errors.New("oracleValRow: len mismatch")
	}
	for i, d := range dest {
		oracleAssign(d, r.vals[i])
	}
	return nil
}

func oracleAssign(dest, src any) {
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *bool:
		*d = src.(bool)
	case *int:
		*d = src.(int)
	case *time.Time:
		*d = src.(time.Time)
	case *[]string:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]string)
		}
	case *[]byte:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	case *json.RawMessage:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case **time.Time:
		if src == nil {
			*d = nil
		} else {
			tm := src.(time.Time)
			*d = &tm
		}
	default:
		panic("oracleValRow.assign: unsupported dest type")
	}
}

// decreeRow строит staticRow в порядке decreeColumns:
// name, on_beacon, where_cel, subject_coven, subject_sid, incarnation_name,
// action_scenario, action_input, cooldown, enabled, created_at, updated_at,
// created_by_aid.
func decreeRow(d *oracle.Decree) []any {
	var whereArg, sidArg, byArg any // nil → SQL NULL в **string-таргете
	if d.WhereCEL != nil {
		whereArg = *d.WhereCEL
	}
	if d.SubjectSID != nil {
		sidArg = *d.SubjectSID
	}
	if d.CreatedByAID != nil {
		byArg = *d.CreatedByAID
	}
	input := d.ActionInput
	if input == nil {
		input = []byte("{}")
	}
	return []any{
		d.Name, d.OnBeacon, whereArg, d.SubjectCoven, sidArg,
		d.IncarnationName, d.ActionScenario, []byte(input), d.Cooldown, d.Enabled,
		time.Now(), time.Now(), byArg,
	}
}

type oracleEmptyRows struct{}

func (r *oracleEmptyRows) Next() bool                                   { return false }
func (r *oracleEmptyRows) Scan(...any) error                            { return nil }
func (r *oracleEmptyRows) Err() error                                   { return nil }
func (r *oracleEmptyRows) Close()                                       {}
func (r *oracleEmptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *oracleEmptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *oracleEmptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *oracleEmptyRows) RawValues() [][]byte                          { return nil }
func (r *oracleEmptyRows) Conn() *pgx.Conn                              { return nil }

// oracleStaticRows — фейковый pgx.Rows над набором staticRow-значений.
type oracleStaticRows struct {
	rows [][]any
	idx  int
}

func (r *oracleStaticRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *oracleStaticRows) Scan(dest ...any) error {
	vals := r.rows[r.idx-1]
	if len(dest) != len(vals) {
		return errors.New("oracleStaticRows: len mismatch")
	}
	for i, d := range dest {
		oracleAssign(d, vals[i])
	}
	return nil
}
func (r *oracleStaticRows) Err() error                                   { return nil }
func (r *oracleStaticRows) Close()                                       {}
func (r *oracleStaticRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *oracleStaticRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *oracleStaticRows) Values() ([]any, error)                       { return nil, nil }
func (r *oracleStaticRows) RawValues() [][]byte                          { return nil }
func (r *oracleStaticRows) Conn() *pgx.Conn                              { return nil }

func decreesRows(ds ...*oracle.Decree) func() (pgx.Rows, error) {
	rows := make([][]any, 0, len(ds))
	for _, d := range ds {
		rows = append(rows, decreeRow(d))
	}
	return func() (pgx.Rows, error) { return &oracleStaticRows{rows: rows}, nil }
}

// fakeEnqueuer — записывает enqueue-вызовы.
type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []EnqueueScenarioRequest
	err   error
}

func (f *fakeEnqueuer) EnqueueScenario(_ context.Context, req EnqueueScenarioRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, req)
	return "apply-" + req.DecreeName, nil
}

func (f *fakeEnqueuer) snapshot() []EnqueueScenarioRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EnqueueScenarioRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// --- helpers ---------------------------------------------------------------

func newOracleHandler(t *testing.T, db oracleDB, enq *fakeEnqueuer, aw audit.Writer) *eventStreamHandler {
	t.Helper()
	h, _ := newOracleHandlerWithMetrics(t, db, enq, aw)
	return h
}

// newOracleHandlerWithMetrics — как newOracleHandler, но дополнительно
// регистрирует keeper_oracle_*-метрики на свежем registry и возвращает его
// gatherer для scrape-проверок (Part 2, ADR-030 S4).
func newOracleHandlerWithMetrics(t *testing.T, db oracleDB, enq *fakeEnqueuer, aw audit.Writer) (*eventStreamHandler, *obs.Registry) {
	t.Helper()
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	reg := obs.NewRegistry()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		Oracle: &OracleDeps{
			DB:          db,
			Where:       where,
			Enqueuer:    enq,
			AuditWriter: aw,
			Metrics:     oracle.RegisterOracleMetrics(reg),
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), reg
}

// newOracleHandlerWithCircuit — handler с включённым circuit-breaker-ом
// (CircuitMaxFires/CircuitWindow заданы, ADR-030(a) S4). maxFires==0 → breaker
// OFF (escape-hatch). Возвращает registry для scrape-проверки circuit_tripped.
func newOracleHandlerWithCircuit(t *testing.T, db oracleDB, enq *fakeEnqueuer, aw audit.Writer, maxFires int, window time.Duration) (*eventStreamHandler, *obs.Registry) {
	t.Helper()
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	reg := obs.NewRegistry()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		Oracle: &OracleDeps{
			DB:              db,
			Where:           where,
			Enqueuer:        enq,
			AuditWriter:     aw,
			Metrics:         oracle.RegisterOracleMetrics(reg),
			CircuitMaxFires: maxFires,
			CircuitWindow:   window,
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), reg
}

func portent(beacon string, data map[string]any) *keeperv1.PortentEvent {
	evt := &keeperv1.PortentEvent{BeaconName: beacon, Sid: "echo-ignored"}
	if data != nil {
		s, _ := structpb.NewStruct(data)
		evt.Data = s
	}
	return evt
}

// --- tests -----------------------------------------------------------------

func TestPortent_MatchEnqueuesScenario(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := &oracleFakeDB{
		soulCoven: []string{"web-app", "web", "prod"},
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			ActionInput: []byte(`{"service":"nginx"}`), Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	calls := enq.snapshot()
	if len(calls) != 1 {
		t.Fatalf("ожидали 1 enqueue, got %d", len(calls))
	}
	if calls[0].ScenarioName != "restart" || calls[0].SubjectSID != "host-a.example.com" {
		t.Errorf("enqueue: %+v", calls[0])
	}
	if calls[0].IncarnationName != "web-app" {
		t.Errorf("incarnation_name не пробросился: %+v", calls[0])
	}
	if calls[0].ActionInput["service"] != "nginx" {
		t.Errorf("action_input не пробросился: %+v", calls[0].ActionInput)
	}
	// audit oracle.fired.
	var fired bool
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			fired = true
		}
	}
	if !fired {
		t.Error("ожидали audit oracle.fired")
	}
}

func TestPortent_NoDecreeDefaultDeny(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{soulCoven: []string{"web"}, decreeRows: decreesRows()}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("default-deny: без Decree ничего не должно ставиться")
	}
}

func TestPortent_SubjectMismatch(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		soulCoven: []string{"db"}, // хост в db, Decree про web
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("subject-mismatch: enqueue не должен произойти")
	}
}

func TestPortent_MembershipMismatch(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	// Субъект В coven `web` (subject-match пройдёт), НО таргет-incarnation Decree-а
	// `other-app` НЕ среди covens хоста → membership-check fail-closed → skip.
	db := &oracleFakeDB{
		soulCoven: []string{"web"},
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "other-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("membership-mismatch: enqueue не должен произойти")
	}
	// fire не пишется и audit oracle.fired не пишется.
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("membership-mismatch: audit oracle.fired НЕ должен писаться")
		}
	}
}

func TestPortent_SIDDecreeEnqueues(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	// sid-Decree: субъект — конкретный SID; членство в incarnation проверяется
	// по covens хоста (incarnation_name `web-app` ∈ covens), как и у coven-Decree.
	db := &oracleFakeDB{
		soulCoven: []string{"web-app"},
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-host", OnBeacon: "svc-down",
			SubjectSID: strptr("host-a.example.com"), IncarnationName: "web-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	calls := enq.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sid-Decree: ожидали 1 enqueue, got %d", len(calls))
	}
	if calls[0].IncarnationName != "web-app" || calls[0].SubjectSID != "host-a.example.com" {
		t.Errorf("sid-Decree enqueue: %+v", calls[0])
	}
	var fired bool
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			fired = true
		}
	}
	if !fired {
		t.Error("sid-Decree: ожидали audit oracle.fired")
	}
}

func TestPortent_WhereCELFilters(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		soulCoven: []string{"web-app", "web"},
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-crit", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			WhereCEL: strptr(`event.data.severity == "critical"`), Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	// severity=info → where false → skip.
	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess",
		portent("svc-down", map[string]any{"severity": "info"}))
	if len(enq.snapshot()) != 0 {
		t.Fatal("where-CEL false должен фильтровать (skip)")
	}

	// severity=critical → where true → enqueue.
	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess",
		portent("svc-down", map[string]any{"severity": "critical"}))
	if len(enq.snapshot()) != 1 {
		t.Fatal("where-CEL true должен пропустить (enqueue)")
	}
}

func TestPortent_CooldownBlocks(t *testing.T) {
	enq := &fakeEnqueuer{}
	recent := time.Now().UTC().Add(-1 * time.Minute) // в окне 5m
	db := &oracleFakeDB{
		soulCoven: []string{"web-app", "web"},
		lastFired: &recent,
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("cooldown в окне должен блокировать повтор")
	}
}

// TestPortent_MetricsEnqueuePath — на полном match-пути инкрементятся
// portents_received / decrees_matched / scenarios_enqueued (Part 2, ADR-030 S4).
func TestPortent_MetricsEnqueuePath(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		soulCoven: []string{"web-app", "web"},
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			Cooldown: "5m", Enabled: true,
		}),
	}
	h, reg := newOracleHandlerWithMetrics(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	body := obstest.Scrape(t, reg.Gatherer())
	for _, want := range []string{
		"keeper_oracle_portents_received_total 1",
		"keeper_oracle_decrees_matched_total 1",
		"keeper_oracle_scenarios_enqueued_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; got=\n%s", want, body)
		}
	}
}

// TestPortent_MetricsCooldownPath — cooldown в окне инкрементит
// portents_received + cooldown_blocked, но НЕ decrees_matched/scenarios_enqueued.
func TestPortent_MetricsCooldownPath(t *testing.T) {
	enq := &fakeEnqueuer{}
	recent := time.Now().UTC().Add(-1 * time.Minute)
	db := &oracleFakeDB{
		soulCoven: []string{"web-app", "web"},
		lastFired: &recent,
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h, reg := newOracleHandlerWithMetrics(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	body := obstest.Scrape(t, reg.Gatherer())
	for _, want := range []string{
		"keeper_oracle_portents_received_total 1",
		"keeper_oracle_cooldown_blocked_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; got=\n%s", want, body)
		}
	}
	if strings.Contains(body, "keeper_oracle_scenarios_enqueued_total 1") {
		t.Errorf("cooldown-путь не должен инкрементить scenarios_enqueued; got=\n%s", body)
	}
}

// TestPortent_MetricsDefaultDeny — без Decree инкрементится только
// portents_received (default-deny на decrees_matched и далее).
func TestPortent_MetricsDefaultDeny(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{soulCoven: []string{"web"}, decreeRows: decreesRows()}
	h, reg := newOracleHandlerWithMetrics(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_oracle_portents_received_total 1") {
		t.Errorf("missing portents_received; got=\n%s", body)
	}
	if strings.Contains(body, "keeper_oracle_decrees_matched_total 1") {
		t.Errorf("default-deny не должен инкрементить decrees_matched; got=\n%s", body)
	}
}

// circuitDB — общий builder fake-DB под circuit-breaker-тесты: один enabled
// coven-Decree на svc-down, заданный bumpReturns/tripWins.
func circuitDB(bumpReturns int, tripWins bool) *oracleFakeDB {
	return &oracleFakeDB{
		soulCoven:   []string{"web-app", "web"},
		bumpReturns: bumpReturns,
		tripWins:    tripWins,
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			Cooldown: "5m", Enabled: true,
		}),
	}
}

// TestPortent_CircuitTripsAtThreshold — cnt >= max_fires и TripDecree выиграл →
// circuit_tripped-метрика + audit decree.circuit_tripped + Decree авто-disable.
func TestPortent_CircuitTripsAtThreshold(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(5, true) // bump вернёт 5 == max, trip single-winner выигрывает
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 5, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if !db.bumpCalled {
		t.Error("breaker включён → BumpCircuit должен вызываться")
	}
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("ожидали circuit_tripped 1; got=\n%s", body)
	}
	var tripped bool
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventDecreeCircuitTripped {
			tripped = true
			if e.Payload["trigger"] != "circuit_breaker" {
				t.Errorf("trigger payload: %v", e.Payload["trigger"])
			}
			if e.Payload["fire_count"] != 5 {
				t.Errorf("fire_count payload: %v", e.Payload["fire_count"])
			}
			if e.Source != audit.SourceSoulGRPC {
				t.Errorf("source: %v, want soul_grpc", e.Source)
			}
		}
	}
	if !tripped {
		t.Error("ожидали audit decree.circuit_tripped")
	}
}

// TestPortent_CircuitBelowThreshold — cnt < max_fires → BumpCircuit вызван, но
// trip не происходит (нет метрики/audit-а circuit_tripped).
func TestPortent_CircuitBelowThreshold(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(3, true) // 3 < 5; tripWins не важен — TripDecree не зовётся
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 5, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if !db.bumpCalled {
		t.Error("breaker включён → BumpCircuit должен вызываться даже ниже порога")
	}
	body := obstest.Scrape(t, reg.Gatherer())
	if strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("ниже порога trip не должен срабатывать; got=\n%s", body)
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventDecreeCircuitTripped {
			t.Error("ниже порога audit decree.circuit_tripped НЕ должен писаться")
		}
	}
}

// TestPortent_CircuitOffSkipsBump — max_fires==0 (escape-hatch) → BumpCircuit
// вообще не вызывается, trip невозможен.
func TestPortent_CircuitOffSkipsBump(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(99, true) // вернул бы много, но breaker OFF
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 0, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if db.bumpCalled {
		t.Error("breaker OFF (max_fires==0) → BumpCircuit вызываться НЕ должен")
	}
	body := obstest.Scrape(t, reg.Gatherer())
	if strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("breaker OFF → trip невозможен; got=\n%s", body)
	}
}

// TestPortent_CircuitTripLoserNoDuplicate — cnt >= max, но TripDecree проиграл
// (RowsAffected==0, другой инстанс выиграл) → НЕ дублируем metric/audit.
func TestPortent_CircuitTripLoserNoDuplicate(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(7, false) // 7 >= 5, но trip проиграл гонку
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 5, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	body := obstest.Scrape(t, reg.Gatherer())
	if strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("trip-проигравший не должен инкрементить circuit_tripped; got=\n%s", body)
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventDecreeCircuitTripped {
			t.Error("trip-проигравший не должен писать audit decree.circuit_tripped")
		}
	}
}

// TestPortent_EnqueueFailNoFireNoAudit — сбой EnqueueScenario (Enqueuer.err) на
// прошедшем фильтр Decree: fire (cooldown-запись) НЕ пишется и audit oracle.fired
// НЕ пишется (qa coverage_gap #2: иначе ложный cooldown заблокировал бы будущие
// реальные реакции). decrees_matched ИНКРЕМЕНТИТСЯ (match фиксируется ДО enqueue
// — учитываем попытку), но scenarios_enqueued — нет.
func TestPortent_EnqueueFailNoFireNoAudit(t *testing.T) {
	enq := &fakeEnqueuer{err: errors.New("enqueue: incarnation not found")}
	aw := &recordingAudit{}
	var fireRecorded bool
	db := &oracleFakeDB{
		soulCoven: []string{"web-app", "web"},
		// recordedFnc сигналит запись в oracle_fires (RecordFire). На fail-пути
		// её быть не должно.
		recordedFnc: func([]any) { fireRecorded = true },
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			Cooldown: "5m", Enabled: true,
		}),
	}
	h, reg := newOracleHandlerWithMetrics(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if fireRecorded {
		t.Error("enqueue-fail: RecordFire (cooldown) НЕ должен писаться — иначе ложный cooldown")
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("enqueue-fail: audit oracle.fired НЕ должен писаться")
		}
	}
	body := obstest.Scrape(t, reg.Gatherer())
	// match зафиксирован ДО enqueue (учёт попытки), enqueued — нет.
	if !strings.Contains(body, "keeper_oracle_decrees_matched_total 1") {
		t.Errorf("match должен фиксироваться до enqueue; got=\n%s", body)
	}
	if strings.Contains(body, "keeper_oracle_scenarios_enqueued_total 1") {
		t.Errorf("enqueue-fail не должен инкрементить scenarios_enqueued; got=\n%s", body)
	}
}

// TestPortent_AuditPayloadExcludesEventData — регрессия-guard (qa coverage_gap
// #4): audit oracle.fired кладёт ровно {sid,beacon,decree,scenario,apply_id} и НЕ
// протекает event.data (недоверенный payload Soul-события). Подаём event.data с
// явным маркером и проверяем, что он не появился ни в одном значении payload-а.
func TestPortent_AuditPayloadExcludesEventData(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := &oracleFakeDB{
		soulCoven: []string{"web-app", "web"},
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	const leak = "SECRET-EVENT-DATA-MARKER"
	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess",
		portent("svc-down", map[string]any{"secret_field": leak, "severity": leak}))

	var fired *audit.Event
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			fired = e
		}
	}
	if fired == nil {
		t.Fatal("ожидали audit oracle.fired")
	}
	// Ровно ожидаемый набор ключей — ни больше, ни меньше.
	wantKeys := map[string]bool{"sid": true, "beacon": true, "decree": true, "scenario": true, "apply_id": true}
	if len(fired.Payload) != len(wantKeys) {
		t.Errorf("payload keys = %v, want exactly %v", keysOf(fired.Payload), keysOf(map[string]any{"sid": 0, "beacon": 0, "decree": 0, "scenario": 0, "apply_id": 0}))
	}
	for k := range fired.Payload {
		if !wantKeys[k] {
			t.Errorf("неожиданный ключ в payload: %q", k)
		}
	}
	// Ни одно значение payload-а не несёт маркер event.data.
	for k, v := range fired.Payload {
		if s, ok := v.(string); ok && strings.Contains(s, leak) {
			t.Errorf("event.data протёк в payload[%q] = %q", k, s)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPortent_SIDDecreeMembershipMismatch — sid-Decree, хост совпал по SID, НО
// таргет-incarnation Decree-а НЕ среди covens хоста → membership-барьер
// fail-closed → skip (qa coverage_gap #5: второй барьер membership работает не
// только для coven-Decree, но и для sid-Decree). enqueue/fire/audit НЕ пишутся.
func TestPortent_SIDDecreeMembershipMismatch(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	var fireRecorded bool
	// SID хоста совпадает с subject_sid Decree-а (subject-match пройдёт), но
	// incarnation `other-app` НЕ среди covens хоста (`web-app`) → membership fail.
	db := &oracleFakeDB{
		soulCoven:   []string{"web-app"},
		recordedFnc: func([]any) { fireRecorded = true },
		decreeRows: decreesRows(&oracle.Decree{
			Name: "restart-host", OnBeacon: "svc-down",
			SubjectSID: strptr("host-a.example.com"), IncarnationName: "other-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("sid-Decree membership-mismatch: enqueue не должен произойти")
	}
	if fireRecorded {
		t.Error("sid-Decree membership-mismatch: RecordFire НЕ должен писаться")
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("sid-Decree membership-mismatch: audit oracle.fired НЕ должен писаться")
		}
	}
}

func strptr(s string) *string { return &s }

func TestActiveVigilsForSID_ConvertsToVigilDef(t *testing.T) {
	db := &oracleVigilDB{
		soulCoven: []string{"web"},
		vigils: [][]any{
			vigilRow("web-watch", []string{"web"}, "30s", "core.beacon.service_down"),
		},
	}
	src := NewVigilSource(db)
	defs, err := src.ActiveVigilsForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ActiveVigilsForSID: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("ожидали 1 VigilDef, got %d", len(defs))
	}
	if defs[0].GetName() != "web-watch" || defs[0].GetInterval() != "30s" || defs[0].GetCheck() != "core.beacon.service_down" {
		t.Errorf("VigilDef mapping: %+v", defs[0])
	}
}

// oracleVigilDB — fake для VigilSource: souls (covens) + vigils (набор).
type oracleVigilDB struct {
	soulCoven []string
	vigils    [][]any
}

func (f *oracleVigilDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *oracleVigilDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "FROM souls") {
		return oracleValRow{vals: []any{
			"host-a.example.com", "agent", "connected", f.soulCoven,
			time.Now(), nil, nil, nil, nil, nil,
		}}
	}
	return oracleErrRow{err: pgx.ErrNoRows}
}
func (f *oracleVigilDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM vigils") {
		return &oracleStaticRows{rows: f.vigils}, nil
	}
	return &oracleEmptyRows{}, nil
}

// vigilRow в порядке vigilColumns:
// name, coven, sid, interval_spec, check_addr, params, enabled, created_at,
// updated_at, created_by_aid.
func vigilRow(name string, coven []string, interval, check string) []any {
	return []any{
		name, coven, nil, interval, check,
		[]byte("{}"), true, time.Now(), time.Now(), nil,
	}
}
