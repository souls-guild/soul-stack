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

// fakeOrphanCandidatesQuerier — fakeQuerier для фазы 1 (SELECT кандидатов).
// Возвращает запрограммированный набор (name, prev_kid, apply_id) либо error.
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

// fakePresence — программируемый presence-чек по KID.
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

// fakeReleaser — программируемый исход снятия по name.
type fakeReleaser struct {
	result   map[string]error // name → результат (nil = снят)
	released []string         // имена, для которых вызов вернул nil
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

// fakeReconcileAudit — счётчик emit-ов reaper.reconcile_orphan_applying.executed.
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

// --- (b) NULL-epoch фильтр: правило НЕ реклеймит applying без applying_by_kid ---

// TestOrphanApplyingSQL_FiltersNullEpoch — SQL-предикат фазы 1 содержит
// `applying_by_kid IS NOT NULL`: legacy/pre-082 (NULL-epoch) applying-строки в
// набор кандидатов НЕ попадают, правило их НЕ реклеймит (документированный
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

// --- (c) presence-живой владелец → правило НЕ реклеймит (split-brain guard) ---

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
		t.Errorf("affected = %d, want 0 (живой владелец — lock не осиротел)", affected)
	}
	if len(releaser.calls) != 0 {
		t.Errorf("releaser вызван %d раз, want 0 (presence жив → НЕ снимаем)", len(releaser.calls))
	}
	if len(ad.events) != 0 {
		t.Errorf("audit events = %d, want 0", len(ad.events))
	}
}

// --- (d/e) presence-мёртвый + ReleaseApplyingOrphan no-op (live-rival FENCING-1
// либо honest-terminal гонка single-winner) → правило НЕ засчитывает снятие ---

func TestOrphanApplyingReconciler_DeadOwner_ReleaseNoOp_NotCounted(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-dead"), applyID: strptr("apply-1")},
	}}
	presence := &fakePresence{alive: aliveMap("keeper-dead", "dead")}
	// ReleaseApplyingOrphan вернёт ErrOrphanLockNotReleased — FENCING-1 (живой
	// rival с другим apply_id) ИЛИ single-winner (честный финал прошлого владельца
	// уже вывел строку из applying).
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
		t.Errorf("affected = %d, want 0 (release no-op — fencing внутри отбил)", affected)
	}
	if len(releaser.calls) != 1 {
		t.Errorf("releaser вызван %d раз, want 1 (presence мёртв → попытка снятия была)", len(releaser.calls))
	}
	if len(ad.events) != 0 {
		t.Errorf("audit events = %d, want 0 (снятия не было)", len(ad.events))
	}
}

// --- (f) presence-мёртвый, без соперника → applying снят, audit эмитнут ---

func TestOrphanApplyingReconciler_DeadOwner_Released(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-dead"), applyID: strptr("apply-7")},
	}}
	presence := &fakePresence{alive: aliveMap("keeper-dead", "dead")}
	releaser := &fakeReleaser{result: map[string]error{"redis-prod": nil}} // снят
	ad := &fakeReconcileAudit{}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, ad, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1 (lock снят)", affected)
	}
	if len(releaser.calls) != 1 || releaser.calls[0].applyID != "apply-7" {
		t.Errorf("release calls = %+v, want один вызов с applyID=apply-7", releaser.calls)
	}
	if len(ad.events) != 1 {
		t.Fatalf("audit events = %d, want 1 (executed)", len(ad.events))
	}
	ev := ad.events[0]
	if ev["incarnation"] != "redis-prod" || ev["prev_kid"] != "keeper-dead" || ev["apply_id"] != "apply-7" {
		t.Errorf("audit payload = %+v, want {incarnation:redis-prod, prev_kid:keeper-dead, apply_id:apply-7}", ev)
	}
}

// --- presence-ошибка → fail-safe skip (НЕ снимаем при неизвестном presence) ---

func TestOrphanApplyingReconciler_PresenceError_FailSafeSkip(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-A"), applyID: strptr("apply-1")},
	}}
	presence := &fakePresence{err: errors.New("redis flap")}
	releaser := &fakeReleaser{result: map[string]error{}}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, nil, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v (ошибка presence одного кандидата не валит проход)", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (presence неизвестен → fail-safe skip)", affected)
	}
	if len(releaser.calls) != 0 {
		t.Errorf("releaser вызван %d раз, want 0 (presence-ошибка → НЕ снимаем)", len(releaser.calls))
	}
}

// --- defensive-skip: пустой epoch (legacy/ручная правка) → skip без presence ---

func TestOrphanApplyingReconciler_DefensiveSkip_EmptyEpoch(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "a", prevKID: nil, applyID: strptr("apply-1")},  // нет prev_kid
		{name: "b", prevKID: strptr("keeper-X"), applyID: nil}, // нет apply_id
	}}
	presence := &fakePresence{alive: aliveMap("keeper-X", "dead")}
	releaser := &fakeReleaser{result: map[string]error{}}
	r := newOrphanApplyingReconcilerForTest(fq, presence, releaser, nil, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (неполный epoch — defensive-skip)", affected)
	}
	if len(presence.calls) != 0 {
		t.Errorf("presence вызван %d раз, want 0 (skip ДО presence-чека)", len(presence.calls))
	}
	if len(releaser.calls) != 0 {
		t.Errorf("releaser вызван %d раз, want 0", len(releaser.calls))
	}
}

// --- presence-gate: nil presence-клиент → graceful no-op (default-ON безопасен) ---

func TestOrphanApplyingReconciler_NilPresence_GracefulNoop(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{rows: []orphanRow{
		{name: "redis-prod", prevKID: strptr("keeper-A"), applyID: strptr("apply-1")},
	}}
	r := newOrphanApplyingReconcilerForTest(fq, nil, &fakeReleaser{}, nil, silentLogger())

	affected, err := r.Run(context.Background(), 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v (presence-gate — graceful no-op, не ошибка)", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0", affected)
	}
	if fq.calls != 0 {
		t.Errorf("querier вызван %d раз, want 0 (presence-gate отсекает ДО SQL)", fq.calls)
	}
}

// --- mixed: один живой, один мёртвый-снят, один мёртвый-no-op → affected=1 ---

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
		t.Errorf("affected = %d, want 1 (только dead-released снят)", affected)
	}
	if len(ad.events) != 1 || ad.events[0]["incarnation"] != "dead-released" {
		t.Errorf("audit = %+v, want один executed для dead-released", ad.events)
	}
}

// --- propagation: ошибка SELECT кандидатов валит проход ---

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
		t.Fatal("ожидалась ошибка при nil pool")
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

// TestDispatch_ReconcileOrphanApplying_DefaultOnWhenAbsent: ключ отсутствует в
// Rules → правило ВСЁ РАВНО исполняется (default-ON path-defaulting).
func TestDispatch_ReconcileOrphanApplying_DefaultOnWhenAbsent(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{}
	rec := newOrphanApplyingReconcilerForTest(fq, &fakePresence{}, &fakeReleaser{}, nil, silentLogger())
	r := newReconcileDispatchRunner(rec)

	r.dispatch(context.Background(), reclaimDispatchCfg(nil))

	if fq.calls != 1 {
		t.Fatalf("reconcile_orphan_applying должно исполниться при отсутствии ключа (default-ON); querier calls=%d, want 1", fq.calls)
	}
}

// TestDispatch_ReconcileOrphanApplying_SkippedWhenDisabled: явный enabled:false
// → правило ПРОПУЩЕНО.
func TestDispatch_ReconcileOrphanApplying_SkippedWhenDisabled(t *testing.T) {
	fq := &fakeOrphanCandidatesQuerier{}
	rec := newOrphanApplyingReconcilerForTest(fq, &fakePresence{}, &fakeReleaser{}, nil, silentLogger())
	r := newReconcileDispatchRunner(rec)

	cfg := reclaimDispatchCfg(map[string]config.ReaperRule{
		"reconcile_orphan_applying": {Enabled: false},
	})
	r.dispatch(context.Background(), cfg)

	if fq.calls != 0 {
		t.Fatalf("reconcile_orphan_applying с enabled:false должно быть пропущено; querier calls=%d, want 0", fq.calls)
	}
}
