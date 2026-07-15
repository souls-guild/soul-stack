// Package util — internal helpers for Soul-side core-modules (ADR-015).
// Implements a mockable Runner (exec wrapper), OS-facts inspection,
// extraction of typed parameters from ApplyRequest.Params, and assembly
// of ApplyEvent with an output structure.
package util

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Runner — mockable wrapper over os/exec. Each core-module takes a Runner in
// its struct, so unit tests can swap in a detection/exec fake without needing
// a real apt/systemctl on the system.
//
// Run must return a non-zero exit via ExitCode (populated), not wrapped in
// *exec.ExitError; Err is set only when the process failed to start ("didn't
// launch": binary not found, permission denied). This lets modules
// distinguish "the command said no" (an idempotency check like `dpkg -l` for
// not-installed) from "there's no dpkg binary at all".
//
// RunOpts — extended variant with cwd/env/stdin, needed for core.exec /
// core.cmd (where the caller explicitly sets the process's environment and
// working directory).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) Result
	RunOpts(ctx context.Context, opts RunOptions) Result
}

// RunOptions — extended run parameters for core.exec / core.cmd.
// Cwd "" = inherit (the Soul agent's current directory). Env == nil = inherit;
// Env != nil = full replace (a complete KEY=VAL list, like os/exec.Cmd.Env).
// Stdin "" = empty stdin; unused by command/shell modules in MVP, but the
// field exists so future extensions don't break the interface.
type RunOptions struct {
	Name  string
	Args  []string
	Cwd   string
	Env   []string
	Stdin string
}

// Result — outcome of a single command run. Stderr/Stdout are strings (not
// []byte): core-modules parse them with regexes and compare as strings,
// binary output isn't expected here.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Err is non-nil only if the process failed to start
	// (binary not found, fork failed). A non-zero exit is ExitCode, not Err.
	Err error
}

// OK — true if the process started without error and exited 0.
func (r Result) OK() bool { return r.Err == nil && r.ExitCode == 0 }

// OSRunner — production Runner implementation over os/exec. Stdout/stderr
// are captured into a bytes.Buffer (memory bounding is the core-modules'
// concern in their wrapper; for apt/systemctl calls this is tens of KB).
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
