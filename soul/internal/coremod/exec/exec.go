// Package exec реализует core-модуль `core.exec` ([ADR-015]).
//
// Состояния:
//   - run: запустить процесс с argv (без shell).
//
// Idempotency-флаги (заимствовано из Ansible command):
//   - creates: если указанный файл существует — пропуск (changed=false).
//   - unless: запустить вспомогательную команду; если её exit=0 — пропуск.
//   - onlyif: запустить вспомогательную команду; если её exit≠0 — пропуск.
//
// Output: stdout, stderr, exit_code. Non-zero exit основной команды НЕ
// считается failed автоматически — пользователь решает через `failed_when:`
// в scenario, что считать ошибкой (например, grep с exit 1 — норма).
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
	// StatFile подменяется в тестах для проверки `creates`. В проде —
	// fileExists поверх os.Stat.
	StatFile func(path string) (bool, error)
}

func New() *Module {
	return &Module{
		Runner:   util.OSRunner{},
		StatFile: fileExists,
	}
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	// Per-field-проверки (known-state + required cmd) декларированы в
	// shared/coremanifest/exec.yaml — единый источник с soul-lint. Cross-field-
	// инвариантов у core.exec нет (creates/unless/onlyif комбинируются свободно).
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (без PlanReadSafe). core.exec — verb-модуль: выполнение argv-
// команды, у него НЕТ желаемого состояния хоста, которое можно сверить pure-
// read-ом. Drift в смысле ADR-031 здесь не определён. Host применяет default-
// deny: dry_run для core.exec возвращает FAILED `plan.unsupported`, и это
// конструктивный отказ — НЕ ложное «нет дрифта».
//
// Для условного выполнения по факту хоста используйте `creates`/`unless`/
// `onlyif` (в Apply), либо вынесите идемпотентную часть в declarative-модуль
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
		return util.SendFailed(stream, fmt.Sprintf("exec %s: %v", cmd, res.Err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"exit_code": float64(res.ExitCode),
	})
}

// shouldSkip — общая проверка `creates`/`unless`/`onlyif` (порядок: creates →
// unless → onlyif; первая срабатывающая выигрывает). reason — короткая метка
// для output, удобно в логах debug-ить «почему ничего не запустилось».
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
