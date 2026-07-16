package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// StateSchemaInfo is a projection of state_schema metadata from a single Service
// repository snapshot for UI Schema explorer (`GET /v1/services/{name}/state-schema`):
// current `state_schema_version`, optional state structure declaration
// (`state_schema:` mapping from service.yml), and flat list of discovered
// migrations in `migrations/<NNN>_to_<MMM>.yml`. Migration content is not parsed
// — only metadata (from / to / relative path) so UI can build a "version → version"
// graph without server-side DSL validation.
//
// JSON field names match UI API (`ServiceStateSchemaReply`); types are minimal:
// Schema is stored as `map[string]any` (repeats raw YAML), Migrations is a list
// of [Migration] sorted by `to` ASC.
type StateSchemaInfo struct {
	Version    int            `json:"state_schema_version"`
	Schema     map[string]any `json:"schema,omitempty"`
	Migrations []Migration    `json:"migrations"`
}

// Migration is one entry in the state_schema migration chain (metadata-only):
// source and target version numbers + relative file path in snapshot. Content
// (DSL operations) is NOT parsed — UI Schema explorer needs only the `from → to`
// graph (user views migration grammar in git repo).
type Migration struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Path string `json:"path"`
}

// reMigrationFile is the canonical migration filename pattern in `migrations/`
// (docs/migrations.md → `<NNN>_to_<MMM>.yml`). NNN/MMM are three digits with
// leading zeros; other files (`README.md`, test directories, etc.) are ignored.
var reMigrationFile = regexp.MustCompile(`^(\d{3})_to_(\d{3})\.yml$`)

// ListStateSchema collects [StateSchemaInfo] from a materialized service repository
// snapshot (serviceRoot is absolute path, typically [ServiceArtifact.LocalDir]).
//
// Algorithm:
//  1. Parses `service.yml` via normative [config.LoadServiceManifestFromBytes];
//     does NOT re-validate manifest-level validation — error diagnostics mean
//     broken manifest in repo, error is raised above (caller returns 502).
//  2. Extracts `state_schema_version` (≥1; ADR-019: monotonic int) and `state_schema:`
//     (optional; service.yml MAY declare structure via MVP JSON Schema subset —
//     type/required/properties/items/additionalProperties, see validateStateSchema).
//     If field is missing — Schema=nil, omitempty drops it from JSON; UI treats as
//     "structure not declared".
//  3. Scans `migrations/` (if directory missing → empty list, no error; parity with
//     [ListScenarios]). Files matching `<NNN>_to_<MMM>.yml` are parsed by name only
//     (regex [reMigrationFile]); other entities (subdirs `<NNN>_to_<MMM>/tests/`,
//     README, etc.) are silently skipped. Sorting is by `to` ASC (chain graph grows
//     by version).
//
// Logger is optional (nil → slog.Default). Stop-rules per spec:
//   - state_schema_version missing from manifest → per MVP spec this is a validation
//     error (config.schemaValidateService) raised above via diag.HasErrors →
//     return error.
//   - migrations/ missing → empty list, no error.
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
	manifest, _, diags, err := config.LoadServiceManifestFromBytes(serviceManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: parsing %s: %w", serviceManifestFile, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s invalid: %s", serviceManifestFile, firstError(diags))
	}

	info := &StateSchemaInfo{
		Version: manifest.StateSchemaVersion,
		Schema:  manifest.StateSchema,
	}

	migrations, err := scanMigrations(serviceRoot, logger)
	if err != nil {
		return nil, err
	}
	info.Migrations = migrations
	return info, nil
}

// scanMigrations reads the `migrations/` directory of snapshot and returns a list
// of discovered steps sorted by `to` ASC. Missing directory → empty list (service
// without migrations is valid). Files not matching `<NNN>_to_<MMM>.yml` are silently
// skipped; subdirectories (migration tests) are also skipped (we do NOT descend,
// content is not parsed).
func scanMigrations(serviceRoot string, logger *slog.Logger) ([]Migration, error) {
	migRoot, err := securejoin.SecureJoin(serviceRoot, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("artifact: unsafe path %s: %w", migrationsDir, err)
	}
	entries, err := os.ReadDir(migRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Migration{}, nil
		}
		return nil, fmt.Errorf("artifact: reading %s: %w", migrationsDir, err)
	}

	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			// `<NNN>_to_<MMM>/tests/` and other subdirs are not scanned.
			continue
		}
		m := reMigrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		from, ferr := strconv.Atoi(m[1])
		to, terr := strconv.Atoi(m[2])
		if ferr != nil || terr != nil {
			// Impossible via regex, but guard in case grammar changes.
			logger.Warn("artifact: migration skipped — unparseable NNN/MMM",
				slog.String("file", e.Name()))
			continue
		}
		out = append(out, Migration{
			From: from,
			To:   to,
			Path: migrationsDir + "/" + e.Name(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].To < out[j].To })
	return out, nil
}
