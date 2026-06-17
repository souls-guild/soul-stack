// Package cmd реализует core-модуль `core.cmd` ([ADR-015]).
//
// Состояние:
//   - shell: запустить shell-строку через `sh -c "<cmd>"`. В отличие от core.exec
//     (argv), здесь shell обрабатывает pipes, redirects, glob, переменные.
//
// Idempotency-флаги те же: creates / unless / onlyif. Output: stdout/stderr/exit_code.
//
// Безопасность: cmd-строка идёт в `sh -c` без escape — это shell by design,
// модуль TRUSTED-ONLY. Любая интерполяция в cmd (CEL-render, register, soulprint)
// исполняется shell-ом как код, поэтому источник cmd-строки должен быть доверенным
// (автор Destiny/scenario), а не внешним вводом. Для argv-формы без shell-семантики
// — `core.exec`. Шаги, где части cmd приходят из CEL-render, должны использовать
// `${ q(...) }` (helper-квотинг, post-MVP).
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

// Validate — known-state + required-param проверки делегированы в
// shared/coremanifest/cmd.yaml (единый источник с soul-lint). Cross-field-
// инвариантов нет (creates/unless/onlyif комбинируются свободно, как в core.exec).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (без PlanReadSafe). core.cmd — verb-модуль: запуск shell-строки,
// у него НЕТ желаемого состояния хоста, сверяемого pure-read-ом. Drift в
// смысле ADR-031 не определён. Host применяет default-deny: dry_run для
// core.cmd возвращает FAILED `plan.unsupported`, и это конструктивный отказ —
// НЕ ложное «нет дрифта».
//
// Для условного выполнения по факту хоста используйте `creates`/`unless`/
// `onlyif` (в Apply), либо вынесите идемпотентную часть в declarative-модуль.
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
