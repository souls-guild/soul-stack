package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/cmd"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// apply — прогон Apply на fake-runner; возвращает финальное событие.
func apply(t *testing.T, m *cmd.Module, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "shell",
		Params: mustStruct(t, params),
	}, stream); err != nil {
		t.Fatalf("Apply вернул error (контракт — failed-event, не error): %v", err)
	}
	ev := stream.Last()
	if ev == nil {
		t.Fatal("Apply не отправил ни одного события")
	}
	return ev
}

func TestApply_UnknownState_Failed(t *testing.T) {
	m := newModule(internaltest.NewRunner(), func(string) (bool, error) { return false, nil })
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "run",
		Params: mustStruct(t, map[string]any{"cmd": "true"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("unknown state должен давать failed-event")
	}
	if ev.Message != `unknown state "run"` {
		t.Fatalf("message=%q", ev.Message)
	}
}

func TestApply_MissingCmd_Failed(t *testing.T) {
	m := newModule(internaltest.NewRunner(), func(string) (bool, error) { return false, nil })
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "shell",
		Params: mustStruct(t, map[string]any{}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("отсутствие cmd должно давать failed-event")
	}
}

// Неверный тип одного из опциональных параметров → failed (ветки извлечения
// cwd/env/creates/unless/onlyif). env как строка вместо map.
func TestApply_BadEnvType_Failed(t *testing.T) {
	m := newModule(internaltest.NewRunner(), func(string) (bool, error) { return false, nil })
	ev := apply(t, m, map[string]any{
		"cmd": "true",
		"env": "PATH=/bin",
	})
	if !ev.Failed {
		t.Fatal("env неверного типа должен давать failed")
	}
}

func TestApply_BadCwdType_Failed(t *testing.T) {
	m := newModule(internaltest.NewRunner(), func(string) (bool, error) { return false, nil })
	ev := apply(t, m, map[string]any{
		"cmd": "true",
		"cwd": float64(7),
	})
	if !ev.Failed {
		t.Fatal("cwd неверного типа должен давать failed")
	}
}

// Неверный тип guard-параметров (creates/unless/onlyif — все ожидают string).
// Каждый должен дать failed на этапе извлечения, не доходя до shouldSkip.
func TestApply_BadGuardTypes_Failed(t *testing.T) {
	cases := []struct {
		name  string
		param string
	}{
		{"creates", "creates"},
		{"unless", "unless"},
		{"onlyif", "onlyif"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := internaltest.NewRunner()
			m := newModule(r, func(string) (bool, error) { return false, nil })
			ev := apply(t, m, map[string]any{
				"cmd":   "true",
				c.param: float64(1), // не строка
			})
			if !ev.Failed {
				t.Fatalf("%s неверного типа должен давать failed", c.param)
			}
			if len(r.Calls) != 0 {
				t.Fatalf("ошибка извлечения параметра не должна запускать команд, calls=%v", r.Calls)
			}
		})
	}
}

// env передаётся в процесс sh -c: RunOpts-ключ обогащается отсортированным
// [env=...] префиксом, проверяем что модуль прокинул именно эти KEY=VAL.
func TestApply_EnvPassedToShell(t *testing.T) {
	r := internaltest.NewRunner()
	key := "[env=A=1,B=2] sh -c env"
	r.Results[key] = []util.Result{{ExitCode: 0, Stdout: "A=1\nB=2\n"}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{
		"cmd": "env",
		"env": map[string]any{"B": "2", "A": "1"},
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if len(r.Calls) != 1 || r.Calls[0] != key {
		t.Fatalf("ожидал вызов %q, получил %v", key, r.Calls)
	}
}

// stdout/stderr/exit_code из Result штатно прокидываются в output даже при
// non-zero exit (exit-код не делает шаг failed — это решает changed_when выше).
func TestApply_OutputCarriesStdoutStderrExit(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c probe"] = []util.Result{{ExitCode: 2, Stdout: "out\n", Stderr: "err\n"}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "probe"})
	if ev.Failed {
		t.Fatal("non-zero exit не должен давать failed")
	}
	if got := ev.Output.Fields["stdout"].GetStringValue(); got != "out\n" {
		t.Fatalf("stdout=%q", got)
	}
	if got := ev.Output.Fields["stderr"].GetStringValue(); got != "err\n" {
		t.Fatalf("stderr=%q", got)
	}
	if got := ev.Output.Fields["exit_code"].GetNumberValue(); got != 2 {
		t.Fatalf("exit_code=%v", got)
	}
}

// Ошибка запуска самого процесса (sh не нашёлся / fork failed) — Result.Err != nil.
// Это единственный путь, ведущий к failed из-за runner-а.
func TestApply_RunnerLaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c true"] = []util.Result{{Err: os.ErrPermission}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "true"})
	if !ev.Failed {
		t.Fatal("Result.Err должен давать failed")
	}
	if ev.Message != "sh -c: permission denied" {
		t.Fatalf("message=%q", ev.Message)
	}
}

// creates: файла нет → НЕ skip, команда выполняется.
func TestApply_Creates_NotExists_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c touch /tmp/x"] = []util.Result{{ExitCode: 0}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "touch /tmp/x", "creates": "/tmp/x"})
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
}

// creates: stat вернул ошибку (не ErrNotExist) → failed, команда НЕ запускается.
func TestApply_Creates_StatError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	m := newModule(r, func(string) (bool, error) { return false, os.ErrPermission })

	ev := apply(t, m, map[string]any{"cmd": "touch /tmp/x", "creates": "/tmp/x"})
	if !ev.Failed {
		t.Fatal("ошибка stat в creates должна давать failed")
	}
	if len(r.Calls) != 0 {
		t.Fatalf("при ошибке guard команда не должна запускаться, calls=%v", r.Calls)
	}
}

// skipped-output: при creates+файл существует событие несёт skipped/reason/exit_code.
func TestApply_Creates_SkipOutput(t *testing.T) {
	r := internaltest.NewRunner()
	m := newModule(r, func(p string) (bool, error) { return p == "/tmp/m", nil })

	ev := apply(t, m, map[string]any{"cmd": "echo hi", "creates": "/tmp/m"})
	if ev.Changed {
		t.Fatal("skip не должен быть changed")
	}
	if ev.Output.Fields["skipped"].GetBoolValue() != true {
		t.Fatal("output.skipped != true")
	}
	if ev.Output.Fields["reason"].GetStringValue() != "creates" {
		t.Fatalf("reason=%q", ev.Output.Fields["reason"].GetStringValue())
	}
	if ev.Output.Fields["exit_code"].GetNumberValue() != 0 {
		t.Fatalf("exit_code=%v", ev.Output.Fields["exit_code"].GetNumberValue())
	}
	if len(r.Calls) != 0 {
		t.Fatalf("при skip команда не запускается, calls=%v", r.Calls)
	}
}

// unless: команда-проверка exit 0 → условие истинно → skip.
func TestApply_Unless_True_Skips(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c test -f /tmp/m"] = []util.Result{{ExitCode: 0}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "make-it", "unless": "test -f /tmp/m"})
	if ev.Changed {
		t.Fatal("unless exit 0 → должно скипнуть")
	}
	if ev.Output.Fields["reason"].GetStringValue() != "unless" {
		t.Fatalf("reason=%q", ev.Output.Fields["reason"].GetStringValue())
	}
	// основная команда не должна запускаться (только unless-проверка)
	for _, c := range r.Calls {
		if c == "sh -c make-it" {
			t.Fatalf("основная команда запустилась вопреки unless: %v", r.Calls)
		}
	}
}

// unless: exit != 0 → условие ложно → команда выполняется.
func TestApply_Unless_False_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c test -f /tmp/m"] = []util.Result{{ExitCode: 1}}
	r.Results["sh -c make-it"] = []util.Result{{ExitCode: 0, Stdout: "done"}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "make-it", "unless": "test -f /tmp/m"})
	if ev.Failed || !ev.Changed {
		t.Fatalf("unless exit!=0 → команда должна выполниться: changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "done" {
		t.Fatalf("stdout=%q", ev.Output.Fields["stdout"].GetStringValue())
	}
}

// unless: ошибка запуска проверки → failed.
func TestApply_Unless_LaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c broken"] = []util.Result{{Err: os.ErrPermission}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "make-it", "unless": "broken"})
	if !ev.Failed {
		t.Fatal("ошибка запуска unless должна давать failed")
	}
}

// onlyif: exit != 0 → условие ложно → skip.
func TestApply_Onlyif_False_Skips(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c which nginx"] = []util.Result{{ExitCode: 1}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "systemctl restart nginx", "onlyif": "which nginx"})
	if ev.Changed {
		t.Fatal("onlyif exit!=0 → должно скипнуть")
	}
	if ev.Output.Fields["reason"].GetStringValue() != "onlyif" {
		t.Fatalf("reason=%q", ev.Output.Fields["reason"].GetStringValue())
	}
}

// onlyif: exit 0 → условие истинно → команда выполняется.
func TestApply_Onlyif_True_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c which nginx"] = []util.Result{{ExitCode: 0}}
	r.Results["sh -c systemctl restart nginx"] = []util.Result{{ExitCode: 0}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "systemctl restart nginx", "onlyif": "which nginx"})
	if ev.Failed || !ev.Changed {
		t.Fatalf("onlyif exit 0 → команда должна выполниться: changed=%v failed=%v", ev.Changed, ev.Failed)
	}
}

// onlyif: ошибка запуска проверки → failed.
func TestApply_Onlyif_LaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c broken"] = []util.Result{{Err: os.ErrPermission}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	ev := apply(t, m, map[string]any{"cmd": "make-it", "onlyif": "broken"})
	if !ev.Failed {
		t.Fatal("ошибка запуска onlyif должна давать failed")
	}
}

// Комбинация guard-ов: creates НЕ срабатывает, дальше unless НЕ срабатывает,
// onlyif срабатывает (exit 0) → команда выполняется. Проверяет порядок и
// сквозной проход через все три ветки shouldSkip без skip.
func TestApply_AllGuards_PassThrough_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c check-absent"] = []util.Result{{ExitCode: 1}}  // unless ложно
	r.Results["sh -c check-present"] = []util.Result{{ExitCode: 0}} // onlyif истинно
	r.Results["sh -c do-work"] = []util.Result{{ExitCode: 0, Stdout: "ok"}}
	m := newModule(r, func(string) (bool, error) { return false, nil }) // creates: нет файла

	ev := apply(t, m, map[string]any{
		"cmd":     "do-work",
		"creates": "/tmp/nope",
		"unless":  "check-absent",
		"onlyif":  "check-present",
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("сквозной проход guard-ов: changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "ok" {
		t.Fatalf("stdout=%q", ev.Output.Fields["stdout"].GetStringValue())
	}
}

// Plan — no-op stub, не должен падать и не шлёт событий.
func TestPlan_NoOp(t *testing.T) {
	m := cmd.New()
	if err := m.Plan(&pluginv1.PlanRequest{}, nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
}

// End-to-end через production New(): реальный sh -c исполняет shell-семантику
// (pipe), и реальный fileExists (StatFile) для creates. Закрывает osstat.go.
func TestApply_Production_RealShell_Pipe(t *testing.T) {
	m := cmd.New()
	ev := apply(t, m, map[string]any{
		"cmd": "printf 'a\\nb\\nc\\n' | wc -l",
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v msg=%q", ev.Changed, ev.Failed, ev.Message)
	}
	// wc -l на 3 строках → "3" (с учётом возможных пробелов от wc).
	stdout := ev.Output.Fields["stdout"].GetStringValue()
	if stdout == "" {
		t.Fatal("ожидал непустой stdout от pipe")
	}
	if ev.Output.Fields["exit_code"].GetNumberValue() != 0 {
		t.Fatalf("exit_code=%v", ev.Output.Fields["exit_code"].GetNumberValue())
	}
}

// Production creates: файл реально существует → реальный fileExists вернёт true
// → skip без запуска команды.
func TestApply_Production_Creates_RealStat_Skips(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m := cmd.New()
	ev := apply(t, m, map[string]any{
		"cmd":     "echo should-not-run > " + filepath.Join(dir, "out"),
		"creates": marker,
	})
	if ev.Changed {
		t.Fatal("существующий creates-файл должен дать skip")
	}
	if ev.Output.Fields["reason"].GetStringValue() != "creates" {
		t.Fatalf("reason=%q", ev.Output.Fields["reason"].GetStringValue())
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatal("команда выполнилась несмотря на creates-skip")
	}
}

// Production creates: файла нет → реальный fileExists вернёт false → команда
// выполняется (ветка ErrNotExist в fileExists).
func TestApply_Production_Creates_Absent_Runs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	m := cmd.New()
	ev := apply(t, m, map[string]any{
		"cmd":     "printf done > " + out,
		"creates": filepath.Join(dir, "nope"),
	})
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("команда не записала файл: %v", err)
	}
	if string(b) != "done" {
		t.Fatalf("содержимое=%q", string(b))
	}
}
