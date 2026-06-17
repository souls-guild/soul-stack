package trial

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	yaml "github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// migrationEvaluator — ленивый держатель общего migration-CEL evaluator-а на
// весь прогон дерева. Собирается единожды при первом L1-кейсе (compile-cache
// горячий путь переиспользуется), при отсутствии L1-кейсов не собирается вовсе.
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

// MigrationCase — один L1-кейс теста миграции state_schema (ADR-019,
// docs/migrations.md §Тесты). Раскладка: `migrations/<NNN>_to_<MMM>/tests/
// <case>.yml`, форма принципиально отличается от L0 (отдельный тип, не
// расширение Case): state_before применяется соседней миграцией и сверяется
// deep-equal с state_after.
//
// Декод strict: неизвестный ключ на верхнем уровне — ошибка, а не silent-skip
// (симметрия с LoadCase для L0).
type MigrationCase struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	StateBefore map[string]any `yaml:"state_before"`
	StateAfter  map[string]any `yaml:"state_after"`
}

// LoadMigrationCase читает и валидирует один L1 case-файл. path — путь к самому
// файлу `tests/<case>.yml` (L1-кейс — обычный файл в tests/, не директория с
// case.yml как у L0).
func LoadMigrationCase(path string) (*MigrationCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("trial: чтение %s: %w", path, err)
	}
	var mc MigrationCase
	if err := yaml.UnmarshalWithOptions(data, &mc, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("trial: разбор %s: %w", path, err)
	}
	if err := mc.validate(); err != nil {
		return nil, fmt.Errorf("trial: %s: %w", path, err)
	}
	return &mc, nil
}

func (mc *MigrationCase) validate() error {
	if mc.Name == "" {
		return fmt.Errorf("name: обязателен")
	}
	if mc.StateBefore == nil {
		return fmt.Errorf("state_before: обязателен")
	}
	if mc.StateAfter == nil {
		return fmt.Errorf("state_after: обязателен")
	}
	return nil
}

// RunMigrationCase прогоняет один L1-кейс герметично: парсит соседнюю миграцию
// (`migrations/<NNN>_to_<MMM>.yml`), применяет её к state_before через чистое
// ядро statemigrate и сверяет итог deep-equal с state_after.
//
// caseFile — путь к самому case-файлу (из discoverCases). Один migration-файл =
// один шаг Chain (per-step тесты, docs/migrations.md §Тесты). ev — общий
// migration-CEL evaluator (compile-cache; при nil раннер собирает свой).
func RunMigrationCase(ctx context.Context, mc *MigrationCase, caseFile string, ev statemigrate.Evaluator) (Result, error) {
	res := Result{Case: mc.Name}

	migPath := migrationPathFor(caseFile)
	data, err := os.ReadFile(migPath)
	if err != nil {
		return res, fmt.Errorf("trial: чтение миграции %s: %w", migPath, err)
	}
	mig, err := statemigrate.Parse(data)
	if err != nil {
		return res, fmt.Errorf("trial: разбор миграции %s: %w", migPath, err)
	}

	if ev == nil {
		ev, err = statemigrate.NewEvaluator()
		if err != nil {
			return res, fmt.Errorf("trial: сборка migration-CEL: %w", err)
		}
	}

	out, err := statemigrate.Apply(ctx, mc.StateBefore, statemigrate.Chain{mig}, ev)
	if err != nil {
		return res, fmt.Errorf("trial: применение миграции %s: %w", migPath, err)
	}

	res.Failures = compareState(mc.StateAfter, out.FinalState)
	res.Pass = len(res.Failures) == 0
	return res, nil
}

// migrationPathFor выводит путь migration-файла из пути L1 case-файла. Раскладка
// (docs/migrations.md §Тесты): `migrations/<NNN>_to_<MMM>/tests/<case>.yml` →
// `migrations/<NNN>_to_<MMM>.yml`. Имя директории `<NNN>_to_<MMM>` совпадает с
// базовым именем migration-файла.
func migrationPathFor(caseFile string) string {
	testsDir := filepath.Dir(caseFile)     // .../migrations/<NNN>_to_<MMM>/tests
	stepDir := filepath.Dir(testsDir)      // .../migrations/<NNN>_to_<MMM>
	migrationsDir := filepath.Dir(stepDir) // .../migrations
	return filepath.Join(migrationsDir, filepath.Base(stepDir)+".yml")
}

// compareState сверяет ожидаемый state_after с итоговым state миграции через
// общий diff-механизм (compareStateChanges) — поле→значение с нормализацией
// через structpb. В отличие от частичного assert.state_changes L0, L1 требует
// ПОЛНОГО совпадения: лишний ключ в итоге (которого нет в state_after) — тоже
// расхождение (миграция = детерминированная функция, state фиксируется целиком).
func compareState(want, got map[string]any) []string {
	fails := compareStateChanges(want, got)
	for _, field := range sortedKeys(got) {
		if _, ok := want[field]; !ok {
			fails = append(fails, fmt.Sprintf("state.%s: лишнее поле в итоге миграции (нет в state_after): %v", field, got[field]))
		}
	}
	return fails
}
