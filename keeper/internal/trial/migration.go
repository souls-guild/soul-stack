package trial

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	yaml "github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/config"
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
// docs/migrations.md §Tests). Layout: `migrations/<NNN>_<slug>/tests/
// <case>.yml`, form differs fundamentally from L0 (separate type, not
// extension of Case): state_before is applied by the step it sits under and
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

// RunMigrationCase runs one L1 case hermetically: parses the step it sits under
// (`migrations/<NNN>_<slug>/main.yml`), applies it to state_before through the pure
// statemigrate core and compares the result deep-equal with state_after.
//
// caseFile — path to case file itself (from discoverCases). One step document =
// one Chain step (per-step tests, docs/migrations.md §Tests). ev — shared
// migration-CEL evaluator (compile-cache; if nil runner assembles its own).
func RunMigrationCase(ctx context.Context, mc *MigrationCase, caseFile string, ev statemigrate.Evaluator) (Result, error) {
	res := Result{Case: mc.Name}

	migPath, toVersion := migrationPathFor(caseFile)
	data, err := os.ReadFile(migPath)
	if err != nil {
		return res, fmt.Errorf("trial: read migration %s: %w", migPath, err)
	}
	mig, err := statemigrate.Parse(data, toVersion, migPath)
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

// migrationPathFor derives the step document and the version it leads to from an L1
// case file path. Layout (docs/migrations.md §Tests):
// `migrations/<NNN>_<slug>/tests/<case>.yml` → `migrations/<NNN>_<slug>/main.yml`,
// leading to version <NNN>.
//
// The case now sits INSIDE the step it exercises rather than beside it, so the
// document is a sibling of its own tests/ directory and no name has to be rebuilt.
// A step directory whose name states no version yields 0 — the test still runs, and
// what it asserts (state_before → state_after) never depended on the number; only
// the diagnostics on a failure lose it.
func migrationPathFor(caseFile string) (string, int) {
	testsDir := filepath.Dir(caseFile) // .../migrations/<NNN>_<slug>/tests
	stepDir := filepath.Dir(testsDir)  // .../migrations/<NNN>_<slug>
	version := 0
	if m := reStepDirVersion.FindStringSubmatch(filepath.Base(stepDir)); m != nil {
		version, _ = strconv.Atoi(m[1])
	}
	return filepath.Join(stepDir, config.MigrationStepFile), version
}

// reStepDirVersion pulls the leading <NNN> off a step directory name. Deliberately
// looser than the linter's rule ([config.ScanMigrationLadder]): the trial runner
// judges migrations, not layouts, and a slug it dislikes must not stop a case from
// running.
var reStepDirVersion = regexp.MustCompile(`^(\d{3})_`)

// compareState compares expected state_after with the final migration state
// through the common by-key mechanism ([compareFieldsByKey]) and then requires
// COMPLETE match: an extra key in the result (one no state_after names) is a
// divergence too. A migration is a deterministic function of the old state
// ([ADR-019]), so the whole result is fixed, and a field appearing that the case
// did not predict is exactly the defect the test exists to catch.
//
// This is the one place that keeps whole-state equality: the L0 scenario form is
// a subset ([compareStateSubset]), where the run is not a pure function of the
// fixture and the case names only the fields it asserts.
func compareState(want, got map[string]any) []string {
	fails := compareFieldsByKey("state_after", want, got)
	for _, field := range sortedKeys(got) {
		if _, ok := want[field]; !ok {
			fails = append(fails, fmt.Sprintf("state.%s: extra field in migration result (not in state_after): %v", field, got[field]))
		}
	}
	return fails
}
