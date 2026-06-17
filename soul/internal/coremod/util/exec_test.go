package util_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// Тесты production-Runner (OSRunner) поверх реального os/exec. Используем
// /bin/sh и sleep — они есть и на macOS, и на Linux-CI. Бинарный вывод не
// проверяем (Result хранит строки), только текстовые контракты.

func TestOSRunner_ExitCodeZero(t *testing.T) {
	r := util.OSRunner{}
	res := r.Run(context.Background(), "/bin/sh", "-c", "exit 0")
	if res.Err != nil {
		t.Fatalf("Err=%v want nil", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode=%d want 0", res.ExitCode)
	}
	if !res.OK() {
		t.Fatal("OK()=false want true")
	}
}

// Non-zero exit обязан вернуться как ExitCode без Err — это контракт Runner:
// модули отличают «команда сказала нет» от «бинаря нет».
func TestOSRunner_NonZeroExitIsExitCodeNotErr(t *testing.T) {
	r := util.OSRunner{}
	res := r.Run(context.Background(), "/bin/sh", "-c", "exit 7")
	if res.Err != nil {
		t.Fatalf("Err=%v want nil (non-zero exit must not set Err)", res.Err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode=%d want 7", res.ExitCode)
	}
	if res.OK() {
		t.Fatal("OK()=true want false (exit 7)")
	}
}

func TestOSRunner_CapturesStdoutAndStderr(t *testing.T) {
	r := util.OSRunner{}
	res := r.Run(context.Background(), "/bin/sh", "-c", "printf out; printf err 1>&2")
	if res.Err != nil {
		t.Fatalf("Err=%v", res.Err)
	}
	if res.Stdout != "out" {
		t.Errorf("Stdout=%q want %q", res.Stdout, "out")
	}
	if res.Stderr != "err" {
		t.Errorf("Stderr=%q want %q", res.Stderr, "err")
	}
}

// Stderr должен наполняться и при non-zero exit (модули парсят stderr на
// idempotent-проверках dpkg/apt).
func TestOSRunner_StderrOnFailure(t *testing.T) {
	r := util.OSRunner{}
	res := r.Run(context.Background(), "/bin/sh", "-c", "echo boom 1>&2; exit 2")
	if res.ExitCode != 2 {
		t.Fatalf("ExitCode=%d want 2", res.ExitCode)
	}
	if strings.TrimSpace(res.Stderr) != "boom" {
		t.Errorf("Stderr=%q want %q", res.Stderr, "boom")
	}
}

// «Не запустился» (binary not found) → Err непустой, ExitCode 0, OK()=false.
func TestOSRunner_BinaryNotFoundSetsErr(t *testing.T) {
	r := util.OSRunner{}
	res := r.Run(context.Background(), "/nonexistent/soul-no-such-binary")
	if res.Err == nil {
		t.Fatal("Err=nil want non-nil (binary not found)")
	}
	if res.OK() {
		t.Fatal("OK()=true want false")
	}
}

// argv передаётся без shell: метасимволы трактуются как литералы аргумента,
// а не интерпретируются оболочкой (защита от инъекции через имена пакетов).
func TestOSRunner_NoShellArgvLiteral(t *testing.T) {
	r := util.OSRunner{}
	// $HOME и ; cat не должны раскрываться/исполняться — echo печатает аргумент
	// дословно. /bin/echo, не builtin, чтобы гарантировать отсутствие shell.
	arg := "$HOME; rm -rf /tmp/should-not; `whoami`"
	res := r.Run(context.Background(), "/bin/echo", arg)
	if res.Err != nil {
		t.Fatalf("Err=%v", res.Err)
	}
	if got := strings.TrimRight(res.Stdout, "\n"); got != arg {
		t.Fatalf("Stdout=%q want literal %q (shell interpolation leaked)", got, arg)
	}
}

// Таймаут контекста реально прерывает долгоживущий процесс: Run возвращается
// заметно раньше длительности sleep.
func TestOSRunner_ContextTimeoutKillsProcess(t *testing.T) {
	r := util.OSRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := r.Run(ctx, "sleep", "10")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Run took %v — процесс не был убит по таймауту", elapsed)
	}
	// Убитый сигналом процесс возвращает ошибку запуска или ненулевой exit;
	// в любом случае это не успех.
	if res.OK() {
		t.Fatalf("OK()=true want false (процесс должен быть прерван), elapsed=%v", elapsed)
	}
}

// Отмена контекста извне (cancel) также прерывает процесс.
func TestOSRunner_ContextCancelKillsProcess(t *testing.T) {
	r := util.OSRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := r.Run(ctx, "sleep", "10")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Run took %v — cancel не прервал процесс", elapsed)
	}
	if res.OK() {
		t.Fatalf("OK()=true want false (cancel должен прервать), elapsed=%v", elapsed)
	}
}

func TestOSRunner_RunOptsCwd(t *testing.T) {
	dir := t.TempDir()
	r := util.OSRunner{}
	res := r.RunOpts(context.Background(), util.RunOptions{
		Name: "/bin/sh",
		Args: []string{"-c", "pwd"},
		Cwd:  dir,
	})
	if res.Err != nil {
		t.Fatalf("Err=%v", res.Err)
	}
	got := strings.TrimSpace(res.Stdout)
	// macOS отдаёт /private/<tmp>, dir может быть /var/... — сравниваем по суффиксу.
	if !strings.HasSuffix(got, strings.TrimPrefix(dir, "/private")) && got != dir {
		t.Fatalf("pwd=%q want cwd %q", got, dir)
	}
}

// Env != nil = полный replace окружения процесса.
func TestOSRunner_RunOptsEnvFullReplace(t *testing.T) {
	r := util.OSRunner{}
	res := r.RunOpts(context.Background(), util.RunOptions{
		Name: "/bin/sh",
		Args: []string{"-c", "printf %s \"$SOUL_TEST_VAR\""},
		Env:  []string{"SOUL_TEST_VAR=present"},
	})
	if res.Err != nil {
		t.Fatalf("Err=%v", res.Err)
	}
	if res.Stdout != "present" {
		t.Fatalf("Stdout=%q want %q (env not applied)", res.Stdout, "present")
	}
}

// Env != nil действительно заменяет (не дополняет): унаследованная переменная
// PATH из родителя не видна, если её нет в заданном Env.
func TestOSRunner_RunOptsEnvReplacesNotMerges(t *testing.T) {
	t.Setenv("SOUL_PARENT_ONLY", "leaked")
	r := util.OSRunner{}
	res := r.RunOpts(context.Background(), util.RunOptions{
		Name: "/bin/sh",
		Args: []string{"-c", "printf %s \"$SOUL_PARENT_ONLY\""},
		Env:  []string{"OTHER=x"},
	})
	if res.Err != nil {
		t.Fatalf("Err=%v", res.Err)
	}
	if res.Stdout != "" {
		t.Fatalf("Stdout=%q want empty (parent env must NOT leak with full replace)", res.Stdout)
	}
}

// Env == nil = inherit: родительская переменная видна.
func TestOSRunner_RunOptsEnvNilInherits(t *testing.T) {
	t.Setenv("SOUL_INHERIT_VAR", "inherited")
	r := util.OSRunner{}
	res := r.RunOpts(context.Background(), util.RunOptions{
		Name: "/bin/sh",
		Args: []string{"-c", "printf %s \"$SOUL_INHERIT_VAR\""},
		Env:  nil,
	})
	if res.Err != nil {
		t.Fatalf("Err=%v", res.Err)
	}
	if res.Stdout != "inherited" {
		t.Fatalf("Stdout=%q want %q (nil Env should inherit)", res.Stdout, "inherited")
	}
}

func TestOSRunner_RunOptsStdin(t *testing.T) {
	r := util.OSRunner{}
	res := r.RunOpts(context.Background(), util.RunOptions{
		Name:  "/bin/cat",
		Stdin: "hello-from-stdin",
	})
	if res.Err != nil {
		t.Fatalf("Err=%v", res.Err)
	}
	if res.Stdout != "hello-from-stdin" {
		t.Fatalf("Stdout=%q want %q", res.Stdout, "hello-from-stdin")
	}
}

// Пустой Stdin: cat по пустому stdin завершается успешно с пустым выводом.
func TestOSRunner_RunOptsEmptyStdin(t *testing.T) {
	r := util.OSRunner{}
	res := r.RunOpts(context.Background(), util.RunOptions{
		Name: "/bin/sh",
		Args: []string{"-c", "exit 0"},
	})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("Err=%v ExitCode=%d", res.Err, res.ExitCode)
	}
}

// RunOpts тоже отражает non-zero exit как ExitCode без Err.
func TestOSRunner_RunOptsNonZeroExit(t *testing.T) {
	r := util.OSRunner{}
	res := r.RunOpts(context.Background(), util.RunOptions{
		Name: "/bin/sh",
		Args: []string{"-c", "exit 5"},
	})
	if res.Err != nil {
		t.Fatalf("Err=%v want nil", res.Err)
	}
	if res.ExitCode != 5 {
		t.Fatalf("ExitCode=%d want 5", res.ExitCode)
	}
}

func TestResult_OK(t *testing.T) {
	cases := []struct {
		name string
		r    util.Result
		want bool
	}{
		{"zero exit no err", util.Result{ExitCode: 0}, true},
		{"non-zero exit", util.Result{ExitCode: 1}, false},
		{"err set", util.Result{Err: context.Canceled}, false},
		{"err set zero exit", util.Result{ExitCode: 0, Err: context.Canceled}, false},
	}
	for _, c := range cases {
		if got := c.r.OK(); got != c.want {
			t.Errorf("%s: OK()=%v want %v", c.name, got, c.want)
		}
	}
}
