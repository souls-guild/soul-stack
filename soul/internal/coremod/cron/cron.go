// Package cron implements the `core.cron` core module ([ADR-015]).
//
// States:
//   - present: the job file `/etc/cron.d/<name>` exists with the given
//     schedule and command.
//   - absent:  file removed.
//
// MVP: only system-level `/etc/cron.d/<name>`, one rule per file.
// User crontab (`crontab -u user -l/-`) is deferred until a real request.
//
// Platform support: Linux distros where the cron daemon reads /etc/cron.d/.
// FreeBSD has no such directory — the module shouldn't be applied there
// (controlled by a `where:` predicate in the scenario, not by the module).
package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.cron"

// CronDir — directory for system cron jobs (architecturally fixed; on
// minimal containers it may not exist, in which case `present` creates it).
const CronDir = "/etc/cron.d"

type Module struct {
	// Dir is substituted in unit tests with t.TempDir(); production uses CronDir.
	Dir string
}

func New() *Module { return &Module{Dir: CronDir} }

// Validate — known-state + per-state required params (name; present →
// schedule/command) are delegated to shared/coremanifest/cron.yaml (single
// source shared with soul-lint). Job name validity ([A-Za-z0-9_-]) is an
// imperative check in Apply (validCronName); the manifest DSL can't express it.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares that core.cron.Plan is pure-read (ADR-031 Scry):
// it reads the existing job file and does NOT mutate the filesystem (marker
// for the host, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads the current contents of
// the job file `<Dir>/<name>` (the same os.ReadFile as the start of Apply)
// and sends PlanEvent.changed — "would Apply change the file?". Does NOT
// mutate the filesystem: no MkdirAll, no WriteFile, no Remove.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if !validCronName(name) {
		return util.PlanFailed(fmt.Sprintf("param %q: invalid cron job name %q (allowed [A-Za-z0-9_-])", "name", name))
	}
	path := filepath.Join(m.Dir, name)
	switch req.State {
	case "present":
		return m.planPresent(stream, req, path)
	case "absent":
		return m.planAbsent(stream, path)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planPresent — pure-read drift check for state present: the same comparison
// read as applyPresent, without writing. drift = file missing OR content differs.
func (m *Module) planPresent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	schedule, err := util.StringParam(req.Params, "schedule")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	command, err := util.StringParam(req.Params, "command")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	cronUser, err := util.OptStringParam(req.Params, "user")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if cronUser == "" {
		cronUser = "root"
	}
	want := fmt.Sprintf("%s %s %s\n", schedule, cronUser, command)
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		return util.SendPlanFinal(stream, string(existing) != want)
	case errors.Is(rerr, fs.ErrNotExist):
		return util.SendPlanFinal(stream, true)
	default:
		return util.PlanFailed(fmt.Sprintf("read %s: %v", path, rerr))
	}
}

// planAbsent — pure-read drift check for state absent: drift = file exists
// (Apply would remove it).
func (m *Module) planAbsent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], path string) error {
	_, statErr := os.Stat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return util.SendPlanFinal(stream, false)
	}
	if statErr != nil {
		return util.PlanFailed(fmt.Sprintf("stat %s: %v", path, statErr))
	}
	return util.SendPlanFinal(stream, true)
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !validCronName(name) {
		return util.SendFailed(stream, fmt.Sprintf("param %q: invalid cron job name %q (allowed [A-Za-z0-9_-])", "name", name))
	}
	path := filepath.Join(m.Dir, name)
	switch req.State {
	case "present":
		return m.applyPresent(stream, req, name, path)
	case "absent":
		return m.applyAbsent(stream, name, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, name, path string) error {
	schedule, err := util.StringParam(req.Params, "schedule")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	command, err := util.StringParam(req.Params, "command")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	cronUser, err := util.OptStringParam(req.Params, "user")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if cronUser == "" {
		cronUser = "root"
	}

	content := fmt.Sprintf("%s %s %s\n", schedule, cronUser, command)
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if string(existing) == content {
			return util.SendFinal(stream, false, map[string]any{
				"name":      name,
				"path":      path,
				"installed": true,
			})
		}
	case errors.Is(rerr, fs.ErrNotExist):
		// will create below
	default:
		return util.SendFailed(stream, fmt.Sprintf("read %s: %v", path, rerr))
	}

	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("mkdir %s: %v", m.Dir, err))
	}
	// 0644: cron strictly requires /etc/cron.d/<file> to be owned by root and
	// not group/world-writable. WriteFile doesn't change the owner; on test
	// TempDirs the owner is already the process uid, which is fine —
	// production Soul runs as root.
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("write %s: %v", path, err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":      name,
		"path":      path,
		"installed": true,
	})
}

func (m *Module) applyAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], name, path string) error {
	_, statErr := os.Stat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return util.SendFinal(stream, false, map[string]any{
			"name":      name,
			"path":      path,
			"installed": false,
		})
	}
	if statErr != nil {
		return util.SendFailed(stream, fmt.Sprintf("stat %s: %v", path, statErr))
	}
	if err := os.Remove(path); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("remove %s: %v", path, err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":      name,
		"path":      path,
		"installed": false,
	})
}

// validCronName — the cron daemon ignores files with dots/special chars in
// the name (up to skipping the whole directory on debian-derivatives). We
// restrict names strictly to [A-Za-z0-9_-] — this rules out path-injection
// and is compatible with run-parts.
func validCronName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
