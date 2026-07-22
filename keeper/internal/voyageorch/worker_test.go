package voyageorch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// fakeDB stub for [voyage.ExecQueryRower] (parity errandrunorch::fakeDB).
type fakeDB struct {
	mu sync.Mutex

	queryRowFn     func(sql string, args []any) pgx.Row
	execTagFn      func(sql string) (pgconn.CommandTag, error)
	claimCallCount atomic.Int64
	finalCallCount atomic.Int64
	finalStatusArg atomic.Value // string

	// targetUpdates capture MarkTargetTerminal: by target_id (args[2]) →
	// last written status (args[3]). Protected by mu.
	targetUpdates map[string]string

	// recordRunningArgs enables capture of MarkTargetRunning (running-transition):
	// runningSQL = SQL, runningBacklink = back-link arg ($4). Protected by mu.
	recordRunningArgs bool
	runningSQL        string
	runningBacklink   string

	// verifyLeaseLost VerifyOwnership (verifyOwnershipSQL) returns ErrNoRows
	// (→ ErrLeaseLost). Controls fencing-check before dispatch (S-med-2).
	// false → VerifyOwnership succeeds (worker still owner).
	verifyLeaseLost atomic.Bool
	// verifyCalls count of VerifyOwnership calls (per-unit fencing, S-med-2):
	// one dispatch of unit = one ownership check.
	verifyCalls atomic.Int64

	// batchProgress capture UpdateBatchProgress: ordered list of
	// completedBatches args (current_batch_index) of each UPDATE. Protected by mu.
	batchProgress []int

	// queryFn optional hook for Query (SelectTargets in reconcileOrphanLock).
	// nil → default "not configured"-error (executeScenario-tests don't use Query).
	// Protected by mu when set from test before run.
	queryFn func(sql string, args []any) (pgx.Rows, error)
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	// UpdateBatchProgress (updateBatchProgressSQL): UPDATE voyages SET
	// current_batch_index. Differs from Finalize (finished_at) and MarkTarget*
	// (UPDATE voyage_targets). args = (id, kid, attempt, completedBatches).
	if strings.Contains(sql, "current_batch_index") && strings.Contains(sql, "UPDATE voyages") {
		f.mu.Lock()
		if len(args) >= 4 {
			if n, ok := args[3].(int); ok {
				f.batchProgress = append(f.batchProgress, n)
			}
		}
		f.mu.Unlock()
	}
	if strings.Contains(sql, "finished_at      = NOW()") {
		f.finalCallCount.Add(1)
		if len(args) >= 3 {
			if s, ok := args[2].(string); ok {
				f.finalStatusArg.Store(s)
			}
		}
	}
	if strings.Contains(sql, "UPDATE voyage_targets") && len(args) >= 4 {
		f.mu.Lock()
		switch {
		case strings.Contains(sql, "finished_at = NOW()"):
			// MarkTargetTerminal: args[3] = terminal status.
			if f.targetUpdates == nil {
				f.targetUpdates = map[string]string{}
			}
			tid, _ := args[2].(string)
			st, _ := args[3].(string)
			f.targetUpdates[tid] = st
		case strings.Contains(sql, "status       = 'awaiting'") && f.recordRunningArgs:
			// MarkTargetRunning: args[3] = back-link id (apply_id / errand_id).
			f.runningSQL = sql
			f.runningBacklink, _ = args[3].(string)
		}
		f.mu.Unlock()
	}
	f.mu.Lock()
	fn := f.execTagFn
	f.mu.Unlock()
	if fn != nil {
		return fn(sql)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// targetStatus reads captured last status of target (thread-safe).
func (f *fakeDB) targetStatus(tid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.targetUpdates[tid]
}

// batchProgressSeq returns copy of current_batch_index-update sequence.
func (f *fakeDB) batchProgressSeq() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.batchProgress...)
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FOR UPDATE SKIP LOCKED") {
		f.claimCallCount.Add(1)
	}
	// verifyOwnershipSQL (VerifyOwnership, S-med-2 fencing): `SELECT 1 ... WHERE
	// claimed_by_kid = ...`. Under verifyLeaseLost → no-rows (→ ErrLeaseLost),
	// else owner-row (Scan 1). Checked before queryRowFn: claim-loop-fake does not
	// model it.
	if strings.Contains(sql, "SELECT 1") && strings.Contains(sql, "claimed_by_kid") {
		f.verifyCalls.Add(1)
		return ownerScanRow{noRows: f.verifyLeaseLost.Load()}
	}
	f.mu.Lock()
	fn := f.queryRowFn
	f.mu.Unlock()
	if fn != nil {
		return fn(sql, args)
	}
	return errRow{err: pgx.ErrNoRows}
}

// ownerScanRow pgx.Row for VerifyOwnership: Scan(*int)=1 (owner) or
// pgx.ErrNoRows (noRows — reclaim/lease lost).
type ownerScanRow struct{ noRows bool }

func (r ownerScanRow) Scan(dest ...any) error {
	if r.noRows {
		return pgx.ErrNoRows
	}
	if len(dest) > 0 {
		if p, ok := dest[0].(*int); ok {
			*p = 1
		}
	}
	return nil
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.mu.Lock()
	fn := f.queryFn
	f.mu.Unlock()
	if fn != nil {
		return fn(sql, args)
	}
	return nil, errors.New("fakeDB: Query not configured")
}

// CopyFrom not called by orchestrator (InsertTargets done by S5-handler via
// own tx), but required for voyage.ExecQueryRower interface (S-med-3).
func (f *fakeDB) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("fakeDB: CopyFrom not configured")
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// scanRow pgx.Row based on values array (one scanVoyage row:
// 26 columns per RETURNING claimNextSQL order).
type scanRow struct{ values []any }

func (r scanRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("scanRow: dest/values len mismatch")
	}
	for i, v := range r.values {
		dv := reflect.ValueOf(dest[i])
		if dv.Kind() != reflect.Ptr {
			return errors.New("scanRow: dest is not a pointer")
		}
		if v == nil {
			dv.Elem().Set(reflect.Zero(dv.Elem().Type()))
			continue
		}
		dv.Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

// claimedVoyageRow 31 values in scanVoyage order (26 base + 4 S-W3/S-W4
// + cadence_id back-link ADR-046). NULL fields = nil.
func claimedVoyageRow(id, kind, status string) scanRow {
	return scanRow{values: []any{
		id,                // 1  voyage_id
		kind,              // 2  kind
		(*string)(nil),    // 3  scenario_name
		(*string)(nil),    // 4  module
		[]byte(`{}`),      // 5  input
		[]byte(`["x"]`),   // 6  target_resolved
		[]byte(nil),       // 7  target_origin
		(*int)(nil),       // 8  batch_size
		(*int)(nil),       // 9  concurrency
		(*string)(nil),    // 10 batch_mode
		false,             // 11 dry_run
		(*time.Time)(nil), // 12 schedule_at
		(*float64)(nil),   // 13 inter_batch_interval (epoch secs)
		(*string)(nil),    // 14 on_failure
		1,                 // 15 total_batches
		0,                 // 16 current_batch_index
		status,            // 17 status
		(*string)(nil),    // 18 claimed_by_kid
		(*time.Time)(nil), // 19 last_renewed_at
		(*time.Time)(nil), // 20 claim_expires_at
		1,                 // 21 attempt
		"archon-alice",    // 22 started_by_aid
		time.Now().UTC(),  // 23 created_at
		(*time.Time)(nil), // 24 started_at
		(*time.Time)(nil), // 25 finished_at
		[]byte(nil),       // 26 summary
		(*int)(nil),       // 27 batch_percent
		(*int)(nil),       // 28 fail_threshold
		(*float64)(nil),   // 29 inter_unit_interval (epoch secs)
		(*bool)(nil),      // 30 require_alive
		(*string)(nil),    // 31 cadence_id (back-link ADR-046; manual run = NULL)
	}}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWorker_Validate(t *testing.T) {
	t.Parallel()
	base := func() *VoyageWorker {
		return &VoyageWorker{
			KID:           "kid-1",
			Pool:          &fakeDB{},
			LeaseTTL:      time.Minute,
			RenewInterval: 20 * time.Second,
			PollInterval:  time.Second,
			Logger:        quietLogger(),
		}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("valid worker: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*VoyageWorker)
		want string
	}{
		{"no kid", func(w *VoyageWorker) { w.KID = "" }, "KID"},
		{"no pool", func(w *VoyageWorker) { w.Pool = nil }, "Pool"},
		{"bad lease", func(w *VoyageWorker) { w.LeaseTTL = 0 }, "LeaseTTL"},
		{"bad renew", func(w *VoyageWorker) { w.RenewInterval = 0 }, "RenewInterval"},
		{"bad poll", func(w *VoyageWorker) { w.PollInterval = 0 }, "PollInterval"},
		{"no logger", func(w *VoyageWorker) { w.Logger = nil }, "Logger"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := base()
			tc.mut(w)
			err := w.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate() = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestWorker_DispatchCommandKind claim one pending Voyage kind=command (S3
// real execution) → fan-out Errands via CommandSpawner → Finalize.
// After first claim fake returns ErrNoRows, worker goes to poll-sleep, test
// cancels ctx. (kind=scenario see TestWorker_DispatchScenarioKind.)
func TestWorker_DispatchCommandKind(t *testing.T) {
	t.Parallel()
	var claimedOnce atomic.Bool
	fdb := &fakeDB{}
	fdb.queryRowFn = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FOR UPDATE SKIP LOCKED") {
			if claimedOnce.CompareAndSwap(false, true) {
				return commandClaimRow()
			}
		}
		return errRow{err: pgx.ErrNoRows}
	}

	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{
		KID:            "kid-1",
		Pool:           fdb,
		LeaseTTL:       time.Minute,
		RenewInterval:  time.Hour, // does not tick during the test
		PollInterval:   5 * time.Millisecond,
		Logger:         quietLogger(),
		CommandSpawner: sp,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for fdb.finalCallCount.Load() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Finalize not called within deadline")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got, _ := fdb.finalStatusArg.Load().(string); got != string(voyage.StatusSucceeded) {
		t.Errorf("finalize status = %q, want succeeded", got)
	}
	sp.mu.Lock()
	calls := len(sp.calls)
	sp.mu.Unlock()
	if calls != 2 {
		t.Errorf("spawn calls = %d, want 2 (two hosts in target_resolved)", calls)
	}
}
