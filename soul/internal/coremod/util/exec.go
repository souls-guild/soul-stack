// Package util — внутренние helper-ы Soul-side core-модулей (ADR-015).
// Реализует mock-абельный Runner (exec обёртка), осмотр OS-фактов,
// извлечение типизированных параметров из ApplyRequest.Params и сборку
// ApplyEvent с output-структурой.
package util

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Runner — мок-абельная обёртка над os/exec. Каждый core-модуль принимает
// Runner в свой struct, благодаря чему unit-тесты подменяют его на
// detection/exec-fake без необходимости иметь реальную систему apt/systemctl.
//
// Run обязан возвращать ExitErr (с populated ExitCode) для non-zero exit
// штатно, без обёртки в *exec.ExitError; ошибка возвращается только при
// «не запустился» (binary not found, permission denied). Это нужно, чтобы
// модули могли отличать «команда сказала нет» (idempotent-проверка типа
// `dpkg -l` для not-installed) от «нет dpkg-binary вовсе».
//
// RunOpts — расширенный вариант с cwd/env/stdin, нужен для core.exec / core.cmd
// (где пользователь явно задаёт окружение и рабочий каталог процесса).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) Result
	RunOpts(ctx context.Context, opts RunOptions) Result
}

// RunOptions — расширенные параметры запуска для core.exec / core.cmd.
// Cwd "" = inherit (текущий каталог Soul-агента). Env == nil = inherit;
// Env != nil = full replace (полный список KEY=VAL, как os/exec.Cmd.Env).
// Stdin "" = пустой stdin; для command/shell-модулей не используется в MVP,
// но поле есть, чтобы будущие надстройки не ломали интерфейс.
type RunOptions struct {
	Name  string
	Args  []string
	Cwd   string
	Env   []string
	Stdin string
}

// Result — итог одного запуска команды. Stderr/Stdout — строки (не []byte):
// core-модули парсят их регулярками и сравнивают как строки, бинарный вывод
// здесь не ожидается.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Err непустой только если процесс не удалось запустить
	// (binary not found, fork failed). Non-zero exit — это ExitCode, не Err.
	Err error
}

// OK — true, если процесс отработал без ошибки запуска и exit-кодом 0.
func (r Result) OK() bool { return r.Err == nil && r.ExitCode == 0 }

// OSRunner — production-реализация Runner поверх os/exec. Stdout/stderr
// захватываются в bytes.Buffer (ограничение по памяти — задача core-модулей
// в их обёртке, для apt/systemctl-вызовов это десятки KB).
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args ...string) Result {
	return runCmd(exec.CommandContext(ctx, name, args...), "")
}

func (OSRunner) RunOpts(ctx context.Context, opts RunOptions) Result {
	cmd := exec.CommandContext(ctx, opts.Name, opts.Args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	return runCmd(cmd, opts.Stdin)
}

func runCmd(cmd *exec.Cmd, stdin string) Result {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = bytes.NewBufferString(stdin)
	}
	err := cmd.Run()
	r := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.ExitCode = ee.ExitCode()
			return r
		}
		r.Err = err
		return r
	}
	return r
}
