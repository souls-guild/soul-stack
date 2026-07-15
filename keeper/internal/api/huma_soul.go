package api

// Registration and spec-dump of the SOUL domain on huma full-typed (ROLLOUT BATCH 2e following
// the role/operator + audit-endpoint reference implementations, ADR-054 §Pattern). create/coven-assign/
// issue-token/ssh-target — WRITE+AUDIT (variant B, huma-audit-middleware; events soul.created/.coven-
// changed/.token-issued/.ssh-target.updated); list/get/soulprint/history — read (NO audit).
// Domain *Typed functions (handlers/soul.go) are extracted from (w,r); the old (w,r) is a thin
// strict wrapper (MCP soul-tools call soul.Service/bootstraptoken directly, bypassing the handler —
// the extraction does not affect them; see keeper/internal/mcp/soul_*.go).
//
// POST /v1/souls/{sid}/exec (ErrandExec) — WRITE+AUDIT (errand.invoked) with dual-status
// 200/202 + Location header. Handler — *handlers.ErrandHandler (ExecTyped), mounted
// on the same /souls group with RBAC errand.run + ErrandSIDSelector.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaSoulCreate mounts POST /v1/souls via huma (WRITE+AUDIT variant B —
// event soul.created). soulH nil → no-op. Handler: claims → convert typed-body → CreateTyped →
// audit-payload on huma-ctx → 201 WITH BODY.
func registerHumaSoulCreate(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulCreateOperation(), func(ctx context.Context, in *soulCreateInput) (*soulCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := soulH.CreateTyped(ctx, claims, handlers.NewSoulCreateRequest(in.Body.SID, in.Body.Transport, in.Body.Covens, in.Body.Note))
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &soulCreateOutput{Status: 201, Body: newSoulCreateReply(reply.Body)}, nil
	})
}

// registerHumaSoulCovenAssign mounts POST /v1/souls/coven via huma (WRITE+AUDIT variant B —
// event soul.coven-changed). soulH nil → no-op. Handler: claims → convert typed-body →
// AssignCovenTyped → audit-payload → 200 WITH BODY (custom MarshalJSON XOR label↔labels).
func registerHumaSoulCovenAssign(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulCovenAssignOperation(), func(ctx context.Context, in *soulCovenAssignInput) (*soulCovenAssignOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := soulH.AssignCovenTyped(ctx, claims, toSoulCovenAssignInput(in.Body), in.DryRun)
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload))
		return &soulCovenAssignOutput{Status: 200, Body: reply.Body}, nil
	})
}

// registerHumaSoulTraitsAssign mounts POST /v1/souls/traits via huma (WRITE+AUDIT variant B —
// event soul.traits-changed). soulH nil → no-op. Handler: claims → convert typed-body →
// AssignTraitsTyped → audit-payload → 200 WITH BODY.
func registerHumaSoulTraitsAssign(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulTraitsAssignOperation(), func(ctx context.Context, in *soulTraitsAssignInput) (*soulTraitsAssignOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := soulH.AssignTraitsTyped(ctx, claims, toSoulTraitsAssignInput(in.Body), in.DryRun)
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload))
		return &soulTraitsAssignOutput{Status: 200, Body: reply.Body}, nil
	})
}

// registerHumaSoulIssueToken mounts POST /v1/souls/{sid}/issue-token via huma (WRITE+AUDIT
// variant B — event soul.token-issued). soulH nil → no-op. Handler: claims → IssueTokenTyped →
// audit-payload → 200 WITH BODY (jwt; parity with operator issue-token).
func registerHumaSoulIssueToken(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulIssueTokenOperation(), func(ctx context.Context, in *soulIssueTokenInput) (*soulIssueTokenOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := soulH.IssueTokenTyped(ctx, claims, in.SID, in.Force)
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &soulIssueTokenOutput{Status: 200, Body: newSoulIssueTokenReply(reply.Body)}, nil
	})
}

// registerHumaSoulSshTarget mounts PUT /v1/souls/{sid}/ssh-target via huma (WRITE+AUDIT
// variant B — event soul.ssh-target.updated). soulH nil → no-op. Handler: convert typed-body →
// UpdateSshTargetTyped → audit-payload → 200 WITH BODY (snapshot).
func registerHumaSoulSshTarget(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulSshTargetOperation(), func(ctx context.Context, in *soulSshTargetInput) (*soulSshTargetOutput, error) {
		reply, err := soulH.UpdateSshTargetTyped(ctx, in.SID, toSoulSshTargetInput(in.Body))
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload))
		return &soulSshTargetOutput{Status: 200, Body: newSoulSshTargetReply(reply.Body)}, nil
	})
}

// registerHumaSoulExec mounts POST /v1/souls/{sid}/exec via huma (WRITE+AUDIT variant B —
// event errand.invoked). errandH nil → no-op (parity with the router mount under `if errandH != nil`).
// Handler: claims → convert typed-body → ExecTyped → audit-payload on huma-ctx (on BOTH
// the 200/202 branches) → 202 WITH BODY Accepted + Location (async) or 200 WITH BODY Result (sync).
// Body is pre-marshaled into json.RawMessage (the errand GET shape). The dispatcher also writes its own
// audit-event source=api inside Dispatch (single source of truth); the middleware-event here is a
// security navigation-trail (parity with cancel / push.apply — the duplication is intentional).
func registerHumaSoulExec(humaAPI huma.API, errandH *handlers.ErrandHandler) {
	if errandH == nil {
		return
	}
	huma.Register(humaAPI, errandExecOperation(), func(ctx context.Context, in *errandExecInput) (*errandExecOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := errandH.ExecTyped(ctx, claims, in.SID, toErrandExecRequest(in.Body))
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		if reply.Async {
			// 202 body — native ErrandAccepted (errand domain): the handler-view WITHOUT json tags
			// cannot be marshaled directly (it would produce UpperCamel keys). The projection is byte-exact
			// with the previous ErrandAccepted (errand_id/status).
			body, merr := json.Marshal(newErrandAccepted(reply.Accepted))
			if merr != nil {
				return nil, soulProblem(merr)
			}
			return &errandExecOutput{Status: 202, Location: "/v1/errands/" + reply.ErrandID, Body: body}, nil
		}
		// 200 body — native ErrandResult (errand domain): see above (the view → native
		// wire-DTO projection gives byte-exact json tags/omitempty/field order).
		body, merr := json.Marshal(newErrandResult(reply.Result))
		if merr != nil {
			return nil, soulProblem(merr)
		}
		return &errandExecOutput{Status: 200, Body: body}, nil
	})
}

// registerHumaSoulList mounts GET /v1/souls via huma (READ-with-typed-query, NO audit).
// soulH nil → no-op. Pagination is parsed by ParsePageWithCursor over the same query values
// that huma binds (offset+cursor conflict → 422, malformed cursor → 400, out-of-range → 400) —
// a single source-of-truth with (w,r). RBAC soul.list — on the group.
func registerHumaSoulList(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulListOperation(), func(ctx context.Context, in *soulListInput) (*soulListOutput, error) {
		page, cursor, perr := soulParsePage(int(in.Offset), int(in.Limit), in.Cursor)
		if perr != nil {
			return nil, perr
		}
		reply, err := soulH.ListTyped(ctx, claimsOrNil(ctx), handlers.SoulListInput{
			Coven:     in.Coven,
			Status:    in.Status,
			Transport: in.Transport,
			Page:      page,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, soulProblem(err)
		}
		items := make([]SoulListEntry, len(reply.Items))
		for i := range reply.Items {
			items[i] = newSoulListEntry(reply.Items[i])
		}
		out := soulListReply{
			Items:      items,
			Offset:     int32(reply.Offset),
			Limit:      int32(reply.Limit),
			Total:      int32(reply.Total),
			NextCursor: reply.NextCursor,
		}
		// total_approximate — omitempty in both forms: keyset mode yields true →
		// *bool(&true) (key present), offset mode yields false → nil (key omitted,
		// byte-exact with PagedResponse.TotalApproximate `bool omitempty`).
		if reply.TotalApproximate {
			ta := true
			out.TotalApproximate = &ta
		}
		return &soulListOutput{Body: out}, nil
	})
}

// registerHumaSoulStats mounts GET /v1/souls/stats via huma (READ aggregate, NO audit).
// soulH nil → no-op. staleFn returns the current disconnect threshold
// (reaper.ResolveMarkDisconnectedStale over the live config — hot-reload) for
// stale_count; nil → default [defaultSoulStatsStale] (spec-dump / tests without wire-up).
// RBAC soul.list — on the group (the same as list/get).
func registerHumaSoulStats(humaAPI huma.API, soulH *handlers.SoulHandler, staleFn func() time.Duration) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulStatsOperation(), func(ctx context.Context, _ *soulStatsInput) (*soulStatsOutput, error) {
		stale := defaultSoulStatsStale
		if staleFn != nil {
			if d := staleFn(); d > 0 {
				stale = d
			}
		}
		reply, err := soulH.StatsTyped(ctx, claimsOrNil(ctx), stale)
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulStatsOutput{Body: newSoulStatsReply(reply.Body)}, nil
	})
}

// defaultSoulStatsStale — fallback threshold for stale_count when staleFn is not set
// (spec-dump / unit tests). 90s — parity with reaper.defaultMarkDisconnectedStale;
// production wire-up passes a provider over the live config (hot-reload).
const defaultSoulStatsStale = 90 * time.Second

// registerHumaSoulGet mounts GET /v1/souls/{sid} via huma (READ-with-path, NO audit).
func registerHumaSoulGet(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulGetOperation(), func(ctx context.Context, in *soulGetInput) (*soulGetOutput, error) {
		reply, err := soulH.GetTyped(ctx, claimsOrNil(ctx), in.SID)
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulGetOutput{Body: newSoulListEntry(reply)}, nil
	})
}

// registerHumaSoulSoulprint mounts GET /v1/souls/{sid}/soulprint via huma (READ-with-path,
// NO audit).
func registerHumaSoulSoulprint(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulSoulprintOperation(), func(ctx context.Context, in *soulSoulprintInput) (*soulSoulprintOutput, error) {
		reply, err := soulH.GetSoulprintTyped(ctx, claimsOrNil(ctx), in.SID)
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulSoulprintOutput{Body: reply}, nil
	})
}

// registerHumaSoulHistory mounts GET /v1/souls/{sid}/history via huma (READ-with-typed-
// query, NO audit). CheckPageBounds → 400 (the range is enforced by the DOMAIN).
func registerHumaSoulHistory(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulHistoryOperation(), func(ctx context.Context, in *soulHistoryInput) (*soulHistoryOutput, error) {
		reply, err := soulH.HistoryTyped(ctx, claimsOrNil(ctx), handlers.SoulHistoryInput{
			SID:    in.SID,
			Types:  in.Types,
			Since:  in.Since,
			Offset: int(in.Offset),
			Limit:  int(in.Limit),
		})
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulHistoryOutput{Body: newSoulHistoryReply(reply)}, nil
	})
}

// soulParsePage replicates ParsePageWithCursor over the already huma-bound offset/limit/
// cursor: out-of-range → 400 (CheckPageBounds), malformed cursor → 400, cursor+offset>0 →
// 422 (conflict between the two pagination modes). A single contract with the (w,r) wrapper SoulHandler.List.
func soulParsePage(offset, limit int, cursorRaw string) (sharedapi.Page, *sharedapi.KeysetCursor, huma.StatusError) {
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return sharedapi.Page{}, nil, humaProblemError{Details: problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	var cursor *sharedapi.KeysetCursor
	if cursorRaw != "" {
		c, err := sharedapi.DecodeKeysetCursor(cursorRaw)
		if err != nil {
			return sharedapi.Page{}, nil, humaProblemError{Details: problem.New(problem.TypeMalformedRequest, "", err.Error())}
		}
		cursor = &c
	}
	if cursor != nil && offset > 0 {
		return sharedapi.Page{}, nil, humaProblemError{Details: problem.New(problem.TypeValidationFailed, "",
			"cursor and offset are mutually exclusive: use either keyset cursor or offset pagination, not both")}
	}
	return sharedapi.Page{Offset: offset, Limit: limit}, cursor, nil
}

// claimsOrNil extracts claims from ctx (RequireJWT placed them there before huma); none → nil (read handlers
// fail-closed on nil claims via readScopeForClaims → 404/empty list).
func claimsOrNil(ctx context.Context) *keeperjwt.Claims {
	claims, ok := apimiddleware.ClaimsFromContext(ctx)
	if !ok {
		return nil
	}
	return claims
}

// toSoulCovenAssignInput — converts the typed huma body → the NATIVE domain model
// handlers.SoulCovenAssignInput (handler-native §Pattern step 3). The huma value/slice
// form is passed through directly (handler.derefCovenAssign treats empty fields as "not set").
func toSoulCovenAssignInput(b SoulCovenAssignRequest) handlers.SoulCovenAssignInput {
	return handlers.SoulCovenAssignInput{
		Mode:   b.Mode,
		Label:  b.Label,
		Labels: b.Labels,
		DryRun: b.DryRun,
		Selector: handlers.SoulCovenAssignSelectorInput{
			All:         b.Selector.All,
			SIDs:        b.Selector.Sids,
			Coven:       b.Selector.Coven,
			Incarnation: b.Selector.Incarnation,
			Status:      b.Selector.Status,
		},
	}
}

// toSoulTraitsAssignInput — converts the typed huma body → the NATIVE domain model
// handlers.SoulTraitsAssignInput (handler-native §Pattern step 3). The huma map/slice
// form is passed through directly (the handler treats empty fields as "not set", mode "" → merge).
func toSoulTraitsAssignInput(b SoulTraitsAssignRequest) handlers.SoulTraitsAssignInput {
	return handlers.SoulTraitsAssignInput{
		Mode:   b.Mode,
		Traits: b.Traits,
		Keys:   b.Keys,
		DryRun: b.DryRun,
		Selector: handlers.SoulCovenAssignSelectorInput{
			All:         b.Selector.All,
			SIDs:        b.Selector.Sids,
			Coven:       b.Selector.Coven,
			Incarnation: b.Selector.Incarnation,
			Status:      b.Selector.Status,
		},
	}
}

// toSoulSshTargetInput — converts the typed huma body → the NATIVE domain model
// handlers.SoulSshTargetInput. ssh_provider empty → routing falls back to the coven/cluster default.
func toSoulSshTargetInput(b SoulSshTarget) handlers.SoulSshTargetInput {
	return handlers.SoulSshTargetInput{
		SSHPort:     b.SSHPort,
		SSHUser:     b.SSHUser,
		SoulPath:    b.SoulPath,
		SSHProvider: b.SSHProvider,
	}
}

// toErrandExecRequest — converts the typed huma body → the NATIVE errand-domain request
// (handlers.ErrandRunInput). The fields input/timeout_seconds/dry_run are already pointer-optional in
// the huma form (the handler dereferences them); module is a value. A direct pass-through without repacking.
func toErrandExecRequest(b ErrandRunRequest) handlers.ErrandRunInput {
	return handlers.ErrandRunInput{
		Module:         b.Module,
		Input:          b.Input,
		TimeoutSeconds: b.TimeoutSeconds,
		DryRun:         b.DryRun,
	}
}

// soulMissingClaims — a defensive response for missing claims in ctx (unreachable: RequireJWT
// places claims there before huma). problem+json (parity with roleMissingClaims).
func soulMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// soulProblem delivers a *Typed-function error through huma as problem+json. A domain
// *handlers.problemError → humaProblemError; a non-problem error → 500 (parity with roleProblem).
func soulProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSoulAPI assembles a huma.API on top of a chi group with huma-audit-middleware (variant B) for
// the given event type (parity with newHumaRoleAPI). Each write route of soul (create/coven-assign/
// issue-token/ssh-target) is mounted on ITS OWN chi group with its own event type.
func newHumaSoulAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSoulSpecYAML assembles the OpenAPI fragment of ALL huma-migrated soul routes (create/
// coven-assign/issue-token/ssh-target/exec/list/get/soulprint/history) as a YAML string, WITHOUT
// mounting on a real router. A hook for the spec-merge target of the rollout and the guard test.
// Delegates to the generic [humaDumpSpec] through the same register functions. exec is mounted via
// ErrandSpecStub (handler — *handlers.ErrandHandler). Returns a 3.1.0 spec (huma default).
func HumaSoulSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SoulSpecStub()
		registerHumaSoulCreate(api, stub)
		registerHumaSoulCovenAssign(api, stub)
		registerHumaSoulTraitsAssign(api, stub)
		registerHumaSoulIssueToken(api, stub)
		registerHumaSoulSshTarget(api, stub)
		registerHumaSoulExec(api, handlers.ErrandSpecStub())
		registerHumaSoulList(api, stub)
		registerHumaSoulStats(api, stub, nil)
		registerHumaSoulGet(api, stub)
		registerHumaSoulSoulprint(api, stub)
		registerHumaSoulHistory(api, stub)
		return nil
	})
}

// soulSentinel — links the soul package (used in the op-file enum sets through the domain;
// the explicit ref guarantees that an enum↔domain desync is caught by the compiler on edits).
var _ = soul.StatusPending
