//go:build integration

package scenario

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// TestIntegration_LockApplyingWithEpoch_Atomic — guard на инвариант ADR-027 amend
// (m-S1) задача (a): перевод incarnation в applying ОДНИМ UPDATE/одной tx
// выставляет ВСЕ 4 epoch-колонки. После транзита нет пути «applying без epoch»:
// status='applying' И applying_apply_id/applying_attempt/applying_by_kid/
// applying_since все НЕпустые в ОДНОЙ строке. Это закрывает окно, при котором
// reconcile_orphan_applying принял бы строку за legacy-NULL (и не реклеймнул бы
// осиротевший lock).
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
		gotSince *string // TIMESTAMPTZ → text-cast, важна лишь НЕ-null-ность
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
	// Все 4 epoch-колонки непустые в одной строке — нет окна applying-без-epoch.
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

// TestIntegration_LockApplyingWithEpoch_FromLocked — guard на инвариант ADR-027
// amend (m-S1) FromLocked: rerun-last-старт пишет epoch на уже-applying-строку.
// UnlockForRerun транзитит error_locked→applying БЕЗ epoch (NULL-колонки);
// lockRun{FromLocked:true} обязан дописать applying_apply_id/applying_attempt/
// applying_by_kid/applying_since тем же lockApplyingWithEpoch (parity с обычным
// lockRun). Без этого rerun-last-applying оставался бы NULL-epoch → не попадал
// под reconcile_orphan_applying → краш владельца mid-rerun-last = orphan навсегда.
func TestIntegration_LockApplyingWithEpoch_FromLocked(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "epoch-fromlocked"
		applyID = "01HEPOCHFROMLOCKED0000001"
		kid     = "keeper-rerun-owner-01"
	)
	seedOperator(t, "archon-alice")
	// Строка стартует из error_locked + последний упавший сценарий = create
	// (scope=create gate в UnlockForRerun).
	inc := &incarnation.Incarnation{
		Name: name, Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusErrorLocked,
	}
	if err := incarnation.Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}
	seedCreateHistory(t, name)

	// Unlock-часть rerun-last: error_locked→applying минуя ready (race-free),
	// БЕЗ epoch — applying_* остаются NULL после этого шага.
	if _, err := incarnation.UnlockForRerun(ctx, integrationPool,
		name, "rerun bootstrap verified", "archon-alice", applyID, applyID); err != nil {
		t.Fatalf("UnlockForRerun: %v", err)
	}

	// Минимальный Runner с заданным KID — FromLocked-ветка lockRun использует
	// только DB + r.kid (до render/dispatch не доходит, возвращает после epoch-записи).
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

	// Статус остаётся applying (повторный set идемпотентен), epoch дописан.
	if status != "applying" {
		t.Errorf("status = %q, want applying (FromLocked не должен ломать статус)", status)
	}
	// Parity с обычным lockRun: все 4 epoch-колонки непустые — rerun-last-applying
	// попадает под reconcile_orphan_applying.
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

// TestIntegration_LockApplyingWithEpoch_RollbackLeavesNoEpoch — атомарность с
// другой стороны: если tx откатилась (имитация краха ДО commit), строка остаётся
// в исходном статусе БЕЗ epoch — нет «полу-записанного» applying-флага.
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
	// Крах до commit — откат всего UPDATE (и status, и epoch).
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
