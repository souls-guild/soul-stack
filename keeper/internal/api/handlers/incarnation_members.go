package handlers

// Operator-facing incarnation MEMBERSHIP (ADR-008 amendment 2026-07-28, NIM-209):
// bind / unbind / read the roster of an incarnation.
//
// Why this exists. Membership became a first-class relation with NIM-124, but the
// only act that wrote it was `core.soul.registered` INSIDE a scenario run — and a
// run resolves its roster at start, so a scenario deploying onto ready hosts
// (`create_from_souls`) aborted with `no_hosts` before it could bind anything.
// An already-onboarded host therefore could not be put into an incarnation from
// the outside at all. These three functions are that missing step.
//
// AUDIT CLASS: SELF-AUDIT — bind/unbind write `incarnation.member_bound` /
// `incarnation.member_unbound` themselves (audit middleware is NOT wired on these
// routes). Reading the roster writes no audit.
//
// TWO GATES, both required (rbac.md § Incarnation membership):
//
//	(a) the incarnation — `incarnation.bind-member` / `unbind-member` with the
//	    incarnation/coven/service selector by path-{name}; applied by middleware.
//	(b) each host — the SID must be inside the caller's soul visibility
//	    (`soul.list` purview). Gate (a) alone is NOT enough: a scope predicate on
//	    `incarnation=` is satisfied without ever looking at the host, so a holder
//	    of `bind-member on incarnation=X` could otherwise pull ANY host into X and
//	    then reach it with `incarnation.run`.
//
// Gate (b) is ALL-OR-NOTHING: one out-of-scope SID rejects the whole request. A
// silent trim would read to the operator as "all bound" and leave a roster that
// is short a host — the run would then fail somewhere further away.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// MaxBindMembersPerRequest caps one bind call. A roster is a human-sized list of
// hosts, not a bulk import: the cap keeps the all-or-nothing gate cheap to
// evaluate and the 422 bodies readable.
const MaxBindMembersPerRequest = 200

// MemberView — FLAT domain projection of one roster entry. Status is the host's
// lifecycle status at read time (membership itself has no status).
type MemberView struct {
	SID        string
	Status     string
	BoundAt    time.Time
	BoundByAID *string
}

// MemberListPage — domain result of GET /v1/incarnations/{id}/members.
type MemberListPage struct {
	Items []MemberView
}

// MemberBindView — FLAT domain projection of the 200 body of POST
// .../members. The bind is idempotent, and the split says what actually
// happened: Bound are the SIDs written by THIS call, AlreadyMember the ones that
// were members before it. Both sorted.
type MemberBindView struct {
	Incarnation   string
	Bound         []string
	AlreadyMember []string
}

// BindMembersTyped — domain function of POST /v1/incarnations/{id}/members
// (SELF-AUDIT `incarnation.member_bound`). Binds already-onboarded hosts to an
// existing incarnation so a scenario can subsequently roll onto them.
//
// Order of checks is deliberate — cheapest and most-revealing first, and every
// rejection happens BEFORE any write: shape → incarnation exists → hosts exist →
// gate (b) scope → host status. So an operator who may not see a host learns
// "forbidden", not "that host is disconnected".
func (h *IncarnationHandler) BindMembersTyped(ctx context.Context, claims *jwt.Claims, name string, sids []string) (MemberBindView, error) {
	var zero MemberBindView

	if !incarnation.ValidID(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'id' must match "+incarnation.IDPattern)
	}
	if len(sids) == 0 {
		return zero, incProblem(problem.TypeValidationFailed, "field 'sids' must contain at least one SID")
	}
	if len(sids) > MaxBindMembersPerRequest {
		return zero, incProblem(problem.TypeValidationFailed,
			fmt.Sprintf("field 'sids' must contain at most %d entries, got %d", MaxBindMembersPerRequest, len(sids)))
	}
	for _, sid := range sids {
		if !soul.ValidSID(sid) {
			return zero, incProblem(problem.TypeValidationFailed, "sids entry "+sid+" must match "+soul.SIDPattern)
		}
	}
	// Duplicates inside one request are collapsed rather than rejected: the
	// relation is a set, and "bind these three, one twice" has an unambiguous
	// meaning. The de-duplicated list is what the reply and the audit report.
	sids = dedupeSorted(sids)

	if _, err := h.loadIncarnationForMembership(ctx, name, "bind-members"); err != nil {
		return zero, err
	}

	// Gates (b) + host status, screened in the DOMAIN so the MCP mirror runs the
	// exact same check ([incarnation.ScreenBindCandidates]). Fail-closed on a
	// missing resolver: without it the per-host boundary cannot be evaluated, and
	// binding regardless is the escalation the gate exists to prevent.
	if h.scoper == nil {
		h.logger.Error("incarnation.bind-members: scoper not configured")
		return zero, incProblem(problem.TypeInternalError, "membership bind unavailable")
	}
	scope := soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))
	rej, err := incarnation.ScreenBindCandidates(ctx, h.db, sids, scope)
	if err != nil {
		h.logger.Error("incarnation.bind-members: screen candidates failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select souls failed")
	}
	if perr := bindRejectionProblem(rej); perr != nil {
		return zero, perr
	}

	boundBy := claims.Subject
	bound, err := incarnation.AddMembersReporting(ctx, h.db, name, sids, &boundBy)
	if err != nil {
		h.logger.Error("incarnation.bind-members: insert membership failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "bind members failed")
	}
	already := diffSorted(sids, bound)

	if h.auditW != nil {
		_ = h.auditW.Write(ctx, &audit.Event{
			EventType: audit.EventIncarnationMemberBound,
			Source:    apimiddleware.ScenarioInvocationSource(ctx),
			ArchonAID: claims.Subject,
			Payload: map[string]any{
				"id":             name,
				"sids":           sids,
				"bound":          bound,
				"already_member": already,
			},
		})
	}

	return MemberBindView{Incarnation: name, Bound: emptyIfNil(bound), AlreadyMember: emptyIfNil(already)}, nil
}

// --- Roster on the create path (NIM-371) ------------------------------------
//
// A create scenario that rolls onto ready hosts declares which `input:` field
// carries them (`source: { roster: true }`), and the create binds that value into
// `incarnation_membership` before starting the bootstrap run. These two functions
// are that step, split at the insert: everything that can refuse runs BEFORE the row
// exists, the write runs after.
//
// The gates are the SAME two as [IncarnationHandler.BindMembersTyped] — nothing
// weaker, or create would be the way around them:
//
//	(a) `incarnation.bind-member` over the incarnation being created
//	    ([ScreenIncarnationCreateRosterScope]) — the route middleware cannot do it
//	    here, since this is the create route and the name may not exist until the
//	    plan resolves;
//	(b) each SID inside the caller's `soul.list` purview, all-or-nothing
//	    ([incarnation.ScreenBindCandidates]) — shared verbatim with the bind route.

// CreateRosterScreening is the outcome of [ScreenCreateRoster]: the SIDs to bind
// (empty = nothing to bind) plus the per-host rejection buckets, if any. Rejection
// is returned rather than pre-phrased so REST and MCP map it to their own transport,
// the same split [incarnation.ScreenBindCandidates] already uses for the bind route.
type CreateRosterScreening struct {
	SIDs      []string
	Rejection *incarnation.BindRejection
}

// ErrCreateRosterTooLarge / ErrCreateRosterInvalidSID — shape refusals of a roster
// carried by a create request (422 on both surfaces). SID is the offending entry.
var ErrCreateRosterTooLarge = errors.New("handlers: create roster exceeds the per-request cap")

// ErrCreateRosterInvalidSID — one roster entry is not a syntactically valid SID.
// The input gate validates `format: sid` when the scenario declares it, so reaching
// this means the declaration omitted the format; refusing here keeps a malformed SID
// out of the membership relation either way.
var ErrCreateRosterInvalidSID = errors.New("handlers: create roster carries an invalid SID")

// ScreenCreateRoster validates and authorizes the roster a create request carries,
// returning the SIDs to bind. Empty result when the scenario declares no roster or
// the operator left the field empty — an empty roster is not an error HERE: a
// `required: true` declaration is enforced by the input gate, which has already run.
//
// Called BEFORE `incarnation.Create` on both surfaces, so every refusal leaves no
// row behind. Check order mirrors the bind route — shape → gate (a) → gate (b) +
// host status — and for the same reason: an operator who may not see a host must be
// told "forbidden", never that it is disconnected.
//
// Package-level with explicit dependencies (not a handler method) so the MCP create
// tool runs this exact screening instead of a second implementation of it — the
// drift class [ScreenIncarnationCreateScope] was extracted to avoid.
//
// A nil scoper is fail-closed (error): without it the per-host boundary cannot be
// evaluated, and binding regardless is the escalation gate (b) exists to prevent.
func ScreenCreateRoster(
	ctx context.Context,
	db incarnation.ExecQueryRower,
	checker apimiddleware.PermissionChecker,
	scoper PurviewResolver,
	aid, name, service string,
	covens []string,
	plan scenario.CreatePlan,
) (CreateRosterScreening, error) {
	var zero CreateRosterScreening
	if plan.RosterField == "" || len(plan.RosterSIDs) == 0 {
		return zero, nil
	}

	if len(plan.RosterSIDs) > MaxBindMembersPerRequest {
		return zero, fmt.Errorf("%w: input %q carries %d SIDs, at most %d allowed",
			ErrCreateRosterTooLarge, plan.RosterField, len(plan.RosterSIDs), MaxBindMembersPerRequest)
	}
	for _, sid := range plan.RosterSIDs {
		if !soul.ValidSID(sid) {
			return zero, fmt.Errorf("%w: input %q entry %q must match %s",
				ErrCreateRosterInvalidSID, plan.RosterField, sid, soul.SIDPattern)
		}
	}
	// Collapsed, not rejected — same reading as the bind route: the roster is a set,
	// and "these three, one twice" has one unambiguous meaning.
	sids := dedupeSorted(plan.RosterSIDs)

	if err := ScreenIncarnationCreateRosterScope(checker, aid, name, service, covens); err != nil {
		return zero, err
	}

	if scoper == nil {
		return zero, errors.New("handlers: create roster screening unavailable: purview resolver not configured")
	}
	scope := soulpurview.Resolve(scoper.ResolvePurview(aid, "soul", "list"))
	rej, err := incarnation.ScreenBindCandidates(ctx, db, sids, scope)
	if err != nil {
		return zero, fmt.Errorf("handlers: create roster: screen candidates: %w", err)
	}
	return CreateRosterScreening{SIDs: sids, Rejection: rej}, nil
}

// screenCreateRoster is the REST projection of [ScreenCreateRoster]: same screening,
// errors mapped to *problemError.
func (h *IncarnationHandler) screenCreateRoster(
	ctx context.Context, claims *jwt.Claims, name, service string, covens []string, plan scenario.CreatePlan,
) ([]string, error) {
	res, err := ScreenCreateRoster(ctx, h.db, h.permChecker, h.scoper,
		claims.Subject, name, service, covens, plan)
	if err != nil {
		switch {
		case errors.Is(err, ErrCreateRosterTooLarge), errors.Is(err, ErrCreateRosterInvalidSID):
			return nil, incProblem(problem.TypeValidationFailed, err.Error())
		case errors.Is(err, ErrCreateRosterScopeExceeded):
			return nil, incProblem(problem.TypeForbidden, createRosterScopeDetail(name, plan.RosterField))
		default:
			h.logger.Error("incarnation.create: screen roster failed",
				slog.String("name", name), slog.Any("error", err))
			return nil, incProblem(problem.TypeInternalError, "roster bind unavailable")
		}
	}
	if perr := bindRejectionProblem(res.Rejection); perr != nil {
		return nil, perr
	}
	return res.SIDs, nil
}

// createRosterScopeDetail phrases the bind-member refusal of a create-carried
// roster. It names the input field because that is the lever the operator has: the
// roster is not a separate request they can drop, it is a field of this one.
func createRosterScopeDetail(name, field string) string {
	return "incarnation.bind-member denied: input '" + field + "' declares a roster for " + name +
		", and populating an incarnation needs bind-member over it — incarnation.create alone is not enough"
}

// bindCreateRoster writes the screened roster into `incarnation_membership` and
// audits it as an ordinary member bind (`incarnation.member_bound`). Runs after the
// insert and before the bootstrap run.
//
// The audit event is the same one the bind route writes, with `via: create` marking
// where it came from: a roster that appeared at birth and one bound a second later
// are the same fact about the incarnation, and splitting them into two event types
// would hide half of them from anyone querying "how did this host get in here".
//
// `already_member` is not reported: the row was just created, so every SID is new.
func (h *IncarnationHandler) bindCreateRoster(ctx context.Context, claims *jwt.Claims, name string, sids []string) error {
	boundBy := claims.Subject
	bound, err := incarnation.AddMembersReporting(ctx, h.db, name, sids, &boundBy)
	if err != nil {
		h.logger.Error("incarnation.create: bind roster failed",
			slog.String("name", name), slog.Any("error", err))
		return incProblem(problem.TypeInternalError,
			"incarnation "+name+" was created, but binding its roster failed — bind the hosts via POST /v1/incarnations/"+name+"/members and run the create scenario")
	}
	if h.auditW != nil {
		_ = h.auditW.Write(ctx, &audit.Event{
			EventType: audit.EventIncarnationMemberBound,
			Source:    apimiddleware.ScenarioInvocationSource(ctx),
			ArchonAID: claims.Subject,
			Payload: map[string]any{
				"id":             name,
				"sids":           sids,
				"bound":          emptyIfNil(bound),
				"already_member": []string{},
				"via":            "create",
			},
		})
	}
	return nil
}

// UnbindMemberTyped — domain function of DELETE /v1/incarnations/{id}/members/{sid}
// (SELF-AUDIT `incarnation.member_unbound`). Idempotent: unbinding a host that is
// not a member succeeds without changing anything, and the audit records the
// attempt either way (`removed` says which it was).
//
// Gate (b) applies here too, but only when the host still exists in the registry:
// FK `sid → souls ON DELETE CASCADE` means a deleted host has already lost its
// memberships, so an unknown SID is a no-op rather than a 404.
func (h *IncarnationHandler) UnbindMemberTyped(ctx context.Context, claims *jwt.Claims, name, sid string) error {
	if !incarnation.ValidID(name) {
		return incProblem(problem.TypeValidationFailed, "path 'id' must match "+incarnation.IDPattern)
	}
	if !soul.ValidSID(sid) {
		return incProblem(problem.TypeValidationFailed, "path 'sid' must match "+soul.SIDPattern)
	}

	if _, err := h.loadIncarnationForMembership(ctx, name, "unbind-member"); err != nil {
		return err
	}

	if h.scoper == nil {
		h.logger.Error("incarnation.unbind-member: scoper not configured")
		return incProblem(problem.TypeInternalError, "membership unbind unavailable")
	}
	scope := soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))
	inScope, known, err := incarnation.HostInScope(ctx, h.db, sid, scope)
	if err != nil {
		h.logger.Error("incarnation.unbind-member: select soul failed",
			slog.String("name", name), slog.String("sid", sid), slog.Any("error", err))
		return incProblem(problem.TypeInternalError, "select soul failed")
	}
	// An unknown host needs no scope check: FK `sid → souls ON DELETE CASCADE`
	// already took its memberships with it, so the delete below is the idempotent
	// no-op rather than a 404.
	if known && !inScope {
		return incProblem(problem.TypeForbidden, "SID "+sid+" is outside the operator's soul scope")
	}

	removed, err := incarnation.RemoveMember(ctx, h.db, name, sid)
	if err != nil {
		h.logger.Error("incarnation.unbind-member: delete membership failed",
			slog.String("name", name), slog.String("sid", sid), slog.Any("error", err))
		return incProblem(problem.TypeInternalError, "unbind member failed")
	}

	if h.auditW != nil {
		_ = h.auditW.Write(ctx, &audit.Event{
			EventType: audit.EventIncarnationMemberUnbound,
			Source:    apimiddleware.ScenarioInvocationSource(ctx),
			ArchonAID: claims.Subject,
			Payload: map[string]any{
				"id":      name,
				"sid":     sid,
				"removed": removed,
			},
		})
	}
	return nil
}

// ListMembersTyped — domain function of GET /v1/incarnations/{id}/members
// (READ, no audit). Two-layer authorization, the souls-read pattern (ADR-047 §g):
// the route gate answers "may this Archon read incarnations at all", and the
// roster is narrowed HERE to the hosts within the caller's soul visibility. So a
// scoped operator sees the part of the roster they are entitled to, and does not
// learn the SIDs of the rest.
func (h *IncarnationHandler) ListMembersTyped(ctx context.Context, claims *jwt.Claims, name string) (MemberListPage, error) {
	var zero MemberListPage

	if !incarnation.ValidID(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'id' must match "+incarnation.IDPattern)
	}
	if _, err := h.loadIncarnationForMembership(ctx, name, "list-members"); err != nil {
		return zero, err
	}

	members, err := incarnation.ListMembers(ctx, h.db, name)
	if err != nil {
		h.logger.Error("incarnation.list-members: failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "list members failed")
	}

	// Fail-closed on a missing resolver, symmetric to the bind path: an
	// unevaluatable boundary yields nothing, never the whole roster.
	if h.scoper == nil {
		h.logger.Error("incarnation.list-members: scoper not configured")
		return MemberListPage{Items: []MemberView{}}, nil
	}
	scope := soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))

	items := make([]MemberView, 0, len(members))
	for _, m := range members {
		if !soulpurview.InScope(scope, m.SID, m.Covens, soulpurview.TraitsFromJSON(m.TraitsRaw)) {
			continue
		}
		items = append(items, MemberView{
			SID:        m.SID,
			Status:     m.Status,
			BoundAt:    m.BoundAt,
			BoundByAID: m.BoundByAID,
		})
	}
	return MemberListPage{Items: items}, nil
}

// loadIncarnationForMembership resolves the incarnation of a membership route,
// mapping absence to 404. Membership is deliberately NOT gated on the
// incarnation's status: binding a roster onto an `error_locked` instance is how
// an operator repairs one, and blocking it would leave no way out.
func (h *IncarnationHandler) loadIncarnationForMembership(ctx context.Context, name, op string) (*incarnation.Incarnation, error) {
	inc, err := incarnation.SelectByID(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return nil, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation."+op+": select failed",
			slog.String("name", name), slog.Any("error", err))
		return nil, incProblem(problem.TypeInternalError, "select incarnation failed")
	}
	return inc, nil
}

// bindRejectionProblem maps a domain screening result onto the wire, one bucket
// per call and in a fixed order: unknown (422) → out-of-scope (403) →
// not-connected (422). Order is a security property, not cosmetics — an operator
// who may not see a host must be told "forbidden", never that it is disconnected.
// A clean screening (nil / empty) yields nil.
func bindRejectionProblem(rej *incarnation.BindRejection) error {
	if rej.Empty() {
		return nil
	}
	if len(rej.UnknownSIDs) > 0 {
		return incProblem(problem.TypeValidationFailed,
			"unknown SID(s) (not in the soul registry): "+strings.Join(rej.UnknownSIDs, ", "))
	}
	if len(rej.OutOfScope) > 0 {
		return incProblem(problem.TypeForbidden,
			"SID(s) outside the operator's soul scope: "+strings.Join(rej.OutOfScope, ", "))
	}
	return incProblem(problem.TypeValidationFailed,
		"SID(s) not connected — only an onboarded, connected host can be bound: "+strings.Join(rej.NotConnected, ", "))
}

// dedupeSorted returns the unique entries of xs, sorted.
func dedupeSorted(xs []string) []string {
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// diffSorted returns the entries of all that are not in sub (both sorted inputs
// are not required; the result is sorted).
func diffSorted(all, sub []string) []string {
	in := make(map[string]struct{}, len(sub))
	for _, s := range sub {
		in[s] = struct{}{}
	}
	var out []string
	for _, s := range all {
		if _, ok := in[s]; !ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
