package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The two rules of [MaintenancePurger]. They are not `reaper.rules` entries:
// their switch is the retention itself (`maintenance.*`, 0 = never), a runtime
// setting (ADR-0073), so they run on every pass of an enabled Reaper.
const (
	ruleExpiredMaintenanceWindows = "purge_expired_maintenance_windows"
	ruleExpiredFreezes            = "purge_expired_freezes"
)

// MaintenancePurger deletes one-off maintenance windows and freezes past their
// retention (ADR-0093, NIM-1137) and audits each deletion as
// `maintenance_window.deleted` / `freeze.deleted` with `reason: "expired"`, the
// Reaper as the actor (source keeper_internal, no archon). audit is nil-safe.
type MaintenancePurger struct {
	pool   maintenance.DB
	audit  audit.Writer
	logger *slog.Logger
}

// NewMaintenancePurger constructs the purger over the Keeper pool.
func NewMaintenancePurger(pool *pgxpool.Pool, aud audit.Writer, logger *slog.Logger) *MaintenancePurger {
	return &MaintenancePurger{pool: pool, audit: aud, logger: logger}
}

// PurgeWindows deletes one batch of once windows that ended more than retention
// ago. The signature is runDurationRule's.
func (p *MaintenancePurger) PurgeWindows(ctx context.Context, retention time.Duration, batchSize int) (int64, error) {
	windows, err := maintenance.DeleteExpiredWindows(ctx, p.pool, retention, batchSize)
	if err != nil {
		return 0, fmt.Errorf("reaper.%s: %w", ruleExpiredMaintenanceWindows, err)
	}
	for _, w := range windows {
		p.emit(ctx, audit.EventMaintenanceWindowDeleted, maintenance.WindowAuditPayload(w, nil), w.ID)
	}
	return int64(len(windows)), nil
}

// PurgeFreezes deletes one batch of freezes that ended more than retention ago.
func (p *MaintenancePurger) PurgeFreezes(ctx context.Context, retention time.Duration, batchSize int) (int64, error) {
	freezes, err := maintenance.DeleteExpiredFreezes(ctx, p.pool, retention, batchSize)
	if err != nil {
		return 0, fmt.Errorf("reaper.%s: %w", ruleExpiredFreezes, err)
	}
	for _, f := range freezes {
		p.emit(ctx, audit.EventFreezeDeleted, maintenance.FreezeAuditPayload(f, nil), f.ID)
	}
	return int64(len(freezes)), nil
}

// emit writes one deletion after the delete has committed. Best-effort, as for
// every Reaper rule: the row is gone whether or not its event is written.
func (p *MaintenancePurger) emit(ctx context.Context, ev audit.EventType, payload map[string]any, id string) {
	if p.audit == nil {
		return
	}
	payload["reason"] = maintenance.ExpiredReason
	err := p.audit.Write(ctx, &audit.Event{EventType: ev, Source: audit.SourceKeeperInternal, Payload: payload})
	if err != nil && p.logger != nil {
		p.logger.Warn("reaper: audit write failed", slog.String("event", string(ev)),
			slog.String("id", id), slog.Any("error", err))
	}
}

// dispatchPurgeExpiredMaintenance runs the two rules of [MaintenancePurger],
// each skipped while its retention is 0. Called once per pass after the rules
// of `reaper.rules`; `reaper.dry_run` and `reaper.batch_size` apply as to them.
func (r *Runner) dispatchPurgeExpiredMaintenance(ctx context.Context, cfg *config.KeeperConfig, batchSize int, dryRun bool) {
	p := r.deps.ExpiredMaintenance
	if p == nil {
		r.deps.Logger.Warn("reaper: expired maintenance windows and freezes skipped: ExpiredMaintenance is not configured")
		return
	}
	for _, rule := range []struct {
		name      string
		retention time.Duration
		call      func(context.Context, time.Duration, int) (int64, error)
	}{
		{ruleExpiredMaintenanceWindows, cfg.Maintenance.ResolvedWindowRetention(), p.PurgeWindows},
		{ruleExpiredFreezes, cfg.Maintenance.ResolvedFreezeRetention(), p.PurgeFreezes},
	} {
		if rule.retention == 0 {
			continue
		}
		r.runDurationRule(ctx, rule.name, "", rule.retention, batchSize, dryRun, rule.call)
	}
}
