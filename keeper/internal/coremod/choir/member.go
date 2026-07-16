// Package choir implements keeper-side core-module `core.choir`
// (ADR-044, pattern from `core.soul.registered` in ADR-017).
//
// Author form of task address is `core.choir.present` / `core.choir.absent`
// (base `core.choir` + state, like `core.file.present`/`core.file.absent`
// on Soul-side). Declared entity is membership "SID is a Voice in
// the given Choir of this incarnation" (declared choir roster, ADR-044 section 2).
//
// State semantics (symmetric present/absent to other core-modules):
//   - present (default): AddVoice — SID becomes Voice of Choir.
//     Idempotent: Voice already exists → changed=false, not error.
//   - absent: RemoveVoice — membership removed. Idempotent: Voice doesn't exist →
//     changed=false, not error.
//
// Membership invariant (ADR-044 section 3 — Voice only for SID already member
// of incarnation) is NOT duplicated here: implemented in choir-CRUD (AddVoice →
// ErrNotMembers) and reused. On ErrNotMembers Apply sends failed-event
// (run enters onfail/error_locked).
//
// RESTRICTIONS S-T5 (future, not implemented here):
//   - Cross-incarnation guard (param.incarnation == run-context incarnation):
//     run-context unavailable to module (ADR-044/architect A1). Module trusts
//     param `incarnation` and only validates its existence. Hard guard is
//     separate task (RunContext-injection in keeper-dispatch, needs_architect).
//   - Roster-growth (new Voice visible to next step of run) — not implemented.
package choir

import (
	"context"
	"errors"
	"fmt"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the base module name without state suffix (Registry key). Author form
// of task address is `core.choir.present` / `core.choir.absent`.
const Name = "core.choir"

// State values (symmetric present/absent to Soul-side core-modules).
const (
	StatePresent = "present"
	StateAbsent  = "absent"
)

// Store is narrow subset of choir-CRUD + incarnation existence check
// needed by module. We don't expose full pgxpool outside (like core.soul.registered):
// fake implements only three methods, contract is explicit.
//
// AddVoice/RemoveVoice are wrappers over same-named choir package functions
// (S-T2). IncarnationExists is lightweight incarnation existence check for
// absent-branch (present is indirectly covered by FK choir→incarnation inside AddVoice).
type Store interface {
	AddVoice(ctx context.Context, v *keeperchoir.Voice) error
	RemoveVoice(ctx context.Context, incarnation, choirName, sid string) error
	IncarnationExists(ctx context.Context, incarnation string) (bool, error)
}

// Module implements sdk/module.SoulModule over Store.
type Module struct {
	Store Store
}

// New builds module with given Store. Caller usually provides adapter over
// pgxpool — see NewPGStore.
func New(store Store) *Module {
	return &Module{Store: store}
}

// Validate checks state and required parameters. Runs before Apply;
// errors returned as ValidateReply.errors[], not as gRPC-error.
//
// Required: incarnation, choir, sid. Optional: role, position (int >= 0),
// state (present/absent; empty → present). soul-lint validates author form
// statically; this method is runtime safeguard (like core.soul.registered).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "" && !isKnownState(req.State) {
		errs = append(errs, fmt.Sprintf("unknown state %q (want present/absent)", req.State))
	}
	if _, err := util.StringParam(req.Params, "incarnation"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.Params, "choir"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.Params, "sid"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptStringParam(req.Params, "role"); err != nil {
		errs = append(errs, err.Error())
	}
	if pos, ok, err := util.OptIntParam(req.Params, "position"); err != nil {
		errs = append(errs, err.Error())
	} else if ok && pos < 0 {
		errs = append(errs, fmt.Sprintf("param %q: must be >= 0, got %d", "position", pos))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is no-op in MVP (symmetric to other core-modules).
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply applies present/absent state. All errors sent as failed-event (not
// gRPC-error) so scenario-applier enters onfail-branch.
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	state := req.State
	if state == "" {
		state = StatePresent
	}
	if !isKnownState(state) {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q (want present/absent)", req.State))
	}

	incarnation, err := util.StringParam(req.Params, "incarnation")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	choirName, err := util.StringParam(req.Params, "choir")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !keeperchoir.ValidChoirName(choirName) {
		return util.SendFailed(stream, fmt.Sprintf("invalid choir name %q", choirName))
	}
	sid, err := util.StringParam(req.Params, "sid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !keepersoul.ValidSID(sid) {
		return util.SendFailed(stream, fmt.Sprintf("invalid sid %q", sid))
	}

	role, err := util.OptStringParam(req.Params, "role")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	position, posSet, err := util.OptIntParam(req.Params, "position")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if posSet && position < 0 {
		return util.SendFailed(stream, fmt.Sprintf("param %q: must be >= 0, got %d", "position", position))
	}

	// S-T5 substitute for hard cross-incarnation guard: explicitly validate that
	// param-incarnation exists. present is indirectly covered by FK choir→incarnation
	// inside AddVoice, but absent (RemoveVoice — single DELETE without FK-check)
	// would silently return ErrVoiceNotFound on incarnation name typo.
	exists, err := m.Store.IncarnationExists(ctx, incarnation)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("check incarnation %q: %v", incarnation, err))
	}
	if !exists {
		return util.SendFailed(stream, fmt.Sprintf("incarnation %q not found", incarnation))
	}

	switch state {
	case StatePresent:
		return m.applyPresent(ctx, stream, incarnation, choirName, sid, role, position, posSet)
	case StateAbsent:
		return m.applyAbsent(ctx, stream, incarnation, choirName, sid)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", state))
	}
}

// applyPresent adds Voice. ErrVoiceExists → idempotent no-op
// (changed=false). ErrNotMembers (membership invariant, ADR-044 section 3) →
// failed-event (run enters error_locked). ErrChoirNotFound → failed.
func (m *Module) applyPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], incarnation, choirName, sid, role string, position int64, posSet bool) error {
	v := &keeperchoir.Voice{
		IncarnationName: incarnation,
		ChoirName:       choirName,
		SID:             sid,
	}
	if role != "" {
		v.Role = &role
	}
	if posSet {
		p := int(position)
		v.Position = &p
	}

	err := m.Store.AddVoice(ctx, v)
	switch {
	case err == nil:
		return util.SendFinal(stream, true, presentOutput(incarnation, choirName, sid, true))
	case errors.Is(err, keeperchoir.ErrVoiceExists):
		// Idempotent: Voice already exists → nothing changed.
		return util.SendFinal(stream, false, presentOutput(incarnation, choirName, sid, false))
	default:
		// ErrNotMembers / ErrChoirNotFound / other → failed-event.
		return util.SendFailed(stream, fmt.Sprintf("add voice %q to choir %q/%q: %v", sid, incarnation, choirName, err))
	}
}

// applyAbsent removes Voice. ErrVoiceNotFound → idempotent no-op
// (changed=false). Other errors → failed-event.
func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], incarnation, choirName, sid string) error {
	err := m.Store.RemoveVoice(ctx, incarnation, choirName, sid)
	switch {
	case err == nil:
		return util.SendFinal(stream, true, absentOutput(incarnation, choirName, sid, true))
	case errors.Is(err, keeperchoir.ErrVoiceNotFound):
		return util.SendFinal(stream, false, absentOutput(incarnation, choirName, sid, false))
	default:
		return util.SendFailed(stream, fmt.Sprintf("remove voice %q from choir %q/%q: %v", sid, incarnation, choirName, err))
	}
}

func presentOutput(incarnation, choirName, sid string, added bool) map[string]any {
	return map[string]any{
		"incarnation": incarnation,
		"choir":       choirName,
		"sid":         sid,
		"state":       StatePresent,
		"added":       added,
	}
}

func absentOutput(incarnation, choirName, sid string, removed bool) map[string]any {
	return map[string]any{
		"incarnation": incarnation,
		"choir":       choirName,
		"sid":         sid,
		"state":       StateAbsent,
		"removed":     removed,
	}
}

func isKnownState(s string) bool {
	return s == StatePresent || s == StateAbsent
}
