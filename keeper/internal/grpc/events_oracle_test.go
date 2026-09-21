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
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// --- fakes -----------------------------------------------------------------

// oracleFakeDB implements oracleDB. SQL routing:
//   - Query "FROM decrees"      → set of Decree;
//   - Query "FROM souls"        → the subject facts ([subject.LoadHost]): the
//     host's own labels plus one row per incarnation it belongs to;
//   - QueryRow "incarnation_membership" → the membership EXISTS gate;
//   - QueryRow "FROM oracle_fires" → cooldown-state (last fired);
//   - Exec "oracle_fires"       → record fire.
//
// The label read and the membership read stay separate facts, as they are in
// the product since NIM-124: a fake that conflated them could not tell a member
// from a host merely tagged with an incarnation's name — the case the Oracle's
// guard exists to refuse. What NIM-280 added is that the SAME load also returns
// the labels an operator put on the INCARNATION, which is how a rule reaches its
// members without any label being inherited.
type oracleFakeDB struct {
	decreeRows func() (pgx.Rows, error)
	// soulCoven — labels attached to THIS host (souls.coven[]). The only place
	// a coven comes from (NIM-281).
	soulCoven []string
	// soulTraits — the host's own traits JSONB (nil → no traits).
	soulTraits []byte
	// memberOf — incarnations the host is bound to (incarnation_membership).
	// Drives BOTH the membership gate and the subject's incarnation dimension.
	memberOf []string
	// memberService — the service half of every membership above (an incarnation
	// name is unique only within its service). Defaults to "svc".
	memberService string
	// incCoven / incTraits — labels an operator attached to the INCARNATIONS the
	// host belongs to; they reach the host as a subject without being copied onto it.
	incCoven  []string
	incTraits []byte
	// hostUnknown — the SID is not in the souls registry (onboarding incomplete).
	hostUnknown bool

	soulErr     error
	lastFired   *time.Time
	recordedFnc func(args []any)

	// circuit-breaker (ADR-030(a), S4). bumpReturns — the fire_count that
	// BumpCircuit (RETURNING) will return. bumpCalled — records that BumpCircuit
	// was called (to verify breaker-off → not called). tripWins — whether
	// TripDecree won (RowsAffected==1, single-winner); false → RowsAffected==0.
	bumpReturns int
	bumpCalled  bool
	tripWins    bool
}

func (f *oracleFakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "oracle_fires") && f.recordedFnc != nil {
		f.recordedFnc(args)
	}
	// TripDecree: UPDATE decrees SET enabled=false … single-winner by RowsAffected.
	if strings.Contains(sql, "UPDATE decrees") {
		if f.tripWins {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *oracleFakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "incarnation_membership"):
		// isMemberSQL: EXISTS(… incarnation_name = $1 AND sid = $2).
		return oracleValRow{vals: []any{f.isMemberOf(args)}}
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

// isMemberOf answers the EXISTS gate from memberOf, reading the incarnation
// name out of the query args ($1) so a fixture with several memberships is
// answered per-incarnation rather than "member of something".
func (f *oracleFakeDB) isMemberOf(args []any) bool {
	if len(args) == 0 {
		return false
	}
	want, ok := args[0].(string)
	if !ok {
		return false
	}
	for _, inc := range f.memberOf {
		if inc == want {
			return true
		}
	}
	return false
}

func (f *oracleFakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM decrees") && f.decreeRows != nil:
		return f.decreeRows()
	case strings.Contains(sql, "FROM souls"):
		return f.hostRows()
	}
	return &oracleEmptyRows{}, nil
}

// hostRows answers [subject.LoadHost]: s.coven, s.traits, i.service, i.id,
// i.covens, i.traits — one row per membership, the host's own labels repeated in
// each (the LEFT JOIN shape), and a single row with NULL incarnation columns when
// the host belongs to nothing. No rows at all → the host is not registered.
func (f *oracleFakeDB) hostRows() (pgx.Rows, error) {
	if f.soulErr != nil {
		return nil, f.soulErr
	}
	if f.hostUnknown {
		return &oracleStaticRows{}, nil
	}
	if len(f.memberOf) == 0 {
		return &oracleStaticRows{rows: [][]any{
			{f.soulCoven, f.soulTraits, nil, nil, nil, nil},
		}}, nil
	}
	svc := f.memberService
	if svc == "" {
		svc = "svc"
	}
	rows := make([][]any, 0, len(f.memberOf))
	for _, inc := range f.memberOf {
		rows = append(rows, []any{f.soulCoven, f.soulTraits, svc, inc, f.incCoven, f.incTraits})
	}
	return &oracleStaticRows{rows: rows}, nil
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

// decreeRow builds a staticRow in decreeColumns order:
// name, on_beacon, where_cel, subject_sid, subject_service, subject_incarnation,
// subject_coven, subject_trait_key, subject_trait_value, incarnation_name,
// action_scenario, action_input, cooldown, enabled, created_at, updated_at,
// created_by_aid, label.
func decreeRow(d *oracle.Decree) []any {
	// nil → SQL NULL in a **string target.
	deref := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	input := d.ActionInput
	if input == nil {
		input = []byte("{}")
	}
	return []any{
		d.ID, d.OnBeacon, deref(d.WhereCEL),
		d.SubjectSID, deref(d.SubjectService), deref(d.SubjectIncarnation),
		d.SubjectCoven, deref(d.SubjectTraitKey), deref(d.SubjectTraitValue),
		d.IncarnationName, d.ActionScenario, []byte(input), d.Cooldown, d.Enabled,
		time.Now(), time.Now(), deref(d.CreatedByAID),
		deref(d.Label), // label (ADR-0085)
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

// oracleStaticRows — a fake pgx.Rows over a set of staticRow values.
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

// fakeEnqueuer — records enqueue calls.
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

// newOracleHandlerWithMetrics — like newOracleHandler, but additionally
// registers keeper_oracle_* metrics on a fresh registry and returns its
// gatherer for scrape checks (Part 2, ADR-030 S4).
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

// newOracleHandlerWithCircuit — handler with the circuit breaker enabled
// (CircuitMaxFires/CircuitWindow set, ADR-030(a) S4). maxFires==0 → breaker
// OFF (escape hatch). Returns the registry for scraping circuit_tripped.
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
		soulCoven: []string{"web", "prod"}, memberOf: []string{"web-app"},
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			ActionInput: []byte(`{"service":"nginx"}`), Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	calls := enq.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 enqueue, got %d", len(calls))
	}
	if calls[0].ScenarioName != "restart" || calls[0].SubjectSID != "host-a.example.com" {
		t.Errorf("enqueue: %+v", calls[0])
	}
	if calls[0].IncarnationName != "web-app" {
		t.Errorf("incarnation_name did not propagate: %+v", calls[0])
	}
	if calls[0].ActionInput["service"] != "nginx" {
		t.Errorf("action_input did not propagate: %+v", calls[0].ActionInput)
	}
	// audit oracle.fired.
	var fired bool
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			fired = true
		}
	}
	if !fired {
		t.Error("expected audit oracle.fired")
	}
}

func TestPortent_NoDecreeDefaultDeny(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{soulCoven: []string{"web"}, decreeRows: decreesRows()}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("default-deny: nothing should be enqueued without a Decree")
	}
}

func TestPortent_SubjectMismatch(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		soulCoven: []string{"db"}, // host is in db, Decree is about web
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("subject-mismatch: enqueue should not happen")
	}
}

func TestPortent_MembershipMismatch(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	// Subject IS in coven `web` (subject-match passes), BUT the host is bound to
	// no incarnation → membership-check fail-closed → skip.
	db := &oracleFakeDB{
		soulCoven: []string{"web"},
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "other-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("membership-mismatch: enqueue should not happen")
	}
	// fire is not recorded and audit oracle.fired is not written.
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("membership-mismatch: audit oracle.fired should NOT be written")
		}
	}
}

// TestPortent_MembershipAloneDoesNotMatchCovenSubject — GUARD (NIM-281): a
// Decree scoped `subject_coven: [web-app]` does NOT reach a member of `web-app`
// that carries no such tag. Membership is not a label, so the subject
// intersection is empty and the rule does not fire.
//
// A Decree fires a scenario, so this direction is the safe one: were the
// subject ever resolved through membership again, binding a host to an
// incarnation would silently enroll it in every rule scoped to that
// incarnation's name. To bind a rule to an incarnation's members, tag them.
func TestPortent_MembershipAloneDoesNotMatchCovenSubject(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		memberOf: []string{"web-app"}, // member, but untagged
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web-app"}, IncarnationName: "web-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if n := len(enq.snapshot()); n != 0 {
		t.Fatalf("got %d enqueues, want 0 — belonging to an incarnation is not a tag", n)
	}
}

// TestPortent_TaggedMemberMatchesCovenSubject — the positive pair: the same
// Decree, the same membership, and now the tag actually on the host. Both
// halves must hold — the subject match on `souls.coven[]` AND the membership
// row — so a break in either one is visible.
func TestPortent_TaggedMemberMatchesCovenSubject(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		soulCoven: []string{"web-app"},
		memberOf:  []string{"web-app"},
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web-app"}, IncarnationName: "web-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if n := len(enq.snapshot()); n != 1 {
		t.Fatalf("got %d enqueues, want 1 — a tagged member must fire", n)
	}
}

// TestPortent_CovenTagIsNotMembership — the other half of the pair above: the
// host carries the tag but holds NO membership row. The subject match passes
// and the gate must still refuse — resolving membership from a label would let
// a coven tag, which anyone may attach, hand a host the right to run a foreign
// incarnation's scenarios (ADR-030(b)).
func TestPortent_CovenTagIsNotMembership(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := &oracleFakeDB{
		soulCoven: []string{"other-app"}, // a tag, not a membership
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-other", OnBeacon: "svc-down",
			SubjectCoven: []string{"other-app"}, IncarnationName: "other-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("a coven tag spelled like an incarnation must not pass the membership gate")
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("non-member: audit oracle.fired must NOT be written")
		}
	}
}

func TestPortent_SIDDecreeEnqueues(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	// sid-Decree: the subject is a specific SID; incarnation membership is still
	// checked, against `incarnation_membership`, same as for a coven-Decree.
	db := &oracleFakeDB{
		memberOf: []string{"web-app"},
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-host", OnBeacon: "svc-down",
			SubjectSID: []string{"host-a.example.com"}, IncarnationName: "web-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	calls := enq.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sid-Decree: expected 1 enqueue, got %d", len(calls))
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
		t.Error("sid-Decree: expected audit oracle.fired")
	}
}

func TestPortent_WhereCELFilters(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		soulCoven: []string{"web"}, memberOf: []string{"web-app"},
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-crit", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			WhereCEL: strptr(`event.data.severity == "critical"`), Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	// severity=info → where false → skip.
	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess",
		portent("svc-down", map[string]any{"severity": "info"}))
	if len(enq.snapshot()) != 0 {
		t.Fatal("where-CEL false must filter out (skip)")
	}

	// severity=critical → where true → enqueue.
	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess",
		portent("svc-down", map[string]any{"severity": "critical"}))
	if len(enq.snapshot()) != 1 {
		t.Fatal("where-CEL true must let through (enqueue)")
	}
}

func TestPortent_CooldownBlocks(t *testing.T) {
	enq := &fakeEnqueuer{}
	recent := time.Now().UTC().Add(-1 * time.Minute) // within the 5m window
	db := &oracleFakeDB{
		soulCoven: []string{"web"}, memberOf: []string{"web-app"},
		lastFired: &recent,
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, &recordingAudit{})

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("cooldown within window must block a repeat")
	}
}

// TestPortent_MetricsEnqueuePath — on the full match path, portents_received /
// decrees_matched / scenarios_enqueued are incremented (Part 2, ADR-030 S4).
func TestPortent_MetricsEnqueuePath(t *testing.T) {
	enq := &fakeEnqueuer{}
	db := &oracleFakeDB{
		soulCoven: []string{"web"}, memberOf: []string{"web-app"},
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
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

// TestPortent_MetricsCooldownPath — cooldown within the window increments
// portents_received + cooldown_blocked, but NOT decrees_matched/scenarios_enqueued.
func TestPortent_MetricsCooldownPath(t *testing.T) {
	enq := &fakeEnqueuer{}
	recent := time.Now().UTC().Add(-1 * time.Minute)
	db := &oracleFakeDB{
		soulCoven: []string{"web"}, memberOf: []string{"web-app"},
		lastFired: &recent,
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
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
		t.Errorf("cooldown path must not increment scenarios_enqueued; got=\n%s", body)
	}
}

// TestPortent_MetricsDefaultDeny — without a Decree, only portents_received is
// incremented (default-deny on decrees_matched and beyond).
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
		t.Errorf("default-deny must not increment decrees_matched; got=\n%s", body)
	}
}

// circuitDB — a shared fake-DB builder for circuit-breaker tests: one enabled
// coven-Decree on svc-down, with configurable bumpReturns/tripWins.
func circuitDB(bumpReturns int, tripWins bool) *oracleFakeDB {
	return &oracleFakeDB{
		soulCoven: []string{"web"}, memberOf: []string{"web-app"},
		bumpReturns: bumpReturns,
		tripWins:    tripWins,
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			Cooldown: "5m", Enabled: true,
		}),
	}
}

// TestPortent_CircuitTripsAtThreshold — cnt >= max_fires and TripDecree won →
// circuit_tripped metric + audit decree.circuit_tripped + Decree auto-disable.
func TestPortent_CircuitTripsAtThreshold(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(5, true) // bump returns 5 == max, trip single-winner wins
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 5, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if !db.bumpCalled {
		t.Error("breaker enabled -> BumpCircuit must be called")
	}
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("expected circuit_tripped 1; got=\n%s", body)
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
		t.Error("expected audit decree.circuit_tripped")
	}
}

// TestPortent_CircuitBelowThreshold — cnt < max_fires → BumpCircuit is called,
// but trip does not occur (no circuit_tripped metric/audit).
func TestPortent_CircuitBelowThreshold(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(3, true) // 3 < 5; tripWins doesn't matter — TripDecree isn't called
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 5, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if !db.bumpCalled {
		t.Error("breaker enabled -> BumpCircuit must be called even below threshold")
	}
	body := obstest.Scrape(t, reg.Gatherer())
	if strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("below threshold trip must not fire; got=\n%s", body)
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventDecreeCircuitTripped {
			t.Error("below threshold audit decree.circuit_tripped must NOT be written")
		}
	}
}

// TestPortent_CircuitOffSkipsBump — max_fires==0 (escape hatch) → BumpCircuit
// is never called, trip is impossible.
func TestPortent_CircuitOffSkipsBump(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(99, true) // would return a lot, but breaker is OFF
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 0, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if db.bumpCalled {
		t.Error("breaker OFF (max_fires==0) -> BumpCircuit must NOT be called")
	}
	body := obstest.Scrape(t, reg.Gatherer())
	if strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("breaker OFF -> trip is impossible; got=\n%s", body)
	}
}

// TestPortent_CircuitTripLoserNoDuplicate — cnt >= max, but TripDecree lost
// (RowsAffected==0, another instance won) → we do NOT duplicate metric/audit.
func TestPortent_CircuitTripLoserNoDuplicate(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := circuitDB(7, false) // 7 >= 5, but trip lost the race
	h, reg := newOracleHandlerWithCircuit(t, db, enq, aw, 5, 10*time.Minute)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	body := obstest.Scrape(t, reg.Gatherer())
	if strings.Contains(body, "keeper_oracle_circuit_tripped_total 1") {
		t.Errorf("trip loser must not increment circuit_tripped; got=\n%s", body)
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventDecreeCircuitTripped {
			t.Error("trip loser must not write audit decree.circuit_tripped")
		}
	}
}

// TestPortent_EnqueueFailNoFireNoAudit — EnqueueScenario failure (Enqueuer.err) on
// a Decree that passed the filter: fire (cooldown record) is NOT written and
// audit oracle.fired is NOT written (qa coverage_gap #2: otherwise a false
// cooldown would block future real reactions). decrees_matched IS INCREMENTED
// (match is recorded BEFORE enqueue — the attempt counts), but scenarios_enqueued is not.
func TestPortent_EnqueueFailNoFireNoAudit(t *testing.T) {
	enq := &fakeEnqueuer{err: errors.New("enqueue: incarnation not found")}
	aw := &recordingAudit{}
	var fireRecorded bool
	db := &oracleFakeDB{
		soulCoven: []string{"web"}, memberOf: []string{"web-app"},
		// recordedFnc signals a write to oracle_fires (RecordFire). On the fail
		// path it must not happen.
		recordedFnc: func([]any) { fireRecorded = true },
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
			SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart",
			Cooldown: "5m", Enabled: true,
		}),
	}
	h, reg := newOracleHandlerWithMetrics(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if fireRecorded {
		t.Error("enqueue-fail: RecordFire (cooldown) must NOT be written - otherwise a false cooldown")
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("enqueue-fail: audit oracle.fired must NOT be written")
		}
	}
	body := obstest.Scrape(t, reg.Gatherer())
	// match is recorded BEFORE enqueue (the attempt counts), enqueued is not.
	if !strings.Contains(body, "keeper_oracle_decrees_matched_total 1") {
		t.Errorf("match must be recorded before enqueue; got=\n%s", body)
	}
	if strings.Contains(body, "keeper_oracle_scenarios_enqueued_total 1") {
		t.Errorf("enqueue-fail must not increment scenarios_enqueued; got=\n%s", body)
	}
}

// TestPortent_AuditPayloadExcludesEventData — regression guard (qa coverage_gap
// #4): audit oracle.fired stores exactly {sid,beacon,decree,scenario,apply_id} and does
// NOT leak event.data (untrusted payload of the Soul event). We feed event.data with
// an explicit marker and verify it never appears in any payload value.
func TestPortent_AuditPayloadExcludesEventData(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	db := &oracleFakeDB{
		soulCoven: []string{"web"}, memberOf: []string{"web-app"},
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-web", OnBeacon: "svc-down",
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
		t.Fatal("expected audit oracle.fired")
	}
	// Exactly the expected set of keys — no more, no less.
	wantKeys := map[string]bool{"sid": true, "beacon": true, "decree": true, "scenario": true, "apply_id": true}
	if len(fired.Payload) != len(wantKeys) {
		t.Errorf("payload keys = %v, want exactly %v", keysOf(fired.Payload), keysOf(map[string]any{"sid": 0, "beacon": 0, "decree": 0, "scenario": 0, "apply_id": 0}))
	}
	for k := range fired.Payload {
		if !wantKeys[k] {
			t.Errorf("unexpected key in payload: %q", k)
		}
	}
	// No payload value carries the event.data marker.
	for k, v := range fired.Payload {
		if s, ok := v.(string); ok && strings.Contains(s, leak) {
			t.Errorf("event.data leaked into payload[%q] = %q", k, s)
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

// TestPortent_SIDDecreeMembershipMismatch — sid-Decree, host matched by SID, BUT
// the host is not bound to the Decree's target incarnation → membership
// barrier fail-closed → skip (qa coverage_gap #5: the second membership barrier
// applies not only to coven-Decree but also to sid-Decree). enqueue/fire/audit are NOT written.
func TestPortent_SIDDecreeMembershipMismatch(t *testing.T) {
	enq := &fakeEnqueuer{}
	aw := &recordingAudit{}
	var fireRecorded bool
	// The host's SID matches the Decree's subject_sid (subject-match passes), but
	// the host is bound to `web-app`, not to the Decree's `other-app` → membership fail.
	db := &oracleFakeDB{
		memberOf:    []string{"web-app"},
		recordedFnc: func([]any) { fireRecorded = true },
		decreeRows: decreesRows(&oracle.Decree{
			ID: "restart-host", OnBeacon: "svc-down",
			SubjectSID: []string{"host-a.example.com"}, IncarnationName: "other-app",
			ActionScenario: "restart", Cooldown: "5m", Enabled: true,
		}),
	}
	h := newOracleHandler(t, db, enq, aw)

	h.handlePortentEvent(context.Background(), "host-a.example.com", "sess", portent("svc-down", nil))

	if len(enq.snapshot()) != 0 {
		t.Error("sid-Decree membership-mismatch: enqueue should not happen")
	}
	if fireRecorded {
		t.Error("sid-Decree membership-mismatch: RecordFire must NOT be written")
	}
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("sid-Decree membership-mismatch: audit oracle.fired must NOT be written")
		}
	}
}

func strptr(s string) *string { return &s }

func TestActiveVigilsForSID_ConvertsToVigilDef(t *testing.T) {
	db := &oracleVigilDB{
		soulCoven: []string{"web"},
		vigils: [][]any{
			vigilRow("web-watch", subject.Selector{Covens: []string{"web"}}, "30s", "core.beacon.service_down"),
		},
	}
	src := NewVigilSource(db)
	defs, err := src.ActiveVigilsForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ActiveVigilsForSID: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 VigilDef, got %d", len(defs))
	}
	if defs[0].GetName() != "web-watch" || defs[0].GetInterval() != "30s" || defs[0].GetCheck() != "core.beacon.service_down" {
		t.Errorf("VigilDef mapping: %+v", defs[0])
	}
}

// TestActiveVigilsForSID_IncarnationDimensions pins the reach of the two label
// levels and of the incarnation dimension itself on the Vigil path (NIM-280):
// what an operator put on the INCARNATION reaches its member hosts, while the
// host's own label set stays exactly what the operator attached to the host.
func TestActiveVigilsForSID_IncarnationDimensions(t *testing.T) {
	db := &oracleVigilDB{
		soulCoven: []string{"db"}, // the host itself carries only `db`
		member: []subject.Incarnation{{
			Service: "redis", Name: "redis-prod",
			Covens: []string{"cache"},
			Traits: map[string]any{"tier": "gold"},
		}},
		vigils: [][]any{
			vigilRow("own-coven", subject.Selector{Covens: []string{"db"}}, "30s", "core.beacon.service_down"),
			vigilRow("inc-coven", subject.Selector{Covens: []string{"cache"}}, "30s", "core.beacon.service_down"),
			vigilRow("inc-trait", subject.Selector{TraitKey: "tier", TraitValue: "gold"}, "30s", "core.beacon.service_down"),
			vigilRow("inc-addr", subject.Selector{Service: "redis", Incarnation: "redis-prod"}, "30s", "core.beacon.service_down"),
			vigilRow("sid", subject.Selector{SIDs: []string{"host-a.example.com"}}, "30s", "core.beacon.service_down"),
			// Negatives: a label nobody attached, and the same incarnation name
			// under another service — the name is unique only within its service.
			vigilRow("other-coven", subject.Selector{Covens: []string{"web"}}, "30s", "core.beacon.service_down"),
			vigilRow("other-service", subject.Selector{Service: "valkey", Incarnation: "redis-prod"}, "30s", "core.beacon.service_down"),
		},
	}

	defs, err := NewVigilSource(db).ActiveVigilsForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ActiveVigilsForSID: %v", err)
	}
	got := make(map[string]bool, len(defs))
	for _, d := range defs {
		got[d.GetName()] = true
	}
	for _, want := range []string{"own-coven", "inc-coven", "inc-trait", "inc-addr", "sid"} {
		if !got[want] {
			t.Errorf("vigil %q should reach the host", want)
		}
	}
	for _, unwanted := range []string{"other-coven", "other-service"} {
		if got[unwanted] {
			t.Errorf("vigil %q must NOT reach the host", unwanted)
		}
	}
}

// oracleVigilDB — fake for VigilSource. Answers both Queries LoadHost and
// SelectActiveVigilsForSubject issue, and filters the Vigil set with the REAL
// matcher over the subject rebuilt from the query arguments: a resolve that
// drops a dimension stops matching the Vigils written on it, instead of
// silently passing because the fake handed back everything.
type oracleVigilDB struct {
	soulCoven []string
	member    []subject.Incarnation
	vigils    [][]any
}

func (f *oracleVigilDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *oracleVigilDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return oracleErrRow{err: pgx.ErrNoRows}
}
func (f *oracleVigilDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM souls"):
		if len(f.member) == 0 {
			return &oracleStaticRows{rows: [][]any{{f.soulCoven, nil, nil, nil, nil, nil}}}, nil
		}
		rows := make([][]any, 0, len(f.member))
		for _, inc := range f.member {
			traits, _ := json.Marshal(inc.Traits)
			if len(inc.Traits) == 0 {
				traits = nil
			}
			rows = append(rows, []any{f.soulCoven, nil, inc.Service, inc.Name, inc.Covens, traits})
		}
		return &oracleStaticRows{rows: rows}, nil
	case strings.Contains(sql, "FROM vigils"):
		host := oracleHostFromArgs(args)
		kept := make([][]any, 0, len(f.vigils))
		for _, r := range f.vigils {
			if oracleVigilSelector(r).Matches(host) {
				kept = append(kept, r)
			}
		}
		return &oracleStaticRows{rows: kept}, nil
	}
	return &oracleEmptyRows{}, nil
}

// oracleHostFromArgs rebuilds the subject from the FLATTENED value arrays the
// shared predicate binds, in subject.MatchSQL order:
// sid, covens, inc services, inc names, trait pairs.
func oracleHostFromArgs(args []any) subject.Host {
	var h subject.Host
	if len(args) < 5 {
		return h
	}
	h.SID, _ = args[0].(string)
	h.Covens, _ = args[1].([]string)
	svcs, _ := args[2].([]string)
	names, _ := args[3].([]string)
	for i, name := range names {
		if i < len(svcs) {
			h.Member = append(h.Member, subject.Incarnation{Service: svcs[i], Name: name})
		}
	}
	pairs, _ := args[4].([]string)
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if h.Traits == nil {
			h.Traits = map[string]any{}
		}
		h.Traits[k] = v
	}
	return h
}

// oracleVigilSelector reads the subject out of a fixture row, in vigilColumns
// order: name, sid, service, incarnation, coven, trait_key, trait_value, …
func oracleVigilSelector(r []any) subject.Selector {
	str := func(v any) string {
		s, _ := v.(string)
		return s
	}
	sids, _ := r[1].([]string)
	covens, _ := r[4].([]string)
	return subject.Selector{
		SIDs:        sids,
		Service:     str(r[2]),
		Incarnation: str(r[3]),
		Covens:      covens,
		TraitKey:    str(r[5]),
		TraitValue:  str(r[6]),
	}
}

// vigilRow in vigilColumns order: name, sid, service, incarnation, coven,
// trait_key, trait_value, interval_spec, check_addr, params, enabled,
// created_at, updated_at, created_by_aid, label.
func vigilRow(name string, sel subject.Selector, interval, check string) []any {
	nilIfEmpty := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	return []any{
		name,
		sel.SIDs, nilIfEmpty(sel.Service), nilIfEmpty(sel.Incarnation), sel.Covens,
		nilIfEmpty(sel.TraitKey), nilIfEmpty(sel.TraitValue),
		interval, check,
		[]byte("{}"), true, time.Now(), time.Now(), nil,
		nil, // label (ADR-0085): unset here, reads NULL
	}
}
