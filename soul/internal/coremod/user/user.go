// Package user implements the core-module `core.user` ([ADR-015]).
//
// States:
//   - present: user exists with the given uid/shell/home/groups.
//   - absent:  user removed.
//
// Optional present params:
//   - uid (int):         explicit uid (useradd -u).
//   - shell (string):    login shell (useradd -s).
//   - home (string):     home directory (useradd -d).
//   - groups ([]string): supplementary groups (useradd -G a,b).
//   - system (bool):     system account (useradd -r), for service accounts of
//     stateful services (e.g. redis).
//   - group (string):    primary group (useradd -g). Must already exist —
//     caller creates it via core.group FIRST. Distinct from `groups`
//     (supplementary, -G).
//
// present semantics are present-or-create (MVP): an existing user is not
// reconciled. New params (system/group, same as uid/shell/home/groups) apply
// ONLY on creation; for an already-existing user they're a no-op and never
// trigger usermod/reconcile.
//
// Backend: useradd/usermod/userdel (busybox-compatible subset). On alpine
// that's the shadow package or busybox built-ins — both understand these flags.
package user

import (
	"context"
	"fmt"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.user"

// maxUID — uid_t is a signed 32-bit int on Linux; useradd rejects values out
// of range. The upper bound rejects obviously-bad input before spawning the
// subprocess.
const maxUID = 2147483647

// nameRe mirrors shadow-utils' default NAME_REGEX
// (`^[a-z_][a-z0-9_-]*\$?$`): name starts with a letter/`_`, may end with `$`
// (NIS/Samba machine account), rest is lowercase/digits/`_`/`-`.
// This matches useradd's own convention — not stricter, to avoid rejecting
// legitimate names. Length (≤ 32) is checked separately in validName.
var nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)

type Module struct {
	Runner     util.Runner
	LookupUser func(name string) (*user.User, error)
}

func New() *Module {
	return &Module{
		Runner:     util.OSRunner{},
		LookupUser: user.Lookup,
	}
}

// Validate — known-state + required-param (name) checks are delegated to
// shared/coremanifest/user.yaml (single source shared with soul-lint, no
// duplication). On top of delegation: early type-guards for optional params
// (the manifest DSL can't express them) plus SEMANTIC format/range checks and
// an arg-injection guard. This is input-validation/safety in OUR code: reject
// injections (leading `-` → argument confusion in useradd's argv) and
// obviously-bad input with a clear error, without tightening useradd's actual
// limits. present/absent semantics are unchanged.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)

	// name is read via StringParam in Apply; the format check here gives an
	// early rejection (soul-lint / Validate phase) without waiting for useradd.
	if name, err := util.StringParam(req.Params, "name"); err == nil {
		if verr := validName("name", name); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	if uid, has, err := util.OptIntParam(req.Params, "uid"); err != nil {
		errs = append(errs, err.Error())
	} else if has && (uid < 0 || uid > maxUID) {
		errs = append(errs, fmt.Sprintf("param %q: out of range [0, %d], got %d", "uid", maxUID, uid))
	}

	if shell, err := util.OptStringParam(req.Params, "shell"); err != nil {
		errs = append(errs, err.Error())
	} else if shell != "" {
		if verr := validAbsPath("shell", shell); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	if home, err := util.OptStringParam(req.Params, "home"); err != nil {
		errs = append(errs, err.Error())
	} else if home != "" {
		if verr := validAbsPath("home", home); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	if groups, err := util.OptStringSliceParam(req.Params, "groups"); err != nil {
		errs = append(errs, err.Error())
	} else {
		for _, g := range groups {
			if verr := validName("groups", g); verr != nil {
				errs = append(errs, verr.Error())
			}
		}
	}

	if _, err := util.OptBoolParam(req.Params, "system"); err != nil {
		errs = append(errs, err.Error())
	}

	if group, err := util.OptStringParam(req.Params, "group"); err != nil {
		errs = append(errs, err.Error())
	} else if group != "" {
		if verr := validName("group", group); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// validName checks a login/group name against shadow-utils' NAME_REGEX + length ≤ 32.
// A leading `-` is rejected by the regex (name must start with a letter/`_`),
// which is exactly the argument-injection guard: `-x` can't land in argv as a flag.
func validName(field, name string) error {
	if name == "" {
		return fmt.Errorf("param %q: must not be empty", field)
	}
	if len(name) > 32 {
		return fmt.Errorf("param %q: too long (max 32), got %d chars", field, len(name))
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("param %q: invalid name %q (must match %s)", field, name, nameRe.String())
	}
	return nil
}

// validAbsPath requires an absolute path without a leading `-` (defense-in-depth
// against argument confusion). File existence is NOT checked — useradd doesn't
// require it, and we don't want to restrict operator flexibility.
func validAbsPath(field, path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("param %q: must be an absolute path (start with %q), got %q", field, "/", path)
	}
	return nil
}

// PlanReadSafe declares core.user.Plan as pure-read (ADR-031 Scry): it calls
// LookupUser and does NOT mutate the host (marker for the host, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): reads current user presence (the
// same LookupUser Apply calls first) and sends PlanEvent.changed — "would Apply
// change the user?". Does NOT mutate the host: no useradd, no userdel.
//
// Semantics match Apply 1:1: present-or-create (uid/shell/home/groups/system/
// group on an already-existing user do NOT trigger reconcile in MVP — see the
// Apply doc), so drift for present = "user missing", for absent = "user exists".
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if verr := validName("name", name); verr != nil {
		return util.PlanFailed(verr.Error())
	}
	_, lookupErr := m.LookupUser(name)
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
	// Format-check name once for both states: Apply may be called without a
	// preceding Validate phase, so an injection/bad name must not reach
	// argv useradd/userdel.
	if verr := validName("name", name); verr != nil {
		return util.SendFailed(stream, verr.Error())
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
	uid, hasUID, err := util.OptIntParam(req.Params, "uid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	shell, err := util.OptStringParam(req.Params, "shell")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	home, err := util.OptStringParam(req.Params, "home")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	groups, err := util.OptStringSliceParam(req.Params, "groups")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	system, err := util.OptBoolParam(req.Params, "system")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	primary, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// Semantic checks for optional params live here too, not just in Validate:
	// Apply may be called without a preceding Validate phase, and bad/injected
	// input must not reach argv useradd. name is already checked in Apply.
	if hasUID && (uid < 0 || uid > maxUID) {
		return util.SendFailed(stream, fmt.Sprintf("param %q: out of range [0, %d], got %d", "uid", maxUID, uid))
	}
	if primary != "" {
		if verr := validName("group", primary); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}
	for _, g := range groups {
		if verr := validName("groups", g); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}
	if shell != "" {
		if verr := validAbsPath("shell", shell); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}
	if home != "" {
		if verr := validAbsPath("home", home); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}

	if existing, lookupErr := m.LookupUser(name); lookupErr == nil && existing != nil {
		// Already exists. MVP doesn't reconcile uid/shell/home/groups/system/
		// group — that needs usermod, which changes state in ways that aren't
		// for the faint of heart (e.g. changing uid cascades into file
		// ownership). present-or-create is enough for v1; reconcile is a
		// future slice. New params (system/group) also do NOT trigger
		// reconcile for an existing user — they only apply on creation.
		return util.SendFinal(stream, false, map[string]any{
			"name":    name,
			"exists":  true,
			"created": false,
		})
	}

	args := []string{"-M"}
	if system {
		args = append(args, "-r")
	}
	if hasUID {
		args = append(args, "-u", strconv.FormatInt(uid, 10))
	}
	if primary != "" {
		args = append(args, "-g", primary)
	}
	if shell != "" {
		args = append(args, "-s", shell)
	}
	if home != "" {
		args = append(args, "-d", home)
	}
	if len(groups) > 0 {
		args = append(args, "-G", strings.Join(groups, ","))
	}
	// `--` separates the positional name from options: a name starting with
	// `-` would otherwise be parsed by useradd as a flag (argument injection,
	// defense-in-depth on top of validName). useradd uses getopt_long — `--`
	// is supported (man useradd).
	args = append(args, "--", name)
	if err := m.must(ctx, "useradd", args...); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":    name,
		"exists":  true,
		"created": true,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], name string) error {
	_, lookupErr := m.LookupUser(name)
	if lookupErr != nil {
		return util.SendFinal(stream, false, map[string]any{
			"name":   name,
			"exists": false,
		})
	}
	if err := m.must(ctx, "userdel", "--", name); err != nil {
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
