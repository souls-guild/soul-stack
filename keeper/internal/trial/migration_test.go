package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMigrationTree создаёт временное дерево migrations/<NNN>_to_<MMM>/
// {<NNN>_to_<MMM>.yml, tests/<case>.yml} и возвращает (путь к case-файлу,
// корень дерева для рекурсивного прогона).
func writeMigrationTree(t *testing.T, step, migrationYML, caseName, caseYML string) (caseFile, root string) {
	t.Helper()
	root = t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	stepDir := filepath.Join(migrationsDir, step)
	testsDir := filepath.Join(stepDir, "tests")
	if err := os.MkdirAll(testsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if migrationYML != "" {
		if err := os.WriteFile(filepath.Join(migrationsDir, step+".yml"), []byte(migrationYML), 0o644); err != nil {
			t.Fatalf("write migration: %v", err)
		}
	}
	caseFile = filepath.Join(testsDir, caseName+".yml")
	if err := os.WriteFile(caseFile, []byte(caseYML), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}
	return caseFile, root
}

const renameMigration = `from_version: 1
to_version: 2
transform:
  - rename:
      from: state.old
      to:   state.new
`

// TestRunMigrationCase_Happy — state_before применяется соседней миграцией и
// совпадает со state_after.
func TestRunMigrationCase_Happy(t *testing.T) {
	caseFile, _ := writeMigrationTree(t, "001_to_002", renameMigration, "rename-ok", `name: rename-ok
state_before:
  old: hello
state_after:
  new: hello
`)
	mc, err := LoadMigrationCase(caseFile)
	if err != nil {
		t.Fatalf("LoadMigrationCase: %v", err)
	}
	res, err := RunMigrationCase(context.Background(), mc, caseFile, nil)
	if err != nil {
		t.Fatalf("RunMigrationCase: %v", err)
	}
	if !res.Pass {
		t.Fatalf("ожидался pass, failures: %v", res.Failures)
	}
}

// TestRunMigrationCase_Mismatch — несовпадение state_after даёт понятный fail
// (а не ошибку прогона), с указанием расходящегося поля.
func TestRunMigrationCase_Mismatch(t *testing.T) {
	caseFile, _ := writeMigrationTree(t, "001_to_002", renameMigration, "rename-bad", `name: rename-bad
state_before:
  old: hello
state_after:
  new: WRONG
`)
	mc, err := LoadMigrationCase(caseFile)
	if err != nil {
		t.Fatalf("LoadMigrationCase: %v", err)
	}
	res, err := RunMigrationCase(context.Background(), mc, caseFile, nil)
	if err != nil {
		t.Fatalf("RunMigrationCase должен вернуть fail-Result, а не ошибку: %v", err)
	}
	if res.Pass {
		t.Fatal("ожидался fail")
	}
	if len(res.Failures) == 0 || !strings.Contains(strings.Join(res.Failures, "\n"), "new") {
		t.Fatalf("ожидалось расхождение по полю new, получено: %v", res.Failures)
	}
}

// TestRunMigrationCase_ExtraField — лишний ключ в итоге миграции (нет в
// state_after) — расхождение (L1 сверяет state целиком, не частично).
func TestRunMigrationCase_ExtraField(t *testing.T) {
	caseFile, _ := writeMigrationTree(t, "001_to_002", renameMigration, "extra", `name: extra
state_before:
  old: hello
  keep: 1
state_after:
  new: hello
`)
	mc, err := LoadMigrationCase(caseFile)
	if err != nil {
		t.Fatalf("LoadMigrationCase: %v", err)
	}
	res, err := RunMigrationCase(context.Background(), mc, caseFile, nil)
	if err != nil {
		t.Fatalf("RunMigrationCase: %v", err)
	}
	if res.Pass {
		t.Fatal("ожидался fail из-за лишнего поля keep")
	}
	if !strings.Contains(strings.Join(res.Failures, "\n"), "keep") {
		t.Fatalf("ожидалось упоминание лишнего поля keep, получено: %v", res.Failures)
	}
}

// TestRunMigrationCase_MissingMigrationFile — отсутствие соседнего
// migration-файла → ошибка прогона (не fail-Result).
func TestRunMigrationCase_MissingMigrationFile(t *testing.T) {
	// migrationYML="" → файл миграции не создаётся.
	caseFile, _ := writeMigrationTree(t, "001_to_002", "", "no-mig", `name: no-mig
state_before:
  old: hello
state_after:
  new: hello
`)
	mc, err := LoadMigrationCase(caseFile)
	if err != nil {
		t.Fatalf("LoadMigrationCase: %v", err)
	}
	_, err = RunMigrationCase(context.Background(), mc, caseFile, nil)
	if err == nil {
		t.Fatal("ожидалась ошибка из-за отсутствия migration-файла")
	}
	if !strings.Contains(err.Error(), "миграции") {
		t.Fatalf("ожидалась ошибка чтения миграции, получено: %v", err)
	}
}

// TestLoadMigrationCase_MissingSection — strict-валидация: case без state_after
// отвергается явной ошибкой.
func TestLoadMigrationCase_MissingSection(t *testing.T) {
	caseFile, _ := writeMigrationTree(t, "001_to_002", renameMigration, "incomplete", `name: incomplete
state_before:
  old: hello
`)
	if _, err := LoadMigrationCase(caseFile); err == nil {
		t.Fatal("ожидалась ошибка из-за отсутствия state_after")
	}
}

// TestLoadMigrationCase_UnknownKey — strict-декод отвергает посторонний ключ.
func TestLoadMigrationCase_UnknownKey(t *testing.T) {
	caseFile, _ := writeMigrationTree(t, "001_to_002", renameMigration, "junk", `name: junk
state_before:
  old: hello
state_after:
  new: hello
unexpected: 1
`)
	if _, err := LoadMigrationCase(caseFile); err == nil {
		t.Fatal("ожидалась ошибка strict-декода на ключе unexpected")
	}
}

// TestRouting_L0L1L2_NotConfused — рекурсивный прогон смешанного дерева
// маршрутизирует кейсы по форме без путаницы: L0 case.yml → L0, миграционный
// tests/<case>.yml → L1, stand-кейс → L2 skip.
func TestRouting_L0L1L2_NotConfused(t *testing.T) {
	root := t.TempDir()

	// L1: migrations/001_to_002/{001_to_002.yml, tests/m1.yml}
	migStepDir := filepath.Join(root, "migrations", "001_to_002")
	mustMkdir(t, filepath.Join(migStepDir, "tests"))
	mustWrite(t, filepath.Join(root, "migrations", "001_to_002.yml"), renameMigration)
	mustWrite(t, filepath.Join(migStepDir, "tests", "m1.yml"), `name: m1
state_before:
  old: x
state_after:
  new: x
`)

	// L0: scenario/create/{main.yml, tests/c1/case.yml}
	scnDir := filepath.Join(root, "scenario", "create")
	mustMkdir(t, filepath.Join(scnDir, "tests", "c1"))
	mustWrite(t, filepath.Join(scnDir, "main.yml"), `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - name: write
    module: core.file.present
    params:
      path: /tmp/x
      content: "${ input.greeting }"
`)
	mustWrite(t, filepath.Join(scnDir, "tests", "c1", "case.yml"), `name: l0-case
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)

	// L2: scenario/create/tests/stand1/case.yml (маркер stand:)
	mustMkdir(t, filepath.Join(scnDir, "tests", "stand1"))
	mustWrite(t, filepath.Join(scnDir, "tests", "stand1", "case.yml"), `name: l2-stand
stand:
  hosts: 1
verify:
  - condition: "true"
`)

	results, err := Run(context.Background(), root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var l0, l1, l2 int
	for _, r := range results {
		switch r.Level {
		case LevelL0:
			l0++
			if !r.Pass {
				t.Errorf("L0 %s не прошёл: %v", r.Case, r.Failures)
			}
		case LevelL1:
			l1++
			if !r.Pass {
				t.Errorf("L1 %s не прошёл: %v", r.Case, r.Failures)
			}
		case LevelL2:
			l2++
			if !r.Skipped {
				t.Errorf("L2 %s должен быть skipped", r.Case)
			}
		}
	}
	if l0 != 1 || l1 != 1 || l2 != 1 {
		t.Fatalf("маршрутизация: L0=%d L1=%d L2=%d, want 1/1/1", l0, l1, l2)
	}
}

// TestRouting_StandTestNotPickedAsL1 — `*.yml` в tests/ ВНЕ migrations/ не
// должен попадать в discovery как L1 (структурный фильтр isMigrationTestFile).
func TestRouting_StandTestNotPickedAsL1(t *testing.T) {
	root := t.TempDir()
	// service-level tests/smoke.yml (не под migrations/) — не L1-кандидат.
	mustMkdir(t, filepath.Join(root, "tests"))
	mustWrite(t, filepath.Join(root, "tests", "smoke.yml"), `name: smoke
tasks:
  - name: ping
    module: core.exec.run
`)
	// чтобы дерево не было пустым — добавим валидный L1-кейс.
	migStepDir := filepath.Join(root, "migrations", "001_to_002")
	mustMkdir(t, filepath.Join(migStepDir, "tests"))
	mustWrite(t, filepath.Join(root, "migrations", "001_to_002.yml"), renameMigration)
	mustWrite(t, filepath.Join(migStepDir, "tests", "m1.yml"), `name: m1
state_before:
  old: x
state_after:
  new: x
`)

	files, err := discoverCases(root)
	if err != nil {
		t.Fatalf("discoverCases: %v", err)
	}
	for _, f := range files {
		if filepath.Base(f) == "smoke.yml" {
			t.Fatalf("smoke.yml не должен попасть в discovery: %v", files)
		}
	}
	if len(files) != 1 {
		t.Fatalf("ожидался ровно 1 case (L1 m1.yml), получено: %v", files)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
