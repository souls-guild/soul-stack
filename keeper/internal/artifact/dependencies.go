package artifact

import (
	"fmt"
	"log/slog"
	"os"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ServiceDependencies — projection of one Service-repo snapshot's git
// dependencies for the UI Service Detail (`GET /v1/services/{name}/dependencies`):
// the destiny building blocks and custom modules declared in `service.yml`,
// each with its own git ref (ADR-007: version = git tag/branch). Source —
// the manifest's top-level `destiny:` / `modules:` blocks (shared/config.ServiceManifest);
// destiny/module content itself is NOT loaded — the operator's Detail view
// only needs `{name, ref}` (what the service pulls, under which tag).
//
// JSON field names match the UI API (`ServiceDependenciesReply`); both slices
// are non-nil after [ListDependencies] (a service without dependencies is
// valid → empty arrays, not null — parity with [StateSchemaInfo.Migrations] /
// [ListScenarios]).
type ServiceDependencies struct {
	Destiny []Dependency `json:"destiny"`
	Modules []Dependency `json:"modules"`
}

// Dependency — one entry of the manifest's `destiny[]` / `modules[]` (metadata-only):
// `name` (kebab-case destiny / two-level `<namespace>.<module>`), `ref`
// (git tag or branch, ADR-007), and optional `git` (per-entry full-URL
// override, supported only for destiny[] — always empty for modules[] per
// the config.validateDependencyRef contract).
type Dependency struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	Git  string `json:"git,omitempty"`
}

// ListDependencies assembles [ServiceDependencies] from a materialized
// service-repo snapshot (serviceRoot — absolute path, usually
// [ServiceArtifact.LocalDir]).
//
// Parses `service.yml` via the normative [config.LoadServiceManifestFromBytes];
// error-level diagnostics == a broken manifest in the repo → error propagates
// up (caller returns 502), parity with [ListStateSchema]. The `destiny:` /
// `modules:` blocks themselves are optional: absent → empty slices (a service
// without dependencies is valid). Logger is optional (nil → slog.Default);
// unused for now (reading the manifest has no partial-success path), but the
// signature stays symmetric with [ListStateSchema] / [ListScenarios].
func ListDependencies(serviceRoot string, logger *slog.Logger) (*ServiceDependencies, error) {
	if logger == nil {
		logger = slog.Default()
	}

	manifestPath, err := securejoin.SecureJoin(serviceRoot, serviceManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь %s: %w", serviceManifestFile, err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("artifact: чтение %s: %w", serviceManifestFile, err)
	}
	manifest, _, diags, err := config.LoadServiceManifestFromBytes(serviceManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s: %w", serviceManifestFile, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s невалиден: %s", serviceManifestFile, firstError(diags))
	}

	return &ServiceDependencies{
		Destiny: toDependencies(manifest.Destiny),
		Modules: toDependencies(manifest.Modules),
	}, nil
}

// toDependencies projects []config.DependencyRef into []Dependency (non-nil
// result — an empty manifest block is returned as `[]`, not null).
func toDependencies(refs []config.DependencyRef) []Dependency {
	out := make([]Dependency, 0, len(refs))
	for _, r := range refs {
		out = append(out, Dependency{Name: r.Name, Ref: r.Ref, Git: r.Git})
	}
	return out
}
