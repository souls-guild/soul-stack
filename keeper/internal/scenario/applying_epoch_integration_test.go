//go:build integration

package scenario

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// TestIntegration_LockApplyingWithEpoch_Atomic — guard on the ADR-027 amend
// (m-S1) task (a) invariant: transitioning an incarnation to applying via ONE
// UPDATE/one tx sets ALL 4 epoch columns. After the transition there's no
// "applying without epoch" path: status='applying' AND
// applying_apply_id/applying_attempt/applying_by_kid/applying_since are all
// non-null in ONE row. This closes the window where
// reconcile_orphan_applying would mistake the row for legacy-NULL and fail to
// reclaim an orphaned lock.
func TestIntegration_LockApplyingWithEpoch_Atomic(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "epoch-prod"
		applyID = "01HEPOCHAPPLY00000000001"
		kid     = "keeper-owner-01"
	)
	seedIncarnation(t, name)

	if err := pgx.BeginFunc(ctx, integrationPool, func(tx pgx.Tx) error {
		return lockApplyingWithEpoch(ctx, tx, name, applyID, kid, 0)
	}); err != nil {
		t.Fatalf("lockApplyingWithEpoch: %v", err)
	}

	var (
		status   string
		gotApply *string
		gotAtt   *int
		gotKID   *string
		gotSince *string // TIMESTAMPTZ → text-cast, only non-null-ness matters
	)
	const q = `
SELECT status, applying_apply_id, applying_attempt, applying_by_kid,
       applying_since::text
FROM incarnation WHERE name = $1`
	if err := integrationPool.QueryRow(ctx, q, name).Scan(
		&status, &gotApply, &gotAtt, &gotKID, &gotSince,
	); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if status != "applying" {
		t.Errorf("status = %q, want applying", status)
	}
	// All 4 epoch columns non-null in one row — no applying-without-epoch window.
	if gotApply == nil || *gotApply != applyID {
		t.Errorf("applying_apply_id = %v, want %q (epoch не записан атомарно)", gotApply, applyID)
	}
	if gotAtt == nil || *gotAtt != 0 {
		t.Errorf("applying_attempt = %v, want 0 (echo начального attempt)", gotAtt)
	}
	if gotKID == nil || *gotKID != kid {
		t.Errorf("applying_by_kid = %v, want %q", gotKID, kid)
	}
	if gotSince == nil {
		t.Error("applying_since = NULL, want NOW() (epoch неполный)")
	}
}

// TestIntegration_LockApplyingWithEpoch_FromLocked — guard on the ADR-027
// amend (m-S1) FromLocked invariant: a rerun-last start writes the epoch onto
// an already-applying row. UnlockForRerun transitions error_locked→applying
// WITHOUT an epoch (NULL columns); lockRun{FromLocked:true} must fill in
// applying_apply_id/applying_attempt/applying_by_kid/applying_since via the
// same lockApplyingWithEpoch (parity with a regular lockRun). Without this,
// a rerun-last-applying row would stay NULL-epoch → missed by
// reconcile_orphan_applying → owner crash mid-rerun-last = orphaned forever.
func TestIntegration_LockApplyingWithEpoch_FromLocked(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "epoch-fromlocked"
		applyID = "01HEPOCHFROMLOCKED0000001"
		kid     = "keeper-rerun-owner-01"
	)
	seedOperator(t, "archon-alice")
	// The row starts from error_locked + the last failed scenario = create
	// (scope=create gate in UnlockForRerun).
	inc := &incarnation.Incarnation{
		Name: name, Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusErrorLocked,
	}
	if err := incarnation.Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}
	seedCreateHistory(t, name)

	// The unlock part of rerun-last: error_locked→applying bypassing ready
	// (race-free), WITHOUT an epoch — applying_* stays NULL after this step.
	if _, err := incarnation.UnlockForRerun(ctx, integrationPool,
		name, "rerun bootstrap verified", "archon-alice", applyID, applyID); err != nil {
		t.Fatalf("UnlockForRerun: %v", err)
	}

	// A minimal Runner with a given KID — the FromLocked branch of lockRun
	// only uses DB + r.kid (never reaches render/dispatch, returns right
	// after writing the epoch).
	r := &Runner{deps: Deps{DB: integrationPool}, kid: kid}
	got, err := r.lockRun(ctx, RunSpec{
		ApplyID:         applyID,
		IncarnationName: name,
		ServiceRef:      artifact.ServiceRef{Name: "noop", Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
		FromLocked:      true,
	})
	if err != nil {
		t.Fatalf("lockRun{FromLocked}: %v", err)
	}
	if got.Status != incarnation.StatusApplying {
		t.Errorf("lockRun вернул status = %q, want applying", got.Status)
	}

	var (
		status   string
		gotApply *string
		gotAtt   *int
		gotKID   *string
		gotSince *string
	)
	const q = `
SELECT status, applying_apply_id, applying_attempt, applying_by_kid,
       applying_since::text
FROM incarnation WHERE name = $1`
	if err := integrationPool.QueryRow(ctx, q, name).Scan(
		&status, &gotApply, &gotAtt, &gotKID, &gotSince,
	); err != nil {
		t.Fatalf("read back: %v", err)
	}

	// Status stays applying (a repeat set is idempotent), the epoch is filled in.
	if status != "applying" {
		t.Errorf("status = %q, want applying (FromLocked не должен ломать статус)", status)
	}
	// Parity with a regular lockRun: all 4 epoch columns non-null —
	// rerun-last-applying is covered by reconcile_orphan_applying.
	if gotApply == nil || *gotApply != applyID {
		t.Errorf("applying_apply_id = %v, want %q (epoch не записан на FromLocked-пути)", gotApply, applyID)
	}
	if gotAtt == nil || *gotAtt != 0 {
		t.Errorf("applying_attempt = %v, want 0 (echo начального attempt)", gotAtt)
	}
	if gotKID == nil || *gotKID != kid {
		t.Errorf("applying_by_kid = %v, want %q (KID этого инстанса)", gotKID, kid)
	}
	if gotSince == nil {
		t.Error("applying_since = NULL, want NOW() (epoch неполный)")
	}
}

// TestIntegration_LockApplyingWithEpoch_RollbackLeavesNoEpoch — atomicity from
// the other side: if the tx rolls back (simulating a crash BEFORE commit),
// the row stays in its original status WITHOUT an epoch — no
// "half-written" applying flag.
func TestIntegration_LockApplyingWithEpoch_RollbackLeavesNoEpoch(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const name = "epoch-rollback"
	seedIncarnation(t, name)

	tx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := lockApplyingWithEpoch(ctx, tx, name, "apply-x", "keeper-x", 0); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("lockApplyingWithEpoch: %v", err)
	}
	// A crash before commit — the whole UPDATE rolls back (both status and epoch).
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	var (
		status   string
		gotApply *string
		gotKID   *string
	)
	const q = `SELECT status, applying_apply_id, applying_by_kid FROM incarnation WHERE name = $1`
	if err := integrationPool.QueryRow(ctx, q, name).Scan(&status, &gotApply, &gotKID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "ready" {
		t.Errorf("status = %q, want ready (rollback откатил applying)", status)
	}
	if gotApply != nil || gotKID != nil {
		t.Errorf("epoch persisted after rollback: apply=%v kid=%v (должны быть NULL)", gotApply, gotKID)
	}
}
