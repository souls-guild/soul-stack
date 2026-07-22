package incarnation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// Sentinel errors of the upgrade prepare phase (target resolve before the tx).
// Separated from the tx-level sentinels of [UpgradeStateSchema]:
//   - ErrServiceNotRegistered → caller maps to internal-error / 500
//     (the incarnation's service must be in the service registry; managed via
//     service.* API, ADR-029).
//   - ErrUpgradeNoop           → 422 / validation-failed (nothing to upgrade:
//     same ref AND same schema).
//   - ErrDowngradeViaRef       → 409 / incarnation-locked (the target ref carries
//     a schema below the current one; forward-only, ADR-019). Early guard before
//     loading the chain; the tx-level [ErrDowngradeUnsupported] remains a guard
//     against a resolve↔FOR UPDATE race.
//   - ErrTargetSnapshotInvalid → internal-error / 500 (snapshot has no manifest).
//   - ErrLoadTargetSnapshot    → internal-error / 500 (git/loader failure).
//   - ErrBuildEvaluator        → internal-error / 500 (CEL initialization).
//
// [artifact.ErrMigrationChainBroken] is propagated from LoadMigrationChain
// as-is (caller maps it to 422 / validation-failed): the incarnation is untouched,
// the problem is in the target itself.
var (
	ErrServiceNotRegistered  = errors.New("incarnation: service is not registered (manage via service.* API, ADR-029)")
	ErrUpgradeNoop           = errors.New("incarnation: to_version matches current — nothing to upgrade")
	ErrDowngradeViaRef       = errors.New("incarnation: to_version downgrades state_schema_version (forward-only, ADR-019)")
	ErrTargetSnapshotInvalid = errors.New("incarnation: target service snapshot has no manifest")
	ErrLoadTargetSnapshot    = errors.New("incarnation: load target service snapshot failed")
	ErrBuildEvaluator        = errors.New("incarnation: build migration evaluator failed")
	ErrLoadMigrationChain    = errors.New("incarnation: load migration chain failed")
)

// ServiceResolver resolves the git coordinates of a service repo by service name
// (`incarnation.service` → the service registry in the DB, ADR-029). A narrow subset
// of scenario.ServiceRegistry; the real resolver satisfies it structurally.
type ServiceResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// ServiceSnapshotLoader materializes a snapshot of the target service ref (to
// read `state_schema_version` from service.yml) and assembles the chain of
// state_schema migrations current→target. A narrow subset of
// [artifact.ServiceLoader]; the real loader satisfies it structurally.
type ServiceSnapshotLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	LoadMigrationChain(art *artifact.ServiceArtifact, from, to int) (statemigrate.Chain, error)
	// ListUpgrades scans upgrade/<slug>/main.yml of the target snapshot (ADR-0068
	// §3), returning scenarios with FromVersions filled in for from→to resolution.
	ListUpgrades(art *artifact.ServiceArtifact) ([]artifact.Scenario, error)
}

// PrepareUpgrade — orchestrates upgrade-target resolution (Variant C): from the
// current inc and target to_version assembles a ready [UpgradeInput] for
// [UpgradeStateSchema]. Clean of HTTP/MCP transport: returns typed sentinel
// errors, each caller (REST-handler / MCP-tool) maps them to its own error
// format itself.
//
// Steps (order matches REST-handler IncarnationHandler.Upgrade):
//  1. Resolve(inc.Service) → git coordinates; .Ref is overridden to toVersion.
//  2. Load(targetRef) → snapshot; Manifest.StateSchemaVersion = target.
//  3. No-op detection: same ref AND same schema → [ErrUpgradeNoop].
//  4. Downgrade guard: target < current → [ErrDowngradeViaRef] (forward-only).
//  5. LoadMigrationChain(art, current, target) → chain (empty = ref-bump).
//  6. NewEvaluator → migration-CEL.
//  7. Assemble UpgradeInput (ApplyID / ChangedByAID passed by the caller).
//
// inc arrives already loaded (the caller does SelectByName itself — for
// 404 semantics and the FOR UPDATE race). evaluator/chain are only prepared
// here; atomic application happens in [UpgradeStateSchema].
func PrepareUpgrade(
	ctx context.Context,
	resolver ServiceResolver,
	loader ServiceSnapshotLoader,
	inc *Incarnation,
	toVersion string,
	applyID string,
	changedByAID *string,
) (UpgradeInput, error) {
	ref, ok := resolver.Resolve(inc.Service)
	if !ok {
		return UpgradeInput{}, fmt.Errorf("%w: %s", ErrServiceNotRegistered, inc.Service)
	}
	ref.Ref = toVersion

	art, err := loader.Load(ctx, ref)
	if err != nil {
		return UpgradeInput{}, fmt.Errorf("%w: %v", ErrLoadTargetSnapshot, err)
	}
	// The narrow interface doesn't guarantee Manifest != nil by contract (a mock
	// could return art without a manifest): defensive check before dereferencing.
	if art == nil || art.Manifest == nil {
		return UpgradeInput{}, ErrTargetSnapshotInvalid
	}
	target := art.Manifest.StateSchemaVersion
	current := inc.StateSchemaVersion

	// No-op: the exact same ref AND the same schema — nothing to upgrade. Changing
	// the ref under the same schema (target == current, toVersion != inc.ServiceVersion)
	// is a legitimate ref-bump (chain empty, UpgradeStateSchema performs a no-op).
	if toVersion == inc.ServiceVersion && target == current {
		return UpgradeInput{}, fmt.Errorf("%w: %s", ErrUpgradeNoop, toVersion)
	}

	// Downgrade via git-ref (target schema < current one): forward-only
	// (ADR-019). The early guard avoids a wasted LoadMigrationChain call (on
	// from>to the loader returns a plain error, not ErrMigrationChainBroken).
	if target < current {
		return UpgradeInput{}, fmt.Errorf("%w: %s", ErrDowngradeViaRef, toVersion)
	}

	chain, err := loader.LoadMigrationChain(art, current, target)
	if err != nil {
		if errors.Is(err, artifact.ErrMigrationChainBroken) {
			// The chain to target is incomplete — the requested to_version is unreachable.
			// Propagate as-is: a semantic request rejection (caller → 422).
			return UpgradeInput{}, err
		}
		return UpgradeInput{}, fmt.Errorf("%w: %v", ErrLoadMigrationChain, err)
	}

	ev, err := statemigrate.NewEvaluator()
	if err != nil {
		return UpgradeInput{}, fmt.Errorf("%w: %v", ErrBuildEvaluator, err)
	}

	// Resolve the upgrade scenario for the inc.ServiceVersion → toVersion transition
	// (ADR-0068 §5): scan upgrade/ of the target snapshot, match from ⊇ the current pin.
	// A scan error falls back to legacy (slug="") — fail-open §5★: a broken/undeclared
	// transition goes to drift instead of failing (patch upgrades must not break).
	upgrades, uerr := loader.ListUpgrades(art)
	if uerr != nil {
		// Not silent: a real scan failure (upgrade/ exists but is unreadable) would
		// otherwise look like "no scenario". slog.Default keeps the signature clean of a transport logger.
		slog.Default().Warn("incarnation: upgrade scan failed, falling back to legacy",
			slog.String("incarnation", inc.Name), slog.String("service", inc.Service),
			slog.String("to_version", toVersion), slog.Any("error", uerr))
	}
	slug, _ := artifact.ResolveUpgradeScenario(upgrades, inc.ServiceVersion)

	return UpgradeInput{
		Name:             inc.Name,
		TargetServiceVer: toVersion,
		TargetSchemaVer:  target,
		Chain:            chain,
		Evaluator:        ev,
		ApplyID:          applyID,
		ChangedByAID:     changedByAID,
		UpgradeSlug:      slug,
		TargetRef:        ref, // ref.Ref already = toVersion (see above) — for runner.Start auto-launch
	}, nil
}
