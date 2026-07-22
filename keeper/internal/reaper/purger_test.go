package reaper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRow is an in-memory pgx.Row for unit tests. Behavior is defined either
// by `err` (Scan returns err and does not write to dest) or by `count` (Scan
// writes count to the first dest, expecting *int64).
type fakeRow struct {
	count int64
	err   error
}

func (f *fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if len(dest) != 1 {
		return errors.New("fakeRow: want exactly 1 dest")
	}
	out, ok := dest[0].(*int64)
	if !ok {
		return errors.New("fakeRow: dest[0] is not *int64")
	}
	*out = f.count
	return nil
}

// fakeQueryRower captures the arguments of each QueryRow and returns a
// preprogrammed [fakeRow]. SQL is not validated here; that is the contract of
// ADR-022(d) / migration 002, not Purger behavior.
type fakeQueryRower struct {
	calls   int
	lastSQL string
	args    []any
	row     *fakeRow
}

func (f *fakeQueryRower) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.calls++
	f.lastSQL = sql
	f.args = args
	if f.row == nil {
		return &fakeRow{}
	}
	return f.row
}

// Query is part of the extended [queryRower]: lease-aware mark_disconnected
// calls select_disconnect_candidates through Query. Existing tests use only
// QueryRow rules and the lease=nil branch, so Query is not called there.
// Returning an error makes it explicit that these fake scenarios are not meant
// for the two-phase rule.
func (f *fakeQueryRower) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeQueryRower: Query not implemented")
}

func TestNewPurger(t *testing.T) {
	// Sanity: NewPurger accepts *pgxpool.Pool. Here we only verify the internal
	// path through newPurgerFromQueryRower because NewPurger requires a real
	// *pgxpool.Pool, which is not available without starting PG.
	fq := &fakeQueryRower{}
	p := newPurgerFromQueryRower(fq)
	if p == nil {
		t.Fatal("newPurgerFromQueryRower returned nil")
	}
	if p.pool == nil {
		t.Error("p.pool is nil")
	}
}

func TestPurger_PurgeAuditOld_PassesIntervalAndBatchSize(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 42}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.PurgeAuditOld(context.Background(), 365*24*time.Hour, 500)
	if err != nil {
		t.Fatalf("PurgeAuditOld: %v", err)
	}
	if got != 42 {
		t.Errorf("count = %d, want 42", got)
	}
	if fq.calls != 1 {
		t.Fatalf("calls = %d, want 1", fq.calls)
	}
	if !strings.Contains(fq.lastSQL, "purge_audit_old") {
		t.Errorf("SQL missing purge_audit_old: %q", fq.lastSQL)
	}
	if len(fq.args) != 2 {
		t.Fatalf("args len = %d, want 2", len(fq.args))
	}
	if fq.args[0] != "31536000 seconds" {
		t.Errorf("args[0] interval = %v, want \"31536000 seconds\"", fq.args[0])
	}
	if fq.args[1] != 500 {
		t.Errorf("args[1] batch_size = %v, want 500", fq.args[1])
	}
}

func TestPurger_PurgeAuditOld_DefaultBatchSize(t *testing.T) {
	cases := []struct {
		name      string
		batchSize int
	}{
		{"zero", 0},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := &fakeQueryRower{row: &fakeRow{count: 0}}
			p := newPurgerFromQueryRower(fq)
			if _, err := p.PurgeAuditOld(context.Background(), time.Hour, tc.batchSize); err != nil {
				t.Fatalf("PurgeAuditOld: %v", err)
			}
			if fq.args[1] != defaultBatchSize {
				t.Errorf("batchSize=%d -> args[1] = %v, want %d", tc.batchSize, fq.args[1], defaultBatchSize)
			}
		})
	}
}

func TestPurger_PurgeAuditOld_PropagatesError(t *testing.T) {
	wantErr := errors.New("pg down")
	fq := &fakeQueryRower{row: &fakeRow{err: wantErr}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.PurgeAuditOld(context.Background(), time.Hour, 100)
	if err == nil {
		t.Fatal("PurgeAuditOld returned nil err; want wrapped error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
	if got != 0 {
		t.Errorf("count on error = %d, want 0", got)
	}
}

func TestPurger_PurgeAuditOld_ZeroDeletedNotError(t *testing.T) {
	// Drain pattern: returning 0 is a normal "nothing left to delete" signal,
	// not an error. The test protects against regressions like
	// `if count == 0 { return err }`.
	fq := &fakeQueryRower{row: &fakeRow{count: 0}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.PurgeAuditOld(context.Background(), time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeAuditOld(empty table): %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
}

func TestPurger_PurgeAuditOld_RejectsNonPositiveMaxAge(t *testing.T) {
	// Boundary cases `maxAge <= 0` are caught in PurgeAuditOld before touching
	// PG. Otherwise a negative duration would become PG interval `-N seconds`,
	// and `NOW() - (-N seconds) = NOW() + N` would delete 0 rows, hiding the
	// configuration error. Cover 0 and a negative case; this test does not need
	// PG because validation happens earlier.
	cases := []struct {
		name   string
		maxAge time.Duration
	}{
		{"zero", 0},
		{"negative_hour", -1 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := &fakeQueryRower{row: &fakeRow{count: 999}}
			p := newPurgerFromQueryRower(fq)
			got, err := p.PurgeAuditOld(context.Background(), tc.maxAge, 100)
			if err == nil {
				t.Fatalf("PurgeAuditOld(maxAge=%v) returned nil err; want validation error", tc.maxAge)
			}
			if got != 0 {
				t.Errorf("count on validation error = %d, want 0", got)
			}
			if fq.calls != 0 {
				t.Errorf("QueryRow.calls = %d, want 0 (PG must not be touched)", fq.calls)
			}
		})
	}
}

// --- Reaper.b: five remaining rules ------------------------------------
//
// Each rule is covered by three tests following the PurgeAuditOld pattern:
//   - PassesArgs: verify argument marshaling (interval / statuses / batch_size)
//     in the right order and format.
//   - DefaultBatchSize: zero/negative -> defaultBatchSize.
//   - PropagatesError: row.Scan error -> wrapped err.
// For rules with statuses[], add an EmptyStatuses case (validation before PG).
// For all rules, RejectsNonPositiveDuration covers zero/negative.
//
// No PG poke is done here: Purger provides a thin wrapper, and integration
// coverage lives under build tag `integration` (integration_test.go).

func TestPurger_PurgeExpiredPendingTokens_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 7}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.PurgeExpiredPendingTokens(context.Background(), 24*time.Hour, 250)
	if err != nil {
		t.Fatalf("PurgeExpiredPendingTokens: %v", err)
	}
	if got != 7 {
		t.Errorf("count = %d, want 7", got)
	}
	if !strings.Contains(fq.lastSQL, "expire_pending_seeds") {
		t.Errorf("SQL missing expire_pending_seeds: %q", fq.lastSQL)
	}
	if len(fq.args) != 2 {
		t.Fatalf("args len = %d, want 2", len(fq.args))
	}
	if fq.args[0] != "86400 seconds" {
		t.Errorf("args[0] interval = %v, want \"86400 seconds\"", fq.args[0])
	}
	if fq.args[1] != 250 {
		t.Errorf("args[1] batch = %v, want 250", fq.args[1])
	}
}

func TestPurger_PurgeUsedTokens_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 11}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.PurgeUsedTokens(context.Background(), 90*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeUsedTokens: %v", err)
	}
	if got != 11 {
		t.Errorf("count = %d, want 11", got)
	}
	if !strings.Contains(fq.lastSQL, "purge_used_tokens") {
		t.Errorf("SQL missing purge_used_tokens: %q", fq.lastSQL)
	}
	if fq.args[0] != "7776000 seconds" { // 90 * 24 * 3600
		t.Errorf("args[0] interval = %v, want \"7776000 seconds\"", fq.args[0])
	}
	if fq.args[1] != 100 {
		t.Errorf("args[1] batch = %v, want 100", fq.args[1])
	}
}

func TestPurger_PurgeSouls_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 3}}
	p := newPurgerFromQueryRower(fq)

	statuses := []string{"disconnected", "expired"}
	got, err := p.PurgeSouls(context.Background(), statuses, 30*24*time.Hour, 500)
	if err != nil {
		t.Fatalf("PurgeSouls: %v", err)
	}
	if got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
	if !strings.Contains(fq.lastSQL, "purge_souls") {
		t.Errorf("SQL missing purge_souls: %q", fq.lastSQL)
	}
	if len(fq.args) != 3 {
		t.Fatalf("args len = %d, want 3", len(fq.args))
	}
	gotStatuses, ok := fq.args[0].([]string)
	if !ok {
		t.Fatalf("args[0] type = %T, want []string", fq.args[0])
	}
	if len(gotStatuses) != 2 || gotStatuses[0] != "disconnected" || gotStatuses[1] != "expired" {
		t.Errorf("args[0] statuses = %v, want %v", gotStatuses, statuses)
	}
	if fq.args[1] != "2592000 seconds" { // 30 * 24 * 3600
		t.Errorf("args[1] interval = %v, want \"2592000 seconds\"", fq.args[1])
	}
	if fq.args[2] != 500 {
		t.Errorf("args[2] batch = %v, want 500", fq.args[2])
	}
}

func TestPurger_PurgeOldSeeds_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 5}}
	p := newPurgerFromQueryRower(fq)

	statuses := []string{"superseded", "expired", "revoked"}
	got, err := p.PurgeOldSeeds(context.Background(), statuses, 90*24*time.Hour, 200)
	if err != nil {
		t.Fatalf("PurgeOldSeeds: %v", err)
	}
	if got != 5 {
		t.Errorf("count = %d, want 5", got)
	}
	if !strings.Contains(fq.lastSQL, "purge_old_seeds") {
		t.Errorf("SQL missing purge_old_seeds: %q", fq.lastSQL)
	}
	gotStatuses, _ := fq.args[0].([]string)
	if len(gotStatuses) != 3 {
		t.Errorf("args[0] len = %d, want 3", len(gotStatuses))
	}
}

func TestPurger_MarkDisconnected_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 2}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	if got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	if !strings.Contains(fq.lastSQL, "mark_disconnected") {
		t.Errorf("SQL missing mark_disconnected: %q", fq.lastSQL)
	}
	if fq.args[0] != "90 seconds" {
		t.Errorf("args[0] interval = %v, want \"90 seconds\"", fq.args[0])
	}
	if fq.args[1] != 1000 {
		t.Errorf("args[1] batch = %v, want 1000", fq.args[1])
	}
}

// fakeCandidateRows is an in-memory pgx.Rows for select_disconnect_candidates:
// it returns one SID row at a time. It implements the minimum pgx.Rows surface
// needed by selectDisconnectCandidates (Next/Scan/Err/Close).
type fakeCandidateRows struct {
	sids []string
	pos  int
}

func (r *fakeCandidateRows) Next() bool { r.pos++; return r.pos <= len(r.sids) }
func (r *fakeCandidateRows) Scan(dest ...any) error {
	out, ok := dest[0].(*string)
	if !ok {
		return errors.New("fakeCandidateRows: dest[0] not *string")
	}
	*out = r.sids[r.pos-1]
	return nil
}
func (r *fakeCandidateRows) Err() error                                   { return nil }
func (r *fakeCandidateRows) Close()                                       {}
func (r *fakeCandidateRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeCandidateRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeCandidateRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeCandidateRows) RawValues() [][]byte                          { return nil }
func (r *fakeCandidateRows) Conn() *pgx.Conn                              { return nil }

// fakeLeaseQueryRower fakes bidirectional lease-aware reconcile. It
// distinguishes directions by SQL function name in the query:
//   - select_disconnect_candidates -> disconnectCands; mark_disconnected_sids -> markedSIDs;
//   - select_reconnect_candidates -> reconnectCands; mark_connected_sids -> connectedSIDs.
//
// `markedSIDs`/`connectedSIDs` capture what went to each mark phase; each
// QueryRow returns count equal to the length of the passed array.
type fakeLeaseQueryRower struct {
	candidates    []string // disconnect candidates (disconnectCands alias for test compatibility)
	reconnectCand []string // reconnect candidates (status='disconnected')

	markedSIDs    []string // SIDs passed to mark_disconnected_sids
	connectedSIDs []string // SIDs passed to mark_connected_sids
}

func (f *fakeLeaseQueryRower) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "select_reconnect_candidates") {
		return &fakeCandidateRows{sids: f.reconnectCand}, nil
	}
	return &fakeCandidateRows{sids: f.candidates}, nil
}
func (f *fakeLeaseQueryRower) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	var sids []string
	if len(args) > 0 {
		sids, _ = args[0].([]string)
	}
	if strings.Contains(sql, "mark_connected_sids") {
		f.connectedSIDs = sids
		return &fakeRow{count: int64(len(sids))}
	}
	f.markedSIDs = sids
	return &fakeRow{count: int64(len(sids))}
}

// fakeLeaseChecker is a deterministic soulLeaseChecker for unit tests.
// alive[sid]=true -> live stream; errSIDs[sid]=true -> check error.
type fakeLeaseChecker struct {
	alive   map[string]bool
	errSIDs map[string]bool
}

func (c *fakeLeaseChecker) SoulStreamAlive(_ context.Context, sid string) (bool, error) {
	if c.errSIDs[sid] {
		return false, errors.New("redis down")
	}
	return c.alive[sid], nil
}

// TestPurger_MarkDisconnected_LeaseAware_SkipsAlive checks that the
// lease-aware branch marks only candidates WITHOUT a live Redis lease; a Soul
// with a live stream is excluded.
func TestPurger_MarkDisconnected_LeaseAware_SkipsAlive(t *testing.T) {
	fq := &fakeLeaseQueryRower{candidates: []string{"a.example.com", "b.example.com", "c.example.com"}}
	lc := &fakeLeaseChecker{alive: map[string]bool{"b.example.com": true}}
	p := newPurgerWithLeaseFromQueryRower(fq, lc, nil)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	// a and c expired with no lease; b is alive and excluded.
	if got != 2 {
		t.Errorf("count = %d, want 2 (a + c)", got)
	}
	if len(fq.markedSIDs) != 2 {
		t.Fatalf("markedSIDs = %v, want 2", fq.markedSIDs)
	}
	for _, sid := range fq.markedSIDs {
		if sid == "b.example.com" {
			t.Errorf("alive b.example.com was marked disconnected")
		}
	}
}

// TestPurger_MarkDisconnected_LeaseAware_FailSafeOnError checks that a Redis
// check error for a specific SID does NOT mark it. This is fail-safe: keeping a
// live stream is more important than timely disconnect marking.
func TestPurger_MarkDisconnected_LeaseAware_FailSafeOnError(t *testing.T) {
	fq := &fakeLeaseQueryRower{candidates: []string{"a.example.com", "b.example.com"}}
	lc := &fakeLeaseChecker{
		alive:   map[string]bool{},
		errSIDs: map[string]bool{"a.example.com": true},
	}
	p := newPurgerWithLeaseFromQueryRower(fq, lc, nil)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	// a has a check error -> fail-safe keep; b really expired.
	if got != 1 {
		t.Errorf("count = %d, want 1 (only b)", got)
	}
	if len(fq.markedSIDs) != 1 || fq.markedSIDs[0] != "b.example.com" {
		t.Errorf("markedSIDs = %v, want [b.example.com]", fq.markedSIDs)
	}
}

// TestPurger_MarkDisconnected_LeaseAware_NoCandidates: an empty candidate list
// returns 0 without calling the lease checker or mark phase.
func TestPurger_MarkDisconnected_LeaseAware_NoCandidates(t *testing.T) {
	fq := &fakeLeaseQueryRower{candidates: nil}
	lc := &fakeLeaseChecker{alive: map[string]bool{}}
	p := newPurgerWithLeaseFromQueryRower(fq, lc, nil)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	if fq.markedSIDs != nil {
		t.Errorf("markedSIDs = %v, want nil (mark phase was not called)", fq.markedSIDs)
	}
}

// TestPurger_MarkDisconnected_LeaseAware_AllAlive: all candidates are alive,
// so the result is 0 and the mark phase is not called.
func TestPurger_MarkDisconnected_LeaseAware_AllAlive(t *testing.T) {
	fq := &fakeLeaseQueryRower{candidates: []string{"a.example.com", "b.example.com"}}
	lc := &fakeLeaseChecker{alive: map[string]bool{"a.example.com": true, "b.example.com": true}}
	p := newPurgerWithLeaseFromQueryRower(fq, lc, nil)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0 (all alive)", got)
	}
	if fq.markedSIDs != nil {
		t.Errorf("markedSIDs = %v, want nil", fq.markedSIDs)
	}
}

// TestPurger_MarkDisconnected_LeaseAware_Reconnect covers the reverse
// direction: a disconnected candidate with a LIVE lease returns to connected;
// disconnected without a lease remains disconnected, not online.
func TestPurger_MarkDisconnected_LeaseAware_Reconnect(t *testing.T) {
	fq := &fakeLeaseQueryRower{
		// The disconnect direction is empty; all work happens in the reconnect phase.
		reconnectCand: []string{"back.example.com", "still-dead.example.com"},
	}
	lc := &fakeLeaseChecker{alive: map[string]bool{"back.example.com": true}}
	p := newPurgerWithLeaseFromQueryRower(fq, lc, nil)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	// back is alive -> reconnect; still-dead has no lease -> remains disconnected.
	if got != 1 {
		t.Errorf("count = %d, want 1 (only back)", got)
	}
	if len(fq.connectedSIDs) != 1 || fq.connectedSIDs[0] != "back.example.com" {
		t.Errorf("connectedSIDs = %v, want [back.example.com]", fq.connectedSIDs)
	}
	if fq.markedSIDs != nil {
		t.Errorf("markedSIDs = %v, want nil (disconnect phase has no candidates)", fq.markedSIDs)
	}
}

// TestPurger_MarkDisconnected_LeaseAware_Bidirectional covers both directions
// in one run: one connected-without-lease is marked disconnected, and one
// disconnected-with-lease returns to connected. The return value is their sum.
func TestPurger_MarkDisconnected_LeaseAware_Bidirectional(t *testing.T) {
	fq := &fakeLeaseQueryRower{
		candidates:    []string{"going-down.example.com"},
		reconnectCand: []string{"coming-back.example.com"},
	}
	lc := &fakeLeaseChecker{alive: map[string]bool{"coming-back.example.com": true}}
	p := newPurgerWithLeaseFromQueryRower(fq, lc, nil)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	if got != 2 {
		t.Errorf("count = %d, want 2 (1 disconnect + 1 reconnect)", got)
	}
	if len(fq.markedSIDs) != 1 || fq.markedSIDs[0] != "going-down.example.com" {
		t.Errorf("markedSIDs = %v, want [going-down.example.com]", fq.markedSIDs)
	}
	if len(fq.connectedSIDs) != 1 || fq.connectedSIDs[0] != "coming-back.example.com" {
		t.Errorf("connectedSIDs = %v, want [coming-back.example.com]", fq.connectedSIDs)
	}
}

// TestPurger_MarkDisconnected_LeaseAware_Reconnect_FailSafeOnError checks that
// a Redis check error for a disconnected candidate does NOT return it to
// connected. This is symmetric fail-safe behavior with the disconnect
// direction: unknown lease state does not move the snapshot.
func TestPurger_MarkDisconnected_LeaseAware_Reconnect_FailSafeOnError(t *testing.T) {
	fq := &fakeLeaseQueryRower{
		reconnectCand: []string{"err.example.com", "alive.example.com"},
	}
	lc := &fakeLeaseChecker{
		alive:   map[string]bool{"alive.example.com": true},
		errSIDs: map[string]bool{"err.example.com": true},
	}
	p := newPurgerWithLeaseFromQueryRower(fq, lc, nil)

	got, err := p.MarkDisconnected(context.Background(), 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	// err.example.com has a check error -> fail-safe skip; alive is online -> reconnect.
	if got != 1 {
		t.Errorf("count = %d, want 1 (only alive)", got)
	}
	if len(fq.connectedSIDs) != 1 || fq.connectedSIDs[0] != "alive.example.com" {
		t.Errorf("connectedSIDs = %v, want [alive.example.com]", fq.connectedSIDs)
	}
}

func TestPurger_PurgeApplyRuns_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 9}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.PurgeApplyRuns(context.Background(), 30*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("PurgeApplyRuns: %v", err)
	}
	if got != 9 {
		t.Errorf("count = %d, want 9", got)
	}
	if !strings.Contains(fq.lastSQL, "purge_apply_runs") {
		t.Errorf("SQL missing purge_apply_runs: %q", fq.lastSQL)
	}
	if len(fq.args) != 2 {
		t.Fatalf("args len = %d, want 2", len(fq.args))
	}
	if fq.args[0] != "2592000 seconds" { // 30 * 24 * 3600
		t.Errorf("args[0] interval = %v, want \"2592000 seconds\"", fq.args[0])
	}
	if fq.args[1] != 1000 {
		t.Errorf("args[1] batch = %v, want 1000", fq.args[1])
	}
}

func TestPurger_PurgeApplyTaskRegister_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 4}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.PurgeApplyTaskRegister(context.Background(), time.Hour, 250)
	if err != nil {
		t.Fatalf("PurgeApplyTaskRegister: %v", err)
	}
	if got != 4 {
		t.Errorf("count = %d, want 4", got)
	}
	if !strings.Contains(fq.lastSQL, "purge_apply_task_register") {
		t.Errorf("SQL missing purge_apply_task_register: %q", fq.lastSQL)
	}
	if len(fq.args) != 2 {
		t.Fatalf("args len = %d, want 2", len(fq.args))
	}
	if fq.args[0] != "3600 seconds" { // grace 1h
		t.Errorf("args[0] interval = %v, want \"3600 seconds\"", fq.args[0])
	}
	if fq.args[1] != 250 {
		t.Errorf("args[1] batch = %v, want 250", fq.args[1])
	}
}

func TestPurger_ReclaimApplyRuns_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 6}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.ReclaimApplyRuns(context.Background(), time.Minute, 300)
	if err != nil {
		t.Fatalf("ReclaimApplyRuns: %v", err)
	}
	if got != 6 {
		t.Errorf("count = %d, want 6", got)
	}
	// Recovery is an inline UPDATE, not a SQL function; verify the target SQL contract.
	if !strings.Contains(fq.lastSQL, "status           = 'planned'") {
		t.Errorf("SQL must set status=planned: %q", fq.lastSQL)
	}
	// S4: reclaim ONLY claimed rows that died before delivery. dispatched/running
	// stay outside the scan because after delivery the Soul owns the run, and
	// re-claiming would mean double apply.
	if !strings.Contains(fq.lastSQL, "status = 'claimed'") {
		t.Errorf("SQL must scan only claimed: %q", fq.lastSQL)
	}
	if strings.Contains(fq.lastSQL, "'dispatched'") || strings.Contains(fq.lastSQL, "'running'") {
		t.Errorf("SQL must NOT reclaim dispatched/running (Soul owns after delivery): %q", fq.lastSQL)
	}
	if !strings.Contains(fq.lastSQL, "claim_expires_at < NOW()") {
		t.Errorf("SQL must filter expired lease: %q", fq.lastSQL)
	}
	// attempt must NOT be reset; the fencing epoch is incremented by the next claim.
	if strings.Contains(fq.lastSQL, "attempt") {
		t.Errorf("SQL must NOT touch attempt (fencing-epoch must grow): %q", fq.lastSQL)
	}
	// claim owner and lease are reset.
	if !strings.Contains(fq.lastSQL, "claim_by_kid     = NULL") {
		t.Errorf("SQL must reset claim_by_kid: %q", fq.lastSQL)
	}
	// The single argument is batch (LIMIT). lease is not passed to SQL because
	// the predicate compares claim_expires_at < NOW() directly.
	if len(fq.args) != 1 {
		t.Fatalf("args len = %d, want 1", len(fq.args))
	}
	if fq.args[0] != 300 {
		t.Errorf("args[0] batch = %v, want 300", fq.args[0])
	}
}

func TestPurger_ReclaimApplyRuns_DefaultBatchSize(t *testing.T) {
	for _, batch := range []int{0, -1} {
		fq := &fakeQueryRower{row: &fakeRow{count: 0}}
		p := newPurgerFromQueryRower(fq)
		if _, err := p.ReclaimApplyRuns(context.Background(), time.Minute, batch); err != nil {
			t.Fatalf("ReclaimApplyRuns(batch=%d): %v", batch, err)
		}
		if fq.args[0] != defaultBatchSize {
			t.Errorf("batch=%d -> args[0] = %v, want %d", batch, fq.args[0], defaultBatchSize)
		}
	}
}

func TestPurger_ReclaimApplyRuns_PropagatesError(t *testing.T) {
	wantErr := errors.New("pg down")
	fq := &fakeQueryRower{row: &fakeRow{err: wantErr}}
	p := newPurgerFromQueryRower(fq)
	got, err := p.ReclaimApplyRuns(context.Background(), time.Minute, 100)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrap of %v", err, wantErr)
	}
	if got != 0 {
		t.Errorf("count on error = %d, want 0", got)
	}
}

func TestPurger_ReclaimApplyRuns_RejectsNonPositiveLease(t *testing.T) {
	for _, lease := range []time.Duration{0, -time.Minute} {
		fq := &fakeQueryRower{row: &fakeRow{count: 999}}
		p := newPurgerFromQueryRower(fq)
		got, err := p.ReclaimApplyRuns(context.Background(), lease, 100)
		if err == nil {
			t.Fatalf("ReclaimApplyRuns(lease=%v) returned nil err; want validation error", lease)
		}
		if got != 0 {
			t.Errorf("count on validation error = %d, want 0", got)
		}
		if fq.calls != 0 {
			t.Errorf("PG was touched (calls = %d); want 0", fq.calls)
		}
	}
}

func TestPurger_ReaperB_DefaultBatchSize(t *testing.T) {
	cases := []struct {
		name string
		call func(p *Purger) error
	}{
		{
			"expire_pending_seeds",
			func(p *Purger) error {
				_, err := p.PurgeExpiredPendingTokens(context.Background(), time.Hour, 0)
				return err
			},
		},
		{
			"purge_used_tokens",
			func(p *Purger) error {
				_, err := p.PurgeUsedTokens(context.Background(), time.Hour, -1)
				return err
			},
		},
		{
			"purge_souls",
			func(p *Purger) error {
				_, err := p.PurgeSouls(context.Background(), []string{"expired"}, time.Hour, 0)
				return err
			},
		},
		{
			"purge_old_seeds",
			func(p *Purger) error {
				_, err := p.PurgeOldSeeds(context.Background(), []string{"expired"}, time.Hour, -5)
				return err
			},
		},
		{
			"mark_disconnected",
			func(p *Purger) error {
				_, err := p.MarkDisconnected(context.Background(), time.Hour, 0)
				return err
			},
		},
		{
			"purge_apply_runs",
			func(p *Purger) error {
				_, err := p.PurgeApplyRuns(context.Background(), time.Hour, 0)
				return err
			},
		},
		{
			"purge_apply_task_register",
			func(p *Purger) error {
				_, err := p.PurgeApplyTaskRegister(context.Background(), time.Hour, 0)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := &fakeQueryRower{row: &fakeRow{count: 0}}
			p := newPurgerFromQueryRower(fq)
			if err := tc.call(p); err != nil {
				t.Fatalf("call: %v", err)
			}
			// The last argument in each case is batch_size.
			lastArg := fq.args[len(fq.args)-1]
			if lastArg != defaultBatchSize {
				t.Errorf("batch_size = %v, want %d", lastArg, defaultBatchSize)
			}
		})
	}
}

func TestPurger_ReaperB_PropagatesError(t *testing.T) {
	wantErr := errors.New("pg down")
	cases := []struct {
		name string
		call func(p *Purger) (int64, error)
	}{
		{"expire_pending_seeds", func(p *Purger) (int64, error) {
			return p.PurgeExpiredPendingTokens(context.Background(), time.Hour, 100)
		}},
		{"purge_used_tokens", func(p *Purger) (int64, error) {
			return p.PurgeUsedTokens(context.Background(), time.Hour, 100)
		}},
		{"purge_souls", func(p *Purger) (int64, error) {
			return p.PurgeSouls(context.Background(), []string{"expired"}, time.Hour, 100)
		}},
		{"purge_old_seeds", func(p *Purger) (int64, error) {
			return p.PurgeOldSeeds(context.Background(), []string{"expired"}, time.Hour, 100)
		}},
		{"mark_disconnected", func(p *Purger) (int64, error) {
			return p.MarkDisconnected(context.Background(), time.Hour, 100)
		}},
		{"purge_apply_runs", func(p *Purger) (int64, error) {
			return p.PurgeApplyRuns(context.Background(), time.Hour, 100)
		}},
		{"purge_apply_task_register", func(p *Purger) (int64, error) {
			return p.PurgeApplyTaskRegister(context.Background(), time.Hour, 100)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := &fakeQueryRower{row: &fakeRow{err: wantErr}}
			p := newPurgerFromQueryRower(fq)
			got, err := tc.call(p)
			if err == nil {
				t.Fatal("expected wrapped error")
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("err = %v, want wrap of %v", err, wantErr)
			}
			if got != 0 {
				t.Errorf("count on error = %d, want 0", got)
			}
		})
	}
}

func TestPurger_ReaperB_RejectsNonPositiveDuration(t *testing.T) {
	// Same as PurgeAuditOld: a negative or zero duration would become a
	// negative PG interval, which turns into `NOW() - (-X) = NOW()+X` and
	// silently deletes/updates 0 rows, hiding the configuration error. Explicit
	// rejection is the only safe mode. PG must not be touched.
	cases := []struct {
		name string
		call func(p *Purger) (int64, error)
	}{
		{"expire_pending_seeds_zero", func(p *Purger) (int64, error) {
			return p.PurgeExpiredPendingTokens(context.Background(), 0, 100)
		}},
		{"expire_pending_seeds_negative", func(p *Purger) (int64, error) {
			return p.PurgeExpiredPendingTokens(context.Background(), -time.Hour, 100)
		}},
		{"purge_used_tokens_zero", func(p *Purger) (int64, error) {
			return p.PurgeUsedTokens(context.Background(), 0, 100)
		}},
		{"purge_souls_zero", func(p *Purger) (int64, error) {
			return p.PurgeSouls(context.Background(), []string{"expired"}, 0, 100)
		}},
		{"purge_old_seeds_zero", func(p *Purger) (int64, error) {
			return p.PurgeOldSeeds(context.Background(), []string{"expired"}, 0, 100)
		}},
		{"mark_disconnected_zero", func(p *Purger) (int64, error) {
			return p.MarkDisconnected(context.Background(), 0, 100)
		}},
		{"mark_disconnected_negative", func(p *Purger) (int64, error) {
			return p.MarkDisconnected(context.Background(), -time.Second, 100)
		}},
		{"purge_apply_runs_zero", func(p *Purger) (int64, error) {
			return p.PurgeApplyRuns(context.Background(), 0, 100)
		}},
		{"purge_apply_runs_negative", func(p *Purger) (int64, error) {
			return p.PurgeApplyRuns(context.Background(), -time.Hour, 100)
		}},
		{"purge_apply_task_register_zero", func(p *Purger) (int64, error) {
			return p.PurgeApplyTaskRegister(context.Background(), 0, 100)
		}},
		{"purge_apply_task_register_negative", func(p *Purger) (int64, error) {
			return p.PurgeApplyTaskRegister(context.Background(), -time.Hour, 100)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := &fakeQueryRower{row: &fakeRow{count: 999}}
			p := newPurgerFromQueryRower(fq)
			got, err := tc.call(p)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if got != 0 {
				t.Errorf("count on validation error = %d, want 0", got)
			}
			if fq.calls != 0 {
				t.Errorf("PG was touched (calls = %d); want 0", fq.calls)
			}
		})
	}
}

func TestPurger_PurgeSouls_RejectsEmptyStatuses(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
	}{
		{"nil", nil},
		{"empty", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := &fakeQueryRower{row: &fakeRow{count: 999}}
			p := newPurgerFromQueryRower(fq)
			got, err := p.PurgeSouls(context.Background(), tc.statuses, time.Hour, 100)
			if err == nil {
				t.Fatal("expected error for empty statuses")
			}
			if got != 0 {
				t.Errorf("count = %d, want 0", got)
			}
			if fq.calls != 0 {
				t.Errorf("PG was touched (calls = %d); want 0", fq.calls)
			}
		})
	}
}

func TestPurger_PurgeOldSeeds_RejectsEmptyStatuses(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 999}}
	p := newPurgerFromQueryRower(fq)
	if _, err := p.PurgeOldSeeds(context.Background(), nil, time.Hour, 100); err == nil {
		t.Fatal("expected error for empty statuses")
	}
	if fq.calls != 0 {
		t.Errorf("PG was touched (calls = %d); want 0", fq.calls)
	}
}

func TestPurger_ArchiveStateHistory_PassesArgs(t *testing.T) {
	fq := &fakeQueryRower{row: &fakeRow{count: 12}}
	p := newPurgerFromQueryRower(fq)

	got, err := p.ArchiveStateHistory(context.Background(), 50, true, 200)
	if err != nil {
		t.Fatalf("ArchiveStateHistory: %v", err)
	}
	if got != 12 {
		t.Errorf("count = %d, want 12", got)
	}
	if !strings.Contains(fq.lastSQL, "archive_state_history") {
		t.Errorf("SQL missing archive_state_history: %q", fq.lastSQL)
	}
	if len(fq.args) != 3 {
		t.Fatalf("args len = %d, want 3", len(fq.args))
	}
	if fq.args[0] != 50 {
		t.Errorf("args[0] keep_last_n = %v, want 50", fq.args[0])
	}
	if fq.args[1] != true {
		t.Errorf("args[1] keep_version_bump = %v, want true", fq.args[1])
	}
	if fq.args[2] != 200 {
		t.Errorf("args[2] batch = %v, want 200", fq.args[2])
	}
}

func TestPurger_ArchiveStateHistory_DefaultBatchSize(t *testing.T) {
	for _, batch := range []int{0, -1} {
		fq := &fakeQueryRower{row: &fakeRow{count: 0}}
		p := newPurgerFromQueryRower(fq)
		if _, err := p.ArchiveStateHistory(context.Background(), 50, true, batch); err != nil {
			t.Fatalf("ArchiveStateHistory(batch=%d): %v", batch, err)
		}
		if fq.args[2] != defaultBatchSize {
			t.Errorf("batch=%d -> args[2] = %v, want %d", batch, fq.args[2], defaultBatchSize)
		}
	}
}

func TestPurger_ArchiveStateHistory_RejectsNonPositiveKeepLastN(t *testing.T) {
	for _, keep := range []int{0, -1, -100} {
		fq := &fakeQueryRower{row: &fakeRow{count: 999}}
		p := newPurgerFromQueryRower(fq)
		got, err := p.ArchiveStateHistory(context.Background(), keep, true, 100)
		if err == nil {
			t.Fatalf("ArchiveStateHistory(keep=%d) returned nil err; want validation error", keep)
		}
		if got != 0 {
			t.Errorf("count on validation error = %d, want 0", got)
		}
		if fq.calls != 0 {
			t.Errorf("PG was touched (calls = %d); want 0", fq.calls)
		}
	}
}

func TestPurger_ArchiveStateHistory_PropagatesError(t *testing.T) {
	wantErr := errors.New("pg down")
	fq := &fakeQueryRower{row: &fakeRow{err: wantErr}}
	p := newPurgerFromQueryRower(fq)
	got, err := p.ArchiveStateHistory(context.Background(), 50, true, 100)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrap of %v", err, wantErr)
	}
	if got != 0 {
		t.Errorf("count on error = %d, want 0", got)
	}
}

func TestDurationToPGInterval(t *testing.T) {
	// Pluralization is fixed as-is: Postgres parses both `1 seconds` and
	// `0 seconds` without issues; cosmetic `1 second` has no semantic value and
	// is not locked by the test.
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"1 hour", time.Hour, "3600 seconds"},
		{"30 days", 30 * 24 * time.Hour, "2592000 seconds"},
		{"365 days", 365 * 24 * time.Hour, "31536000 seconds"},
		{"zero", 0, "0 seconds"},
		{"1 second", time.Second, "1 seconds"},
		{"sub-second truncates", 500 * time.Millisecond, "0 seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := durationToPGInterval(tc.in)
			if got != tc.want {
				t.Errorf("durationToPGInterval(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
