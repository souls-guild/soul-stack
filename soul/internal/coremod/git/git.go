// Package git implements the `core.git` core module ([ADR-015]).
//
// States:
//   - cloned: path contains a git repo (or gets cloned if missing).
//   - pulled: path contains a git repo + `git pull` (or clone + pull semantics).
//
// Idempotency:
//   - cloned: no-op if path/.git exists.
//   - pulled: always clone-if-missing + pull. Changed=true only if HEAD
//     changed (rev-parse HEAD compared before/after).
//
// MVP: no remote URL change / submodule / lfs / sparse-checkout — too many
// branches for a first version.
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
	// StatDir is a swappable-in-tests os.Stat for path/.git. Returns (true, nil)
	// for a directory, (false, nil) for missing/non-directory.
	StatDir func(path string) (bool, error)
}

func New() *Module {
	return &Module{
		Runner:  util.OSRunner{},
		StatDir: dirExists,
	}
}

// Validate delegates known-state + required-params (repo/path) to
// shared/coremanifest/git.yaml (single source shared with soul-lint). Types
// of the optional branch/depth params are checked by the Apply getters; no
// cross-field invariants.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op (no PlanReadSafe). core.git doesn't declare read-safe Plan
// in the MVP, so the host applies default-deny on dry_run (FAILED
// `plan.unsupported`). Reason: state `pulled`'s drift check ("any upstream
// updates?") needs `git fetch`, a network read against the remote that Apply
// doesn't do before mutating (clone+pull are the mutation itself), so
// pure-read output isn't available without extending the protocol with a
// fetch phase. `cloned` alone could support it (StatDir(path/.git) + headRev),
// but implementing only half the contract would split state behavior — a
// follow-up slice should either do both or split into "core.git.cloned" (with
// PlanReadSafe) and "core.git.pulled" (without).
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
	// `--` separates positional args from options: a repo starting with `-`
	// (e.g. `--upload-pack=<cmd>`) would otherwise parse as a git option —
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

// headRev runs `git rev-parse HEAD` in cwd=path. Returns the sha; best-effort
// (empty string on error, doesn't block the main flow).
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
