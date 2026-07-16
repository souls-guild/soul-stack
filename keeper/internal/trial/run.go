package trial

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Run runs tests on target. target — path to single case file
// (L0 `case.yml` or L1 `migrations/.../tests/<case>.yml`), to case directory
// (`tests/<case>/`), or to directory tree where case files
// are searched recursively. Returns results for each case in deterministic
// order.
//
// Level routing by file form (soft pre-parse BEFORE strict decode):
//
//	stand:/verify:               → L2, skip (ADR-023 post-MVP, not executed)
//	state_before:/state_after:   → L1, RunMigrationCase (migration test)
//	otherwise                    → L0, RunCase (render-only strict, unknown-field — error)
func Run(ctx context.Context, target string) ([]Result, error) {
	files, err := discoverCases(target)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("trial: no case files found in %q", target)
	}

	// Single migration-CEL evaluator for entire run (compile-cache): reused
	// by all L1 cases. Built lazily at first L1 case.
	var migEv migrationEvaluator

	results := make([]Result, 0, len(files))
	for _, f := range files {
		// Probe order is important: first L2 (skip), then L1 (migration), otherwise L0.
		// Strict decode for L0 is not weakened — L0 case does not carry L1/L2 markers and goes
		// to LoadCase as before, unknown-field in it remains error.
		isL2, err := isL2Case(f)
		if err != nil {
			return results, err
		}
		if isL2 {
			results = append(results, Result{Case: f, Pass: true, Skipped: true, Level: LevelL2})
			continue
		}

		isL1, err := isL1Case(f)
		if err != nil {
			return results, err
		}
		if isL1 {
			ev, err := migEv.get()
			if err != nil {
				return results, err
			}
			mc, err := LoadMigrationCase(f)
			if err != nil {
				return results, err
			}
			res, err := RunMigrationCase(ctx, mc, f, ev)
			if err != nil {
				return results, err
			}
			res.Level = LevelL1
			results = append(results, res)
			continue
		}

		c, file, err := LoadCase(f)
		if err != nil {
			return results, err
		}
		res, err := RunCase(ctx, c, file)
		if err != nil {
			return results, err
		}
		res.Level = LevelL0
		results = append(results, res)
	}
	return results, nil
}

// discoverCases resolves target to list of case file paths.
//   - file → [file] (L0/L1/L2 determined by form at run time);
//   - directory with case.yml inside → [that case.yml];
//   - directory tree → recursive search of all case files: `case.yml`
//     (L0/L2 form) + any `*.yml` under `migrations/.../tests/` (L1 form).
func discoverCases(target string) ([]string, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("trial: %w", err)
	}
	if !info.IsDir() {
		return []string{target}, nil
	}

	direct := filepath.Join(target, caseFileName)
	if _, err := os.Stat(direct); err == nil {
		return []string{direct}, nil
	}

	var found []string
	err = filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == caseFileName || isMigrationTestFile(path) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("trial: traversing %q: %w", target, err)
	}
	sort.Strings(found)
	return found, nil
}

// isMigrationTestFile — structural marker of L1 case file: `*.yml` (except
// `case.yml`), located in `tests/` directory whose grandparent is `migrations/`
// (`.../migrations/<NNN>_to_<MMM>/tests/<case>.yml`). Exact layout
// (docs/migrations.md §Tests), not «any yml in tests/»: otherwise stand tests of
// service (`<service>/tests/smoke.yml`) not related to migrations would match.
// Final level classification — by form at run time.
func isMigrationTestFile(path string) bool {
	if !strings.HasSuffix(path, ".yml") || filepath.Base(path) == caseFileName {
		return false
	}
	testsDir := filepath.Dir(path)
	if filepath.Base(testsDir) != "tests" {
		return false
	}
	stepDir := filepath.Dir(testsDir) // <NNN>_to_<MMM>
	return filepath.Base(filepath.Dir(stepDir)) == "migrations"
}
