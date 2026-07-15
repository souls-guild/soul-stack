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

// apply runs Apply (state=run) on the given module and returns the final
// event. Core module contract: domain-logic errors go into the failed
// event; Apply itself only returns a Go error on a stream failure — not
// exercised here.
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

// noStat is a StatFile that never finds a file (creates always misses).
func noStat(string) (bool, error) { return false, nil }

// Unknown state → failed with a clear message (branch req.State != "run").
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

// argv form: metacharacters (|, >, ;, $VAR) are NOT interpreted by a
// shell, they pass through as literal arguments in RunOpts. This is the
// key difference from core.cmd. Verifies the module does NOT wrap the
// call in sh -c, but builds the key directly from cmd+args.
func TestApply_Argv_MetacharsAreLiteral(t *testing.T) {
	r := internaltest.NewRunner()
	// If the module used a shell, the key would be "sh -c ...". The argv
	// form gives key "echo a|b > c ; $HOME" — metacharacters as plain args.
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

// Launch error for the main process (binary not found / permission) →
// Result.Err → failed with message "exec <cmd>: <err>". The only path to
// failed from the runner.
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

// Extracting optional parameters of the wrong type → failed at the parsing
// stage, never reaching the runner. Covers every err branch from the
// OptStringSlice/OptString/OptStringMap extraction in Apply.
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

// creates: stat returned an error (not ErrNotExist, e.g. permission
// denied) → shouldSkip returns serr → failed, the command does NOT run.
// Covers the serr branch in Apply and the err branch of StatFile in
// shouldSkip.
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

// creates: file absent → NOT skipped, the main command runs (exists==false branch).
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

// unless: exit != 0 → condition false → the main command runs (ExitCode
// != 0 branch in unless). The guard check goes through a shell (sh -c),
// the main command is argv.
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

// unless: guard-check launch error → failed (r.Err branch in the unless
// path of shouldSkip + serr in Apply).
func TestApply_Unless_LaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c broken"] = []util.Result{{Err: os.ErrPermission}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{"cmd": "x", "unless": "broken"})
	if !ev.Failed {
		t.Fatal("ошибка запуска unless должна давать failed")
	}
}

// onlyif: exit == 0 → condition true → the main command runs (ExitCode ==
// 0 branch in onlyif).
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

// onlyif: guard-check launch error → failed (r.Err branch in onlyif).
func TestApply_Onlyif_LaunchError_Failed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c broken"] = []util.Result{{Err: os.ErrPermission}}
	m := newModule(r, noStat)

	ev := apply(t, m, map[string]any{"cmd": "x", "onlyif": "broken"})
	if !ev.Failed {
		t.Fatal("ошибка запуска onlyif должна давать failed")
	}
}

// End-to-end pass through all three guards without a skip: creates misses,
// unless is false (exit!=0), onlyif is true (exit 0) → the command runs.
// Verifies the creates → unless → onlyif order and that guard checks go
// through a shell while the main command is argv.
func TestApply_AllGuards_PassThrough_Runs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c absent"] = []util.Result{{ExitCode: 1}}  // unless false
	r.Results["sh -c present"] = []util.Result{{ExitCode: 0}} // onlyif true
	r.Results["do work"] = []util.Result{{ExitCode: 0, Stdout: "done"}}
	m := newModule(r, noStat) // creates: no file

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
	// order: unless check before onlyif check, main command last
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

// stderr from Result is threaded into output alongside stdout/exit_code
// (the stderr field isn't covered by the basic tests).
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

// Plan is a no-op stub: doesn't panic and sends no events (a nil stream is
// fine since the implementation ignores it).
func TestPlan_NoOp(t *testing.T) {
	m := exec.New()
	if err := m.Plan(&pluginv1.PlanRequest{}, nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
}

// Production New(): a real argv launch of /bin/true without a shell. The
// metacharacter in the argument stays literal — printf prints it verbatim,
// no shell expansion happens. Covers the production Runner path +
// envSlice(nil).
func TestApply_Production_RealArgv_NoShellExpansion(t *testing.T) {
	m := exec.New()
	ev := apply(t, m, map[string]any{
		"cmd":  "/usr/bin/printf",
		"args": []any{"%s", "$HOME|;>"},
	})
	if ev.Failed {
		// fallback: on some systems printf lives in /bin
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
	// no shell was invoked → $HOME wasn't expanded, metacharacters stay literal.
	if got != "$HOME|;>" && got != "$HOME|;>\n" {
		t.Fatalf("ожидал литеральный вывод без shell-расширения, got=%q", got)
	}
	if ev.Output.Fields["exit_code"].GetNumberValue() != 0 {
		t.Fatalf("exit_code=%v", ev.Output.Fields["exit_code"].GetNumberValue())
	}
}

// Production New(): non-zero exit from a real /bin/false does NOT fail the
// step (core.exec's contract — the user decides via failed_when). exit_code=1.
func TestApply_Production_RealFalse_NonZeroNotFailed(t *testing.T) {
	m := exec.New()
	ev := apply(t, m, map[string]any{"cmd": "/usr/bin/false"})
	if ev.Failed {
		// fallback to /bin/false
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

// Production New(): a real binary not found → Result.Err → failed
// (production runCmd returns Err for ENOENT).
func TestApply_Production_BinaryNotFound_Failed(t *testing.T) {
	m := exec.New()
	ev := apply(t, m, map[string]any{"cmd": "/nonexistent/soul-stack/no-such-binary-zzz"})
	if !ev.Failed {
		t.Fatal("несуществующий бинарь должен давать failed")
	}
}

// Production creates: a real existing file → the real fileExists/osStat
// returns true → skip without running the command. Covers osstat.go
// (err==nil branch).
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

// Production creates: stat really fails with an error other than
// ErrNotExist (permission denied on the parent dir) → osStat returns
// (false, err) → shouldSkip returns serr → failed, the command does NOT
// run. Covers the last branch of osstat.go (return false, err). EACCES
// doesn't happen under root — the test is then irrelevant and skipped.
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
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // so TempDir cleanup can remove it

	m := exec.New()
	ev := apply(t, m, map[string]any{
		"cmd":     "/usr/bin/true",
		"creates": target,
	})
	if !ev.Failed {
		t.Fatalf("stat с permission denied должен давать failed, ev=%+v", ev)
	}
}

// Production creates: no file → the real osStat returns (false, nil) via
// the ErrNotExist branch → the command runs. Covers osstat.go (ErrNotExist).
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
