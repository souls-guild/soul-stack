package handlers

// GET /v1/incarnations/{name}/upgrade-paths (ADR-0068 §6) — READ analysis of
// incarnation upgrade paths. A separate file (not incarnation_typed.go) to avoid
// conflicting with the Slice 2 upgrade flow. Read-only reuse of the
// incarnation.PrepareUpgrade building blocks (resolve+load+analyze) without changing the pin or running.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// Transition directions (ADR-0068 §6, on-demand analysis of a single target).
const (
	upgradeDirectionNoop       = "no-op"       // to==pin AND target schema==current
	upgradeDirectionDowngrade  = "downgrade"   // target schema < current (forward-only, ADR-019)
	upgradeDirectionForward    = "forward"     // target schema > current
	upgradeDirectionSameSchema = "same-schema" // schemas equal, different ref (ref-bump)
)

// Modes for whether an upgrade scenario exists for the transition (ADR-0068 §5).
const (
	upgradeModeFound  = "found"  // an upgrade scenario exists whose from ⊇ current pin
	upgradeModeLegacy = "legacy" // none — the upgrade would drift without host orchestration
)

// IncarnationUpgradePathsView — a FLAT domain projection of GET .../upgrade-paths.
// Two modes of one endpoint (ADR-0068 §6):
//   - cheap (toRef==""): Paths is filled — service registry tags + an is_current mark;
//     direction/found are NOT computed (ADR-007 bans semver parsing — direction is
//     unreliable from tag names).
//   - on-demand (toRef!=""): Target is filled — analysis of a SINGLE target (direction/mode/slug/
//     state_migrations).
type IncarnationUpgradePathsView struct {
	CurrentVersion            string                 // inc.ServiceVersion (current pin)
	CurrentStateSchemaVersion int                    // inc.StateSchemaVersion
	Paths                     []UpgradePathRefView   // cheap mode (nil in on-demand)
	Target                    *UpgradePathTargetView // on-demand (nil in cheap)
}

// UpgradePathRefView — one git ref of the service registry (cheap mode). IsCurrent —
// a no-op mark ref == inc.ServiceVersion (ADR-0068 §6).
type UpgradePathRefView struct {
	Ref       string
	Type      string // tag | branch (artifact.GitRef.Type)
	Commit    string
	IsCurrent bool
}

// UpgradePathTargetView — read-only analysis of a single target (on-demand). A slice of
// incarnation.PrepareUpgrade without changing the pin or running (ADR-0068 §6).
type UpgradePathTargetView struct {
	To                       string
	ResolvedCommit           string // art.SHA1 of the target snapshot
	TargetStateSchemaVersion int
	Direction                string // no-op | downgrade | forward | same-schema
	Mode                     string // found | legacy
	Slug                     string // slug of the upgrade scenario when found
	Downgrade                bool   // target is lower by schema → chain NOT loaded (forward-only)
	// Reachable — the target is reachable by upgrade. false ONLY when the migration
	// chain is structurally broken (unreachable_reason); downgrade/no-op → reachable=true
	// (that is a different direction, signaled by Direction, not "unreachability").
	Reachable bool
	// UnreachableReason — machine-readable reason for unreachability (empty when reachable=true).
	UnreachableReason string
	// StateMigrations — the applied current→target chain (shape {from,to,path}).
	// On downgrade / broken chain — empty (not loaded / cannot be assembled).
	StateMigrations []artifact.Migration
}

// UpgradePathsTyped — GET /v1/incarnations/{name}/upgrade-paths (READ, no audit,
// ADR-0068 §6). toRef=="" → cheap tag list; toRef!="" → on-demand target analysis.
// inScope — operator scope predicate (like GetTyped): out of scope → 404 (do not leak
// existence). Read-only: the pin is NOT changed, the upgrade is NOT executed.
func (h *IncarnationHandler) UpgradePathsTyped(ctx context.Context, name, toRef string, inScope func(*incarnation.Incarnation) bool) (IncarnationUpgradePathsView, error) {
	var zero IncarnationUpgradePathsView
	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	// existence-probe + scope: not-found / out of scope → 404 (do not leak existence).
	inc, err := h.existenceProbeInScope(ctx, name, inScope, "upgrade-paths")
	if err != nil {
		return zero, err
	}

	view := IncarnationUpgradePathsView{
		CurrentVersion:            inc.ServiceVersion,
		CurrentStateSchemaVersion: inc.StateSchemaVersion,
	}
	if toRef == "" {
		paths, perr := h.upgradePathsCheap(ctx, inc)
		if perr != nil {
			return zero, perr
		}
		view.Paths = paths
		return view, nil
	}
	target, terr := h.upgradePathsTarget(ctx, inc, toRef)
	if terr != nil {
		return zero, terr
	}
	view.Target = target
	return view, nil
}

// upgradePathsCheap — cheap mode (ADR-0068 §6): ls-remote of service registry tags +
// an is_current mark. Same source as ServiceHandler.ListRefsTyped (h.services.
// Resolve → git coordinates, h.refs.ListRefs → tags). Direction/found are NOT computed
// (ADR-007 bans semver parsing; unreliable from tag names) — that is on-demand ?to=.
func (h *IncarnationHandler) upgradePathsCheap(ctx context.Context, inc *incarnation.Incarnation) ([]UpgradePathRefView, error) {
	if h.refs == nil {
		return nil, incProblem(problem.TypeInternalError, "service refs lister is not configured")
	}
	if h.services == nil {
		return nil, incProblem(problem.TypeInternalError, "service resolver is not configured")
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil, incProblem(problem.TypeNotFound, "service "+inc.Service+" is not registered")
	}
	refs, err := h.refs.ListRefs(ctx, ref.Name, ref.Git)
	if err != nil {
		h.logger.Warn("incarnation.upgrade-paths: ls-remote failed",
			slog.String("name", inc.Name), slog.String("service", inc.Service), slog.Any("error", err))
		return nil, incProblem(problem.TypeBadGateway, "ls-remote failed for service "+inc.Service+": "+err.Error())
	}
	out := make([]UpgradePathRefView, 0, len(refs))
	for _, r := range refs {
		out = append(out, UpgradePathRefView{
			Ref:       r.Name,
			Type:      r.Type,
			Commit:    r.Commit,
			IsCurrent: r.Name == inc.ServiceVersion,
		})
	}
	return out, nil
}

// upgradePathsTarget — on-demand analysis of a SINGLE target (ADR-0068 §6): load the
// toRef snapshot → direction / mode(found|legacy) / state_migrations. Read-only slice of
// incarnation.PrepareUpgrade — the pin is NOT changed, the upgrade is NOT executed.
func (h *IncarnationHandler) upgradePathsTarget(ctx context.Context, inc *incarnation.Incarnation, toRef string) (*UpgradePathTargetView, error) {
	if h.services == nil || h.loader == nil {
		return nil, incProblem(problem.TypeInternalError, "service loader is not configured")
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil, incProblem(problem.TypeNotFound, "service "+inc.Service+" is not registered")
	}
	ref.Ref = toRef
	art, err := h.loader.Load(ctx, ref)
	if err != nil {
		h.logger.Warn("incarnation.upgrade-paths: load target snapshot failed",
			slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", err))
		return nil, incProblem(problem.TypeBadGateway, "load target snapshot "+toRef+" failed: "+err.Error())
	}
	if art == nil || art.Manifest == nil {
		return nil, incProblem(problem.TypeInternalError, "target snapshot "+toRef+" has no manifest")
	}
	target := art.Manifest.StateSchemaVersion
	current := inc.StateSchemaVersion

	tgt := &UpgradePathTargetView{
		To:                       toRef,
		ResolvedCommit:           art.SHA1,
		TargetStateSchemaVersion: target,
		Reachable:                true, // reset to false only on a broken chain
	}
	switch {
	case target < current:
		tgt.Direction = upgradeDirectionDowngrade
		tgt.Downgrade = true
	case target > current:
		tgt.Direction = upgradeDirectionForward
	case toRef == inc.ServiceVersion:
		tgt.Direction = upgradeDirectionNoop
	default:
		tgt.Direction = upgradeDirectionSameSchema
	}

	// mode found/legacy — ONLY for upgrade directions (forward/same-schema): on
	// downgrade/no-op it is semantically meaningless, so ListUpgrades is skipped. Scan the
	// target's upgrade/, match from ⊇ current pin (ResolveUpgradeScenario). A scan failure
	// falls back to legacy (ADR-0068 §5★ fail-open): upgrade-paths honestly shows WHAT would happen.
	if tgt.Direction == upgradeDirectionForward || tgt.Direction == upgradeDirectionSameSchema {
		upgrades, uerr := h.loader.ListUpgrades(art)
		if uerr != nil {
			h.logger.Warn("incarnation.upgrade-paths: upgrade scan failed, reporting legacy",
				slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", uerr))
		}
		if slug, found := artifact.ResolveUpgradeScenario(upgrades, inc.ServiceVersion); found {
			tgt.Mode = upgradeModeFound
			tgt.Slug = slug
		} else {
			tgt.Mode = upgradeModeLegacy
		}
	}

	// state_migrations — the applied current→target chain. On downgrade it is NOT loaded
	// (forward-only, ADR-019; LoadMigrationChain on from>to would return an error).
	if !tgt.Downgrade {
		chain, cerr := h.loader.LoadMigrationChain(art, current, target)
		if cerr != nil {
			if errors.Is(cerr, artifact.ErrMigrationChainBroken) {
				// Preview endpoint (ADR-0068 §6): a structurally broken chain — an unreachable
				// target as DATA, NOT an HTTP error. direction/mode are already computed (forward,
				// found/legacy), the chain cannot be assembled → reachable=false + reason,
				// state_migrations empty. The operator sees "cannot move here", not a 4xx.
				h.logger.Warn("incarnation.upgrade-paths: target unreachable — migration chain broken",
					slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", cerr))
				tgt.Reachable = false
				tgt.UnreachableReason = "migration chain to " + toRef + " is broken: " + cerr.Error()
				return tgt, nil
			}
			// Any other failure (parsing a malformed migrations/ file / I/O of an already
			// materialized snapshot) = keeper-internal defect → 500 (parity UpgradeTyped default). 502
			// is reserved ONLY for loader.Load — there the culprit is genuinely external git.
			h.logger.Error("incarnation.upgrade-paths: load migration chain failed",
				slog.String("name", inc.Name), slog.String("to", toRef), slog.Any("error", cerr))
			return nil, incProblem(problem.TypeInternalError, "load migration chain to "+toRef+" failed")
		}
		tgt.StateMigrations = upgradeMigrationSteps(chain)
	}
	return tgt, nil
}

// upgradeMigrationSteps projects the applied chain into the {from,to,path} shape
// ([artifact.Migration]). Path — the canonical migration file name (docs/migrations.md,
// migrations/<NNN>_to_<MMM>.yml) from the step's own versions (a display path, not a
// duplication of LoadMigrationChain logic).
func upgradeMigrationSteps(chain statemigrate.Chain) []artifact.Migration {
	steps := make([]artifact.Migration, 0, len(chain))
	for _, m := range chain {
		steps = append(steps, artifact.Migration{
			From: m.FromVersion,
			To:   m.ToVersion,
			Path: fmt.Sprintf("migrations/%03d_to_%03d.yml", m.FromVersion, m.ToVersion),
		})
	}
	return steps
}
