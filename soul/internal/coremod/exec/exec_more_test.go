package exec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/exec"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// apply — прогон Apply (state=run) на переданном модуле; возвращает финальное
// событие. Контракт core-модуля: ошибки доменной логики идут в failed-event,
// сам Apply возвращает Go-error только при сбое стрима — здесь его нет.
func apply(t *testing.T, m *exec.Module, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "run",
		Params: mustStruct(t, params),
	}, stream); err != nil {
		t.Fatalf("Apply вернул Go-error (контракт — failed-event, не error): %v", err)
	}
	ev := stream.Last()
	if ev == nil {
		t.Fatal("Apply не отправил ни одного события")
	}
	return ev
}

// noStat — StatFile, который никогда не находит файл (creates всегда промахивается).
func noStat(string) (bool, error) { return false, nil }

// Неизвестный state → failed с понятным message (ветка req.State != "run").
func TestApply_UnknownState_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	m := newModule(r, noStat)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "shell",
		Params: mustStruct(t, map[string]any{"cmd": "true"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("неизвестный state должен давать failed-event")
	}
	if ev.Message != `unknown state "shell"` {
		t.Fatalf("message=%q", ev.Message)
	}
	if len(r.Calls) != 0 {
		t.Fatalf("при неизвестном state команда не запускается, calls=%v", r.Calls)
	}
}

// argv-форма: метасимволы (|, >, ;, $VAR) НЕ интерпретируются shell-ом, а
// уходят как литеральные аргументы в RunOpts. Это ключевое отличие core.exec
// от core.cmd. Проверяем, что модуль НЕ оборачивает вызов в sh -c, а строит
// ключ напрямую из cmd+args.
func TestApply_Argv_MetacharsAreLiteral(t *testing.T) {
	r := internaltest.NewRunner()
	// Если бы модуль использовал shell, ключ был бы "sh -c ...". argv-форма
	// даёт ключ "echo a|b > c ; $HOME" — метасимволы как обычные аргументы.
	key := "echo a|b > c ; $HOME"
	r.Results[key] = []util.Result{{ExitCode: 0, Stdout: "a|b > c ; $HOME\n"}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{
		"cmd":  "echo",
		"args": []any{"a|b", ">", "c", ";", "$HOME"},
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if len(r.Calls) != 1 || r.Calls[0] != key {
		t.Fatalf("ожидал argv-вызов %q без shell-обёртки, получил %v", key, r.Calls)
	}
	if got := ev.Output.Fields["stdout"].GetStringValue(); got != "a|b > c ; $HOME\n" {
		t.Fatalf("stdout=%q", got)
	}
}

// Ошибка запуска основного процесса (бинарь не найден / permission) → Result.Err
// → failed с message "exec <cmd>: <err>". Единственный путь к failed от runner-а.
func TestApply_RunnerLaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["nonexistent-binary"] = []util.Result{{Err: os.ErrNotExist}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{"cmd": "nonexistent-binary"})
	if !ev.Failed {
		t.Fatal("Result.Err должен давать failed")
	}
	if ev.Message != "exec nonexistent-binary: file does not exist" {
		t.Fatalf("message=%q", ev.Message)
	}
}

// Извлечение опциональных параметров неверного типа → failed на этапе разбора,
// не доходя до runner-а. Покрывает каждую ветку err из OptStringSlice/
// OptString/OptStringMap извлечения в Apply.
func TestApply_BadParamTypes_Failed(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"args_not_list", map[string]any{"cmd": "x", "args": "single"}},
		{"args_elem_not_string", map[string]any{"cmd": "x", "args": []any{float64(1)}}},
		{"cwd_not_string", map[string]any{"cmd": "x", "cwd": float64(7)}},
		{"env_not_map", map[string]any{"cmd": "x", "env": "PATH=/bin"}},
		{"env_value_not_string", map[string]any{"cmd": "x", "env": map[string]any{"A": float64(1)}}},
		{"creates_not_string", map[string]any{"cmd": "x", "creates": float64(1)}},
		{"unless_not_string", map[string]any{"cmd": "x", "unless": float64(1)}},
		{"onlyif_not_string", map[string]any{"cmd": "x", "onlyif": float64(1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := internaltest.NewRunner()
			m := newModule(r, noStat)
			ev := apply(t, m, c.params)
			if !ev.Failed {
				t.Fatalf("%s: ожидал failed", c.name)
			}
			if len(r.Calls) != 0 {
				t.Fatalf("%s: ошибка разбора параметра не должна запускать команд, calls=%v", c.name, r.Calls)
			}
		})
	}
}

// creates: stat вернул ошибку (не ErrNotExist, напр. permission denied) →
// shouldSkip отдаёт serr → failed, команда НЕ запускается. Ветка serr в Apply
// и err-ветка StatFile в shouldSkip.
func TestApply_Creates_StatError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	m := newModule(r, func(string) (bool, error) { return false, os.ErrPermission })

	ev := apply(t, m, map[string]any{"cmd": "touch", "args": []any{"/x"}, "creates": "/x"})
	if !ev.Failed {
		t.Fatal("ошибка stat в creates должна давать failed")
	}
	if len(r.Calls) != 0 {
		t.Fatalf("при ошибке guard команда не запускается, calls=%v", r.Calls)
	}
}

// creates: файла нет → НЕ skip, основная команда выполняется (ветка exists==false).
func TestApply_Creates_Absent_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["touch /x"] = []util.Result{{ExitCode: 0}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{"cmd": "touch", "args": []any{"/x"}, "creates": "/x"})
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if len(r.Calls) != 1 || r.Calls[0] != "touch /x" {
		t.Fatalf("ожидал запуск основной команды, calls=%v", r.Calls)
	}
}

// unless: exit != 0 → условие ложно → основная команда выполняется (ветка
// ExitCode != 0 в unless). Guard-проверка идёт через shell (sh -c), основная
// команда — argv.
func TestApply_Unless_False_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c test -f /etc/passwd"] = []util.Result{{ExitCode: 1}}
	r.Results["useradd bob"] = []util.Result{{ExitCode: 0, Stdout: "added"}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{
		"cmd":    "useradd",
		"args":   []any{"bob"},
		"unless": "test -f /etc/passwd",
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("unless exit!=0 → команда должна выполниться: changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "added" {
		t.Fatalf("stdout=%q", ev.Output.Fields["stdout"].GetStringValue())
	}
}

// unless: ошибка запуска guard-проверки → failed (ветка r.Err в unless-ветке
// shouldSkip + serr в Apply).
func TestApply_Unless_LaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c broken"] = []util.Result{{Err: os.ErrPermission}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{"cmd": "x", "unless": "broken"})
	if !ev.Failed {
		t.Fatal("ошибка запуска unless должна давать failed")
	}
}

// onlyif: exit == 0 → условие истинно → основная команда выполняется (ветка
// ExitCode == 0 в onlyif).
func TestApply_Onlyif_True_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c which make"] = []util.Result{{ExitCode: 0}}
	r.Results["make build"] = []util.Result{{ExitCode: 0, Stdout: "built"}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{
		"cmd":    "make",
		"args":   []any{"build"},
		"onlyif": "which make",
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("onlyif exit 0 → команда должна выполниться: changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "built" {
		t.Fatalf("stdout=%q", ev.Output.Fields["stdout"].GetStringValue())
	}
}

// onlyif: ошибка запуска guard-проверки → failed (ветка r.Err в onlyif).
func TestApply_Onlyif_LaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c broken"] = []util.Result{{Err: os.ErrPermission}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{"cmd": "x", "onlyif": "broken"})
	if !ev.Failed {
		t.Fatal("ошибка запуска onlyif должна давать failed")
	}
}

// Сквозной проход всех трёх guard-ов без skip: creates промах, unless ложно
// (exit!=0), onlyif истинно (exit 0) → команда выполняется. Проверяет порядок
// creates → unless → onlyif и что guard-проверки идут через shell, а основная
// команда — argv.
func TestApply_AllGuards_PassThrough_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c absent"] = []util.Result{{ExitCode: 1}}  // unless ложно
	r.Results["sh -c present"] = []util.Result{{ExitCode: 0}} // onlyif истинно
	r.Results["do work"] = []util.Result{{ExitCode: 0, Stdout: "done"}}
	m := newModule(r, noStat) // creates: нет файла

	ev := apply(t, m, map[string]any{
		"cmd":     "do",
		"args":    []any{"work"},
		"creates": "/tmp/nope",
		"unless":  "absent",
		"onlyif":  "present",
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("сквозной проход guard-ов: changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "done" {
		t.Fatalf("stdout=%q", ev.Output.Fields["stdout"].GetStringValue())
	}
	// порядок: unless-проверка раньше onlyif-проверки, основная команда последней
	want := []string{"sh -c absent", "sh -c present", "do work"}
	if len(r.Calls) != 3 {
		t.Fatalf("ожидал 3 вызова %v, получил %v", want, r.Calls)
	}
	for i := range want {
		if r.Calls[i] != want[i] {
			t.Fatalf("вызов[%d]=%q want %q (полный список %v)", i, r.Calls[i], want[i], r.Calls)
		}
	}
}

// stderr из Result прокидывается в output вместе со stdout/exit_code (поле
// stderr не покрыто базовыми тестами).
func TestApply_OutputCarriesStderr(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["probe"] = []util.Result{{ExitCode: 3, Stdout: "out", Stderr: "err-text"}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{"cmd": "probe"})
	if ev.Failed {
		t.Fatal("non-zero exit не должен давать failed")
	}
	if got := ev.Output.Fields["stderr"].GetStringValue(); got != "err-text" {
		t.Fatalf("stderr=%q", got)
	}
	if got := ev.Output.Fields["exit_code"].GetNumberValue(); got != 3 {
		t.Fatalf("exit_code=%v", got)
	}
}

// Plan — no-op stub: не падает и не шлёт событий (nil-stream допустим, т.к.
// реализация его игнорирует).
func TestPlan_NoOp(t *testing.T) {
	m := exec.New()
	if err := m.Plan(&pluginv1.PlanRequest{}, nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
}

// Production New(): реальный argv-запуск /bin/true без shell. Метасимвол в
// аргументе остаётся литералом — printf печатает его дословно, shell-расширения
// не происходит. Закрывает production-путь Runner + envSlice(nil).
func TestApply_Production_RealArgv_NoShellExpansion(t *testing.T) {
	m := exec.New()
	ev := apply(t, m, map[string]any{
		"cmd":  "/usr/bin/printf",
		"args": []any{"%s", "$HOME|;>"},
	})
	if ev.Failed {
		// fallback: на некоторых системах printf в /bin
		m2 := exec.New()
		ev = apply(t, m2, map[string]any{
			"cmd":  "/bin/echo",
			"args": []any{"$HOME|;>"},
		})
	}
	if ev.Failed {
		t.Fatalf("argv-запуск упал: msg=%q", ev.Message)
	}
	got := ev.Output.Fields["stdout"].GetStringValue()
	// shell не вызывался → $HOME не раскрыто, метасимволы литеральны.
	if got != "$HOME|;>" && got != "$HOME|;>\n" {
		t.Fatalf("ожидал литеральный вывод без shell-расширения, got=%q", got)
	}
	if ev.Output.Fields["exit_code"].GetNumberValue() != 0 {
		t.Fatalf("exit_code=%v", ev.Output.Fields["exit_code"].GetNumberValue())
	}
}

// Production New(): non-zero exit от реального /bin/false НЕ делает шаг failed
// (контракт core.exec — пользователь решает через failed_when). exit_code=1.
func TestApply_Production_RealFalse_NonZeroNotFailed(t *testing.T) {
	m := exec.New()
	ev := apply(t, m, map[string]any{"cmd": "/usr/bin/false"})
	if ev.Failed {
		// fallback на /bin/false
		m2 := exec.New()
		ev = apply(t, m2, map[string]any{"cmd": "/bin/false"})
	}
	if ev.Failed {
		t.Fatalf("non-zero exit не должен давать failed: msg=%q", ev.Message)
	}
	if !ev.Changed {
		t.Fatal("запущенная команда должна быть changed")
	}
	if ev.Output.Fields["exit_code"].GetNumberValue() != 1 {
		t.Fatalf("exit_code=%v want 1", ev.Output.Fields["exit_code"].GetNumberValue())
	}
}

// Production New(): реальный бинарь не найден → Result.Err → failed (production
// runCmd возвращает Err для ENOENT).
func TestApply_Production_BinaryNotFound_Failed(t *testing.T) {
	m := exec.New()
	ev := apply(t, m, map[string]any{"cmd": "/nonexistent/soul-stack/no-such-binary-zzz"})
	if !ev.Failed {
		t.Fatal("несуществующий бинарь должен давать failed")
	}
}

// Production creates: реальный существующий файл → реальный fileExists/osStat
// вернёт true → skip без запуска команды. Закрывает osstat.go (ветка err==nil).
func TestApply_Production_Creates_RealStat_Skips(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out := filepath.Join(dir, "out")
	m := exec.New()
	ev := apply(t, m, map[string]any{
		"cmd":     "/usr/bin/touch",
		"args":    []any{out},
		"creates": marker,
	})
	if ev.Changed {
		t.Fatal("существующий creates-файл должен дать skip")
	}
	if ev.Output.Fields["reason"].GetStringValue() != "creates" {
		t.Fatalf("reason=%q", ev.Output.Fields["reason"].GetStringValue())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("команда выполнилась несмотря на creates-skip")
	}
}

// Production creates: stat реально упал с ошибкой, отличной от ErrNotExist
// (permission denied на родительском каталоге) → osStat возвращает (false,err)
// → shouldSkip отдаёт serr → failed, команда НЕ запускается. Закрывает
// последнюю ветку osstat.go (return false, err). Под root EACCES не возникает —
// тогда тест нерелевантен и пропускается.
func TestApply_Production_Creates_StatPermissionError_Failed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("под root permission denied не воспроизводится")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	target := filepath.Join(locked, "marker")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // чтобы TempDir-cleanup смог удалить

	m := exec.New()
	ev := apply(t, m, map[string]any{
		"cmd":     "/usr/bin/true",
		"creates": target,
	})
	if !ev.Failed {
		t.Fatalf("stat с permission denied должен давать failed, ev=%+v", ev)
	}
}

// Production creates: файла нет → реальный osStat вернёт (false,nil) через
// ветку ErrNotExist → команда выполняется. Закрывает osstat.go (ErrNotExist).
func TestApply_Production_Creates_Absent_Runs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	m := exec.New()
	ev := apply(t, m, map[string]any{
		"cmd":     "/usr/bin/touch",
		"args":    []any{out},
		"creates": filepath.Join(dir, "nope"),
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v msg=%q", ev.Changed, ev.Failed, ev.Message)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("команда не создала файл: %v", err)
	}
}
