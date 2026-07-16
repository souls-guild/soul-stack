package voyageorch

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// fakeOrphanReleaser — stub [OrphanLockReleaser]: captures passed arguments
// and returns pre-set (released, err).
type fakeOrphanReleaser struct {
	released bool
	err      error

	calls          int
	gotVoyageID    string
	gotIncarnation string
	gotAttempt     int
	gotKID         string
	gotApplyID     string
}

func (f *fakeOrphanReleaser) ReleaseOrphanLock(_ context.Context, voyageID, name string, attempt int, kid, orphanApplyID string) (bool, error) {
	f.calls++
	f.gotVoyageID = voyageID
	f.gotIncarnation = name
	f.gotAttempt = attempt
	f.gotKID = kid
	f.gotApplyID = orphanApplyID
	return f.released, f.err
}

// targetRows — fake pgx.Rows over slice of voyage_targets rows (8 columns in order of
// selectTargetsSQL: voyage_id, target_kind, target_id, batch_index, status,
// apply_id, errand_id, finished_at).
type targetRows struct {
	rows [][]any
	i    int
}

func (r *targetRows) Next() bool { r.i++; return r.i <= len(r.rows) }

func (r *targetRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(dest) != len(row) {
		return errors.New("targetRows: dest/row len mismatch")
	}
	for i, v := range row {
		switch p := dest[i].(type) {
		case *string:
			*p = v.(string)
		case **string:
			if v == nil {
				*p = nil
			} else {
				s := v.(string)
				*p = &s
			}
		case *int:
			*p = v.(int)
		default:
			// finished_at (**time.Time) — always nil in these tests.
			if v != nil {
				return errors.New("targetRows: unexpected non-nil for unsupported dest")
			}
		}
	}
	return nil
}

func (r *targetRows) Close()                                       {}
func (r *targetRows) Err() error                                   { return nil }
func (r *targetRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *targetRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *targetRows) Values() ([]any, error)                       { return nil, nil }
func (r *targetRows) RawValues() [][]byte                          { return nil }
func (r *targetRows) Conn() *pgx.Conn                              { return nil }

// targetRow — one row of voyage_targets for fake-Query.
func targetRow(voyageID, kind, targetID, status string, applyID *string) []any {
	var ap any
	if applyID != nil {
		ap = *applyID
	}
	return []any{voyageID, kind, targetID, 0, status, ap, nil, nil}
}

func strptr(s string) *string { return &s }

// withTargets configures fakeDB.Query to return specified voyage_targets.
func withTargets(f *fakeDB, rows ...[]any) {
	f.queryFn = func(_ string, _ []any) (pgx.Rows, error) {
		return &targetRows{rows: rows}, nil
	}
}

// TestReconcileOrphanLock_NilReleaser — detection disabled (OrphanReleaser=nil):
// reconcileOrphanLock — no-op, SelectTargets not even called (Query not
// configured → if called, would error). nil error.
func TestReconcileOrphanLock_NilReleaser(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{} // Query "not configured" — will error if detection calls it
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger()}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock (nil releaser): %v", err)
	}
}

// TestReconcileOrphanLock_OrphanRunning_Released — orphan from previous attempt
// (target in running with back-link apply_id) → releaser called with this apply_id,
// voyage_id, attempt, kid; released=true → nil error.
func TestReconcileOrphanLock_OrphanRunning_Released(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusRunning), strptr("ap-orphan")))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 1 {
		t.Fatalf("releaser calls = %d, want 1", rel.calls)
	}
	if rel.gotApplyID != "ap-orphan" {
		t.Errorf("orphan apply_id = %q, want ap-orphan", rel.gotApplyID)
	}
	if rel.gotVoyageID != "v1" || rel.gotIncarnation != "a" || rel.gotAttempt != 2 || rel.gotKID != "k" {
		t.Errorf("releaser args = (voyage=%q inc=%q attempt=%d kid=%q)",
			rel.gotVoyageID, rel.gotIncarnation, rel.gotAttempt, rel.gotKID)
	}
}

// TestReconcileOrphanLock_TerminalTarget_NoOp — target already terminal
// (succeeded): incarnation not hanging → releaser NOT called (nothing to release).
func TestReconcileOrphanLock_TerminalTarget_NoOp(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusSucceeded), strptr("ap-done")))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 0 {
		t.Errorf("releaser calls = %d, want 0 (terminal target — nothing to release)", rel.calls)
	}
}

// TestReconcileOrphanLock_NoBacklink_NoOp — target in awaiting without apply_id
// (spawn of previous attempt did not reach MarkTargetRunning): releaser NOT called.
func TestReconcileOrphanLock_NoBacklink_NoOp(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusAwaiting), nil))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 0 {
		t.Errorf("releaser calls = %d, want 0 (no back-link — not our orphan)", rel.calls)
	}
}

// TestReconcileOrphanLock_LegitInLockWindow_NotPickedUp — guard on "reaudit
// minor": legit-run (direct run / FOREIGN Voyage), caught in window of
// lockRun→Insert(apply_runs) — applying committed, apply_runs not yet, its
// apply_id=ap-legit — NOT released by orphan-seam. Reason: reconcileOrphanLock
// takes orphan-candidate ONLY from voyage_targets of THIS Voyage (back-link of
// previous attempt). voyage_targets of our Voyage does NOT contain row of incarnation
// "a" with apply_id=ap-legit (it belongs to foreign run — here we have row of
// different incarnation "b") → orphanApplyID not found → releaser NOT called. Protection
// depends on FENCING-2/3 pairing (back-link under attempt-CAS), not just
// FENCING-1 (live-rival apply_runs-EXISTS): even if apply_runs still empty
// (window before Insert), absence of back-link under our voyage_id already cuts off foreign
// lock at detection stage, before reaching releaser.
func TestReconcileOrphanLock_LegitInLockWindow_NotPickedUp(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// voyage_targets of THIS Voyage contains only row of incarnation "b". Incarnation
	// "a" (where legit-run with ap-legit hangs) in targets of our Voyage does NOT
	// appear — back-link under our voyage_id for "a" absent.
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "b",
		string(voyage.TargetStatusRunning), strptr("ap-legit")))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	// Reconciliation for "a" (our re-run targets it) — "a" has no back-link → no-op.
	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 0 {
		t.Errorf("releaser calls = %d, want 0 (legit-run of foreign apply_id without back-link under our voyage NOT released)", rel.calls)
	}
}

// TestReconcileOrphanLock_ReleaserError_Propagates — CRUD failure of releaser
// (e.g. VerifyOwnership transient) propagated to caller (→ fail-closed
// per incarnation in runOneIncarnation).
func TestReconcileOrphanLock_ReleaserError_Propagates(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusRunning), strptr("ap-orphan")))
	rel := &fakeOrphanReleaser{err: errors.New("pg down")}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	err := w.reconcileOrphanLock(context.Background(), run, "a")
	if err == nil {
		t.Fatal("reconcileOrphanLock: want error, got nil")
	}
}

// TestReconcileOrphanLock_ReleaserNoOp_NoError — releaser returned released=false
// (fenced no-op: not applying / we reclaimed / apply_id-mismatch) → nil error,
// caller continues re-run (lockRun will reject if not runnable).
func TestReconcileOrphanLock_ReleaserNoOp_NoError(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusRunning), strptr("ap-orphan")))
	rel := &fakeOrphanReleaser{released: false}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock (no-op): %v", err)
	}
	if rel.calls != 1 {
		t.Errorf("releaser calls = %d, want 1", rel.calls)
	}
}
