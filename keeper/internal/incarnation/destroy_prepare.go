package incarnation

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// Sentinel errors for the destroy prepare phase (resolves the service snapshot
// BEFORE the Destroy transaction). Separate from the tx-level
// [ErrIncarnationNotDestroyable]:
//   - ErrServiceNotRegistered  → caller maps to internal-error / 500 (the
//     incarnation's service must be in the service registry; reused from
//     upgrade_prepare.go — same reason).
//   - ErrDestroyScenarioMissing → 422 / 409 validation-failed (force=false and
//     the snapshot has no `destroy` scenario: nothing to run teardown with).
//     Returned BEFORE the transition to destroying — incarnation stays untouched.
//   - ErrLoadTargetSnapshot     → internal-error / 500 (git/loader failure;
//     reused from upgrade_prepare.go).
//   - ErrTargetSnapshotInvalid  → internal-error / 500 (snapshot without a
//     manifest; reused from upgrade_prepare.go).
//
// ErrServiceNotRegistered / ErrLoadTargetSnapshot / ErrTargetSnapshotInvalid
// are declared in upgrade_prepare.go — reused as-is (same snapshot-resolve
// semantics, a duplicate would be a bug).
var ErrDestroyScenarioMissing = errors.New("incarnation: service snapshot has no `destroy` scenario")

// destroyScenarioName — the teardown scenario's name in the service snapshot
// (scenario/orchestration.md §1: `scenario/<name>/main.yml`). Matches
// destroyScenarioLabel (destroy.go) in value, but they're different roles: label
// is the state_history transition tag, name is the scenario file name in the repo.
// Kept as a separate constant so a future change to one doesn't silently drag the
// other.
const destroyScenarioName = "destroy"

// destroyScenarioMainFile — relative path to the teardown scenario's entry point
// in the service snapshot. Same format as scenario.scenarioMainFile.
const destroyScenarioMainFile = "scenario/" + destroyScenarioName + "/main.yml"

// DestroyScenarioReader — narrow surface of the service snapshot loader for the
// teardown-scenario pre-check: materializes the snapshot at a ref and reads a file
// from it. The real [artifact.ServiceLoader] satisfies it structurally; unit tests
// supply a fake. Resolving ref → git coordinates is [ServiceResolver]'s job
// (shared with upgrade_prepare.go).
type DestroyScenarioReader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
}

// PrepareDestroy — handler-level destroy preparation (mirrors the
// [PrepareUpgrade] pattern): BEFORE the [Destroy] transaction, checks that
// teardown is runnable. Loading the snapshot from git under FOR UPDATE is
// unacceptable (long I/O under a row lock), so the check happens here, BEFORE the
// transition to destroying.
//
// Steps:
//  1. Resolve(inc.Service) → git coordinates of the current service version. Ref
//     is overridden to inc.ServiceVersion (teardown runs against the deployed
//     service version, not tip).
//  2. Load(ref) → service snapshot.
//  3. Check for scenario/destroy/main.yml in the snapshot.
//
// force semantics:
//   - force=false AND scenario `destroy` is missing → [ErrDestroyScenarioMissing]
//     (nothing to run teardown with — caller returns 422/409 BEFORE destroying).
//   - force=true → the snapshot is still loaded and checked (diagnostics), but a
//     missing scenario does NOT block: force means "tear down without teardown"
//     (S-D3 deletes the row directly). ok=true.
//   - scenario present → ok=true regardless of force.
//
// Returns the loaded service snapshot (`art`): the caller reads
// `lifecycle.auto_destroy` from it (S3 enforcement) without a second git load. On
// error art == nil. art != nil ⇒ art.Manifest is valid (Load parses the manifest).
//
// Transport-agnostic (no HTTP/MCP): returns typed sentinel errors, the caller
// maps them to its own error format. Wiring this into the handler is S-D4's job.
// inc arrives already loaded (the caller does SelectByName itself — for 404
// semantics and the FOR UPDATE race in Destroy).
func PrepareDestroy(
	ctx context.Context,
	resolver ServiceResolver,
	reader DestroyScenarioReader,
	inc *Incarnation,
	force bool,
) (*artifact.ServiceArtifact, error) {
	ref, ok := resolver.Resolve(inc.Service)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrServiceNotRegistered, inc.Service)
	}
	// Teardown runs against the deployed service version, not branch tip: the
	// `destroy` scenario is taken from the same ref as the current incarnation.
	ref.Ref = inc.ServiceVersion

	art, err := reader.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadTargetSnapshot, err)
	}
	if art == nil {
		return nil, ErrTargetSnapshotInvalid
	}

	hasScenario, err := destroyScenarioExists(reader, art)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadTargetSnapshot, err)
	}
	if !hasScenario && !force {
		return nil, fmt.Errorf("%w: %s", ErrDestroyScenarioMissing, inc.Service)
	}
	return art, nil
}

// HasDestroyScenario — exported probe for the `destroy` scenario's presence in an
// already-loaded snapshot (S3: handler/MCP re-check teardown runnability AFTER
// resolving `lifecycle.auto_destroy`, when effectiveForce=false — the regular
// teardown path). No second git load: art is already materialized by
// [PrepareDestroy]. Thin wrapper over [destroyScenarioExists] (same
// os.ErrNotExist→false semantics).
func HasDestroyScenario(reader DestroyScenarioReader, art *artifact.ServiceArtifact) (bool, error) {
	return destroyScenarioExists(reader, art)
}

// destroyScenarioExists checks for scenario/destroy/main.yml in the snapshot. A
// missing file (os.ErrNotExist through the loader wrapper) is (false, nil), not an
// error: "no teardown scenario" is a normal check outcome, and PrepareDestroy
// decides what to do with it based on force. Any other I/O read error is
// propagated to the caller.
func destroyScenarioExists(reader DestroyScenarioReader, art *artifact.ServiceArtifact) (bool, error) {
	_, err := reader.ReadFile(art, destroyScenarioMainFile)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
