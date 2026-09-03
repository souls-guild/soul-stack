package artifact

import (
	"fmt"
	"log/slog"
	"os"

	securejoin "github.com/cyphar/filepath-securejoin"
	yaml "gopkg.in/yaml.v3"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// StateSchemaInfo is a projection of state_schema metadata from a single Service
// repository snapshot for UI Schema explorer (`GET /v1/services/{id}/state-schema`):
// current `state_schema_version`, optional state structure declaration
// (`state_schema:` mapping from service.yml), and the flat list of discovered
// migration steps under `migrations/<NNN>_<slug>/`. Migration content is not parsed
// — only metadata (from / to / relative path) so UI can build a "version → version"
// graph without server-side DSL validation.
//
// JSON field names match UI API (`ServiceStateSchemaReply`); types are minimal:
// Schema is stored as `map[string]any` (repeats raw YAML), Migrations is a list
// of [Migration] sorted by `to` ASC.
//
// Schema stays the RAW mapping rather than the manifest's typed
// [config.InputSchemaMap], the same way the scenario input projection does
// ([resolveScenarioTypeRefs]): the UI renders the form and does not want the
// server's opinion about which keys exist. Since [NIM-740] what it carries is the
// input dialect — a map of state field → schema, with no `type: object` wrapper —
// and `$type` references are substituted before it goes out, because a UI handed a
// bare `{$type: AclUser}` can only fail silently.
type StateSchemaInfo struct {
	Version    int            `json:"state_schema_version"`
	Schema     map[string]any `json:"schema,omitempty"`
	Migrations []Migration    `json:"migrations"`
}

// Migration is one entry in the state_schema migration chain (metadata-only):
// source and target version numbers + relative path of the step document in the
// snapshot. Content (DSL operations) is NOT parsed — UI Schema explorer needs only
// the `from → to` graph (user views migration grammar in git repo).
//
// From is derived and never read: the ladder is forward-only and goes by one, so a
// step leading to N comes from N-1.
type Migration struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Path string `json:"path"`
}

// ListStateSchema collects [StateSchemaInfo] from a materialized service repository
// snapshot (serviceRoot is absolute path, typically [ServiceArtifact.LocalDir]).
//
// Algorithm:
//  1. Parses `service.yml` via normative [config.LoadServiceManifestFromBytes];
//     does NOT re-validate manifest-level validation — error diagnostics mean
//     broken manifest in repo, error is raised above (caller returns 502).
//  2. Extracts `state_schema:` (optional; a map of state field → schema in the input
//     dialect, see validateStateSchema) as a raw mapping with `$type` resolved —
//     [rawStateSchema]. If field is missing — Schema=nil, omitempty drops it from
//     JSON; UI treats as "structure not declared".
//  3. Scans `migrations/` via [config.ScanMigrationLadder] (directory missing →
//     empty list, no error; parity with [ListScenarios]). The version reported is
//     the top of that ladder — the SAME read the upgrade path makes, so the
//     explorer and the engine cannot disagree about which version a ref is on.
//
// Logger is optional (nil → slog.Default). Stop-rules per spec:
//   - migrations/ missing → empty list, version 1, no error.
//   - YAML broken → error (caller returns 502 bad-gateway).
func ListStateSchema(serviceRoot string, logger *slog.Logger) (*StateSchemaInfo, error) {
	if logger == nil {
		logger = slog.Default()
	}

	manifestPath, err := securejoin.SecureJoin(serviceRoot, serviceManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: unsafe path %s: %w", serviceManifestFile, err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("artifact: reading %s: %w", serviceManifestFile, err)
	}
	_, _, diags, err := config.LoadServiceManifestFromBytes(serviceManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: parsing %s: %w", serviceManifestFile, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s invalid: %s", serviceManifestFile, firstError(diags))
	}

	ladder, _ := config.ScanMigrationLadder(serviceRoot)
	migrations := make([]Migration, 0, len(ladder.Steps))
	for _, s := range ladder.Steps {
		migrations = append(migrations, Migration{From: s.Version - 1, To: s.Version, Path: s.Path})
	}
	return &StateSchemaInfo{
		Version:    ladder.Version(),
		Schema:     rawStateSchema(data, serviceRoot, logger),
		Migrations: migrations,
	}, nil
}

// rawStateSchema re-reads the `state_schema:` block off the manifest bytes as a raw
// mapping and resolves its `$type` references against the service's type catalog —
// the projection the UI Schema explorer gets (see [StateSchemaInfo]).
//
// It decodes a second time on purpose. The manifest's own StateSchema is the typed
// [config.InputSchemaMap], which is what every ENGINE consumer wants and what an
// operator form does not: marshalling it would put Go field names and every absent
// key on the wire, and the projection would then be pinned to the struct's shape
// rather than to the dialect. `data` has already been parsed once and found valid, so
// this decode is over known-good bytes; a failure here is not fatal — the UI treats
// an absent schema as "structure not declared", which beats a 502 over a listing.
func rawStateSchema(data []byte, serviceRoot string, logger *slog.Logger) map[string]any {
	var raw struct {
		StateSchema map[string]any `yaml:"state_schema"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		logger.Warn("artifact: state_schema projection skipped — invalid YAML",
			slog.Any("error", err))
		return nil
	}
	if raw.StateSchema == nil {
		return nil
	}
	return resolveScenarioTypeRefs(raw.StateSchema, loadTypeCatalog(serviceRoot, logger))
}
