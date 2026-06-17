// Package git реализует core-модуль `core.git` ([ADR-015]).
//
// Состояния:
//   - cloned: путь содержит git-репо (или будет склонирован, если отсутствует).
//   - pulled: путь содержит git-репо + `git pull` (или клон + сразу pull-семантика).
//
// Idempotency:
//   - cloned: если path/.git существует — no-op.
//   - pulled: всегда делает clone-if-missing + pull. Changed=true только если
//     HEAD изменился (сравнение rev-parse HEAD до и после).
//
// MVP: не реализуем смену remote URL / submodule / lfs / sparse-checkout —
// слишком много развилок для первой версии.
package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.git"

type Module struct {
	Runner util.Runner
	// StatDir — подменяемый в тестах os.Stat для path/.git. Возвращает (true, nil)
	// для каталога, (false, nil) для отсутствующего/не-каталога.
	StatDir func(path string) (bool, error)
}

func New() *Module {
	return &Module{
		Runner:  util.OSRunner{},
		StatDir: dirExists,
	}
}

// Validate — known-state + required-params (repo/path) делегированы в
// shared/coremanifest/git.yaml (единый источник с soul-lint). Типы опциональных
// branch/depth проверяют Apply-getters; cross-field-инвариантов нет.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (без PlanReadSafe). core.git НЕ объявляет read-safe Plan в MVP,
// host применяет default-deny на dry_run (FAILED `plan.unsupported`). Причина:
// для state `pulled` drift «есть ли upstream-обновления?» требует `git fetch` —
// сетевое read-only действие к remote-у, чего Apply ДО мутации НЕ делает (clone
// + pull выполняются как мутация). Pure-read-вывод из существующей Apply-логики
// получить нельзя без расширения протокола fetch-уровня. Для `cloned` локального
// state достаточно (StatDir(path/.git) + headRev), но реализовывать половину
// контракта означало бы рассогласовать поведение state — отдельный slice (Slice
// B) сделает либо обе ветки, либо явный split «core.git.cloned» с PlanReadSafe и
// «core.git.pulled» без.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	repo, err := util.StringParam(req.Params, "repo")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	branch, err := util.OptStringParam(req.Params, "branch")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if branch == "" {
		branch = "main"
	}
	depth, hasDepth, err := util.OptIntParam(req.Params, "depth")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	switch req.State {
	case "cloned":
		return m.applyCloned(ctx, stream, repo, path, branch, depth, hasDepth)
	case "pulled":
		return m.applyPulled(ctx, stream, repo, path, branch, depth, hasDepth)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyCloned(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], repo, path, branch string, depth int64, hasDepth bool) error {
	exists, err := m.StatDir(filepath.Join(path, ".git"))
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if exists {
		rev, _ := m.headRev(ctx, path)
		return util.SendFinal(stream, false, map[string]any{
			"path":   path,
			"cloned": true,
			"head":   rev,
		})
	}
	if err := m.runClone(ctx, repo, path, branch, depth, hasDepth); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	rev, _ := m.headRev(ctx, path)
	return util.SendFinal(stream, true, map[string]any{
		"path":   path,
		"cloned": true,
		"head":   rev,
	})
}

func (m *Module) applyPulled(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], repo, path, branch string, depth int64, hasDepth bool) error {
	exists, err := m.StatDir(filepath.Join(path, ".git"))
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !exists {
		if err := m.runClone(ctx, repo, path, branch, depth, hasDepth); err != nil {
			return util.SendFailed(stream, err.Error())
		}
		rev, _ := m.headRev(ctx, path)
		return util.SendFinal(stream, true, map[string]any{
			"path":   path,
			"cloned": true,
			"head":   rev,
		})
	}
	before, _ := m.headRev(ctx, path)
	if err := m.runPull(ctx, path); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	after, _ := m.headRev(ctx, path)
	return util.SendFinal(stream, before != after, map[string]any{
		"path":   path,
		"cloned": true,
		"head":   after,
	})
}

func (m *Module) runClone(ctx context.Context, repo, path, branch string, depth int64, hasDepth bool) error {
	args := []string{"clone", "--branch", branch}
	if hasDepth && depth > 0 {
		args = append(args, "--depth", strconv.FormatInt(depth, 10))
	}
	// `--` отделяет позиционные аргументы от опций: repo, начинающийся с `-`
	// (например `--upload-pack=<cmd>`), иначе распарсится git как опция —
	// argument injection (security review L1).
	args = append(args, "--", repo, path)
	r := m.Runner.Run(ctx, "git", args...)
	if r.Err != nil {
		return fmt.Errorf("git clone: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("git clone exited %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

func (m *Module) runPull(ctx context.Context, path string) error {
	r := m.Runner.RunOpts(ctx, util.RunOptions{
		Name: "git",
		Args: []string{"pull", "--ff-only"},
		Cwd:  path,
	})
	if r.Err != nil {
		return fmt.Errorf("git pull: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("git pull exited %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// headRev — `git rev-parse HEAD` в cwd=path. Возвращает sha; best-effort
// (если ошибка — пустая строка, не блокируем основной flow).
func (m *Module) headRev(ctx context.Context, path string) (string, error) {
	r := m.Runner.RunOpts(ctx, util.RunOptions{
		Name: "git",
		Args: []string{"rev-parse", "HEAD"},
		Cwd:  path,
	})
	if r.Err != nil || r.ExitCode != 0 {
		return "", fmt.Errorf("rev-parse: %v", r.Err)
	}
	return strings.TrimSpace(r.Stdout), nil
}

func dirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		return info.IsDir(), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
