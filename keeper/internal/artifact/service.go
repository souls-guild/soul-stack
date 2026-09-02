package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// serviceManifestFile is the root service manifest filename in the repository.
const serviceManifestFile = "service.yml"

// migrationsDir is the directory for state_schema migration chain in Service repo
// (docs/migrations.md §Layout: `migrations/<NNN>_to_<MMM>.yml`).
const migrationsDir = "migrations"

// ErrMigrationChainBroken is returned when an expected migration step is missing
// (`migrations/<NNN>_to_<MMM>.yml`): upgrade requires it but file does not exist.
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

// LoadMigrationChain collects state_schema migration chain from→to from service
// snapshot (docs/migrations.md): for each version v∈[from, to) reads
// `migrations/<NNN>_to_<MMM>.yml` (NNN = "%03d" v, MMM = "%03d" v+1) and parses
// via [statemigrate.Parse]. Forward-only (ADR-019): from > to → error (downgrade
// unsupported), from == to → empty Chain (no-op ref-bump).
//
// Missing migration file → [ErrMigrationChainBroken] (upgrade requires it but
// file does not exist). Pattern is [DestinyLoader.parseTasks]/[ReadFile]: read via
// securejoin, parse via pure function [statemigrate.Parse].
func (l *ServiceLoader) LoadMigrationChain(art *ServiceArtifact, from, to int) (statemigrate.Chain, error) {
	if from > to {
		// Downgrade guard at loader level (duplicates caller-side guard in
		// incarnation.UpgradeStateSchema; forward-only, ADR-019).
		return nil, fmt.Errorf("artifact: migration downgrade unsupported: from=%d > to=%d", from, to)
	}
	if from == to {
		return statemigrate.Chain{}, nil
	}

	chain := make(statemigrate.Chain, 0, to-from)
	for v := from; v < to; v++ {
		rel := path.Join(migrationsDir, fmt.Sprintf("%03d_to_%03d.yml", v, v+1))
		data, err := readSnapshotFile(art.LocalDir, rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s service %q is missing", ErrMigrationChainBroken, rel, art.Ref.Name)
			}
			return nil, fmt.Errorf("artifact: reading %s service %q: %w", rel, art.Ref.Name, err)
		}
		m, err := statemigrate.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("artifact: parsing %s service %q: %w", rel, art.Ref.Name, err)
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

// CertPolicyInfo — projection of the cert-rotation policy of a Service-repo snapshot (NIM-99):
// the manifest's `certificate_rotation:` section (nil = rotation not declared) +
// the snapshot's scenario/ names (for validating Rotation.Scenario via resolver/UI) + snapshot
// SHA1 (diagnostics for "which commit").
type CertPolicyInfo struct {
	Rotation  *config.CertificateRotationConfig
	Scenarios []string
	SHA1      string
}

// LoadCertPolicy materializes the service ref snapshot and extracts the cert-rotation
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
		Rotation:  art.Manifest.CertificateRotation,
		Scenarios: names,
		SHA1:      art.SHA1,
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
