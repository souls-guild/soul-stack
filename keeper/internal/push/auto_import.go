package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// AutoImportSystemAID — the fixed AID under which auto-import writes
// `created_by_aid` for `push_providers` rows and `archon_aid` in audit-events.
// Not an Archon operator (no JWT initiator), not [config.SourceKeeperInternal]
// (different semantics — data migration by operator consent, not an autonomous
// Keeper initiative). Kept as a separate system AID for the audit filter.
//
// The FK on `operators(aid)` for `push_providers.created_by_aid` requires the
// `archon-system` row to exist in the registry before the first auto-import.
// The row `operators(archon-system, created_via='system', created_by_aid=NULL)`
// is seeded by migration 086 (ADR-058(d)) — the FK is guaranteed after migrations apply.
const AutoImportSystemAID = "archon-system"

// AutoImporterReader — a narrow read surface over push_providers storage,
// needed by the importer: existence check by PK before INSERT. Symmetric to
// [PushProviderResolver]; split into its own interface so a unit test can
// swap in a fake without spinning up Postgres.
type AutoImporterReader interface {
	SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error)
}

// AutoImporterInserter — a narrow write surface for INSERT push_providers
// during auto-import. Separate from [pushprovider.Service.Create]: import
// must not publish per-row `push-providers:changed` and doesn't write
// `push-provider.created` audit (instead — `push-provider.imported_from_config`
// with source `config_bootstrap`).
type AutoImporterInserter interface {
	Insert(ctx context.Context, p *pushprovider.PushProvider) error
}

// AutoImporterTargetReader — a narrow read surface for checking the current
// `souls.ssh_target` (NULL → import, not NULL → skip). Symmetric to
// [PGTargetReader], but redeclared to keep an even per-importer set of
// narrow interfaces (in case they diverge in the future).
type AutoImporterTargetReader interface {
	SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error)
}

// AutoImporterTargetWriter — a narrow write surface for UPDATE
// `souls.ssh_target` during auto-import.
type AutoImporterTargetWriter interface {
	UpdateSshTarget(ctx context.Context, sid string, target *soul.SSHTarget) error
}

// AuditWriter — a narrow subset of [audit.Writer]; redeclared locally so a
// fake in a unit test doesn't depend on the audit package.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// AutoImporterDeps — dependencies of [AutoImporter].
//
// All fields are immutable after construction. Doesn't use *pgxpool.Pool
// directly — accepts narrow read/write surfaces (read+write for targets,
// read+insert for providers) so unit tests can go through a fake without PG.
type AutoImporterDeps struct {
	TargetReader   AutoImporterTargetReader
	TargetWriter   AutoImporterTargetWriter
	ProviderReader AutoImporterReader
	ProviderWriter AutoImporterInserter
	Auditor        AuditWriter
	Logger         *slog.Logger
}

// AutoImporter — a one-shot migration of inline `keeper.yml::push` blocks
// into PG sources (ADR-032 amendment 2026-05-26, S7-4).
//
// Runs once in the Keeper start pipeline (see wire-up in
// `runLegacyAutoImport` / `keeper/cmd/keeper`). Idempotent: rows already
// present in PG are skipped — a repeat start is a no-op.
//
// Gated by the `push.auto_import_legacy_targets` and
// `push.auto_import_legacy_providers` flags in [config.KeeperPush]. Default
// false: without explicit opt-in, no import runs (silent data migration is
// forbidden by security policy).
type AutoImporter struct {
	deps AutoImporterDeps
}

// NewAutoImporter assembles the importer. Any nil in deps → constructor error
// (the importer is meaningless without at least one complete pair: read +
// write for targets, read + write for providers; auditor + logger are
// required for the audit trail).
func NewAutoImporter(deps AutoImporterDeps) (*AutoImporter, error) {
	if deps.TargetReader == nil || deps.TargetWriter == nil {
		return nil, errors.New("push: AutoImporter targets reader/writer is nil")
	}
	if deps.ProviderReader == nil || deps.ProviderWriter == nil {
		return nil, errors.New("push: AutoImporter providers reader/writer is nil")
	}
	if deps.Auditor == nil {
		return nil, errors.New("push: AutoImporter auditor is nil")
	}
	if deps.Logger == nil {
		return nil, errors.New("push: AutoImporter logger is nil")
	}
	return &AutoImporter{deps: deps}, nil
}

// ImportLegacyOnStart runs the one-shot migration of the two blocks per the
// [config.KeeperPush.AutoImportLegacyTargets] / `AutoImportLegacyProviders` flags.
//
// Error contract:
//   - a PG error reading/writing any row aborts the import (fail-closed: the
//     operator turned on auto-import and shouldn't silently end up with "half
//     imported, half not"). Already-imported rows stay; not-yet-imported ones
//     get picked up on the next start (idempotent);
//   - a missing `souls` row for a config-target SID — WARN-skip (not fatal:
//     the soul isn't registered yet, importing an SSH credential is
//     meaningless — the operator later PUTs it themselves via
//     `/v1/souls/{sid}/ssh-target`);
//   - audit-write failure — WARN log, import continues (storage is already
//     committed, an audit↔storage mismatch doesn't block subsequent rows;
//     the bootstrap pattern).
func (i *AutoImporter) ImportLegacyOnStart(ctx context.Context, cfg config.KeeperPush) error {
	if cfg.AutoImportLegacyTargets && len(cfg.Targets) > 0 {
		if err := i.importTargets(ctx, cfg.Targets); err != nil {
			return fmt.Errorf("push: auto-import legacy targets: %w", err)
		}
	}
	if cfg.AutoImportLegacyProviders && len(cfg.Providers) > 0 {
		if err := i.importProviders(ctx, cfg.Providers); err != nil {
			return fmt.Errorf("push: auto-import legacy providers: %w", err)
		}
	}
	return nil
}

func (i *AutoImporter) importTargets(ctx context.Context, targets []config.KeeperPushTarget) error {
	imported, skipped, missingSoul := 0, 0, 0

	for _, t := range targets {
		if t.SID == "" {
			// The schema phase already rejects this; defense-in-depth.
			skipped++
			continue
		}
		existing, err := i.deps.TargetReader.SelectSshTarget(ctx, t.SID)
		if errors.Is(err, soul.ErrSoulNotFound) {
			i.deps.Logger.Warn("push: auto-import targets: souls row missing, skipping",
				slog.String("sid", t.SID))
			missingSoul++
			continue
		}
		if err != nil {
			return fmt.Errorf("read souls.ssh_target %q: %w", t.SID, err)
		}
		if existing != nil {
			// The PG canonical source already has data — don't overwrite it
			// (idempotent, a repeat start is a no-op).
			skipped++
			continue
		}

		// The pilot form S6 stores only what the operator set (resolvers fill
		// in defaults for empty fields). Import keeps the same semantics: 0/""
		// is written to jsonb as-is, PGFallbackTargetResolver fills in defaults
		// at resolve time.
		target := &soul.SSHTarget{
			SSHPort:  t.SSHPort,
			SSHUser:  t.SSHUser,
			SoulPath: t.SoulPath,
		}
		if err := i.deps.TargetWriter.UpdateSshTarget(ctx, t.SID, target); err != nil {
			return fmt.Errorf("update souls.ssh_target %q: %w", t.SID, err)
		}

		i.writeAudit(ctx, &audit.Event{
			EventType: audit.EventSoulSshTargetImportedFromConfig,
			Source:    audit.SourceConfigBootstrap,
			// archon_aid: NULL — system action, not an operator initiator.
			Payload: map[string]any{
				"sid":       t.SID,
				"ssh_port":  t.SSHPort,
				"ssh_user":  t.SSHUser,
				"soul_path": t.SoulPath,
			},
		})
		imported++
	}

	i.deps.Logger.Info("push: S7-4 auto-import targets completed",
		slog.Int("imported", imported),
		slog.Int("skipped_existing", skipped),
		slog.Int("skipped_missing_soul", missingSoul),
		slog.Int("total_in_config", len(targets)))
	return nil
}

func (i *AutoImporter) importProviders(ctx context.Context, providers []config.KeeperPushProvider) error {
	imported, skipped := 0, 0

	for _, p := range providers {
		if p.Name == "" {
			skipped++
			continue
		}
		_, err := i.deps.ProviderReader.SelectByName(ctx, p.Name)
		if err == nil {
			// The PG canonical source already has the row — skip.
			skipped++
			continue
		}
		if !errors.Is(err, pushprovider.ErrPushProviderNotFound) {
			return fmt.Errorf("read push_providers %q: %w", p.Name, err)
		}

		entry := &pushprovider.PushProvider{
			Name:         p.Name,
			Params:       p.Params,
			CreatedByAID: AutoImportSystemAID,
		}
		if err := i.deps.ProviderWriter.Insert(ctx, entry); err != nil {
			// ErrPushProviderAlreadyExists on a race is impossible here (startup
			// is sequential, a single importer); propagate as a PG error.
			return fmt.Errorf("insert push_providers %q: %w", p.Name, err)
		}

		i.writeAudit(ctx, &audit.Event{
			EventType: audit.EventPushProviderImportedFromConfig,
			Source:    audit.SourceConfigBootstrap,
			Payload: map[string]any{
				"name":        p.Name,
				"params_keys": paramsKeys(p.Params),
			},
		})
		imported++
	}

	i.deps.Logger.Info("push: S7-4 auto-import providers completed",
		slog.Int("imported", imported),
		slog.Int("skipped_existing", skipped),
		slog.Int("total_in_config", len(providers)))
	return nil
}

// writeAudit — a best-effort wrapper over auditor.Write. Storage is already
// committed, an audit-write failure must not abort importing subsequent rows
// (the bootstrap.ErrAuditWriteFailed pattern — the mismatch is resolved by
// the operator manually).
func (i *AutoImporter) writeAudit(ctx context.Context, event *audit.Event) {
	if err := i.deps.Auditor.Write(ctx, event); err != nil {
		i.deps.Logger.Warn("push: S7-4 auto-import audit write failed",
			slog.String("event_type", string(event.EventType)),
			slog.Any("error", err))
	}
}

// paramsKeys returns a sorted list of params keys without values (symmetric
// with the pushprovider.Service.Create payload: keys are recorded for the
// audit trail, sensitive values are never written to audit per policy —
// robust to a future allow-list extension).
func paramsKeys(params map[string]any) []string {
	if len(params) == 0 {
		return []string{}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PGTargetReadWriter / PGProviderReadWriter — concrete adapters from
// pgxpool.Pool (or any matching ExecQueryRower) to the
// AutoImporterTargetReader+Writer / AutoImporterReader+Inserter interfaces.
// A thin live wrapper; needed so daemon wire-up doesn't build two separate
// adapters (read+write over the same *pgxpool.Pool) and doesn't leak pgx
// deps into Auto-Importer.

// pgTargetReadWriter — the production implementation for targets.
type pgTargetReadWriter struct {
	db soul.ExecQueryRower
}

// NewPGTargetReadWriter returns an implementation of AutoImporterTargetReader +
// AutoImporterTargetWriter over soul.ExecQueryRower (type *pgxpool.Pool).
func NewPGTargetReadWriter(db soul.ExecQueryRower) interface {
	AutoImporterTargetReader
	AutoImporterTargetWriter
} {
	return &pgTargetReadWriter{db: db}
}

func (a *pgTargetReadWriter) SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error) {
	return soul.SelectSshTarget(ctx, a.db, sid)
}

func (a *pgTargetReadWriter) UpdateSshTarget(ctx context.Context, sid string, target *soul.SSHTarget) error {
	return soul.UpdateSshTarget(ctx, a.db, sid, target)
}

// pgProviderReadWriter — the production implementation for providers.
type pgProviderReadWriter struct {
	db pushprovider.ExecQueryRower
}

// NewPGProviderReadWriter returns an implementation of AutoImporterReader +
// AutoImporterInserter over pushprovider.ExecQueryRower (*pgxpool.Pool).
func NewPGProviderReadWriter(db pushprovider.ExecQueryRower) interface {
	AutoImporterReader
	AutoImporterInserter
} {
	return &pgProviderReadWriter{db: db}
}

func (a *pgProviderReadWriter) SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error) {
	return pushprovider.SelectByName(ctx, a.db, name)
}

func (a *pgProviderReadWriter) Insert(ctx context.Context, p *pushprovider.PushProvider) error {
	return pushprovider.Insert(ctx, a.db, p)
}
