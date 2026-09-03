package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// serviceManifestFile is the root service manifest filename in the repository.
const serviceManifestFile = "service.yml"

// ErrMigrationChainBroken is returned when an expected migration step is missing
// (no `migrations/<NNN>_<slug>/` leads to the version the chain needs): upgrade
// requires it but the ladder has a gap there.
// Symmetric with statemigrate codes: snake_case marker for diagnostics.
var ErrMigrationChainBroken = errors.New("artifact: migration_chain_broken")

// ServiceLoader loads Service repositories into cache under cacheRoot and parses
// their `service.yml`. Safe for concurrent use: per-service Mutex serializes git
// operations on a single working clone (different services run in parallel).
type ServiceLoader struct {
	snap snapshotter
}

// NewServiceLoader creates a loader with cache root cacheRoot. If logger is nil,
// slog.Default is used.
func NewServiceLoader(cacheRoot string, logger *slog.Logger) *ServiceLoader {
	return &ServiceLoader{snap: newSnapshotter(cacheRoot, logger)}
}

// Load materializes an immutable snapshot of the service at the commit that ref
// resolves to and parses its `service.yml`.
func (l *ServiceLoader) Load(ctx context.Context, ref ServiceRef) (*ServiceArtifact, error) {
	sha1, dir, err := l.snap.snapshot(ctx, ref.Name, ref.Git, ref.Ref, "service")
	if err != nil {
		return nil, err
	}
	art := &ServiceArtifact{Ref: ref, SHA1: sha1, LocalDir: dir}
	manifest, err := l.parseManifest(art)
	if err != nil {
		return nil, err
	}
	art.Manifest = manifest
	// The state-schema version is DERIVED, not declared (NIM-735): the top of the
	// ladder, 1 when `migrations/` is empty. The scan's diagnostics are deliberately
	// dropped here — a snapshot whose ladder is malformed still has to answer "what
	// version is this", and the honest answer is the top of what is actually on
	// disk. Refusing to load instead would turn the documented preview answer for a
	// broken chain (ADR-0068 §6: `reachable: false`, as data) into a 502, and the
	// strict reading has its own home offline: `soul-lint validate-service` runs
	// [config.ValidateMigrationLadder] before the repository is ever pushed.
	ladder, _ := config.ScanMigrationLadder(art.LocalDir)
	art.StateSchemaVersion = ladder.Version()
	return art, nil
}

// parseManifest reads and validates `service.yml` of the snapshot using the
// normative `shared/config` parser. Diagnostics at error level are treated as a
// load error (broken manifest in repo).
func (l *ServiceLoader) parseManifest(art *ServiceArtifact) (*config.ServiceManifest, error) {
	data, err := l.ReadFile(art, serviceManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: reading %s service %q: %w", serviceManifestFile, art.Ref.Name, err)
	}
	manifest, _, diags, err := config.LoadServiceManifestFromBytes(serviceManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: parsing %s service %q: %w", serviceManifestFile, art.Ref.Name, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s service %q invalid: %s", serviceManifestFile, art.Ref.Name, firstError(diags))
	}
	if rdiags := resolveManifestStateSchemaTypeRefs(art, manifest); diag.HasErrors(rdiags) {
		return nil, fmt.Errorf("artifact: %s service %q invalid: %s", serviceManifestFile, art.Ref.Name, firstError(rdiags))
	}
	return manifest, nil
}

// resolveManifestStateSchemaTypeRefs substitutes the `$type` references of
// `state_schema:` with the service's named types, in place on the manifest.
//
// It is the state-side twin of [resolveScenarioInputTypeRefs] and exists for the same
// reason: every runtime consumer of the schema — [config.CollectSecretFields] and so
// the whole declared-secret path, the collection-kind lookup in stateop, the
// top-level-field check in core.state — reads the SHAPE, and an unresolved
// `{$type: AclUser}` node has none. Left to each consumer, one of them would forget
// and answer "no secrets here" about a schema that declares one. Doing it at the
// single load-time chokepoint means every path downstream sees the same resolved
// schema.
//
// types.yml absent → an empty catalog: a reference against it still yields
// input_type_unknown pointing at the reference, and a schema without references
// passes through untouched. Any other read error is an error — a snapshot whose
// catalog cannot be read must not load as a service whose secrets went missing.
func resolveManifestStateSchemaTypeRefs(art *ServiceArtifact, manifest *config.ServiceManifest) []diag.Diagnostic {
	if manifest == nil || !config.SchemaHasTypeRef(manifest.StateSchema) {
		return nil
	}
	data, err := readSnapshotFile(art.LocalDir, config.TypesCatalogFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseParse,
				File: config.TypesCatalogFile, Code: "io_error",
				Message: err.Error(),
				Hint:    "types.yml exists but cannot be read — the $type references in state_schema will not resolve",
			}}
		}
		data = nil
	}
	catalog, diags := config.ParseTypeCatalog(config.TypesCatalogFile, data)
	resolved, rdiags := config.ResolveStateSchemaTypeRefs(manifest.StateSchema, catalog)
	diags = append(diags, withFile(rdiags, serviceManifestFile)...)
	if diag.HasErrors(diags) {
		return diags
	}
	// A declared secret reaching through `$type` is judged only now: at load the
	// element shape was still a reference, so its `key:` had no sibling to name.
	secretDiags := config.ValidateStateSchemaSecrets(manifest.StateSchema, resolved)
	manifest.StateSchema = resolved
	return append(diags, withFile(secretDiags, serviceManifestFile)...)
}

// withFile stamps File on diagnostics that carry none (the config resolver is
// I/O-free and does not know which file it was handed).
func withFile(ds []diag.Diagnostic, file string) []diag.Diagnostic {
	for i := range ds {
		if ds[i].File == "" {
			ds[i].File = file
		}
	}
	return ds
}

// ReadFile reads a file from snapshot by relative path. Path is resolved via
// securejoin: escaping LocalDir (via `..`/absolute path/symlink) is excluded.
func (l *ServiceLoader) ReadFile(art *ServiceArtifact, file string) ([]byte, error) {
	return readSnapshotFile(art.LocalDir, file)
}

// LoadMigrationChain collects the state_schema migration chain from→to from a
// service snapshot (docs/migrations.md): for each version v∈(from, to] it finds the
// step directory that LEADS TO v — `migrations/<NNN>_<slug>/` with NNN = "%03d" v —
// and parses its `main.yml` via [statemigrate.Parse]. Forward-only (ADR-019): from >
// to → error (downgrade unsupported), from == to → empty Chain (no-op ref-bump).
//
// The step is found by scanning rather than by constructing a filename: the slug is
// the author's and only the number is the engine's, which is the whole point of the
// layout. No step leading to v → [ErrMigrationChainBroken] (upgrade requires it but
// the ladder has a gap there). Pattern is [DestinyLoader.parseTasks]/[ReadFile]:
// read via securejoin, parse via a pure function.
func (l *ServiceLoader) LoadMigrationChain(art *ServiceArtifact, from, to int) (statemigrate.Chain, error) {
	if from > to {
		// Downgrade guard at loader level (duplicates caller-side guard in
		// incarnation.UpgradeStateSchema; forward-only, ADR-019).
		return nil, fmt.Errorf("artifact: migration downgrade unsupported: from=%d > to=%d", from, to)
	}
	if from == to {
		return statemigrate.Chain{}, nil
	}

	ladder, _ := config.ScanMigrationLadder(art.LocalDir)
	chain := make(statemigrate.Chain, 0, to-from)
	for v := from + 1; v <= to; v++ {
		step, ok := ladder.Step(v)
		if !ok {
			return nil, fmt.Errorf("%w: %s/%03d_*/%s service %q is missing",
				ErrMigrationChainBroken, config.MigrationsDirName, v, config.MigrationStepFile, art.Ref.Name)
		}
		data, err := readSnapshotFile(art.LocalDir, step.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s service %q is missing", ErrMigrationChainBroken, step.Path, art.Ref.Name)
			}
			return nil, fmt.Errorf("artifact: reading %s service %q: %w", step.Path, art.Ref.Name, err)
		}
		m, err := statemigrate.Parse(data, v, step.Path)
		if err != nil {
			return nil, fmt.Errorf("artifact: parsing %s service %q: %w", step.Path, art.Ref.Name, err)
		}
		chain = append(chain, m)
	}
	return chain, nil
}

// ListUpgrades is a wrapper method over package [ListUpgrades] (ADR-0068 §3):
// scans upgrade/<slug>/main.yml in snapshot art to resolve upgrade target in
// [incarnation.PrepareUpgrade]. Delegates to package function with snapshot's
// localDir and loader's logger; signature narrowed for
// incarnation.ServiceSnapshotLoader.
func (l *ServiceLoader) ListUpgrades(art *ServiceArtifact) ([]Scenario, error) {
	return ListUpgrades(art.LocalDir, l.snap.logger)
}

// CertPolicyInfo — projection of the cert policy of a Service-repo snapshot (NIM-99,
// NIM-745): the manifest's `certificate:` section (nil = nothing declared; a section
// with no `rotate:` block declares the PKI role and no rotation) + the snapshot's
// scenario/ names (for validating Certificate.Rotate.Scenario via resolver/UI) +
// snapshot SHA1 (diagnostics for "which commit").
type CertPolicyInfo struct {
	Certificate *config.CertificateConfig
	Scenarios   []string
	SHA1        string
}

// LoadCertPolicy materializes the service ref snapshot and extracts the `certificate:`
// section of the manifest + the scenario/ names. Pattern — [ListUpgrades]: delegates the
// scan to the batch [ListScenarios] with the snapshot's localDir and the loader's logger.
func (l *ServiceLoader) LoadCertPolicy(ctx context.Context, ref ServiceRef) (*CertPolicyInfo, error) {
	art, err := l.Load(ctx, ref)
	if err != nil {
		return nil, err
	}
	scns, err := ListScenarios(art.LocalDir, l.snap.logger)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(scns))
	for i := range scns {
		names = append(names, scns[i].Name)
	}
	return &CertPolicyInfo{
		Certificate: art.Manifest.Certificate,
		Scenarios:   names,
		SHA1:        art.SHA1,
	}, nil
}

// ReadSnapshotFile reads a file from snapshot by absolute localDir (root of
// materialized service/destiny snapshot) and relative path. Exported wrapper over
// common securejoin-reader for out-of-package callers (render-wiring builds
// [render.TemplateReader] from it over concrete snapshot, without knowing internal
// cache layout). Escaping localDir (`..`/absolute path/symlink outward) is clamped
// by securejoin.
func ReadSnapshotFile(localDir, relPath string) ([]byte, error) {
	return readSnapshotFile(localDir, relPath)
}

// readSnapshotFile reads a file from snapshot localDir by relative path. Common
// for service and destiny snapshots: securejoin clamps escaping localDir.
func readSnapshotFile(localDir, path string) ([]byte, error) {
	full, err := securejoin.SecureJoin(localDir, path)
	if err != nil {
		return nil, fmt.Errorf("artifact: unsafe path %q: %w", path, err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("artifact: reading %s: %w", filepath.Base(path), err)
	}
	return data, nil
}

// firstError returns the message of the first error diagnostic for a brief report.
func firstError(diags []diag.Diagnostic) string {
	for i := range diags {
		if diags[i].Level == diag.LevelError {
			return diags[i].Message
		}
	}
	return "unknown validation error"
}
