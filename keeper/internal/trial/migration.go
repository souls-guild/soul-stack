package trial

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	yaml "github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// migrationEvaluator — lazy holder of shared migration-CEL evaluator for
// entire tree run. Assembled once on first L1 case (compile-cache hot path
// is reused), not assembled at all if no L1 cases.
type migrationEvaluator struct {
	ev  statemigrate.Evaluator
	err error
	got bool
}

func (m *migrationEvaluator) get() (statemigrate.Evaluator, error) {
	if !m.got {
		m.ev, m.err = statemigrate.NewEvaluator()
		m.got = true
	}
	return m.ev, m.err
}

// MigrationCase — one L1 case of state_schema migration test (ADR-019,
// docs/migrations.md §Tests). Layout: `migrations/<NNN>_to_<MMM>/tests/
// <case>.yml`, form differs fundamentally from L0 (separate type, not
// extension of Case): state_before is applied by neighboring migration and
// compared deep-equal with state_after.
//
// Strict decode: unknown key at top level — error, not silent-skip
// (symmetric with LoadCase for L0).
type MigrationCase struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	StateBefore map[string]any `yaml:"state_before"`
	StateAfter  map[string]any `yaml:"state_after"`
}

// LoadMigrationCase reads and validates one L1 case file. path — path to
// the file itself `tests/<case>.yml` (L1 case — ordinary file in tests/, not
// directory with case.yml like L0).
func LoadMigrationCase(path string) (*MigrationCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("trial: read %s: %w", path, err)
	}
	var mc MigrationCase
	if err := yaml.UnmarshalWithOptions(data, &mc, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("trial: parse %s: %w", path, err)
	}
	if err := mc.validate(); err != nil {
		return nil, fmt.Errorf("trial: %s: %w", path, err)
	}
	return &mc, nil
}

func (mc *MigrationCase) validate() error {
	if mc.Name == "" {
		return fmt.Errorf("name: required")
	}
	if mc.StateBefore == nil {
		return fmt.Errorf("state_before: required")
	}
	if mc.StateAfter == nil {
		return fmt.Errorf("state_after: required")
	}
	return nil
}

// RunMigrationCase runs one L1 case hermetically: parses neighboring migration
// (`migrations/<NNN>_to_<MMM>.yml`), applies it to state_before through pure
// statemigrate core and compares result deep-equal with state_after.
//
// caseFile — path to case file itself (from discoverCases). One migration file =
// one Chain step (per-step tests, docs/migrations.md §Tests). ev — shared
// migration-CEL evaluator (compile-cache; if nil runner assembles its own).
func RunMigrationCase(ctx context.Context, mc *MigrationCase, caseFile string, ev statemigrate.Evaluator) (Result, error) {
	res := Result{Case: mc.Name}

	migPath := migrationPathFor(caseFile)
	data, err := os.ReadFile(migPath)
	if err != nil {
		return res, fmt.Errorf("trial: read migration %s: %w", migPath, err)
	}
	mig, err := statemigrate.Parse(data)
	if err != nil {
		return res, fmt.Errorf("trial: parse migration %s: %w", migPath, err)
	}

	if ev == nil {
		ev, err = statemigrate.NewEvaluator()
		if err != nil {
			return res, fmt.Errorf("trial: build migration-CEL: %w", err)
		}
	}

	out, err := statemigrate.Apply(ctx, mc.StateBefore, statemigrate.Chain{mig}, ev)
	if err != nil {
		return res, fmt.Errorf("trial: apply migration %s: %w", migPath, err)
	}

	res.Failures = compareState(mc.StateAfter, out.FinalState)
	res.Pass = len(res.Failures) == 0
	return res, nil
}

// migrationPathFor derives migration file path from L1 case file path. Layout
// (docs/migrations.md §Tests): `migrations/<NNN>_to_<MMM>/tests/<case>.yml` →
// `migrations/<NNN>_to_<MMM>.yml`. Directory name `<NNN>_to_<MMM>` matches
// migration file basename.
func migrationPathFor(caseFile string) string {
	testsDir := filepath.Dir(caseFile)     // .../migrations/<NNN>_to_<MMM>/tests
	stepDir := filepath.Dir(testsDir)      // .../migrations/<NNN>_to_<MMM>
	migrationsDir := filepath.Dir(stepDir) // .../migrations
	return filepath.Join(migrationsDir, filepath.Base(stepDir)+".yml")
}

// compareState compares expected state_after with final migration state through
// common diff mechanism (compareStateChanges) — field→value with normalization
// via structpb. Unlike partial assert.state_changes L0, L1 requires COMPLETE
// match: extra key in result (not in state_after) — also divergence (migration =
// deterministic function, state is fixed entirely).
func compareState(want, got map[string]any) []string {
	fails := compareStateChanges(want, got)
	for _, field := range sortedKeys(got) {
		if _, ok := want[field]; !ok {
			fails = append(fails, fmt.Sprintf("state.%s: extra field in migration result (not in state_after): %v", field, got[field]))
		}
	}
	return fails
}
