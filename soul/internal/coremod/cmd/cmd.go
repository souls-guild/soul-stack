// Package cmd implements the `core.cmd` core module ([ADR-015]).
//
// State:
//   - shell: run a shell string via `sh -c "<cmd>"`. Unlike core.exec (argv),
//     the shell here handles pipes, redirects, globs, and variables.
//
// Same idempotency flags as core.exec: creates / unless / onlyif. Output:
// stdout/stderr/exit_code.
//
// Security: the cmd string goes into `sh -c` unescaped — shell by design,
// TRUSTED-ONLY module. Any interpolation into cmd (CEL-render, register,
// soulprint) is executed by the shell as code, so the cmd string's source
// must be trusted (Destiny/scenario author), never external input. Use
// `core.exec` for argv form without shell semantics. Steps where parts of
// cmd come from CEL-render should use `${ q(...) }` (quoting helper, post-MVP).
package cmd

import (
	"context"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.cmd"

type Module struct {
	Runner   util.Runner
	StatFile func(path string) (bool, error)
}

func New() *Module {
	return &Module{
		Runner:   util.OSRunner{},
		StatFile: fileExists,
	}
}

// Validate delegates known-state + required-param checks to
// shared/coremanifest/cmd.yaml (single source shared with soul-lint). No
// cross-field invariants (creates/unless/onlyif combine freely, as in core.exec).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op (no PlanReadSafe). core.cmd is a verb module: it runs a
// shell string and has no desired host state a pure-read could compare
// against — drift is undefined in the ADR-031 sense. The host applies
// default-deny: dry_run for core.cmd returns FAILED `plan.unsupported`, a
// deliberate refusal, not a false "no drift".
//
// For host-conditional execution use `creates`/`unless`/`onlyif` (in Apply),
// or move the idempotent part into a declarative module.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	if req.State != "shell" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	shellCmd, err := util.StringParam(req.Params, "cmd")
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
		Name: "sh",
		Args: []string{"-c", shellCmd},
		Cwd:  cwd,
		Env:  envSlice(envMap),
	})
	if res.Err != nil {
		return util.SendFailed(stream, fmt.Sprintf("sh -c: %v", res.Err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"exit_code": float64(res.ExitCode),
	})
}

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
