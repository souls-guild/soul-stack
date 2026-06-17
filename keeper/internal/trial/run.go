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

// Run прогоняет испытания по target. target — путь к одному case-файлу
// (L0 `case.yml` либо L1 `migrations/.../tests/<case>.yml`), к директории кейса
// (`tests/<case>/`), либо к директории-дереву, внутри которого case-файлы
// ищутся рекурсивно. Возвращает результаты по каждому кейсу в детерминированном
// порядке.
//
// Маршрутизация уровня по форме файла (мягкий пред-парс ДО strict-декода):
//
//	stand:/verify:               → L2, skip (ADR-023 post-MVP, не исполняется)
//	state_before:/state_after:   → L1, RunMigrationCase (тест миграции)
//	иначе                        → L0, RunCase (render-only strict, unknown-field — ошибка)
func Run(ctx context.Context, target string) ([]Result, error) {
	files, err := discoverCases(target)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("trial: не найдено ни одного case-файла в %q", target)
	}

	// Один migration-CEL evaluator на весь прогон (compile-cache): переиспользуется
	// всеми L1-кейсами. Собирается лениво при первом L1-кейсе.
	var migEv migrationEvaluator

	results := make([]Result, 0, len(files))
	for _, f := range files {
		// Порядок проб важен: сперва L2 (skip), затем L1 (миграция), иначе L0.
		// Strict-декод для L0 не ослаблен — L0-кейс маркеров L1/L2 не несёт и идёт
		// в LoadCase как прежде, unknown-field в нём остаётся ошибкой.
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

// discoverCases резолвит target в список путей case-файлов.
//   - файл → [файл] (L0/L1/L2 определяется формой при прогоне);
//   - директория с case.yml внутри → [этот case.yml];
//   - директория-дерево → рекурсивный поиск всех case-файлов: `case.yml`
//     (L0/L2-форма) + любой `*.yml` под `migrations/.../tests/` (L1-форма).
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
		return nil, fmt.Errorf("trial: обход %q: %w", target, err)
	}
	sort.Strings(found)
	return found, nil
}

// isMigrationTestFile — структурный признак L1 case-файла: `*.yml` (кроме
// `case.yml`), лежащий в директории `tests/`, чей дед — `migrations/`
// (`.../migrations/<NNN>_to_<MMM>/tests/<case>.yml`). Точная раскладка
// (docs/migrations.md §Тесты), а не «любой yml в tests/»: иначе под обход
// попали бы stand-тесты сервиса (`<service>/tests/smoke.yml`), не относящиеся
// к миграциям. Окончательная классификация уровня — по форме при прогоне.
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
