package reaper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// reclaimDispatchCfg — KeeperConfig с включённым Reaper и заданным набором
// правил. rules == nil → ключ reclaim_voyages в map отсутствует (кейс
// path-defaulting default-ON).
func reclaimDispatchCfg(rules map[string]config.ReaperRule) *config.KeeperConfig {
	return &config.KeeperConfig{
		Reaper: &config.KeeperReaper{
			Enabled: true,
			Rules:   rules,
		},
	}
}

// newReclaimDispatchRunner собирает Runner с реальным VoyageReclaimer поверх
// fake-querier — так dispatch доходит до SQL, и fq.calls фиксирует факт запуска
// правила.
func newReclaimDispatchRunner(fq voyagesQuerier) *Runner {
	return &Runner{
		deps: Deps{
			Logger:        silentLogger(),
			VoyageReclaim: newVoyageReclaimerFromQuerier(fq, nil, silentLogger()),
		},
	}
}

// TestDispatch_ReclaimVoyages_DefaultOnWhenAbsent: ключ reclaim_voyages
// ОТСУТСТВУЕТ в Rules → правило ВСЁ РАВНО исполняется (default-ON через
// path-defaulting в dispatchReclaimVoyages, ADR-043 §8).
func TestDispatch_ReclaimVoyages_DefaultOnWhenAbsent(t *testing.T) {
	fq := &fakeVoyagesQuerier{}
	r := newReclaimDispatchRunner(fq)

	r.dispatch(context.Background(), reclaimDispatchCfg(nil))

	if fq.calls != 1 {
		t.Fatalf("reclaim_voyages должно исполниться при отсутствии ключа (default-ON); querier calls=%d, want 1", fq.calls)
	}
}

// TestDispatch_ReclaimVoyages_SkippedWhenDisabled: явный enabled:false →
// правило ПРОПУЩЕНО (единственный путь выключения default-ON-правила).
func TestDispatch_ReclaimVoyages_SkippedWhenDisabled(t *testing.T) {
	fq := &fakeVoyagesQuerier{}
	r := newReclaimDispatchRunner(fq)

	cfg := reclaimDispatchCfg(map[string]config.ReaperRule{
		"reclaim_voyages": {Enabled: false},
	})
	r.dispatch(context.Background(), cfg)

	if fq.calls != 0 {
		t.Fatalf("reclaim_voyages с enabled:false должно быть пропущено; querier calls=%d, want 0", fq.calls)
	}
}

// fakeVoyagesQuerier — fake voyagesQuerier для unit-тестов reclaim_voyages.
// Захватывает последний SQL, возвращает запрограммированный набор строк
// (по одной reclaimed-Voyage) либо error.
type fakeVoyagesQuerier struct {
	calls   int
	lastSQL string
	rows    []reclaimRow
	err     error
}

// reclaimRow — одна строка результата UPDATE ... RETURNING.
type reclaimRow struct {
	voyageID    string
	lastRenewed *time.Time
	attempt     int
}

func (f *fakeVoyagesQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.calls++
	f.lastSQL = sql
	if f.err != nil {
		return nil, f.err
	}
	return &fakeReclaimRows{rows: f.rows}, nil
}

// fakeReclaimRows — pgx.Rows-stub поверх среза reclaimRow.
type fakeReclaimRows struct {
	rows []reclaimRow
	idx  int
}

func (r *fakeReclaimRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeReclaimRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	if len(dest) != 3 {
		return errors.New("fakeReclaimRows: expected 3 dest")
	}
	*dest[0].(*string) = row.voyageID
	*dest[1].(**time.Time) = row.lastRenewed
	*dest[2].(*int) = row.attempt
	return nil
}

func (r *fakeReclaimRows) Err() error                                   { return nil }
func (r *fakeReclaimRows) Close()                                       {}
func (r *fakeReclaimRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeReclaimRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeReclaimRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeReclaimRows) RawValues() [][]byte                          { return nil }
func (r *fakeReclaimRows) Conn() *pgx.Conn                              { return nil }

// fakeReclaimAudit — счётчик emit-ов voyage.reclaimed.
type fakeReclaimAudit struct {
	events []string // voyage_id каждого записанного события
}

func (a *fakeReclaimAudit) Write(_ context.Context, ev *audit.Event) error {
	a.events = append(a.events, ev.Payload["voyage_id"].(string))
	return nil
}

func voyTime(t time.Time) *time.Time { return &t }

func TestVoyageReclaimer_Run_ReclaimsRowsAndReturnsCount(t *testing.T) {
	fq := &fakeVoyagesQuerier{rows: []reclaimRow{
		{voyageID: "v1", lastRenewed: voyTime(time.Now()), attempt: 2},
		{voyageID: "v2", lastRenewed: voyTime(time.Now()), attempt: 3},
		{voyageID: "v3", lastRenewed: nil, attempt: 4},
		{voyageID: "v4", lastRenewed: voyTime(time.Now()), attempt: 5},
		{voyageID: "v5", lastRenewed: voyTime(time.Now()), attempt: 6},
	}}
	ad := &fakeReclaimAudit{}
	r := newVoyageReclaimerFromQuerier(fq, ad, silentLogger())

	got, err := r.Run(context.Background(), time.Minute, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 5 {
		t.Errorf("affected = %d, want 5", got)
	}
	if fq.calls != 1 {
		t.Fatalf("calls = %d, want 1", fq.calls)
	}
	if fq.lastSQL != reclaimVoyagesSQL {
		t.Errorf("SQL mismatch:\n got: %q\nwant: %q", fq.lastSQL, reclaimVoyagesSQL)
	}
	if len(ad.events) != 5 {
		t.Errorf("audit events = %d, want 5 (per-row voyage.reclaimed)", len(ad.events))
	}
}

func TestVoyageReclaimer_Run_NoRows(t *testing.T) {
	fq := &fakeVoyagesQuerier{rows: nil}
	ad := &fakeReclaimAudit{}
	r := newVoyageReclaimerFromQuerier(fq, ad, silentLogger())

	got, err := r.Run(context.Background(), time.Minute, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 0 {
		t.Errorf("affected = %d, want 0", got)
	}
	if len(ad.events) != 0 {
		t.Errorf("audit events = %d, want 0", len(ad.events))
	}
}

func TestVoyageReclaimer_Run_NilAuditSafe(t *testing.T) {
	fq := &fakeVoyagesQuerier{rows: []reclaimRow{{voyageID: "v1", attempt: 2}}}
	r := newVoyageReclaimerFromQuerier(fq, nil, silentLogger())

	got, err := r.Run(context.Background(), time.Minute, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 1 {
		t.Errorf("affected = %d, want 1", got)
	}
}

func TestVoyageReclaimer_Run_PropagatesError(t *testing.T) {
	want := errors.New("pg down")
	fq := &fakeVoyagesQuerier{err: want}
	r := newVoyageReclaimerFromQuerier(fq, nil, silentLogger())

	_, err := r.Run(context.Background(), time.Minute, 1000)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

func TestVoyageReclaimer_Run_NilPoolReturnsError(t *testing.T) {
	r := &VoyageReclaimer{pool: nil}
	if _, err := r.Run(context.Background(), time.Minute, 1000); err == nil {
		t.Fatal("ожидалась ошибка при nil pool")
	}
}

// TestReclaimVoyagesSQL_Shape — sanity SQL-инвариантов: CTE picked снимает
// дореклеймное last_renewed_at, UPDATE по running с expired claim, сброс
// claim-полей, attempt++, возврат в pending (не scheduled), FOR UPDATE SKIP
// LOCKED, RETURNING attempt.
func TestReclaimVoyagesSQL_Shape(t *testing.T) {
	for _, frag := range []string{
		"WITH picked AS (",
		"SELECT voyage_id, last_renewed_at",
		"UPDATE voyages v",
		"SET status           = 'pending'",
		"claimed_by_kid   = NULL",
		"last_renewed_at  = NULL",
		"claim_expires_at = NULL",
		"attempt          = attempt + 1",
		// reclaim откатывает current_batch_index в 0 — переисполнение прогона с нуля
		// (idempotent re-apply; resume-from-batch — отдельный эпик).
		"current_batch_index = 0",
		"WHERE status = 'running' AND claim_expires_at < NOW()",
		"FOR UPDATE SKIP LOCKED",
		"RETURNING v.voyage_id, v.attempt",
	} {
		if !strings.Contains(reclaimVoyagesSQL, frag) {
			t.Errorf("reclaimVoyagesSQL missing %q\nSQL: %s", frag, reclaimVoyagesSQL)
		}
	}
}
