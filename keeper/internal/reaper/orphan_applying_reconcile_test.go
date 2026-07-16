package reaper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// --- fakes ---

// fakeOrphanCandidatesQuerier is a fakeQuerier for phase 1 (SELECT candidates).
// It returns a programmed set of (name, prev_kid, apply_id) or an error.
type fakeOrphanCandidatesQuerier struct {
	calls   int
	lastSQL string
	rows    []orphanRow
	err     error
}

type orphanRow struct {
	name    string
	prevKID *string
	applyID *string
}

func (f *fakeOrphanCandidatesQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.calls++
	f.lastSQL = sql
	if f.err != nil {
		return nil, f.err
	}
	return &fakeOrphanRows{rows: f.rows}, nil
}

type fakeOrphanRows struct {
	rows []orphanRow
	idx  int
}

func (r *fakeOrphanRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeOrphanRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	if len(dest) != 3 {
		return errors.New("fakeOrphanRows: expected 3 dest")
	}
	*dest[0].(*string) = row.name
	*dest[1].(**string) = row.prevKID
	*dest[2].(**string) = row.applyID
	return nil
}

func (r *fakeOrphanRows) Err() error                                   { return nil }
func (r *fakeOrphanRows) Close()                                       {}
func (r *fakeOrphanRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeOrphanRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeOrphanRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeOrphanRows) RawValues() [][]byte                          { return nil }
func (r *fakeOrphanRows) Conn() *pgx.Conn                              { return nil }

// fakePresence is a programmable presence check by KID.
type fakePresence struct {
	alive map[string]bool
	err   error
	calls []string
}

func (p *fakePresence) InstanceAlive(_ context.Context, kid string) (bool, error) {
	p.calls = append(p.calls, kid)
	if p.err != nil {
		return false, p.err
	}
	return p.alive[kid], nil
}

// fakeReleaser is a programmable release outcome by name.
type fakeReleaser struct {
	result   map[string]error // name -> result (nil = released)
	released []string         // names for which the call returned nil
	calls    []releaseCall
}

type releaseCall struct {
	name    string
	applyID string
}

func (r *fakeReleaser) ReleaseApplyingOrphan(_ context.Context, name, orphanApplyID, _ string) error {
	r.calls = append(r.calls, releaseCall{name: name, applyID: orphanApplyID})
	err := r.result[name]
	if err == nil {
		r.released = append(r.released, name)
	}
	return err
}

// fakeReconcileAudit counts reaper.reconcile_orphan_applying.executed emits.
type fakeReconcileAudit struct {
	events []map[string]any
}

func (a *fakeReconcileAudit) Write(_ context.Context, ev *audit.Event) error {
	a.events = append(a.events, ev.Payload)
	return nil
}

func strptr(s string) *string { return &s }

// --- helpers ---

func aliveMap(pairs ...string) map[string]bool {
	m := map[string]bool{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1] == "alive"
	}
	return m
}

// --- (b) NULL-epoch filter: rule does NOT reclaim applying without applying_by_kid ---

// TestOrphanApplyingSQL_FiltersNullEpoch: phase 1 SQL predicate contains
// `applying_by_kid IS NOT NULL`: legacy/pre-082 (NULL-epoch) applying rows do
// NOT enter the candidate set, and the rule does NOT reclaim them (documented
// known-gap, ADR-027 amend (m-S1)).
func TestOrphanApplyingSQL_FiltersNullEpoch(t *testing.T) {
	for _, frag := range []string{
		"status = 'applying'",
		"applying_since < $1",
		"applying_by_kid IS NOT NULL",
		"SELECT name, applying_by_kid, applying_apply_id",
	} {
		if !strings.Contains(orphanApplyingCandidatesSQL, frag) {
			t.Errorf("orphanApplyingCandidatesSQL missing %q\nSQL: %s", frag, orphanApplyingCandidatesSQL)
		}
	}
}

// --- (c) presence-live owner -> rule does NOT reclaim (split-brain guard) ---

func TestOrphanApplyingReconciler_PresenceAlive_NotReclaimed(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-A"), applyID: strptr("apply-1")},
	}}
	presence := &fakePresence{alive: aliveMap("keeper-A", "alive")}
	releaser := &fakeReleaser{result: map[string]error{}}
	ad := &fakeReconcileAudit{}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, ad, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (live owner, lock is not orphaned)", affected)
	}
	if len(releaser.calls) != 0 {
		t.Errorf("releaser called %d times, want 0 (presence is live, do NOT release)", len(releaser.calls))
	}
	if len(ad.events) != 0 {
		t.Errorf("audit events = %d, want 0", len(ad.events))
	}
}

// --- (d/e) dead presence + ReleaseApplyingOrphan no-op (live-rival FENCING-1
// or honest-terminal single-winner race) -> rule does NOT count a release ---

func TestOrphanApplyingReconciler_DeadOwner_ReleaseNoOp_NotCounted(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-dead"), applyID: strptr("apply-1")},
	}}
	presence := &fakePresence{alive: aliveMap("keeper-dead", "dead")}
	// ReleaseApplyingOrphan returns ErrOrphanLockNotReleased: FENCING-1 with a
	// live rival using another apply_id, OR single-winner where the previous
	// owner's honest terminal already moved the row out of applying.
	releaser := &fakeReleaser{result: map[string]error{
		"redis-prod": incarnation.ErrOrphanLockNotReleased,
	}}
	ad := &fakeReconcileAudit{}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, ad, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (release no-op, fencing rejected internally)", affected)
	}
	if len(releaser.calls) != 1 {
		t.Errorf("releaser called %d times, want 1 (presence is dead, release was attempted)", len(releaser.calls))
	}
	if len(ad.events) != 0 {
		t.Errorf("audit events = %d, want 0 (no release happened)", len(ad.events))
	}
}

// --- (f) dead presence, no rival -> applying released, audit emitted ---

func TestOrphanApplyingReconciler_DeadOwner_Released(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-dead"), applyID: strptr("apply-7")},
	}}
	presence := &fakePresence{alive: aliveMap("keeper-dead", "dead")}
	releaser := &fakeReleaser{result: map[string]error{"redis-prod": nil}} // released
	ad := &fakeReconcileAudit{}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, ad, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1 (lock released)", affected)
	}
	if len(releaser.calls) != 1 || releaser.calls[0].applyID != "apply-7" {
		t.Errorf("release calls = %+v, want one call with applyID=apply-7", releaser.calls)
	}
	if len(ad.events) != 1 {
		t.Fatalf("audit events = %d, want 1 (executed)", len(ad.events))
	}
	ev := ad.events[0]
	if ev["incarnation"] != "redis-prod" || ev["prev_kid"] != "keeper-dead" || ev["apply_id"] != "apply-7" {
		t.Errorf("audit payload = %+v, want {incarnation:redis-prod, prev_kid:keeper-dead, apply_id:apply-7}", ev)
	}
}

// --- presence error -> fail-safe skip (do NOT release when presence is unknown) ---

func TestOrphanApplyingReconciler_PresenceError_FailSafeSkip(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-A"), applyID: strptr("apply-1")},
	}}
	presence := &fakePresence{err: errors.New("redis flap")}
	releaser := &fakeReleaser{result: map[string]error{}}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, nil, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v (presence error for one candidate must not fail the pass)", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (presence unknown -> fail-safe skip)", affected)
	}
	if len(releaser.calls) != 0 {
		t.Errorf("releaser called %d times, want 0 (presence error -> do NOT release)", len(releaser.calls))
	}
}

// --- defensive skip: empty epoch (legacy/manual edit) -> skip without presence ---

func TestOrphanApplyingReconciler_DefensiveSkip_EmptyEpoch(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "a", prevKID: nil, applyID: strptr("apply-1")},  // no prev_kid
		{name: "b", prevKID: strptr("keeper-X"), applyID: nil}, // no apply_id
	}}
	presence := &fakePresence{alive: aliveMap("keeper-X", "dead")}
	releaser := &fakeReleaser{result: map[string]error{}}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, nil, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (incomplete epoch, defensive skip)", affected)
	}
	if len(presence.calls) != 0 {
		t.Errorf("presence called %d times, want 0 (skip BEFORE presence check)", len(presence.calls))
	}
	if len(releaser.calls) != 0 {
		t.Errorf("releaser called %d times, want 0", len(releaser.calls))
	}
}

// --- presence gate: nil presence client -> graceful no-op (default-ON is safe) ---

func TestOrphanApplyingReconciler_NilPresence_GracefulNoop(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-A"), applyID: strptr("apply-1")},
	}}
	r := newOrphanApplyingReconcilerForTest(fq, nil, &fakeReleaser{}, nil, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v (presence gate is graceful no-op, not an error)", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0", affected)
	}
	if fq.calls != 0 {
		t.Errorf("querier called %d times, want 0 (presence gate cuts off BEFORE SQL)", fq.calls)
	}
}

// --- mixed: one live, one dead-released, one dead-no-op -> affected=1 ---

func TestOrphanApplyingReconciler_Mixed(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "alive-inc", prevKID: strptr("keeper-live"), applyID: strptr("a1")},
		{name: "dead-released", prevKID: strptr("keeper-d1"), applyID: strptr("a2")},
		{name: "dead-noop", prevKID: strptr("keeper-d2"), applyID: strptr("a3")},
	}}
	presence := &fakePresence{alive: aliveMap(
		"keeper-live", "alive",
		"keeper-d1", "dead",
		"keeper-d2", "dead",
	)}
	releaser := &fakeReleaser{result: map[string]error{
		"dead-released": nil,
		"dead-noop":     incarnation.ErrOrphanLockNotReleased,
	}}
	ad := &fakeReconcileAudit{}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, ad, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1 (only dead-released was released)", affected)
	}
	if len(ad.events) != 1 || ad.events[0]["incarnation"] != "dead-released" {
		t.Errorf("audit = %+v, want one executed event for dead-released", ad.events)
	}
}

// --- propagation: SELECT candidates error fails the pass ---

func TestOrphanApplyingReconciler_QueryError_Propagates(t *testing.T) {
	want := errors.New("pg down")
	fq := &fakeOrphanCandidatesQuerier{err: want}
	r := newOrphanApplyingReconcilerForTest(fq, &fakePresence{}, &fakeReleaser{}, nil, silentLogger())

	if _, err := r.Run(context.Background(), 90*time.Second, 1000); !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
}

func TestOrphanApplyingReconciler_NilPoolReturnsError(t *testing.T) {
	r := &OrphanApplyingReconciler{pool: nil}
	if _, err := r.Run(context.Background(), 90*time.Second, 1000); err == nil {
		t.Fatal("expected error with nil pool")
	}
}

// --- dispatch default-ON path-defaulting (parity reclaim_voyages) ---

func newReconcileDispatchRunner(rec *OrphanApplyingReconciler) *Runner {
	return &Runner{
		deps: Deps{
			Logger:         silentLogger(),
			OrphanApplying: rec,
		},
	}
}

// TestDispatch_ReconcileOrphanApplying_DefaultOnWhenAbsent: key is absent from
// Rules, but the rule STILL executes through default-ON path defaulting.
func TestDispatch_ReconcileOrphanApplying_DefaultOnWhenAbsent(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{}
	rec := newOrphanApplyingReconcilerForTest(fq, &fakePresence{}, &fakeReleaser{}, nil, silentLogger())
	r := newReconcileDispatchRunner(rec)

	r.dispatch(context.Background(), reclaimDispatchCfg(nil))

	if fq.calls != 1 {
		t.Fatalf("reconcile_orphan_applying must execute when key is absent (default-ON); querier calls=%d, want 1", fq.calls)
	}
}

// TestDispatch_ReconcileOrphanApplying_SkippedWhenDisabled: explicit
// enabled:false means the rule is SKIPPED.
func TestDispatch_ReconcileOrphanApplying_SkippedWhenDisabled(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{}
	rec := newOrphanApplyingReconcilerForTest(fq, &fakePresence{}, &fakeReleaser{}, nil, silentLogger())
	r := newReconcileDispatchRunner(rec)

	cfg := reclaimDispatchCfg(map[string]config.ReaperRule{
		"reconcile_orphan_applying": {Enabled: false},
	})
	r.dispatch(context.Background(), cfg)

	if fq.calls != 0 {
		t.Fatalf("reconcile_orphan_applying with enabled:false must be skipped; querier calls=%d, want 0", fq.calls)
	}
}
