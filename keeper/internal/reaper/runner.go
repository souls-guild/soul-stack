// Runner — background loop of Reaper (see docs/keeper/reaper.md, ADR-006(d)).
//
// Leadership (Redis-lease acquire / renew `lock_ttl/3` / re-acquire on loss /
// graceful Release) is factored into generic [leaderloop.Loop] — common framework
// of HA-singleton tasks of Keeper cluster. Runner — thin consumer: provides
// leaseKey, tick-callback ([Runner.dispatch]) and hot-reload functions of interval and
// lock_ttl on top of its cfg-cache. lease-semantics unchanged after factoring.
//
// Runner's responsibilities:
//
//   - rule dispatch: tick → read fresh cfg → execute enabled rules.
//     `time.Ticker` interval = `reaper.interval` (cron-grammar not needed).
//   - Hot-reload — Runner subscribes to Store via [config.Store.OnReload] and
//     caches fresh snapshot in `atomic.Pointer[KeeperConfig]`. tick/intervalFn/
//     lockTTLFn read atomic-pointer without Store access; subscriber updates
//     pointer **immediately** on swap. `enabled`/`interval`/`lock_ttl`/`max_age`/
//     `batch_size`/`dry_run` updated without restart.
//   - lease-Gauge `keeper_reaper_lease_held` — via OnLeaseChange-callback
//     of leaderloop (1 on capture, 0 on exit from tick-loop).
//
// Restart-policy on lease loss — inside leaderloop (re-acquire); caller
// (runDaemon) just runs [Runner.Run] until SIGTERM.
package reaper

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/leaderloop"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/config"
)

// LeaderLeaseKey — Redis key for Reaper leadership. Fixed in
// docs/keeper/reaper.md and not placed in config — this is cluster invariant
// (one rule = one key, changing requires ADR update). Exported so that
// `GET /v1/cluster` can read the leader holder (key value = KID
// of current Reaper leader) without duplicating literal.
const LeaderLeaseKey = "reaper:leader"

// Defaults for empty fields in keeper.yml (parser leaves zero-value).
// Match docs/keeper/reaper.md (Config).
const (
	defaultInterval       = time.Hour
	defaultLockTTL        = 5 * time.Minute
	defaultRuleBatch      = 1000
	defaultPurgeMaxAge    = 365 * 24 * time.Hour
	defaultAcquireBackoff = 5 * time.Second

	// Per-rule defaults max_age / stale_after — docs/keeper/reaper.md.
	// Used if cfg-value is empty (semantic-validate already
	// rejects invalid format; empty is normal for omitting field).
	defaultExpirePendingSeedsMaxAge = 24 * time.Hour
	defaultPurgeUsedTokensMaxAge    = 90 * 24 * time.Hour
	defaultPurgeSoulsMaxAge         = 30 * 24 * time.Hour
	defaultPurgeOldSeedsMaxAge      = 90 * 24 * time.Hour
	// Retention of history of service cert rotations (`purge_old_certs`, R4,
	// cert-rotation Var1). 90d — parity defaultPurgeOldSeedsMaxAge (same class
	// "cert-material history", keep trail of rotations long enough for
	// incident investigation).
	defaultPurgeOldCertsMaxAge   = 90 * 24 * time.Hour
	defaultMarkDisconnectedStale = 90 * time.Second
	defaultPurgeApplyRunsMaxAge  = 30 * 24 * time.Hour

	// Retention of Voyage-run history (`purge_voyages`, ADR-046 §79). 30d
	// deliberately ALIGNED with defaultPurgeApplyRunsMaxAge: voyage_targets
	// carry soft-link apply_id to apply_runs (no FK), and drill "voyage →its
	// apply_runs" must see both sides until one moment — otherwise voyage
	// deleted while apply_runs still live (or vice versa), and All-runs view loses
	// correlation. Changing one window without other — drill desynchronization.
	defaultPurgeVoyagesMaxAge = 30 * 24 * time.Hour

	// Retention of growing run-history of push-runs (`purge_push_runs`,
	// migration 076). 30d — parity with defaultPurgeApplyRunsMaxAge: push_runs —
	// run-history table of same class as apply_runs/voyages, and keeping
	// its tail in same window avoids desync when drilling "push-run
	// → its per-host summary". Do NOT confuse with TTL of rule
	// `purge_orphan_push_runs` (1h, zombie termination) — different rules,
	// different windows.
	defaultPurgePushRunsMaxAge = 30 * 24 * time.Hour

	// Retention of compliance-class archived data (`purge_incarnation_archive` /
	// `purge_state_history_archive` / `purge_archived_state_history`, migration
	// 077). 365d — deliberately CONSERVATIVE than run-history windows (30d): archive
	// (incarnation_archive / state_history_archive from 039 + soft-deleted
	// state_history from 048) — this is historical-compliance data of deleted
	// incarnations, which operator may keep year per audit requirements.
	// Configured via keeper.yml → reaper.rules.<rule>.max_age; in example
	// set to 365d with note "compliance-window". Age — from archived_at.
	defaultPurgeArchiveMaxAge = 365 * 24 * time.Hour

	// Grace after apply_run terminal, after which register-rows
	// of run are deleted. Short (1h, not 30d like apply_run itself): register
	// needed only until barrier, after terminal it's plaintext-garbage. Certainly
	// longer than time "barrier → reading register" in cross-Keeper routing.
	defaultPurgeApplyTaskRegisterGrace = time.Hour

	// Grace for apply_run_plan (NIM-37): run plan needed to /tasks endpoint as long as
	// apply-history lives — aligned with defaultPurgeApplyRunsMaxAge (30d),
	// otherwise /tasks would lose plan before RunDetail loses run itself. FK-cascade for plan
	// doesn't exist — without rule plan would grow as orphans.
	defaultPurgeApplyRunPlanGrace = defaultPurgeApplyRunsMaxAge

	// Formal duration-argument of reclaim_apply_runs rule: recovery
	// compares claim_expires_at < NOW() directly (lease already embedded in
	// claim_expires_at when capturing Ward), so this value doesn't enter predicate.
	// Keep meaningful default for consistency of duration-runner.
	defaultReclaimApplyRunsLease = time.Minute

	// Grace by age of Vault-secret for reap_orphan_vault_keys: secret
	// `secret/keeper/sigil-keys/<key_id>` is considered orphaned only if it
	// older than this threshold. Cuts race with Introduce (write-to-Vault before
	// PG-commit): fresh secret may still get row in sigil_signing_keys.
	// 24h — generous margin beyond any realistic Introduce window.
	defaultReapOrphanVaultKeysGrace = 24 * time.Hour

	// Defaults of archive_state_history rule (ADR-Q19 retention,
	// docs/keeper/reaper.md). N=50 — product decision: enough so
	// Operator API can always show "last month of activity"
	// of typical incarnation (1-2 apply per day), and small enough so tail of
	// state_history doesn't grow without archiving. keep_version_bump=true — protection of snapshots
	// of state_schema-migration steps (scenario='migration') from soft-delete;
	// restorable anchor for schema recovery on rollback ADR-019.
	defaultArchiveStateHistoryKeepLastN       = 50
	defaultArchiveStateHistoryKeepVersionBump = true
)

// Default statuses for rules with statuses[]-filter (docs/keeper/reaper.md).
// Used if cfg.statuses empty — for example, operator enabled
// rule with single line `enabled: true` without overriding list.
var (
	defaultPurgeSoulsStatuses    = []string{"disconnected", "expired"}
	defaultPurgeOldSeedsStatuses = []string{"superseded", "expired", "revoked"}
	// purge_old_certs (R4): do NOT touch active/rotating (live material / cert in
	// rotation). superseded — replaced by rotation, expired — expired, failed
	// — failed rotation (settled outside active).
	defaultPurgeOldCertsStatuses = []string{"superseded", "expired", "failed"}
)

// PurgerAPI is the narrow interface called by Runner. Narrowed for
// unit tests: inject fake without standing up PG. Real [*Purger]
// satisfies it automatically.
type PurgerAPI interface {
	PurgeAuditOld(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeExpiredPendingTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeUsedTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeSouls(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error)
	PurgeOldSeeds(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error)
	PurgeOldCerts(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error)
	MarkDisconnected(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error)
	PurgeApplyRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeVoyages(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgePushRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeIncarnationArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeStateHistoryArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeArchivedStateHistory(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeApplyTaskRegister(ctx context.Context, gracePeriod time.Duration, batchSize int) (int64, error)
	PurgeApplyRunPlan(ctx context.Context, gracePeriod time.Duration, batchSize int) (int64, error)
	ReclaimApplyRuns(ctx context.Context, lease time.Duration, batchSize int) (int64, error)
	ReportOrphanVaultKeys(ctx context.Context, grace time.Duration, batchSize int) (int64, error)
	ArchiveStateHistory(ctx context.Context, keepLastN int, keepVersionBump bool, batchSize int) (int64, error)
}

// defaultPurgeOrphanPushRunsMaxAge is TTL of rule `purge_orphan_push_runs`
// (docs/keeper/reaper.md). Push run stuck in pending/running longer than an hour
// is almost certainly orphaned (Keeper instance died during execution, executeAsync
// did not write terminal). 1h is safely longer than a realistic push run
// (render+per-host SendApply under 30m at pilot scale); meaningful reserve.
const defaultPurgeOrphanPushRunsMaxAge = time.Hour

// defaultPurgeOldErrandsMaxAge is the formal duration argument of
// `purge_old_errands` rule (ADR-033, docs/keeper/reaper.md). TTL is stored in the
// row itself as `errands.ttl_at` (baked on INSERT by dispatcher, default
// `started_at + 7d`), so rule predicate is `ttl_at < NOW()` without
// additional offset. This value is NOT passed to SQL, but runner
// requires positive duration for the common parseRuleDuration path. Keep 7d
// for consistency with errand.TTLDefault.
const defaultPurgeOldErrandsMaxAge = 7 * 24 * time.Hour

// defaultReclaimVoyagesLease is the formal duration argument of
// `reclaim_voyages` rule (ADR-043 S4). Recovery compares `claim_expires_at < NOW()`
// directly (lease is baked into claim_expires_at when claimed through voyage.ClaimNext),
// value does NOT enter SQL predicate. Keep meaningful default for consistency
// of duration-runner.
const defaultReclaimVoyagesLease = time.Minute

// defaultReconcileOrphanApplyingStale is `stale_after` of
// `reconcile_orphan_applying` rule (ADR-027 amend (m)). applying row is considered
// stale candidate for orphan lock release if applying_since is older than
// this threshold. 90s — parity defaultMarkDisconnectedStale: same class
// "owner silent too long" (presence check completes decision). ENTERS SQL predicate
// (cutoff = NOW()-stale_after), unlike lease arguments of reclaim rules.
const defaultReconcileOrphanApplyingStale = 90 * time.Second

// defaultPurgeOrphanEphemeralTidingsGrace is grace of rule
// `purge_orphan_ephemeral_tidings` (ADR-052(g) amendment N2). `max_age`
// semantically = grace AFTER Voyage terminal and ENTERS predicate (like
// `purge_apply_task_register`). Grace is required for correctness:
// dispatcher matches terminal event against ephemeral rule asynchronously
// (tap-consumer goroutine through bounded channel, ADR-052(c)); removing rule before
// consumer reads event and enqueues notification would lose completion
// notification. 5m is safely longer than tap-consumer window (bounded channel
// drains in milliseconds even during finalization storm) with large reserve
// for drain/retry; shorter than 7d errand TTL because ephemeral Tiding after delivery
// is garbage and need not be kept.
const defaultPurgeOrphanEphemeralTidingsGrace = 5 * time.Minute

// Deps are Runner external dependencies. All fields are required except
// AcquireBackoff (default — [defaultAcquireBackoff]).
type Deps struct {
	// Purger executes SQL rules. In Reaper.a, the only
	// rule is `purge_audit_old`.
	Purger PurgerAPI

	// Redis is the client used to acquire leadership lease.
	Redis *redis.Client

	// Store is KeeperConfig snapshot with hot-reload semantics (M0.3).
	// Runner reads `cfg.Reaper.*` on every iteration.
	Store *config.Store[config.KeeperConfig]

	// Holder is Keeper instance identifier (KID), written into lease key.
	Holder string

	// Logger is slog logger. Structured fields: `key`, `holder`, `rule`,
	// `deleted`, `error`. Metrics via OTel are a separate slice (see reaper.md).
	Logger *slog.Logger

	// AcquireBackoff is pause between Acquire attempts on leadership
	// conflict. At production values ~5s is large enough to avoid
	// flooding Redis, and small enough for failover after leader loss
	// to happen within a few seconds. Field is in Deps (not
	// package-level var) so tests can replace it with a short value without
	// races under `go test -parallel`.
	//
	// Zero-value → [defaultAcquireBackoff].
	AcquireBackoff time.Duration

	// Metrics are Prometheus collectors for per-rule metrics (executions /
	// purged / duration / errors) and lease gauge. Nil is allowed — methods
	// on [*ReaperMetrics] no-op on nil receiver (for unit tests
	// of Runner without obs stack).
	Metrics *ReaperMetrics

	// Scry contains dependencies for background drift rule `scry_background`
	// (ADR-031 Slice C). Optional: nil → rule in dispatch
	// is skipped with warn (see runScryBackground). Production wire-up
	// assembles [ScryDeps] in daemon.setupReaper.
	Scry *ScryDeps

	// OrphanPushRuns is dependency of rule `purge_orphan_push_runs`
	// (Variant C push orchestrator, docs/keeper/push.md). Optional: nil →
	// rule in dispatch is skipped with warn (Scry pattern).
	// Production wire-up passes [*orphanPurger] from
	// [NewOrphanPushRunsPurger] over pushorch.Store.
	OrphanPushRuns *orphanPurger

	// OldErrands is dependency of rule `purge_old_errands` (ADR-033,
	// docs/keeper/reaper.md). Optional: nil → rule in dispatch
	// is skipped with warn (OrphanPushRuns pattern). Production wire-up
	// passes [*ErrandsPurger] from [NewErrandsPurger] over d.pool.
	OldErrands *ErrandsPurger

	// VoyageReclaim is dependency of rule `reclaim_voyages` (ADR-043 S4,
	// docs/keeper/reaper.md). Optional: nil → rule in dispatch
	// is skipped with warn (OldErrands pattern). Production wire-up passes
	// [*VoyageReclaimer] from [NewVoyageReclaimer] over d.pool.
	VoyageReclaim *VoyageReclaimer

	// OrphanEphemeralTidings is dependency of rule
	// `purge_orphan_ephemeral_tidings` (ADR-052(g) amendment N2,
	// docs/keeper/reaper.md). Removes orphaned ephemeral Tidings (run in
	// terminal > grace or missing). Optional: nil → rule in
	// dispatch is skipped with warn (VoyageReclaim pattern).
	// Production wire-up passes [*EphemeralTidingsPurger] from
	// [NewEphemeralTidingsPurger] over d.pool.
	OrphanEphemeralTidings *EphemeralTidingsPurger

	// CertRotator is dependency of rule `rotate_due_certs` (cert-rotation Var1).
	// Centralized rotation of expiring service certs: scan warrant by
	// not_after (jitter) → csrgen (R2) → Vault PKI sign → WriteKV → supersede+
	// insert warrant → spawn Voyage(rotate_tls). Rule DEFAULT OFF (map-driven,
	// requires explicit enabled:true in reaper.rules) + mandatory dry_run. nil →
	// rule degrades with warn (no Vault/PKI/PG — dev build). Production
	// wire-up passes [*CertRotator] from [NewCertRotator] (Vault-signer+writer+
	// csrgen).
	CertRotator *CertRotator

	// OrphanApplying is dependency of rule `reconcile_orphan_applying` (ADR-027
	// amend (m)). Releases orphaned applying-lock of incarnation from direct
	// (standalone, not under Voyage) scenario-run whose Keeper owner crashed:
	// stale applying row with NON-empty epoch + owner presence death in
	// Conclave → applying→ready. Rule is default-ON via path-defaulting
	// (dispatchReconcileOrphanApplying), like reclaim_voyages, so unconditional
	// wire-up is required. nil → rule degrades with warn (presence check
	// requires live Redis). Production wire-up passes
	// [*OrphanApplyingReconciler] from [NewOrphanApplyingReconciler].
	OrphanApplying *OrphanApplyingReconciler
}

// Runner is the root structure. One instance per keeper process.
type Runner struct {
	deps           Deps
	acquireBackoff time.Duration

	// currentCfg is atomic cache of latest successful snapshot from Store.
	// Filled in NewRunner and updated by callback subscribed
	// through [config.Store.OnReload] (see ADR-021 + Architecture E above).
	//
	// tick-loop reads only this pointer and does NOT access Store on
	// every tick: on successful reload subscriber updates cache
	// **immediately**, and next tick (or current acquire-loop iteration)
	// already sees new cfg without waiting for next tick latency.
	currentCfg atomic.Pointer[config.KeeperConfig]
}

// NewRunner validates deps and returns Runner. Missing required
// dependencies are errors: caller programming error
// (runDaemon), not a runtime condition.
func NewRunner(d Deps) (*Runner, error) {
	if d.Purger == nil {
		return nil, errors.New("reaper.NewRunner: Purger is required")
	}
	if d.Redis == nil {
		return nil, errors.New("reaper.NewRunner: Redis is required")
	}
	if d.Store == nil {
		return nil, errors.New("reaper.NewRunner: Store is required")
	}
	if d.Holder == "" {
		return nil, errors.New("reaper.NewRunner: Holder is required (use cfg.KID)")
	}
	if d.Logger == nil {
		return nil, errors.New("reaper.NewRunner: Logger is required")
	}
	backoff := d.AcquireBackoff
	if backoff <= 0 {
		backoff = defaultAcquireBackoff
	}
	r := &Runner{deps: d, acquireBackoff: backoff}
	// Fill cache with initial snapshot. It may be nil — valid
	// state "initial load failed validation", recovery via
	// first successful Reload + subscriber. tick-loop is protected from nil-cfg.
	r.currentCfg.Store(d.Store.Get())
	// Subscriber updates cache on every successful Reload swap.
	// Unsubscribe is not stored: Runner lives for whole process lifetime,
	// subscription does not need release (Store dies with Runner).
	d.Store.OnReload(func(_, newCfg *config.KeeperConfig) {
		r.currentCfg.Store(newCfg)
	})
	return r, nil
}

// Run runs leader-loop until ctx is canceled. Returns nil on
// graceful stop (ctx.Done) and wrapped error on fatal acquire-phase conditions.
//
// Leadership, renewal, re-acquire, and graceful shutdown are delegated to generic
// [leaderloop.Loop] (extracted from original Reaper runner; lease semantics
// identical: lock_ttl/3 renew, backoff, immediate-tick, Release-on-stop).
// Reaper remains a thin consumer: its tick callback is [Runner.dispatch]
// over fresh cfg snapshot, lease gauge via OnLeaseChange, hot-reload
// interval/lock_ttl via intervalFn/lockTTLFn over atomic cfg cache.
func (r *Runner) Run(ctx context.Context) error {
	loop, err := leaderloop.New(leaderloop.Config{
		LeaseKey:       LeaderLeaseKey,
		Holder:         r.deps.Holder,
		Redis:          r.deps.Redis,
		Logger:         r.deps.Logger,
		AcquireBackoff: r.acquireBackoff,
		IntervalFn:     r.tickInterval,
		LockTTLFn:      r.lockTTL,
		Tick:           r.tick,
		OnLeaseChange:  r.deps.Metrics.SetLeaseHeld,
	})
	if err != nil {
		// All required fields are checked by NewRunner → New should not
		// fail here. Propagate for contract desynchronization.
		return err
	}
	return loop.Run(ctx)
}

// tick is leaderloop tick-callback: reads fresh cfg snapshot and dispatches
// rules. Identical to original tickLoop body: nil-cfg (invalid initial load)
// is skipped with warn, without dropping loop.
func (r *Runner) tick(ctx context.Context) {
	cfg := r.currentCfg.Load()
	if cfg == nil {
		r.deps.Logger.Warn("reaper: config snapshot is nil, skipping tick")
		return
	}
	r.dispatch(ctx, cfg)
}

// tickInterval is intervalFn for leaderloop: interval between ticks from fresh
// cfg snapshot (hot-reload). nil-cfg → [defaultInterval] so leaderloop always
// gets valid positive interval (tick on nil-cfg will skip work itself).
func (r *Runner) tickInterval() time.Duration {
	cfg := r.currentCfg.Load()
	if cfg == nil {
		return defaultInterval
	}
	return parseDurationOr(cfg.Reaper, defaultInterval, reaperInterval)
}

// lockTTL is lockTTLFn for leaderloop: TTL of Redis lease from fresh cfg snapshot
// (hot-reload between re-acquire). nil-cfg → [defaultLockTTL].
func (r *Runner) lockTTL() time.Duration {
	cfg := r.currentCfg.Load()
	if cfg == nil {
		return defaultLockTTL
	}
	return parseDurationOr(cfg.Reaper, defaultLockTTL, reaperLockTTL)
}

// dispatch applies each enabled rule one by one. Knows
// all rules from docs/keeper/reaper.md; unknown name gets warn so
// typo in keeper.yml is not silent no-op. Exception:
// reclaim_voyages is default-ON through path-defaulting (ADR-043 §8),
// executed by separate dispatchReclaimVoyages branch after loop.
func (r *Runner) dispatch(ctx context.Context, cfg *config.KeeperConfig) {
	if cfg.Reaper == nil || !cfg.Reaper.Enabled {
		return
	}
	dryRun := cfg.Reaper.DryRun
	batchSize := cfg.Reaper.BatchSize
	if batchSize <= 0 {
		batchSize = defaultRuleBatch
	}

	for name, rule := range cfg.Reaper.Rules {
		// reclaim_voyages / reconcile_orphan_applying are default-ON through
		// path-defaulting in separate branches below (dispatchReclaimVoyages /
		// dispatchReconcileOrphanApplying); excluded from main loop so with
		// `Enabled:true` rule is not executed twice.
		if name == "reclaim_voyages" || name == "reconcile_orphan_applying" {
			continue
		}
		if !rule.Enabled {
			continue
		}
		switch name {
		case "purge_audit_old":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeAuditOld)
		case "expire_pending_seeds":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultExpirePendingSeedsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeExpiredPendingTokens)
		case "purge_used_tokens":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeUsedTokensMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeUsedTokens)
		case "purge_souls":
			r.runStatusesRule(ctx, name, rule.Statuses, defaultPurgeSoulsStatuses,
				rule.MaxAge, defaultPurgeSoulsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeSouls)
		case "purge_old_seeds":
			r.runStatusesRule(ctx, name, rule.Statuses, defaultPurgeOldSeedsStatuses,
				rule.MaxAge, defaultPurgeOldSeedsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeOldSeeds)
		case "purge_old_certs":
			// Retention of service cert rotation history (R4, cert-rotation Var1):
			// warrant in superseded/expired/failed older than max_age → DELETE;
			// active/rotating untouched (statuses filter). map-driven (OFF without
			// explicit enabled:true). Parity purge_old_seeds.
			r.runStatusesRule(ctx, name, rule.Statuses, defaultPurgeOldCertsStatuses,
				rule.MaxAge, defaultPurgeOldCertsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeOldCerts)
		case "rotate_due_certs":
			// Centralized rotation of expiring service certs (cert-rotation
			// Var1): scan warrant by not_after (jitter) → csrgen (R2) → Vault PKI
			// sign → WriteKV → supersede+insert warrant → spawn Voyage(rotate_tls).
			// DEFAULT OFF (map-driven; reach here only with explicit enabled:true).
			// R1 barrier: automatic replacement of production TLS without operator is risky,
			// so dry_run for this rule is PERSONAL (default true) and does NOT inherit
			// global reaper.dry_run; otherwise operator with reaper.dry_run:false (for
			// production purge) would silently get production rotation. Production rotation only
			// with explicit rotate_due_certs.dry_run:false. nil CertRotator → rule
			// degrades with warn (no Vault/PKI/PG). Policy (threshold/jitter/
			// cap) is inside CertRotator.cfg() from keeper.yml, not general
			// ReaperRule.MaxAge; here we pass formal duration argument (does not
			// enter predicate, parity with reclaim rules).
			if r.deps.CertRotator == nil {
				r.deps.Logger.Warn("reaper: rotate_due_certs skipped: CertRotator is not configured",
					slog.String("rule", name))
				continue
			}
			certDryRun := rule.DryRun == nil || *rule.DryRun
			r.runDurationRule(ctx, name, rule.MaxAge, defaultReclaimVoyagesLease, batchSize, certDryRun,
				r.deps.CertRotator.Run)
		case "mark_disconnected":
			r.runDurationRule(ctx, name, rule.StaleAfter, defaultMarkDisconnectedStale, batchSize, dryRun,
				r.deps.Purger.MarkDisconnected)
		case "purge_apply_runs":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeApplyRunsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeApplyRuns)
		case "purge_voyages":
			// Retention of growing Voyage-run history (ADR-046 §79,
			// docs/keeper/reaper.md): finished voyages (succeeded/failed/
			// partial_failed/cancelled) older than `max_age` (default 30d) →
			// DELETE; voyage_targets are removed ON DELETE CASCADE. scheduled/
			// pending/running are NOT touched. Default window aligned with
			// purge_apply_runs — drill "voyage → apply_runs" must see both
			// sides until same point (see defaultPurgeVoyagesMaxAge).
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeVoyagesMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeVoyages)
		case "purge_push_runs":
			// Retention of growing run-history of push-runs (migration 076,
			// docs/keeper/reaper.md): finished push_runs (success/
			// partial_failed/failed/cancelled) older than `max_age` (default 30d) →
			// DELETE. pending/running are NOT touched — that's rule
			// `purge_orphan_push_runs` (zombie terminalization). No cascade:
			// per-host results inline in push_runs.summary (jsonb), no child FK
			// to push_runs (051). Default window aligned with
			// purge_apply_runs (see defaultPurgePushRunsMaxAge).
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgePushRunsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgePushRuns)
		case "purge_incarnation_archive":
			// Retention of deleted incarnation archive (incarnation_archive,
			// migration 039; SQL 077): rows with archived_at older than `max_age`
			// (default 365d — compliance window, MORE CONSERVATIVE than run-history 30d) →
			// DELETE. No child FK to archive (039), no cascade. Age — from
			// archived_at.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeArchiveMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeIncarnationArchive)
		case "purge_state_history_archive":
			// Retention of state_history log archive for deleted incarnations
			// (state_history_archive, migration 039; SQL 077): rows with
			// archived_at older than `max_age` (default 365d) → DELETE. Parity
			// purge_incarnation_archive; no child FK.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeArchiveMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeStateHistoryArchive)
		case "purge_archived_state_history":
			// Physical removal of soft-deleted snapshots (archived_at IS NOT NULL) from
			// LIVE state_history (migration 048; SQL 077) older than `max_age`
			// (default 365d). Do NOT confuse with archive_state_history (049), which
			// ONLY sets soft-delete flag — this rule removes already
			// marked rows after compliance window. Active snapshots
			// (archived_at IS NULL) are NOT touched. Age — from archived_at.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeArchiveMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeArchivedStateHistory)
		case "purge_apply_task_register":
			// `max_age` here semantically = grace after apply_run terminal
			// (see docs/keeper/reaper.md). Field is shared with ReaperRule structure.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeApplyTaskRegisterGrace, batchSize, dryRun,
				r.deps.Purger.PurgeApplyTaskRegister)
		case "purge_apply_run_plan":
			// `max_age` here = grace after run terminal (NIM-37): task plan
			// (apply_run_plan) older than grace WITHOUT non-terminal apply_runs → DELETE.
			// Active run plan is not touched. No FK cascade — rule
			// required, otherwise orphan growth. Default grace = 30d (align apply history).
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeApplyRunPlanGrace, batchSize, dryRun,
				r.deps.Purger.PurgeApplyRunPlan)
		case "reclaim_apply_runs":
			// Recovery scan of under-delivered Ward (ADR-027 amend, S4): only
			// `claimed` with expired claim_expires_at (died BEFORE handoff to Soul) →
			// `planned`. `dispatched` is NOT reclaimed — after handoff Soul owns
			// the run, re-claim = double apply. `stale_after` is formal
			// lease argument (does not enter predicate, recovery compares
			// claim_expires_at < NOW() directly). Rule is OFF by default —
			// enable only with attempt-fencing on RunResult intake, otherwise
			// recovery may conflict with stale result (docs/keeper/reaper.md).
			r.runDurationRule(ctx, name, rule.StaleAfter, defaultReclaimApplyRunsLease, batchSize, dryRun,
				r.deps.Purger.ReclaimApplyRuns)
		case "scry_background":
			// Background periodic drift scanning (ADR-031 Slice C). Default
			// OFF (through enabled: false above) + opt-in; parameters
			// max_concurrent_in_flight / min_interval_per_incarnation are resolved
			// inside runScryBackground. Starts per-incarnation goroutines,
			// tick synchronously waits for their completion (see docstring).
			r.runScryBackground(ctx, name, rule, batchSize, dryRun, r.deps.Scry)
		case "archive_state_history":
			// ADR-Q19 retention (PM decision, 2026-05): soft-delete active
			// state_history snapshots beyond latest N per incarnation, optionally with
			// version-bump protection (scenario='migration'). Does not fit
			// duration/statuses runners (no max_age/stale_after; parameters are
			// integer N + bool keep_version_bump), so use separate runner.
			r.runArchiveStateHistory(ctx, name, rule, batchSize, dryRun)
		case "purge_orphan_push_runs":
			// Variant C push-orchestrator (docs/keeper/push.md): in-flight
			// push-runs older than `max_age` (default 1h) are moved to `cancelled`
			// with `orphan_purged: true` marker in summary. One LIST + per-row
			// UPDATE; single-winner-guard (WHERE status IN pending/running)
			// against race with real MarkTerminal. nil OrphanPushRuns → rule
			// degrades with warn (push wire-up not connected yet).
			if r.deps.OrphanPushRuns == nil {
				r.deps.Logger.Warn("reaper: purge_orphan_push_runs skipped: OrphanPushRuns is not configured",
					slog.String("rule", name))
				continue
			}
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeOrphanPushRunsMaxAge, batchSize, dryRun,
				r.deps.OrphanPushRuns.Run)
		case "reap_orphan_vault_keys":
			// Cross-store reconcile (report-only, GATE-2): finds private keys
			// for Sigil signing in Vault without row in sigil_signing_keys and ONLY
			// counts/meters/logs them — deletes nothing. `max_age`
			// semantically = grace by Vault-secret age (cuts race with
			// Introduce write-before-PG-commit), like `purge_apply_task_register`
			// uses MaxAge-as-grace. OFF by default (needs Vault and
			// list rights on secret/metadata/keeper/sigil-keys/*). See
			// docs/keeper/reaper.md.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultReapOrphanVaultKeysGrace, batchSize, dryRun,
				r.deps.Purger.ReportOrphanVaultKeys)
		case "purge_old_errands":
			// Errand TTL retention (ADR-033, docs/keeper/reaper.md). `DELETE FROM
			// errands WHERE ttl_at < NOW()` — TTL is baked into row on INSERT
			// by dispatcher (default 7d through errand.TTLDefault), rule `max_age`
			// does NOT enter predicate (formal argument for common runner). nil
			// OldErrands → rule degrades with warn (errand wire-up not
			// connected yet — single-keeper dev without errand stack).
			if r.deps.OldErrands == nil {
				r.deps.Logger.Warn("reaper: purge_old_errands skipped: OldErrands is not configured",
					slog.String("rule", name))
				continue
			}
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeOldErrandsMaxAge, batchSize, dryRun,
				r.deps.OldErrands.Run)
		case "purge_orphan_ephemeral_tidings":
			// Cleanup of orphaned ephemeral Tidings (ADR-052(g) amendment N2,
			// docs/keeper/reaper.md). Voyage terminal should remove its one-shot
			// subscriptions; this rule is safeguard with grace period. `max_age`
			// semantically = grace AFTER terminal (ENTERS predicate, parity with
			// `purge_apply_task_register`): removal before tap-consumer window would race
			// terminal notification delivery (dispatcher is asynchronous,
			// ADR-052(c)). nil OrphanEphemeralTidings → rule degrades with warn
			// (herald wire-up may be disabled).
			if r.deps.OrphanEphemeralTidings == nil {
				r.deps.Logger.Warn("reaper: purge_orphan_ephemeral_tidings skipped: OrphanEphemeralTidings is not configured",
					slog.String("rule", name))
				continue
			}
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeOrphanEphemeralTidingsGrace, batchSize, dryRun,
				r.deps.OrphanEphemeralTidings.Run)
		default:
			r.deps.Logger.Warn("reaper: unknown rule name, skipping",
				slog.String("rule", name),
			)
		}
	}

	r.dispatchReclaimVoyages(ctx, cfg, batchSize, dryRun)
	r.dispatchReconcileOrphanApplying(ctx, cfg, batchSize, dryRun)
}

// dispatchReclaimVoyages executes reclaim_voyages rule with default-ON
// path-defaulting (ADR-043 §8): rule runs if key is absent in
// cfg.Reaper.Rules OR present with Enabled:true; skipped ONLY on
// explicit Enabled:false. Called from dispatch ONCE after main loop
// (where reclaim_voyages is excluded via `continue`), so Enabled:true
// does not run it twice.
//
// Recovery scan of expired Voyage claims (ADR-043 S4, docs/keeper/reaper.md):
// `status='running' AND claim_expires_at < NOW()` → `pending` for re-claim
// by another Keeper instance, attempt++ (fencing-epoch). `stale_after` is formal
// lease argument (does NOT enter SQL predicate, lease baked into claim_expires_at); if
// key is absent in map, defaultReclaimVoyagesLease is used. nil
// VoyageReclaim → rule degrades with warn (voyage wire-up may be
// disabled).
//
// Default-ON is safe: duplicate commit is rejected by CAS ownership guard in
// voyage.Finalize (WHERE claimed_by_kid=$2 → ErrLeaseLost for stale worker).
func (r *Runner) dispatchReclaimVoyages(ctx context.Context, cfg *config.KeeperConfig, batchSize int, dryRun bool) {
	const name = "reclaim_voyages"
	rule, ok := cfg.Reaper.Rules[name]
	if ok && !rule.Enabled {
		return
	}
	if r.deps.VoyageReclaim == nil {
		r.deps.Logger.Warn("reaper: reclaim_voyages skipped: VoyageReclaim is not configured",
			slog.String("rule", name))
		return
	}
	r.runDurationRule(ctx, name, rule.StaleAfter, defaultReclaimVoyagesLease, batchSize, dryRun,
		r.deps.VoyageReclaim.Run)
}

// dispatchReconcileOrphanApplying executes reconcile_orphan_applying rule with
// default-ON path-defaulting (ADR-027 amend (m), like reclaim_voyages): rule
// runs if key is absent in cfg.Reaper.Rules OR present with
// Enabled:true; skipped ONLY on explicit Enabled:false. Called from
// dispatch ONCE after main loop (where rule is excluded via
// `continue`), so Enabled:true does not run it twice.
//
// Release of orphaned applying-lock from direct (standalone) scenario-run
// whose Keeper owner crashed (ADR-027 amend (m), docs/keeper/reaper.md):
// stale applying row (applying_since < NOW()-stale_after) with NON-empty epoch +
// owner presence death in Conclave → applying→ready via idempotent
// ReleaseApplyingOrphan. `stale_after` ENTERS SQL predicate (cutoff =
// NOW()-stale_after), default 90s (parity mark_disconnected). nil OrphanApplying
// → rule degrades with warn (reconciler not configured). Real
// presence gate against unavailable Redis is at InstanceAlive level: presence check
// error ⇒ fail-safe skip candidate, not rule no-op.
//
// Default-ON is safe: presence-gate (InstanceAlive=false required) + FENCING-1
// (no-live-rival) + single-winner CAS inside ReleaseApplyingOrphan prevent releasing
// live lock; residual double-apply (network partition of live owner) is same
// acceptable class as reclaim_apply_runs/(l), protected by gate-1 fencing.
func (r *Runner) dispatchReconcileOrphanApplying(ctx context.Context, cfg *config.KeeperConfig, batchSize int, dryRun bool) {
	const name = "reconcile_orphan_applying"
	rule, ok := cfg.Reaper.Rules[name]
	if ok && !rule.Enabled {
		return
	}
	if r.deps.OrphanApplying == nil {
		r.deps.Logger.Warn("reaper: reconcile_orphan_applying skipped: OrphanApplying is not configured",
			slog.String("rule", name))
		return
	}
	r.runDurationRule(ctx, name, rule.StaleAfter, defaultReconcileOrphanApplyingStale, batchSize, dryRun,
		r.deps.OrphanApplying.Run)
}

// runDurationRule is common runner for rules with signature
// `(ctx, duration, batchSize) → (count, err)`. Extracts duration from
// raw cfg string, handles dry_run, and logs uniformly.
//
// `rawDuration` is `rule.MaxAge` (for most rules) or
// `rule.StaleAfter` (for `mark_disconnected`); selector is chosen by caller
// when selecting runner.
func (r *Runner) runDurationRule(
	ctx context.Context,
	ruleName string,
	rawDuration string,
	defaultDuration time.Duration,
	batchSize int,
	dryRun bool,
	call func(context.Context, time.Duration, int) (int64, error),
) {
	duration, err := parseRuleDuration(rawDuration, defaultDuration)
	if err != nil {
		r.deps.Logger.Warn("reaper: invalid duration, using default",
			slog.String("rule", ruleName),
			slog.String("raw", rawDuration),
			slog.Any("error", err),
			slog.Duration("default", defaultDuration),
		)
		duration = defaultDuration
	}

	if dryRun {
		r.deps.Logger.Info("reaper: dry_run, skipping",
			slog.String("rule", ruleName),
			slog.Duration("duration", duration),
			slog.Int("batch_size", batchSize),
		)
		return
	}

	start := time.Now()
	affected, err := call(ctx, duration, batchSize)
	r.deps.Metrics.ObserveRule(ruleName, affected, err, time.Since(start))
	if err != nil {
		r.deps.Logger.Error("reaper: rule failed",
			slog.String("rule", ruleName),
			slog.Any("error", err),
		)
		return
	}
	r.deps.Logger.Info("reaper: rule applied",
		slog.String("rule", ruleName),
		slog.Int64("affected", affected),
		slog.Duration("duration", duration),
		slog.Int("batch_size", batchSize),
	)
}

// runStatusesRule is common runner for rules with statuses[] filter
// (`purge_souls`, `purge_old_seeds`). Same as runDurationRule, but
// also resolves statuses (cfg → default if empty).
func (r *Runner) runStatusesRule(
	ctx context.Context,
	ruleName string,
	rawStatuses []string,
	defaultStatuses []string,
	rawMaxAge string,
	defaultMaxAge time.Duration,
	batchSize int,
	dryRun bool,
	call func(context.Context, []string, time.Duration, int) (int64, error),
) {
	statuses := rawStatuses
	if len(statuses) == 0 {
		statuses = defaultStatuses
	}

	maxAge, err := parseRuleDuration(rawMaxAge, defaultMaxAge)
	if err != nil {
		r.deps.Logger.Warn("reaper: invalid max_age, using default",
			slog.String("rule", ruleName),
			slog.String("raw", rawMaxAge),
			slog.Any("error", err),
			slog.Duration("default", defaultMaxAge),
		)
		maxAge = defaultMaxAge
	}

	if dryRun {
		r.deps.Logger.Info("reaper: dry_run, skipping",
			slog.String("rule", ruleName),
			slog.Any("statuses", statuses),
			slog.Duration("max_age", maxAge),
			slog.Int("batch_size", batchSize),
		)
		return
	}

	start := time.Now()
	affected, err := call(ctx, statuses, maxAge, batchSize)
	r.deps.Metrics.ObserveRule(ruleName, affected, err, time.Since(start))
	if err != nil {
		r.deps.Logger.Error("reaper: rule failed",
			slog.String("rule", ruleName),
			slog.Any("error", err),
		)
		return
	}
	r.deps.Logger.Info("reaper: rule applied",
		slog.String("rule", ruleName),
		slog.Int64("affected", affected),
		slog.Any("statuses", statuses),
		slog.Duration("max_age", maxAge),
		slog.Int("batch_size", batchSize),
	)
}

// runArchiveStateHistory is runner for `archive_state_history` rule (ADR-Q19
// retention). Rule parameters are integer N + bool keep_version_bump
// (see ReaperRule.KeepLastN / KeepVersionBumpSnapshots), incompatible with
// runDurationRule/runStatusesRule, so separate function.
//
// Defaults (cfg `*int`/`*bool` = nil → unset):
//   - keep_last_n = 50 ([defaultArchiveStateHistoryKeepLastN])
//   - keep_version_bump = true ([defaultArchiveStateHistoryKeepVersionBump])
func (r *Runner) runArchiveStateHistory(ctx context.Context, ruleName string, rule config.ReaperRule, batchSize int, dryRun bool) {
	keepLastN := defaultArchiveStateHistoryKeepLastN
	if rule.KeepLastN != nil {
		keepLastN = *rule.KeepLastN
	}
	if keepLastN <= 0 {
		r.deps.Logger.Warn("reaper: archive_state_history keep_last_n must be > 0, using default",
			slog.String("rule", ruleName),
			slog.Int("raw", keepLastN),
			slog.Int("default", defaultArchiveStateHistoryKeepLastN),
		)
		keepLastN = defaultArchiveStateHistoryKeepLastN
	}

	keepVersionBump := defaultArchiveStateHistoryKeepVersionBump
	if rule.KeepVersionBumpSnapshots != nil {
		keepVersionBump = *rule.KeepVersionBumpSnapshots
	}

	if dryRun {
		r.deps.Logger.Info("reaper: dry_run, skipping",
			slog.String("rule", ruleName),
			slog.Int("keep_last_n", keepLastN),
			slog.Bool("keep_version_bump", keepVersionBump),
			slog.Int("batch_size", batchSize),
		)
		return
	}

	start := time.Now()
	affected, err := r.deps.Purger.ArchiveStateHistory(ctx, keepLastN, keepVersionBump, batchSize)
	r.deps.Metrics.ObserveRule(ruleName, affected, err, time.Since(start))
	if err != nil {
		r.deps.Logger.Error("reaper: rule failed",
			slog.String("rule", ruleName),
			slog.Any("error", err),
		)
		return
	}
	r.deps.Logger.Info("reaper: rule applied",
		slog.String("rule", ruleName),
		slog.Int64("affected", affected),
		slog.Int("keep_last_n", keepLastN),
		slog.Bool("keep_version_bump", keepVersionBump),
		slog.Int("batch_size", batchSize),
	)
}

// reaperLockTTL/reaperInterval are selectors for parseDurationOr. Extracted
// into typed functions to avoid duplicating nil-check on `Reaper`
// and spawning inline conditionals.
func reaperLockTTL(r *config.KeeperReaper) string  { return r.LockTTL }
func reaperInterval(r *config.KeeperReaper) string { return r.Interval }

// parseDurationOr reads duration string from cfg.Reaper through selector.
// With nil-Reaper or empty string, returns fallback. On invalid format
// also returns fallback (semantic validation of keeper.yml already rejects
// invalid durations; valid config should not reach this path).
func parseDurationOr(r *config.KeeperReaper, fallback time.Duration, sel func(*config.KeeperReaper) string) time.Duration {
	if r == nil {
		return fallback
	}
	d, err := parseRuleDuration(sel(r), fallback)
	if err != nil {
		return fallback
	}
	return d
}

// parseRuleDuration wraps [config.ParseDuration] with default for
// empty string. Unified Soul Stack `duration` convention (Go duration or
// `<N>d` with overflow guard) is provided by shared/config.
func parseRuleDuration(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	return config.ParseDuration(s)
}

// ResolveMarkDisconnectedStale returns effective `stale_after` of
// `mark_disconnected` rule (cfg value or [defaultMarkDisconnectedStale]).
// Single source of truth for disconnect threshold is needed outside Reaper:
// EventStream flush of `last_seen_at` derives its throttle interval from this threshold,
// so flush is guaranteed to be more frequent than Reaper marking stream disconnected
// (see ADR-006(a)).
//
// Invalid cfg string (semantic validate would reject it at startup) → default.
func ResolveMarkDisconnectedStale(cfg *config.KeeperReaper) time.Duration {
	if cfg == nil {
		return defaultMarkDisconnectedStale
	}
	rule, ok := cfg.Rules["mark_disconnected"]
	if !ok {
		return defaultMarkDisconnectedStale
	}
	d, err := parseRuleDuration(rule.StaleAfter, defaultMarkDisconnectedStale)
	if err != nil {
		return defaultMarkDisconnectedStale
	}
	return d
}
