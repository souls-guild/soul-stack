// Package soul implements the keeper-side core module `core.soul.registered`
// (ADR-017, docs/keeper/modules.md).
//
// State:
//   - registered: declarative form "Soul with the given sid is in the registry
//     and bound to the specified set of Coven labels".
//
// Mode semantics:
//   - append (default): existing ∪ provided.
//   - replace: provided (empty set is an error, footgun protection).
//   - remove: existing \ provided.
//
// Side-effect: if there is no record in `souls` for sid, the module creates it
// with status: pending (new host added by scenario — host branch add_replica
// or after cloud-provision). Does not issue bootstrap tokens/SoulSeed — that is
// the responsibility of onboarding.
//
// `sid` accepts a string OR a list of strings (ADR-061): a single SID remains
// valid (backward compatibility), a list — registration + waiting for N hosts
// created in one barrier step. `coven` applies to all SIDs in the list.
//
// Onboarding barrier (ADR-061): with `await_online: true`, after registering
// all SIDs, the step polls presence (Redis SID-lease via PresenceChecker) until
// `await_min_count` online or `await_timeout`. B1-strict: quota shortfall at
// timeout — step failed (see await.go).
//
// `refresh_soulprint` (ADR-061 §S2/§S3 — revived): when `true`, the step becomes
// a passage-determining boundary (Stratify), and the scenario runner AFTER its
// success re-resolves roster before the next Passage (run.go stage-loop). Output
// carries `refreshed` = flag value (true ⇒ re-resolve is guaranteed to execute).
package soul

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the base name of the module without state suffix (Registry key). The
// author form of the task address is `core.soul.registered` (base + state, like
// Soul-side core modules); state `registered` arrives in pluginv1.ApplyRequest.state.
const Name = "core.soul"

// Mode values. Match docs/keeper/modules.md → mode semantics.
const (
	ModeAppend  = "append"
	ModeReplace = "replace"
	ModeRemove  = "remove"
)

// Store is a narrow subset of keeper/internal/soul needed by the module.
// Full pgxpool is not exposed — this simplifies unit testing (fake implements
// only four methods) and fixes the contract.
//
// SoulsWithSoulprint is a batch check "typed soulprint is recorded in PG"
// (subset of sids with `soulprint_facts IS NOT NULL`) for the facts-part of
// the onboarding barrier (ADR-061 amendment: refresh_soulprint ⇒ barrier waits
// for presence AND first soulprint).
type Store interface {
	SelectBySID(ctx context.Context, sid string) (*keepersoul.Soul, error)
	Insert(ctx context.Context, s *keepersoul.Soul) error
	UpdateCoven(ctx context.Context, sid string, coven []string) ([]string, error)
	SoulsWithSoulprint(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// PresenceChecker is a narrow surface for batch-checking "is the Redis SID-lease
// alive" (presence=online, ADR-006(a)/ADR-061), needed by the `await_online`
// onboarding barrier. Narrowing to one method isolates the module from the full
// keeperredis.Client and allows fakes in unit tests; the real implementation is
// a wrapper over keeperredis.SoulsStreamAlive, assembled in cmd/keeper
// (symmetrically to topology.SoulLeaseChecker).
//
// The source of truth for "online" is precisely the lease (live EventStream),
// not PG souls.status (lifecycle snapshot, lags behind): the barrier must not
// consider a host online until the actual stream.
type PresenceChecker interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// Module implements sdk/module.SoulModule over Store.
//
// presence/maxAwaitTimeout are populated optionally via WithPresence — needed
// only by the onboarding barrier (`await_online`, ADR-061). Without them, the
// step works as before ADR-061 (registration without barrier); a request with
// `await_online: true` without a configured presence-checker fails (the barrier
// cannot work without a presence source — silent success is not allowed).
type Module struct {
	Store    Store
	presence PresenceChecker
	// maxAwaitTimeout is a provider of the string ceiling await_timeout from the
	// current keeper.yml snapshot (hot-reload: read on each Apply). nil → default
	// config.DefaultMaxAwaitTimeout. A function (not a value): config.Store.Get()
	// changes on reload.
	maxAwaitTimeout func() string
}

// New constructs the module with the given Store. Caller typically provides an
// adapter over pgxpool — see NewPGStore.
func New(store Store) *Module {
	return &Module{Store: store}
}

// WithPresence connects a presence-checker (Redis SID-lease) and a provider of
// the await_timeout ceiling for the onboarding barrier (ADR-061). maxAwaitTimeout
// is a function returning the current string value of keeper.yml::max_await_timeout
// (hot-reload-aware); a nil function or empty string → config.DefaultMaxAwaitTimeout.
func (m *Module) WithPresence(p PresenceChecker, maxAwaitTimeout func() string) *Module {
	m.presence = p
	m.maxAwaitTimeout = maxAwaitTimeout
	return m
}

// Validate checks state and required parameters. Runs before Apply; errors
// returned as `ValidateReply.errors[]`, not as gRPC-error.
//
// Not delegated to manifest-checking (like url/repo on Soul-side): beyond
// known-state + required(sid/coven), the module has an enum `mode` (append/replace/
// remove) that the reduced plugin.InputParamDef DSL does not express. soul-lint
// validates author form statically against shared/coremanifest/soul.yaml; this
// method is runtime insurance. (keeper-side util does not carry
// ValidateAgainstManifest — its coremanifest facade lives in Soul-side util;
// duplicating for one module is excessive now.)
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "registered" {
		errs = append(errs, fmt.Sprintf("unknown state %q (want registered)", req.State))
	}
	sids, err := util.StringOrSliceParam(req.Params, "sid")
	if err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringSliceParam(req.Params, "coven"); err != nil {
		errs = append(errs, err.Error())
	}
	if mode, err := util.OptStringParam(req.Params, "mode"); err != nil {
		errs = append(errs, err.Error())
	} else if mode != "" && !isValidMode(mode) {
		errs = append(errs, fmt.Sprintf("param %q: unknown mode (want append/replace/remove)", "mode"))
	}
	errs = append(errs, validateAwaitParams(req.Params, len(sids))...)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op in MVP (symmetrically to Soul-side core modules).
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply applies the registered state. All errors are sent as failed-event
// (not gRPC-error) so that scenario-applier sees them through ApplyEvent.failed
// and enters the `onfail:` branch.
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	if req.State != "registered" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}

	// sid accepts a string OR a list (ADR-061). A single string is normalized
	// to a list of one element; output form (aggregate vs single) is chosen by
	// len(sids) at the very end.
	sids, err := util.StringOrSliceParam(req.Params, "sid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	for _, sid := range sids {
		if !keepersoul.ValidSID(sid) {
			return util.SendFailed(stream, fmt.Sprintf("invalid sid %q", sid))
		}
	}
	wanted, err := util.StringSliceParam(req.Params, "coven")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Symmetry with API boundary (POST /v1/souls → soul.ValidCoven): souls.coven
	// only accepts kebab-case labels. Without this check, garbage like "Prod"/"a_b"
	// would silently persist to the registry through scenario step.
	for _, c := range wanted {
		if !keepersoul.ValidCoven(c) {
			return util.SendFailed(stream, fmt.Sprintf("invalid coven %q (want kebab-case, 1..63)", c))
		}
	}

	modeParam, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if modeParam == "" {
		modeParam = ModeAppend
	}
	if !isValidMode(modeParam) {
		return util.SendFailed(stream, fmt.Sprintf("unknown mode %q (want append/replace/remove)", modeParam))
	}

	// refresh_soulprint (ADR-061 §S3 — revived). When true, the scenario runner
	// AFTER this step's success re-resolves roster before the NEXT Passage (S2
	// already made the step passage-determining, S3 executes re-resolve in run.go
	// stage-loop). Therefore, echo refreshed = flag value: true ⇒ re-resolve is
	// guaranteed to execute (created+onboarded hosts will enter roster of subsequent
	// Passages). false / absent ⇒ refreshed:false (behavior before ADR unchanged).
	refreshSoulprint, _, err := util.OptBoolParam(req.Params, "refresh_soulprint")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// Onboarding barrier (ADR-061): parse+validate BEFORE any side-effects in
	// souls — unmet quorum / ceiling overflow should not leave partial registration
	// (fail-fast on barrier parameters).
	awaitCfg, aerr := m.parseAwait(req.Params, len(sids))
	if aerr != nil {
		return util.SendFailed(stream, aerr.Error())
	}
	// refresh_soulprint ⇒ barrier waits for presence AND first typed soulprint
	// (ADR-061 amendment): render of next Passage reads soulprint.self.*,
	// presence alone does not guarantee it (first report write is async).
	if awaitCfg != nil {
		awaitCfg.requireFacts = refreshSoulprint
	}

	// replace + empty coven is an error (double footgun protection).
	if modeParam == ModeReplace && len(wanted) == 0 {
		return util.SendFailed(stream, "mode=replace requires non-empty coven (footgun protection: host must keep at least one coven label)")
	}

	// Register all SIDs (common coven set applies to each).
	anyCreated := false
	anyChanged := false
	var savedFirst, removedFirst []string
	for i, sid := range sids {
		res, rerr := m.registerOne(ctx, sid, wanted, keepersoul.CovenMode(modeParam))
		if rerr != nil {
			return util.SendFailed(stream, rerr.Error())
		}
		anyCreated = anyCreated || res.created
		anyChanged = anyChanged || res.created || res.covenChanged
		if i == 0 {
			savedFirst, removedFirst = res.saved, res.removed
		}
	}

	// Barrier: blocking wait for readiness (presence, and if refresh_soulprint —
	// first typed soulprint) across all SIDs up to min_count/timeout.
	var barrier awaitResult
	barrier.satisfied = true
	if awaitCfg != nil {
		barrier, err = m.awaitOnline(ctx, sids, awaitCfg)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
		if !barrier.satisfied {
			// B1-strict: quota shortfall at timeout — failed (fail-stop run,
			// state not committed — error_locked).
			msg := barrierTimeoutMessage(sids, awaitCfg, barrier)
			return util.SendFailed(stream, msg)
		}
	}

	out := buildOutput(sids, savedFirst, modeParam, anyCreated, removedFirst, refreshSoulprint, awaitCfg != nil, barrier.online, barrier.pending, barrier.satisfied)
	return util.SendFinal(stream, anyChanged, out)
}

// registerResult is the result of registering a single SID.
type registerResult struct {
	created      bool
	covenChanged bool
	saved        []string
	removed      []string
}

// registerOne creates/updates a souls record for a single SID and applies coven-mode.
func (m *Module) registerOne(ctx context.Context, sid string, wanted []string, mode keepersoul.CovenMode) (registerResult, error) {
	cur, created, ferr := m.fetchOrCreate(ctx, sid)
	if ferr != nil {
		return registerResult{}, ferr
	}
	before := append([]string(nil), cur.Coven...)
	final, removed := keepersoul.ApplyCovenMode(before, wanted, mode)
	covenChanged := !keepersoul.CovenSetEqual(before, final)
	saved := before
	if covenChanged {
		var err error
		saved, err = m.Store.UpdateCoven(ctx, sid, final)
		if err != nil {
			return registerResult{}, fmt.Errorf("update coven %q: %w", sid, err)
		}
	}
	return registerResult{created: created, covenChanged: covenChanged, saved: saved, removed: removed}, nil
}

// buildOutput constructs register-payload. A single SID preserves the historical
// form (`sid` as string, `coven`/`removed` from the single host); a list — `sid`
// as array. Barrier fields (online/pending/satisfied) are added only with await.
func buildOutput(sids, savedFirst []string, mode string, created bool, removedFirst []string, refreshed, awaited bool, online, pending []string, satisfied bool) map[string]any {
	out := map[string]any{
		"mode":      mode,
		"created":   created,
		"refreshed": refreshed,
		"coven":     toAnySlice(savedFirst),
		"removed":   toAnySlice(removedFirst),
	}
	if len(sids) == 1 {
		out["sid"] = sids[0]
	} else {
		out["sid"] = toAnySlice(sids)
	}
	if awaited {
		out["online"] = toAnySlice(online)
		out["pending"] = toAnySlice(pending)
		out["satisfied"] = satisfied
	}
	return out
}

// fetchOrCreate returns the current souls record; if it doesn't exist, creates
// one with status: pending and empty coven (the module will later update coven
// via UpdateCoven, see Apply).
func (m *Module) fetchOrCreate(ctx context.Context, sid string) (*keepersoul.Soul, bool, error) {
	got, err := m.Store.SelectBySID(ctx, sid)
	if err == nil {
		return got, false, nil
	}
	if !errors.Is(err, keepersoul.ErrSoulNotFound) {
		return nil, false, fmt.Errorf("lookup soul %q: %w", sid, err)
	}
	// Create a pending record. LastSeenAt/CreatedByAID fields are nil: cloud-provision
	// or scenario-host-add do not carry an operator (this is a keeper-internal action).
	stub := &keepersoul.Soul{
		SID:       sid,
		Transport: keepersoul.TransportAgent,
		Status:    keepersoul.StatusPending,
		Coven:     []string{},
	}
	if err := m.Store.Insert(ctx, stub); err != nil {
		return nil, false, fmt.Errorf("create soul %q: %w", sid, err)
	}
	return stub, true, nil
}

// isValidMode is a closed-enum check of the module's mode string. Delegates to
// keeper-side soul.ValidCovenMode (unified mode vocabulary; refactor-pilot bulk
// coven-assign moved set-semantics to keeper/internal/soul).
func isValidMode(m string) bool {
	return keepersoul.ValidCovenMode(keepersoul.CovenMode(m))
}

// toAnySlice converts []string → []any for structpb.NewStruct.
// structpb does not handle list of string directly: only list of any with auto-coerce.
func toAnySlice(xs []string) []any {
	if xs == nil {
		return []any{}
	}
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
