// Package line implements the core module `core.line` ([ADR-015]) —
// in-place line editing of an existing file. The first core module that
// does NOT rewrite the whole file (like core.file) but edits individual
// lines.
//
// MVP states (a deliberately narrowed safe subset — the exact reason
// lineinfile got deferred elsewhere: "regex matches not what you think"):
//
//   - present: line `line` is present in the file. With `regexp`, the first
//     matching line is replaced by `line` (>1 match → only the first is
//     changed + warning); without `regexp`, the exact line is appended if
//     missing.
//   - absent: with `regexp`, ALL matching lines are removed; without
//     `regexp`, all exact matches of `line` are removed.
//
// Deliberate MVP limitations (extensible later without a breaking change):
//   - backrefs (substituting regexp groups into `line`) are NOT supported.
//   - insertafter / insertbefore accept only a literal string or EOF / BOF
//     respectively, NOT regexp (predictable insertion position).
//
// Writes are atomic (util.AtomicWrite: temp+rename), not in-place truncate.
// Idempotent: a repeat run → changed=false.
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
package line

import (
	"context"
	"fmt"
	"os/user"
	"regexp"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the module's canonical address.
const Name = "core.line"

// Module implements sdk/module.SoulModule for core.line.
//
// LookupUser / LookupGroup are fields for testability (substitution without
// touching /etc/passwd) — symmetric with core.file; used only when
// create=true with owner/group.
type Module struct {
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
	}
}

// Validate is NOT fully delegated to util.ValidateAgainstManifest (unlike
// core.exec): beyond known-state + required(path), core.line has cross-field
// invariants the manifest DSL can't express — `line` is required for present
// (not for absent), absent requires line OR regexp, insertafter/insertbefore
// are mutually exclusive, regexp must compile. known-state/required(path)
// intentionally duplicate line.yaml — no single source is possible without
// cross-field checks in the DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case "present", "absent":
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want present|absent)", req.State))
	}
	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}

	rx, rerr := util.OptStringParam(req.Params, "regexp")
	if rerr != nil {
		errs = append(errs, rerr.Error())
	} else if rx != "" {
		if _, cerr := regexp.Compile(rx); cerr != nil {
			errs = append(errs, fmt.Sprintf("param %q: invalid regexp: %v", "regexp", cerr))
		}
	}

	// present requires line — the exact value being managed (appended when
	// regexp is absent / replaced onto when regexp is present). absent
	// without regexp uses line as the exact-match criterion for removal, so
	// at least one of line/regexp must be present.
	line, lerr := util.OptStringParam(req.Params, "line")
	if lerr != nil {
		errs = append(errs, lerr.Error())
	}
	switch req.State {
	case "present":
		if line == "" {
			errs = append(errs, `param "line": required for state present`)
		}
	case "absent":
		if line == "" && rx == "" {
			errs = append(errs, `state absent requires "line" or "regexp"`)
		}
	}

	// insertafter / insertbefore are mutually exclusive; the specific
	// allowed values (EOF/BOF/literal) are resolved in Apply at insertion
	// time.
	insAfter, iaerr := util.OptStringParam(req.Params, "insertafter")
	if iaerr != nil {
		errs = append(errs, iaerr.Error())
	}
	insBefore, iberr := util.OptStringParam(req.Params, "insertbefore")
	if iberr != nil {
		errs = append(errs, iberr.Error())
	}
	if insAfter != "" && insBefore != "" {
		errs = append(errs, `params "insertafter" and "insertbefore" are mutually exclusive`)
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares core.line.Plan as pure-read (ADR-031 Scry): reads
// the file and runs the same pure presentEdit/absentEdit functions as
// applyPresent/applyAbsent (changed is already there). Doesn't mutate the
// filesystem (marker for the host, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads the file's current
// content (the same readFile as Apply) and reuses the pure
// presentEdit/absentEdit functions — they already return `changed`. Doesn't
// mutate the filesystem: no AtomicWrite, no AtomicWritePreserving.
//
// create=true (state present, file missing) → drift=true (Apply would
// create the file).
// create=false (state present, file missing) → drift=true (Apply would
// return failed, but that's still "differs from desired", and Plan honestly
// reports it — the operator sees dry_run would hit create:false).
//
// absent + file missing → drift=false (nothing to remove, symmetric with
// applyAbsent).
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	p, err := readParamsFromStruct(req.Params)
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	switch req.State {
	case "present":
		if p.line == "" {
			return util.PlanFailed(`param "line": required for state present`)
		}
	case "absent":
		if p.line == "" && p.regexp == nil {
			return util.PlanFailed(`state absent requires "line" or "regexp"`)
		}
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}

	content, existed, rerr := readFile(path)
	if rerr != nil {
		return util.PlanFailed(rerr.Error())
	}
	switch req.State {
	case "present":
		if !existed {
			// Apply would create the file (if create=true) or return failed;
			// either way the desired state is NOT reached → drift.
			return util.SendPlanFinal(stream, true)
		}
		lines, _ := splitLines(content)
		return util.SendPlanFinal(stream, presentEdit(lines, p).changed)
	case "absent":
		if !existed {
			return util.SendPlanFinal(stream, false)
		}
		lines, _ := splitLines(content)
		return util.SendPlanFinal(stream, absentEdit(lines, p).changed)
	}
	// unreachable.
	return util.PlanFailed("unreachable")
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	switch req.State {
	case "present":
		return m.applyPresent(stream, req, path)
	case "absent":
		return m.applyAbsent(stream, req, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}
