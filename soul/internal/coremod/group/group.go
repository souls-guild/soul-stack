// Package group implements the `core.group` core module ([ADR-015]).
//
// States:
//   - present: the group exists with the given gid.
//   - absent:  the group is removed.
//
// Optional present params:
//   - gid (int):     explicit gid (groupadd -g).
//   - system (bool): system group (groupadd -r), gid from the system range.
//     Compatible with gid (both can be set). Needed for service accounts of
//     stateful services (e.g. redis's primary group).
//
// Backend: groupadd/groupdel.
package group

import (
	"context"
	"fmt"
	"os/user"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.group"

type Module struct {
	Runner      util.Runner
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		Runner:      util.OSRunner{},
		LookupGroup: user.LookupGroup,
	}
}

// Validate delegates known-state + required-param (name) checks to
// shared/coremanifest/group.yaml (single source shared with soul-lint, no
// duplication). Type-checking of optional gid/system stays on top of the
// delegation: it's an early type guard the manifest DSL can't express — the
// contract "non-bool system / non-int gid rejected at Validate" (see group_test.go).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	if _, _, err := util.OptIntParam(req.Params, "gid"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptBoolParam(req.Params, "system"); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares core.group.Plan as pure-read (ADR-031 Scry): it reads
// LookupGroup and does NOT mutate the host (marker for the host's default-deny).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): checks current group presence
// (the same LookupGroup Apply starts with) and sends PlanEvent.changed —
// "would Apply change the group?". Does NOT mutate the host: no groupadd or
// groupdel.
//
// Semantics match Apply 1:1: present-or-create (gid/system on an already
// existing group do NOT trigger reconcile in MVP — see Apply's doc), so
// drift for present = "group missing", for absent = "group exists".
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	_, lookupErr := m.LookupGroup(name)
	exists := lookupErr == nil
	switch req.State {
	case "present":
		return util.SendPlanFinal(stream, !exists)
	case "absent":
		return util.SendPlanFinal(stream, exists)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	switch req.State {
	case "present":
		return m.applyPresent(ctx, stream, req, name)
	case "absent":
		return m.applyAbsent(ctx, stream, name)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, name string) error {
	gid, hasGID, err := util.OptIntParam(req.Params, "gid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	system, err := util.OptBoolParam(req.Params, "system")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if _, lookupErr := m.LookupGroup(name); lookupErr == nil {
		return util.SendFinal(stream, false, map[string]any{
			"name":    name,
			"exists":  true,
			"created": false,
		})
	}
	args := []string{}
	if system {
		args = append(args, "-r")
	}
	if hasGID {
		args = append(args, "-g", strconv.FormatInt(gid, 10))
	}
	args = append(args, name)
	if err := m.must(ctx, "groupadd", args...); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":    name,
		"exists":  true,
		"created": true,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], name string) error {
	if _, lookupErr := m.LookupGroup(name); lookupErr != nil {
		return util.SendFinal(stream, false, map[string]any{
			"name":   name,
			"exists": false,
		})
	}
	if err := m.must(ctx, "groupdel", name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":   name,
		"exists": false,
	})
}

func (m *Module) must(ctx context.Context, name string, args ...string) error {
	r := m.Runner.Run(ctx, name, args...)
	if r.Err != nil {
		return fmt.Errorf("%s: %v", name, r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", name, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}
