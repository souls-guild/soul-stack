// Package exec implements the core module `core.exec` ([ADR-015]).
//
// States:
//   - run: launch a process with argv (no shell).
//
// Idempotency flags:
//   - creates: skip if the given file exists (changed=false).
//   - unless: run a helper command; skip if its exit=0.
//   - onlyif: run a helper command; skip if its exit≠0.
//
// Output: stdout, stderr, exit_code — carried on success AND on a failure by
// exit code, so the stderr that explains the failure never goes missing.
//
// Which codes count as success is the `exit_codes` param, and by default only
// 0 does: a command that ran and exited non-zero fails the task (NIM-687).
// Codes that are an answer rather than an error — grep exiting 1, `diff`
// exiting 1 — are declared, either by widening the set (`exit_codes: [0, 1]`,
// `exit_codes: [0, "2-5"]`) or by overriding the verdict from the scenario with
// `failed_when:`, which is applied after Apply and has the last word.
package exec

import (
	"context"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.exec"

type Module struct {
	Runner util.Runner
	// StatFile is swapped out in tests to check `creates`. In production it's
	// fileExists over os.Stat.
	StatFile func(path string) (bool, error)
}

func New() *Module {
	return &Module{
		Runner:   util.OSRunner{},
		StatFile: fileExists,
	}
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	// Per-field checks (known-state + required cmd) are declared in
	// shared/coremanifest/exec.yaml — a single source shared with soul-lint.
	// core.exec has no cross-field invariants (creates/unless/onlyif combine freely).
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op (no PlanReadSafe). core.exec is a verb module: running an
// argv command has NO desired host state to check with a pure read. Drift in
// the ADR-031 sense isn't defined here. The host applies default-deny:
// dry_run for core.exec returns FAILED `plan.unsupported`, and that's a
// deliberate rejection — NOT a false "no drift".
//
// For conditional execution based on host facts, use `creates`/`unless`/
// `onlyif` (in Apply), or move the idempotent part into a declarative module
// (core.file/core.service/...).
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	if req.State != "run" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	cmd, err := util.StringParam(req.Params, "cmd")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	args, err := util.OptStringSliceParam(req.Params, "args")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	cwd, err := util.OptStringParam(req.Params, "cwd")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	envMap, err := util.OptStringMapParam(req.Params, "env")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	creates, err := util.OptStringParam(req.Params, "creates")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	unless, err := util.OptStringParam(req.Params, "unless")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	onlyif, err := util.OptStringParam(req.Params, "onlyif")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Read before the guards: a malformed exit_codes is an author error, and it
	// should surface whether or not this particular host happens to skip.
	exitCodes, err := util.OptExitCodesParam(req.Params, "exit_codes")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	skip, reason, serr := m.shouldSkip(ctx, creates, unless, onlyif)
	if serr != nil {
		return util.SendFailed(stream, serr.Error())
	}
	if skip {
		return util.SendFinal(stream, false, map[string]any{
			"skipped":   true,
			"reason":    reason,
			"exit_code": float64(0),
		})
	}

	res := m.Runner.RunOpts(ctx, util.RunOptions{
		Name: cmd,
		Args: args,
		Cwd:  cwd,
		Env:  envSlice(envMap),
	})
	if res.Err != nil {
		// A different class from a bad exit code: nothing ran, so there is no
		// output to report and no code to judge.
		return util.SendFailed(stream, fmt.Sprintf("exec %s: %v", cmd, res.Err))
	}
	output := map[string]any{
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"exit_code": float64(res.ExitCode),
	}
	if !exitCodes.Allows(res.ExitCode) {
		return util.SendFailedWithOutput(stream, fmt.Sprintf(
			"exec %s: exit code %d is not accepted (exit_codes: %s)", cmd, res.ExitCode, exitCodes), output)
	}
	return util.SendFinal(stream, true, output)
}

// shouldSkip is the combined `creates`/`unless`/`onlyif` check (order: creates
// → unless → onlyif; the first match wins). reason is a short label for
// output, handy for debug-logging "why nothing ran".
func (m *Module) shouldSkip(ctx context.Context, creates, unless, onlyif string) (bool, string, error) {
	if creates != "" {
		exists, err := m.StatFile(creates)
		if err != nil {
			return false, "", fmt.Errorf("stat %s: %v", creates, err)
		}
		if exists {
			return true, "creates", nil
		}
	}
	if unless != "" {
		r := m.Runner.Run(ctx, "sh", "-c", unless)
		if r.Err != nil {
			return false, "", fmt.Errorf("unless: %v", r.Err)
		}
		if r.ExitCode == 0 {
			return true, "unless", nil
		}
	}
	if onlyif != "" {
		r := m.Runner.Run(ctx, "sh", "-c", onlyif)
		if r.Err != nil {
			return false, "", fmt.Errorf("onlyif: %v", r.Err)
		}
		if r.ExitCode != 0 {
			return true, "onlyif", nil
		}
	}
	return false, "", nil
}

func envSlice(m map[string]string) []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func fileExists(path string) (bool, error) {
	return osStat(path)
}
