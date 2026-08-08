package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulforget"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// SoulPool — the surface over pgxpool.Pool that the Soul handler needs.
//
//   - BeginTx — Create (souls-row + bootstrap-token atomically) and IssueToken
//     (force-flow: expire active + insert new in one transaction).
//   - ExecQueryRower — List (count + select without a transaction, via
//     [soul.SelectAll]); read-only, a transaction is redundant.
//
// `*pgxpool.Pool` satisfies both; unit tests — fake.
type SoulPool interface {
	soul.ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// PurviewResolver — the enforcer read surface for the operator's scope boundary:
// "return [rbac.Purview] (the upper bound of visibility/targeting) for
// (resource, action)". Implemented by [rbac.Enforcer] and [rbac.Holder] (hot-reload-
// aware). Generalizes the former CovenScoper (S0 `(covens, unrestricted)`) to the full
// Purview — one resolver for both souls scope consumers:
//   - `GET /v1/souls` scoped visibility (ADR-047 S3b, via keeper/internal/
//     soulpurview) — coven pushdown into SQL;
//   - bulk coven-assign scope intersection (`POST /v1/souls/coven`) — the coven
//     dimension of Purview yields the former `(covens, unrestricted)` shape [soul.BulkScope].
//
// Symmetric to the least-privilege subset-check in rbac.Service (that one trims the grant
// of rights, this one — the scope of the bulk mutation/visibility).
type PurviewResolver interface {
	ResolvePurview(aid, resource, action string) rbac.Purview
}

// covenScoper is the coven-projection surface of the RBAC resolver
// ([rbac.Enforcer.CovenScope] / [rbac.Holder.CovenScope]). The shared
// [PurviewResolver] only declares ResolvePurview; this narrow interface is
// asserted at the bulk coven/traits-assign call sites to reach the coven
// projection without widening the shared contract (NIM-128, mirrors the MCP
// slice). A resolver that does not implement it → fail-closed 500.
type covenScoper interface {
	CovenScope(aid, resource, action string) ([]string, bool)
}

// traitScoper is the trait-projection surface of the RBAC resolver
// ([rbac.Enforcer.TraitScope] / [rbac.Holder.TraitScope]), asserted at the bulk
// traits-assign call site for gate (b) — the pair being written must lie inside
// the operator's own trait scope. A resolver that does not implement it →
// fail-closed 500.
type traitScoper interface {
	TraitScope(aid, resource, action string) (map[string][]string, bool)
}

// SoulPresence — a narrow surface for the batch check "is the Redis SID lease alive"
// (a live EventStream), needed by the presence overlay of `GET /v1/souls` (ADR-006(a)).
// Symmetric to [topology.SoulLeaseChecker]: the real implementation is a wrapper over
// [keeperredis.SoulsStreamAlive], assembled in cmd/keeper; the production wire-up
// passes the same Redis client as the topology resolver.
//
// presence is the authority for online/offline (ADR-006(a)); the PG column `souls.status`
// is only a "last known" snapshot lazily reconciled by the Reaper, so on the
// reconnect hot-path it does not flip to connected, and the read path must
// derive presence from the lease rather than return a stale snapshot.
//
// Returns the set of SIDs with a live lease. A nil checker (single-instance dev
// without Redis / unit tests) → the overlay is off, the PG status snapshot is returned as-is
// (in single-instance it is coherent with the stream fact by construction, symmetric to
// the reaper and the topology resolver).
type SoulPresence interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// SoulHandler — Soul onboarding endpoints: Create + IssueToken + List +
// AssignCoven (bulk).
//
// On force-reissue the freed token is marked with the system marker
// [bootstraptoken.SystemKIDForceReissue] in `bootstrap_tokens.used_by_kid`
// (see IssueToken). All dependencies are immutable; safe for concurrent use.
type SoulHandler struct {
	pool     SoulPool
	scoper   PurviewResolver
	presence SoulPresence
	teardown soulforget.Teardown
	logger   *slog.Logger
}

// NewSoulHandler creates the handler. scoper — the read surface of the operator's
// scope boundary ([PurviewResolver], the production wire-up passes rbac.Holder).
// Used both by `GET /v1/souls` scoped visibility (ADR-047 S3b) and by bulk
// AssignCoven. nil is allowed only in tests that do not use the List/bulk route:
// List with a nil scoper is fail-closed (an empty list — the safe default, NOT all
// souls), AssignCoven returns 500.
//
// presence — the lease-overlay presence for `GET /v1/souls` (ADR-006(a)): when
// non-nil the `status` field in the List/Get response is derived from the live Redis
// SID lease rather than returned as a stale PG snapshot (see [SoulPresence]). nil →
// the overlay is off (single-instance dev / unit tests), the PG snapshot is returned.
func NewSoulHandler(pool SoulPool, scoper PurviewResolver, presence SoulPresence, logger *slog.Logger) *SoulHandler {
	return NewSoulHandlerWithTeardown(pool, scoper, presence, nil, logger)
}

// NewSoulHandlerWithTeardown is [NewSoulHandler] plus the one dependency only
// `DELETE /v1/souls/{sid}` needs (NIM-386): the release side of forgetting a
// host — closing the EventStream this instance holds for it, telling the other
// instances to close theirs, and purging its per-SID Redis keys.
//
// A separate constructor rather than a fifth parameter on [NewSoulHandler]:
// that one has ~150 call sites, and none of them forget hosts. The daemon uses
// this one; everything else keeps the short form.
//
// teardown nil = single-instance dev / unit tests: there is no second instance
// to notify and the stream dies with the process, so [soulforget.Erase] does
// the PG half alone. In the daemon it is always non-nil — a nil there would
// mean the endpoint deletes rows while leaving live streams and a TTL-less
// heartbeat key behind, which is exactly the failure this dependency exists to
// prevent.
func NewSoulHandlerWithTeardown(pool SoulPool, scoper PurviewResolver, presence SoulPresence, teardown soulforget.Teardown, logger *slog.Logger) *SoulHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SoulHandler{pool: pool, scoper: scoper, presence: presence, teardown: teardown, logger: logger}
}

// SoulSpecStub — a non-empty *SoulHandler stub for generating the huma OpenAPI fragment
// (HumaSoulSpecYAML): on dump the domain handler is not called, but huma.Register requires
// non-nil for its nil check (parity [RoleSpecStub]). pool nil — the handler does not execute
// in spec mode.
func SoulSpecStub() *SoulHandler {
	return &SoulHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// soulCreateRequest — POST /v1/souls body. NOT an alias of [SoulCreateRequest]:
// the domain carries the server-only field `note` (written to souls.note), which is not in
// the OpenAPI schema SoulCreateRequest; a pure alias would drop it (the strict decoder
// would reject `note` as unknown → 400). So it stays a domain type
// (symmetric to soulCovenAssignResponse — an alias is impossible there too).
//
// The `covens` field matches by name the OpenAPI SoulCreateRequest and the response
// (`SoulCreateReply.covens`): the strict decoder rejects unknown fields, so a
// mismatch between the request field name and the schema = 400 on a documented client.
type soulCreateRequest struct {
	SID       string   `json:"sid"`
	Transport string   `json:"transport"`
	Covens    []string `json:"covens,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// NewSoulCreateRequest constructs a domain [soulCreateRequest] from huma-body primitives
// (soulCreateRequest is unexported — the huma route in package api assembles the request via this
// constructor, parity with the thin converter §Pattern step 3).
func NewSoulCreateRequest(sid, transport string, covens []string, note string) soulCreateRequest {
	return soulCreateRequest{SID: sid, Transport: transport, Covens: covens, Note: note}
}

// SoulCreateView — the FLAT domain projection of the 201 body of POST /v1/souls (handler-native
// T5d). Package api projects it into the native schema SoulCreateReply. status/transport — the domain's
// RAW string (the native type in api holds the enum form). Covens — a non-nil slice (`&covens`
// after coalesceCoven → the key is always present). BootstrapToken/ExpiresAt — pointer-optional
// (present only for transport=agent; nil → key omitted by the native type). date-time —
// SECOND-precision wire (.UTC().Truncate(time.Second), parity with the legacy RFC3339 `.Format`).
type SoulCreateView struct {
	SID            string
	Transport      string
	Status         string
	Covens         []string
	RegisteredAt   time.Time
	CreatedByAID   string
	BootstrapToken *string
	ExpiresAt      *time.Time
}

// soulCreateView builds the domain projection [SoulCreateView] from a registry record.
func soulCreateView(s *soul.Soul, createdByAID string) SoulCreateView {
	return SoulCreateView{
		SID:          s.SID,
		Transport:    string(s.Transport),
		Status:       string(s.Status),
		Covens:       coalesceCoven(s.Coven),
		RegisteredAt: s.RegisteredAt.UTC().Truncate(time.Second),
		CreatedByAID: createdByAID,
	}
}

// SoulCreateReply — the result of [SoulHandler.CreateTyped] (handler-native). Carries the domain
// projection of the 201 body (SoulCreateView) + audit fields. bootstrap_token is present only for
// transport=agent.
type SoulCreateReply struct {
	Body         SoulCreateView
	SID          string
	Transport    string
	Covens       []string
	CreatedByAID string
	TokenIssued  bool
}

// AuditPayload — audit fields of the 201 Create (parity with the legacy SetAuditPayload; the bootstrap
// token itself is not written).
func (r SoulCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"sid":            r.SID,
		"transport":      r.Transport,
		"covens":         r.Covens,
		"created_by_aid": r.CreatedByAID,
		"token_issued":   r.TokenIssued,
	}
}

// CreateTyped — the extracted domain function of POST /v1/souls (FULL-TYPED rollout ADR-054
// §Pattern): onboarding of the souls row (+ a bootstrap token for agent) without the http boundary. req is the already
// decoded body. Errors — *problemError (422 invalid sid/transport/coven; 409
// soul-exists; 500 PG failure); success — [SoulCreateReply] (201 body + audit fields).
func (h *SoulHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req soulCreateRequest) (SoulCreateReply, error) {
	var zero SoulCreateReply
	if req.SID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'sid' is required")}
	}
	if !soul.ValidSID(req.SID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'sid' must match "+soul.SIDPattern)}
	}
	transport, ok := parseTransport(req.Transport)
	if !ok {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'transport' is required and must be one of: agent, ssh")}
	}
	for _, label := range req.Covens {
		if !soul.ValidCoven(label) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"coven label "+label+" must match "+soul.CovenPattern)}
		}
	}

	creator := claims.Subject
	s := &soul.Soul{
		SID:          req.SID,
		Transport:    transport,
		Status:       soul.StatusPending,
		Coven:        req.Covens,
		CreatedByAID: &creator,
		Note:         req.Note,
	}

	issueToken := transport == soul.TransportAgent

	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("soul.create: begin tx failed", slog.String("sid", req.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	if err := soul.Insert(ctx, tx, s); err != nil {
		if errors.Is(err, soul.ErrSoulAlreadyExists) {
			return zero, &problemError{problem.New(problem.TypeSoulExists, "", "soul "+req.SID+" already exists")}
		}
		if errors.Is(err, soul.ErrSoulCreatorNotFound) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"creator AID "+creator+" not found in operators registry")}
		}
		h.logger.Error("soul.create: insert failed",
			slog.String("sid", req.SID), slog.String("by_aid", creator), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
	}

	resp := soulCreateView(s, creator)
	if issueToken {
		plain, err := bootstraptoken.Generate()
		if err != nil {
			h.logger.Error("soul.create: token generate failed", slog.String("sid", req.SID), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
		}
		rec, err := bootstraptoken.Insert(ctx, tx, req.SID, plain.Hash(), bootstraptoken.DefaultTokenTTL, &creator)
		if err != nil {
			h.logger.Error("soul.create: token insert failed", slog.String("sid", req.SID), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
		}
		token := plain.Reveal()
		expiresAt := rec.ExpiresAt.UTC().Truncate(time.Second)
		resp.BootstrapToken = &token
		resp.ExpiresAt = &expiresAt
	}

	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("soul.create: commit failed", slog.String("sid", req.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
	}
	committed = true

	return SoulCreateReply{
		Body:         resp,
		SID:          s.SID,
		Transport:    string(s.Transport),
		Covens:       coalesceCoven(s.Coven),
		CreatedByAID: creator,
		TokenIssued:  issueToken,
	}, nil
}

// SoulIssueTokenView — the FLAT domain projection of the 200 body of POST /v1/souls/{sid}/issue-token
// (handler-native T5d). Package api projects it into the native schema SoulIssueTokenReply. All fields
// required; expires_at — SECOND-precision wire (.UTC().Truncate(time.Second), parity with legacy).
// bootstrap_token is returned once (SENSITIVE; secret-mask strips it from logs).
type SoulIssueTokenView struct {
	SID            string
	BootstrapToken string
	ExpiresAt      time.Time
}

// soulIssueTokenView builds the domain projection [SoulIssueTokenView].
func soulIssueTokenView(sid, token string, expiresAt time.Time) SoulIssueTokenView {
	return SoulIssueTokenView{
		SID:            sid,
		BootstrapToken: token,
		ExpiresAt:      expiresAt.UTC().Truncate(time.Second),
	}
}

// SoulIssueTokenReply — the result of [SoulHandler.IssueTokenTyped] (handler-native). Carries the
// domain projection of the 200 body (SoulIssueTokenView: sid/bootstrap_token/expires_at) + audit
// fields. bootstrap_token is returned once (SENSITIVE).
type SoulIssueTokenReply struct {
	Body            SoulIssueTokenView
	SID             string
	Force           bool
	ExpiredPrevious bool
	ExpiresAtRFC    string
}

// AuditPayload — audit fields of the 200 IssueToken. Keys WITHOUT a `token` substring (the audit secret-mask
// redacts any key containing `token` to `***MASKED***`); the token itself is not written.
func (r SoulIssueTokenReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"sid":              r.SID,
		"force":            r.Force,
		"expired_previous": r.ExpiredPrevious,
		"expires_at":       r.ExpiresAtRFC,
	}
}

// IssueTokenTyped — the extracted domain function of POST /v1/souls/{sid}/issue-token
// (FULL-TYPED): reissuing a bootstrap token (transport=agent) without the http boundary.
// Errors — *problemError (422 invalid sid / transport=ssh; 404 no soul; 409 an active
// token without force; 500 PG failure); success — [SoulIssueTokenReply] (200 body + audit fields).
func (h *SoulHandler) IssueTokenTyped(ctx context.Context, claims *jwt.Claims, sid string, force bool) (SoulIssueTokenReply, error) {
	var zero SoulIssueTokenReply
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}

	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("soul.issue-token: begin tx failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	s, err := soul.SelectBySID(ctx, tx, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.issue-token: select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	if s.Transport != soul.TransportAgent {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"soul "+sid+" has transport "+string(s.Transport)+"; bootstrap tokens are only issued for transport=agent")}
	}

	creator := claims.Subject
	var expiredPrevious bool
	if force {
		_, expired, err := bootstraptoken.ExpireActiveBySID(ctx, tx, sid, bootstraptoken.SystemKIDForceReissue)
		if err != nil {
			h.logger.Error("soul.issue-token: expire active failed", slog.String("sid", sid), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
		}
		expiredPrevious = expired
	}

	plain, err := bootstraptoken.Generate()
	if err != nil {
		h.logger.Error("soul.issue-token: generate failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	rec, err := bootstraptoken.Insert(ctx, tx, sid, plain.Hash(), bootstraptoken.DefaultTokenTTL, &creator)
	if err != nil {
		if errors.Is(err, bootstraptoken.ErrTokenActiveExists) {
			return zero, &problemError{problem.New(problem.TypeBootstrapTokenActive, "",
				"soul "+sid+" already has an active bootstrap token; pass ?force=true to expire it and reissue")}
		}
		h.logger.Error("soul.issue-token: insert failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}

	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("soul.issue-token: commit failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	committed = true

	return SoulIssueTokenReply{
		Body:            soulIssueTokenView(sid, plain.Reveal(), rec.ExpiresAt),
		SID:             sid,
		Force:           force,
		ExpiredPrevious: expiredPrevious,
		ExpiresAtRFC:    rec.ExpiresAt.UTC().Format(time.RFC3339),
	}, nil
}

// SoulForgetView — the FLAT domain projection of the 200 body of
// DELETE /v1/souls/{sid} (handler-native T5d). Package api projects it into the
// native schema SoulForgetReply.
//
// The endpoint deliberately does NOT answer 204. Forgetting a host fires four
// ON DELETE CASCADE edges, and two of them reach other operators' objects —
// incarnation rosters and Choir Voices. An empty body would let a host be
// erased out of three rosters with nothing on screen to say so. Every field
// here is measured inside the delete transaction, never inferred.
//
// The release half is reported next to the counts, not folded into them: a
// forgotten host that could not be released is NOT a plain success, and
// `warnings` is where that shows up.
type SoulForgetView struct {
	SID                string
	StatusBefore       string
	SeedsRevoked       int64
	BootstrapsBurned   int64
	MembershipsSevered int64
	ChoirVoicesRemoved int64
	LocalStreamClosed  bool
	Broadcast          bool
	CacheKeysPurged    int64
	Warnings           []string
}

// soulForgetView builds the domain projection [SoulForgetView]. warnings nil →
// `[]`: the field is non-nullable on the wire, and "no warnings" must read as
// an empty list rather than as a missing field a client may skip.
func soulForgetView(sid string, res soulforget.Result) SoulForgetView {
	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	return SoulForgetView{
		SID:                sid,
		StatusBefore:       res.StatusBefore,
		SeedsRevoked:       res.SeedsRevoked,
		BootstrapsBurned:   res.BootstrapsBurned,
		MembershipsSevered: res.MembershipsSevered,
		ChoirVoicesRemoved: res.ChoirVoicesRemoved,
		LocalStreamClosed:  res.LocalStreamClosed,
		Broadcast:          res.Broadcast,
		CacheKeysPurged:    res.CacheKeysPurged,
		Warnings:           warnings,
	}
}

// SoulForgetReply — the result of [SoulHandler.ForgetTyped] (handler-native):
// the domain projection of the 200 body + the audit fields.
type SoulForgetReply struct {
	Body SoulForgetView
}

// AuditPayload — audit fields of the 200 Forget.
//
// This payload matters more than most: the rows it describes are gone by the
// time it is written, so `soul.forgotten` is the ONLY durable record that the
// host, its seeds, its memberships and its Voices ever existed. It therefore
// carries the full count set and the release outcome, including `warnings` —
// an operator reading the audit trail must be able to tell "forgotten and
// released" from "forgotten, and something was left holding on".
func (r SoulForgetReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"sid":                  r.Body.SID,
		"status_before":        r.Body.StatusBefore,
		"seeds_revoked":        r.Body.SeedsRevoked,
		"bootstraps_burned":    r.Body.BootstrapsBurned,
		"memberships_severed":  r.Body.MembershipsSevered,
		"choir_voices_removed": r.Body.ChoirVoicesRemoved,
		"local_stream_closed":  r.Body.LocalStreamClosed,
		"broadcast":            r.Body.Broadcast,
		"cache_keys_purged":    r.Body.CacheKeysPurged,
		"warnings":             r.Body.Warnings,
	}
}

// forgetReason names the operator in `soul_seeds.revocation_reason`. The reason
// is written inside the same transaction that deletes the seed rows, so this
// string and the audit event are the only places the act survives — a later
// "who revoked this fingerprint" has nothing else to read.
//
// The nil fallback is unreachable through the endpoint (nil claims fail the
// scope gate above with 404) and exists so a caller can never reach
// [soulforget.Forget]'s empty-reason rejection, which would surface as an
// opaque 500 on an operation that had already been authorized.
func forgetReason(claims *jwt.Claims) string {
	if claims == nil || claims.Subject == "" {
		return "forgotten by an unidentified operator"
	}
	return "forgotten by " + claims.Subject
}

// ForgetTyped — the extracted domain function of DELETE /v1/souls/{sid}
// (FULL-TYPED, NIM-386): erase a host from the registry and release what it
// held, without the http boundary.
//
// Authorization is the route's `RequirePermission(soul, forget,
// SoulSIDSelector)` and nothing else — the same shape as issue-token and
// ssh-target-update, whose scope context is likewise the SID in the path. It
// deliberately does NOT also apply the read path's [SoulHandler.inScope] gate:
// that one resolves the Purview of `soul.list`, so reusing it would make
// `soul.forget` silently unusable for any operator whose list-scope happens not
// to cover the host — a cross-permission dependency nothing in the catalog
// declares. There is no existence leak in leaving it out: an operator without
// `soul.forget` on this host is stopped by the route with 403 before the
// handler runs, and one who has it is entitled to learn whether the host
// exists.
//
// The state of the host is NOT a gate. A host may be forgotten while
// `connected` (the call tears its stream down), while `pending` (its unburnt
// bootstrap token is burned), or in a state this cluster has never observed.
// The safety property is not "only dead hosts may be erased" — it is that a
// forgotten host cannot come back: seed auth is an allowlist over
// `soul_seeds.fingerprint` and the seeds cascade away with the row.
//
// There is no `force`. A single verb cannot quietly degrade from "release the
// host" to "drop its row": if the release cannot happen, the caller gets 503
// with nothing deleted ([soulforget.ErrTeardownUnavailable]), and if it only
// partly happened the reply says which resource is still held.
//
// Errors — *problemError (422 invalid sid; 404 no soul / out of scope; 503 the
// cluster could not be told, nothing deleted; 500 PG failure); success —
// [SoulForgetReply] (200 body + audit fields).
func (h *SoulHandler) ForgetTyped(ctx context.Context, claims *jwt.Claims, sid string) (SoulForgetReply, error) {
	var zero SoulForgetReply
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}

	// No pre-fetch of the row: [soulforget.Forget] locks and reads it inside the
	// transaction that deletes it, so a separate existence check would only add
	// a window in which the answer could change.
	res, err := soulforget.Erase(ctx, h.pool, h.teardown, sid, forgetReason(claims))
	if err != nil {
		switch {
		case errors.Is(err, soul.ErrSoulNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		case errors.Is(err, soulforget.ErrTeardownUnavailable):
			h.logger.Error("soul.forget: cluster teardown notice failed, nothing deleted",
				slog.String("sid", sid), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeTeardownUnavailable, "",
				"soul "+sid+" was NOT forgotten: the cluster-wide teardown notice could not be sent, "+
					"so a stream on another Keeper instance could not be closed; nothing was deleted, retry once Redis is reachable")}
		default:
			h.logger.Error("soul.forget: erase failed", slog.String("sid", sid), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "forget soul failed")}
		}
	}

	// Two log lines, not one, and both at Info: forgetting is irreversible, and
	// the cascade counts are the part an operator goes looking for afterwards.
	h.logger.Info("soul.forget: host forgotten",
		slog.String("sid", sid),
		slog.String("status_before", res.StatusBefore),
		slog.Int64("seeds_revoked", res.SeedsRevoked),
		slog.Int64("bootstraps_burned", res.BootstrapsBurned),
		slog.Int64("memberships_severed", res.MembershipsSevered),
		slog.Int64("choir_voices_removed", res.ChoirVoicesRemoved),
		slog.Bool("local_stream_closed", res.LocalStreamClosed),
		slog.Int64("cache_keys_purged", res.CacheKeysPurged))
	for _, w := range res.Warnings {
		h.logger.Warn("soul.forget: resource not released", slog.String("sid", sid), slog.String("warning", w))
	}

	return SoulForgetReply{Body: soulForgetView(sid, res)}, nil
}

// SoulListView — the FLAT domain projection of one `souls` registry row (handler-native
// T5d): the shared element of the `GET /v1/souls` list AND the get-Body of `GET /v1/souls/{sid}`. Package api
// projects it into the native schema SoulListEntry. A registry projection; the SoulSeed fingerprint and
// any secrets are deliberately NOT included. status/transport — the domain's RAW string (the native type
// in api holds the enum form). covens — a non-nullable slice (nil → `[]` via coalesceCoven).
// LastSeenAt/LastSeenByKid/RequestedAt/CreatedByAID — pointer-nullable (the native type without
// omitempty → `null` on nil). date-time — NANOSECOND wire (bare UTC, no Truncate).
type SoulListView struct {
	SID           string
	Transport     string
	Status        string
	Covens        []string
	Traits        map[string]any
	LastSeenAt    *time.Time
	LastSeenByKid *string
	RegisteredAt  time.Time
	RequestedAt   *time.Time
	CreatedByAID  *string
}

// toSoulListView projects [soul.Soul] into the domain [SoulListView]. date-time — `.UTC()`
// WITHOUT Truncate (pgx already returns UTC; truncation would break byte-for-byte). covens nil → `[]`.
func toSoulListView(s *soul.Soul) SoulListView {
	item := SoulListView{
		SID:           s.SID,
		Transport:     string(s.Transport),
		Status:        string(s.Status),
		Covens:        coalesceCoven(s.Coven),
		Traits:        coalesceTraits(s.Traits),
		LastSeenByKid: s.LastSeenByKID,
		RegisteredAt:  s.RegisteredAt.UTC(),
		CreatedByAID:  s.CreatedByAID,
	}
	if s.LastSeenAt != nil {
		t := s.LastSeenAt.UTC()
		item.LastSeenAt = &t
	}
	if s.RequestedAt != nil {
		t := s.RequestedAt.UTC()
		item.RequestedAt = &t
	}
	return item
}

// overlayPresence derives the response's `status` field from the live Redis SID lease
// (ADR-006(a)): the PG column `souls.status` is a "last known" snapshot lazily reconciled
// by the Reaper; it does NOT flip to connected on the reconnect hot-path,
// so the read path must determine presence from the lease, otherwise a reconnected
// Soul stays `disconnected` until the next Reaper tick (a live bug). Symmetric to
// [topology.Resolver.filterAlive] — a single presence source for all consumers.
//
// The overlay touches ONLY presence-snapshot statuses (`connected`/`disconnected`):
//   - lease alive → `connected`;
//   - lease dead  → `disconnected`.
//
// Lifecycle statuses (`pending`/`revoked`/`expired`/`destroyed`) are NOT presence but
// onboarding/terminal state; they have no lease semantics and are returned as-is
// (the same exception as in topology.rosterSQL).
//
// presence==nil (single-instance dev / unit) → no-op: the PG snapshot is returned as-is.
// A Redis-check error → fail-safe: warn + PG snapshot without overlay (a Redis network
// failure must not skew the whole list one way).
func (h *SoulHandler) overlayPresence(ctx context.Context, items []SoulListView) {
	if h.presence == nil || len(items) == 0 {
		return
	}
	sids := make([]string, 0, len(items))
	for i := range items {
		if presenceSnapshotStatus(items[i].Status) {
			sids = append(sids, items[i].SID)
		}
	}
	if len(sids) == 0 {
		return
	}
	alive, err := h.presence.SoulsStreamAlive(ctx, sids)
	if err != nil {
		h.logger.Warn("soul.presence: lease check failed — returning PG snapshot (fail-safe)",
			slog.Any("error", err))
		return
	}
	for i := range items {
		if !presenceSnapshotStatus(items[i].Status) {
			continue
		}
		if _, ok := alive[items[i].SID]; ok {
			items[i].Status = string(soul.StatusConnected)
		} else {
			items[i].Status = string(soul.StatusDisconnected)
		}
	}
}

// presenceSnapshotStatus — a status whose semantics = a presence snapshot (online/offline),
// and thus participates in the lease overlay. Lifecycle statuses are excluded (see overlayPresence).
func presenceSnapshotStatus(status string) bool {
	switch soul.Status(status) {
	case soul.StatusConnected, soul.StatusDisconnected:
		return true
	}
	return false
}

// GET /v1/souls (ListTyped) — visibility scoped by RBAC (ADR-047 S3b): an operator sees only
// hosts within their scope boundary (`soul.list` Purview). The scope is transparent to the client —
// derived from the JWT, NOT a query parameter; the coven query (filter) narrows WITHIN the scope (AND).
// ONE mode since NIM-128: the whole boolean purview (coven/host/trait) pushes into SQL, so this is
// plain offset pagination with an exact total (see [SoulHandler.listOffsetTyped]) — next_cursor stays
// null and total_approximate false, both kept on the wire for envelope compatibility. fail-closed
// (OPPOSITE to presence fail-SAFE): no claims / nil-scoper / empty Purview → an EMPTY list
// (not all souls).

// SoulListInput — parameters of [SoulHandler.ListTyped] (FULL-TYPED). Status/Transport/
// SIDPrefix — string filters (empty = do not apply); Covens — any-of (empty = do not
// apply); Unassigned — membership filter. Page/Cursor — already parsed by the huma layer
// (offset+cursor conflict and a broken cursor are resolved BEFORE ListTyped).
type SoulListInput struct {
	// Covens matches hosts carrying ANY of these labels (repeatable `?coven=`). Plural
	// because the create-form roster picker asks for hosts across the SET of covens the
	// incarnation declares (NIM-371).
	Covens    []string
	Status    string
	Transport string
	// Unassigned narrows to hosts belonging to no incarnation — the "free souls" view
	// the roster picker offers (NIM-371).
	Unassigned bool
	// SIDPrefix narrows to SIDs with this prefix (autocomplete).
	SIDPrefix string
	Page      sharedapi.Page
	Cursor    *sharedapi.KeysetCursor
}

// SoulListReply — the wire type of the GET /v1/souls response (paged-envelope SoulListView). An alias of
// sharedapi.PagedResponse[SoulListView] — the single shape of offset and keyset modes (CURSOR,
// 6 fields). Package api projects it into the native envelope soulListReply via RegisterTypeAlias
// (handler-native T5d: the element schema SoulListView collapses onto the contractual SoulListEntry).
type SoulListReply = sharedapi.PagedResponse[SoulListView]

// SoulStatsView — the flat domain projection of the 200 body of GET /v1/souls/stats (Souls
// Overview UI). Maps "axis value → host count"; keys — the domain's RAW string
// (status/transport in domain form). Transport — agent/ssh (NOT pull/push):
// the UI maps to pull/push labels itself. Empty axes → empty (not nil) maps, so the
// wire always carries an object (not null).
type SoulStatsView struct {
	ByStatus    map[string]int
	ByTransport map[string]int
	ByCoven     map[string]int
	Total       int
	StaleCount  int
}

// SoulStatsReply — the result of [SoulHandler.StatsTyped]. Carries the domain projection
// of the aggregate; package api projects it into a native wire-DTO.
type SoulStatsReply struct {
	Body SoulStatsView
}

// StatsTyped — the domain function of GET /v1/souls/stats: an aggregate of the souls registry
// within the operator's Purview scope (the same fail-closed scope resolution as
// ListTyped). staleThreshold — the cutoff for a "stale" last_seen_at, coming from
// reaper.ResolveMarkDisconnectedStale (the register layer reads the current config),
// so stale_count matches the Reaper's disconnect threshold.
//
// fail-closed (symmetric to ListTyped): no claims / nil-scoper / empty Purview →
// a ZERO aggregate (200, not 403) — we do not leak the existence of hosts outside the scope.
// Errors — *problemError (500 PG). Since NIM-128 the aggregate carries the SAME
// boolean purview as the list ([soul.SelectStats] takes the scope and pushes it
// down), so stats and list agree on what the operator may see — there is no
// partially-applied scope left to degrade.
func (h *SoulHandler) StatsTyped(ctx context.Context, claims *jwt.Claims, staleThreshold time.Duration) (SoulStatsReply, error) {
	scope, ok := h.resolveListScopeForClaims(claims)
	if !ok {
		// fail-closed: scope undefined / empty → a zero aggregate, NOT all souls.
		return SoulStatsReply{Body: emptySoulStatsView()}, nil
	}
	stats, err := soul.SelectStats(ctx, h.pool, scope, staleThreshold)
	if err != nil {
		h.logger.Error("soul.stats: select failed", slog.Any("error", err))
		return SoulStatsReply{}, &problemError{problem.New(problem.TypeInternalError, "", "souls stats failed")}
	}
	return SoulStatsReply{Body: soulStatsView(stats)}, nil
}

// soulStatsView projects the domain [soul.Stats] into a flat wire-view (typed axis
// maps → string keys). Initializes empty maps as non-nil, so the wire carries an
// object even for an empty axis.
func soulStatsView(s soul.Stats) SoulStatsView {
	v := SoulStatsView{
		ByStatus:    make(map[string]int, len(s.ByStatus)),
		ByTransport: make(map[string]int, len(s.ByTransport)),
		ByCoven:     make(map[string]int, len(s.ByCoven)),
		Total:       s.Total,
		StaleCount:  s.StaleCount,
	}
	for k, n := range s.ByStatus {
		v.ByStatus[string(k)] = n
	}
	for k, n := range s.ByTransport {
		v.ByTransport[string(k)] = n
	}
	for k, n := range s.ByCoven {
		v.ByCoven[k] = n
	}
	return v
}

// emptySoulStatsView — a zero aggregate (fail-closed): empty (not nil) maps + zeros.
func emptySoulStatsView() SoulStatsView {
	return SoulStatsView{
		ByStatus:    map[string]int{},
		ByTransport: map[string]int{},
		ByCoven:     map[string]int{},
	}
}

// ListTyped — the extracted domain function of GET /v1/souls (FULL-TYPED): scoped visibility
// (offset-fast-path or keyset mode, the SERVER picks the mode from the Purview). fail-closed: no
// claims / nil-scoper / empty Purview / broken regex → an EMPTY list (200). Errors —
// *problemError (422 invalid status/transport filter; 500 PG); success — [SoulListReply].
func (h *SoulHandler) ListTyped(ctx context.Context, claims *jwt.Claims, in SoulListInput) (SoulListReply, error) {
	var zero SoulListReply
	var filter soul.ListFilter
	filter.Unassigned = in.Unassigned
	filter.SIDPrefix = in.SIDPrefix
	for _, label := range in.Covens {
		if label == "" {
			continue
		}
		if !soul.ValidCoven(label) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'coven' filter "+label+": must match "+soul.CovenPattern)}
		}
		filter.Covens = append(filter.Covens, label)
	}
	if in.Status != "" {
		st := soul.Status(in.Status)
		if !soul.ValidStatus(st) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'status' filter: must be one of pending/connected/disconnected/revoked/expired/destroyed")}
		}
		filter.Status = st
	}
	if in.Transport != "" {
		t := soul.Transport(in.Transport)
		if !soul.ValidTransport(t) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'transport' filter: must be one of agent/ssh")}
		}
		filter.Transport = t
	}

	scope, ok := h.resolveListScopeForClaims(claims)
	if !ok {
		// fail-closed: scope undefined / empty → an empty list, NOT all souls (200, not 403).
		return h.emptyListReply(in.Page), nil
	}
	return h.listOffsetTyped(ctx, filter, scope, in.Page)
}

// listOffsetTyped — the single souls-list path (NIM-128): the full boolean
// purview (coven/host/trait) is pushed into SQL by [soul.SelectAll], so offset
// pagination and total are exact. There is no keyset/Go-post-filter mode anymore.
func (h *SoulHandler) listOffsetTyped(ctx context.Context, filter soul.ListFilter, scope soulpurview.Scope, page sharedapi.Page) (SoulListReply, error) {
	items, total, err := soul.SelectAll(ctx, h.pool, filter, scope, page.Offset, page.Limit)
	if err != nil {
		h.logger.Error("soul.list: select failed", slog.Any("filter", filter), slog.Any("error", err))
		return SoulListReply{}, &problemError{problem.New(problem.TypeInternalError, "", "list souls failed")}
	}
	dtos := make([]SoulListView, 0, len(items))
	for _, s := range items {
		dtos = append(dtos, toSoulListView(s))
	}
	h.overlayPresence(ctx, dtos)
	return SoulListReply{
		Items:  dtos,
		Offset: page.Offset,
		Limit:  page.Limit,
		Total:  total,
	}, nil
}

func (h *SoulHandler) emptyListReply(page sharedapi.Page) SoulListReply {
	return SoulListReply{
		Items:  []SoulListView{},
		Offset: page.Offset,
		Limit:  page.Limit,
		Total:  0,
	}
}

// resolveListScope derives the RBAC scope boundary of `GET /v1/souls` (and
// `/stats`) from the operator's Purview (ADR-047 S3b, NIM-128). Returns
// (scope, true) — apply the SQL pushdown; (_, false) — fail-closed (the caller
// returns an empty list / zero aggregate).
//
// fail-closed branches (NOT all souls when in doubt — OPPOSITE to presence
// fail-safe):
//   - no claims in the context (guard: the route is under RequireJWT, unreachable normally);
//   - scoper not configured (nil) — we do not build a scope, we hide everything;
//   - Purview is empty ([soulpurview.Scope.Empty]) — the operator is entitled to
//     no hosts (default-deny, revoked, or no matching permission).
//
// The whole boolean scope (coven/host/trait) pushes down to SQL, so there is no
// partial/under-show branch anymore — every dimension is evaluated exactly.
func (h *SoulHandler) resolveListScopeForClaims(claims *jwt.Claims) (soulpurview.Scope, bool) {
	if claims == nil || h.scoper == nil {
		return soulpurview.Scope{}, false
	}
	sc := soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))
	if sc.Empty() {
		return soulpurview.Scope{}, false
	}
	return sc, true
}

// GetTyped — the extracted domain function of GET /v1/souls/{sid} (FULL-TYPED): a single-soul
// read with a scope gate. claims — for readScopeForClaims (outside the scope → 404, we do not leak someone else's
// host). Errors — *problemError (422 invalid sid; 404 no soul / outside scope; 500 PG);
// success — [SoulListView] (the same projection as in list).
func (h *SoulHandler) GetTyped(ctx context.Context, claims *jwt.Claims, sid string) (SoulListView, error) {
	var zero SoulListView
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	s, err := soul.SelectBySID(ctx, h.pool, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.get: select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soul failed")}
	}
	if !h.inScope(claims, s) {
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
	}
	dtos := []SoulListView{toSoulListView(s)}
	h.overlayPresence(ctx, dtos)
	return dtos[0], nil
}

// inScope is the single-object scope gate, resolved over the host's OWN
// coven/traits and nothing else (NIM-281). The list endpoint pushes down the
// same predicate in SQL, so get/soulprint/history and the list agree on which
// hosts an operator may see.
//
// There is no label inheritance: a label grants only where an operator attached
// it, and belonging to an incarnation attaches nothing. An operator who needs to
// reach the members of an incarnation is scoped `incarnation=<name>`.
func (h *SoulHandler) inScope(claims *jwt.Claims, s *soul.Soul) bool {
	scope := h.readScopeForClaims(claims)
	if scope.Unrestricted() {
		return true
	}
	if scope.Empty() {
		return false
	}
	return soulpurview.InScope(scope, s.SID, s.Coven, soulpurview.TraitsFromJSON(s.TraitsRaw))
}

// readScopeForClaims derives the single-read scope boundary from the operator's Purview
// (`soul.list`). fail-closed: nil claims / nil-scoper → an empty Purview
// ([soulpurview.Scope.Empty]) → [soulpurview.InScope] false → 404. Symmetric to
// [SoulHandler.resolveListScopeForClaims], but a single-object check via InScope
// rather than the list's SQL pushdown.
func (h *SoulHandler) readScopeForClaims(claims *jwt.Claims) soulpurview.Scope {
	if claims == nil || h.scoper == nil {
		return soulpurview.Resolve(rbac.Purview{})
	}
	return soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))
}

// soulprintReadReply — the 200 body of GET /v1/souls/{sid}/soulprint. A projection of
// `souls.{soulprint_facts, soulprint_collected_at, soulprint_received_at}`. The type
// name = the contractual hand-written schema name (docs/keeper/openapi.yaml :6858 →
// SoulprintReadReply): huma's DefaultSchemaNamer capitalizes the first letter →
// "SoulprintReadReply".
//
// typed_facts — byte-passthrough `json.RawMessage` (category D, ADR-051/ADR-018
// amend): the raw JSONB bytes of `souls.soulprint_facts` are returned AS-IS, without
// unmarshal→map→re-marshal. This guarantees forward-compat — new proto fields of the
// Soul agent's `SoulprintFacts` reach the wire without recompiling the Keeper (the Keeper
// does not parse the content). Key order is PG-jsonb-normalized (the jsonb column
// renormalizes on write). The former `map[string]any` path sorted keys
// lexicographically on re-marshal; byte-passthrough returns the jsonb order —
// a one-time intentional wire change of key order (under a guard test).
type soulprintReadReply struct {
	SID         string          `json:"sid"`
	TypedFacts  json.RawMessage `json:"typed_facts"`
	CollectedAt *time.Time      `json:"collected_at,omitempty"`
	ReceivedAt  *time.Time      `json:"received_at,omitempty"`
}

// GetSoulprint — GET /v1/souls/{sid}/soulprint.
//
// Returns the last received typed SoulprintReport (`SoulprintFacts`,
// ADR-018). Permission — `soul.list` (the same as for get; rbac.md note —
// `soul.get` is deferred to a separate PR, the list permission covers reading
// the detail and soulprint — symmetric to service.list / omen.list).
//
// Visibility scoped by RBAC (ADR-047 S3b-1, the same [readScope] gate as Get):
// the scope check runs BEFORE revealing facts — the host's covens are taken by a separate
// registry fetch ([soul.SelectBySID]; SelectSoulprint does not return them). Outside
// scope / fail-closed → 404 (like a non-existent SID, we do not leak the existence
// of someone else's host nor reveal its facts).
//
// Contract:
//   - 200 + `{sid, typed_facts, collected_at, received_at}`.
//   - 404 (not-found) — the SID is absent from the `souls` registry OR outside the operator's scope.
//   - 410 (gone, soulprint-not-received) — the Soul record exists, but
//     a `SoulprintReport` never arrived (`soulprint_facts IS NULL`).
//     A separate code vs 404 — the UI decides "no data yet" vs "no host".
//   - 422 — an invalid path SID.
//
// typed_facts — byte-passthrough (category D): the raw JSONB is returned as-is, without
// unmarshal validation, so the former 500 on "broken JSONB" is removed — the storage
// invariant (eventstream writes valid JSON via `protojson.Marshal` into a jsonb
// column, which itself rejects invalid JSON on write) guarantees
// validity, and the Keeper does not duplicate the check (forward-compat — we do not parse).

// SoulprintReadReply — the exported alias of the GET /v1/souls/{sid}/soulprint response wire type
// (typed_facts byte-passthrough). The huma route (package api) types the 200 output through it.
type SoulprintReadReply = soulprintReadReply

// GetSoulprintTyped — the extracted domain function of GET /v1/souls/{sid}/soulprint (FULL-TYPED):
// a typed SoulprintReport with a scope gate BEFORE revealing facts. Errors — *problemError (422
// invalid sid; 404 no soul / outside scope; 410 soulprint not received; 500 PG); success —
// [SoulprintReadReply].
func (h *SoulHandler) GetSoulprintTyped(ctx context.Context, claims *jwt.Claims, sid string) (SoulprintReadReply, error) {
	var zero SoulprintReadReply
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}

	// scope gate BEFORE revealing facts: the host's covens come from the registry (SelectSoulprint does not
	// carry them). host not-found and not-found due to scope both give a single 404.
	s, err := soul.SelectBySID(ctx, h.pool, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.soulprint.get: scope select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soulprint failed")}
	}
	if !h.inScope(claims, s) {
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
	}

	rec, err := soul.SelectSoulprint(ctx, h.pool, sid)
	if err != nil {
		switch {
		case errors.Is(err, soul.ErrSoulNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		case errors.Is(err, soul.ErrSoulprintNotReceived):
			return zero, &problemError{problem.New(problem.TypeSoulprintNotReceived, "",
				"soulprint for soul "+sid+" has not been received yet")}
		}
		h.logger.Error("soul.soulprint.get: select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soulprint failed")}
	}

	resp := soulprintReadReply{
		SID:        rec.SID,
		TypedFacts: json.RawMessage(rec.FactsJSON),
	}
	if !rec.CollectedAt.IsZero() {
		t := rec.CollectedAt
		resp.CollectedAt = &t
	}
	if !rec.ReceivedAt.IsZero() {
		t := rec.ReceivedAt
		resp.ReceivedAt = &t
	}
	return resp, nil
}

// SoulHistoryItemView — the FLAT domain projection of one per-host timeline entry
// (handler-native T5d). Package api projects it into the native schema SoulHistoryItem. Type — the domain's RAW
// string (the native type in api holds the enum form). Fields specific to a single source are
// pointer-optional (incarnation/scenario — scenario only; module — errand only; voyage_id —
// a back-link to Voyage; nil → key omitted by the native type). date-time — SECOND-precision wire.
type SoulHistoryItemView struct {
	Type        string
	ID          string
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	Incarnation *string
	Scenario    *string
	Module      *string
	VoyageID    *string
}

// SoulHistoryView — the domain result of GET /v1/souls/{sid}/history (handler-native T5d).
// Package api projects {SID, Items, Offset, Limit, Total} → native SoulHistoryReply
// (a standalone envelope with a top-level sid, NOT a generic PagedResponse).
type SoulHistoryView struct {
	SID    string
	Items  []SoulHistoryItemView
	Offset int
	Limit  int
	Total  int
}

// toSoulHistoryItemView — the projection of [soul.HistoryItem] into the domain [SoulHistoryItemView].
// date-time started_at/finished_at — SECOND-precision wire (.UTC().Truncate(time.Second), parity
// with legacy RFC3339 seconds). incarnation/scenario/module/voyage_id — empty string → nil → key
// omitted (byte-for-byte with the old `string ...,omitempty` DTO).
func toSoulHistoryItemView(it soul.HistoryItem) SoulHistoryItemView {
	dto := SoulHistoryItemView{
		Type:      string(it.Type),
		ID:        it.ID,
		Status:    it.Status,
		StartedAt: it.StartedAt.UTC().Truncate(time.Second),
	}
	if it.Incarnation != "" {
		v := it.Incarnation
		dto.Incarnation = &v
	}
	if it.Scenario != "" {
		v := it.Scenario
		dto.Scenario = &v
	}
	if it.Module != "" {
		v := it.Module
		dto.Module = &v
	}
	if it.FinishedAt != nil {
		t := it.FinishedAt.UTC().Truncate(time.Second)
		dto.FinishedAt = &t
	}
	if it.VoyageID != nil {
		dto.VoyageID = it.VoyageID
	}
	return dto
}

// GET /v1/souls/{sid}/history (HistoryTyped) — the aggregated per-host timeline:
// scenario tasks (`apply_runs`) + ad-hoc exec (`errands`) under the target SID, merged by
// started_at DESC. Permission — `soul.list`. Visibility scoped by RBAC (ADR-047 §d, pattern
// 1:1 with GetSoulprint): the scope check BEFORE revealing the timeline — the host's covens by a separate
// registry fetch ([soul.SelectBySID]). Outside scope / fail-closed → 404 (we do not leak someone else's host).
// A revoked operator is cut off by the revoked-aware [rbac.Enforcer.ResolvePurview].

// SoulHistoryInput — parameters of [SoulHandler.HistoryTyped] (FULL-TYPED). Types — multi-value
// (scenario|errand). Since — a zero time.Time → the boundary filter is not applied (huma on a bad
// date-time gives 400 on bind; the legacy 422 is unreachable via the router — the single source).
// Offset/Limit — the range is enforced by CheckPageBounds → 400.
type SoulHistoryInput struct {
	SID    string
	Types  []string
	Since  time.Time
	Offset int
	Limit  int
}

// HistoryTyped — the extracted domain function of GET /v1/souls/{sid}/history (FULL-TYPED):
// a per-host timeline (apply_runs + errands) with a scope gate BEFORE revealing. Errors —
// *problemError (400 out-of-range pagination; 422 invalid sid / type; 404 no soul / outside
// scope; 500 PG); success — [SoulHistoryView].
func (h *SoulHandler) HistoryTyped(ctx context.Context, claims *jwt.Claims, in SoulHistoryInput) (SoulHistoryView, error) {
	var zero SoulHistoryView
	if !soul.ValidSID(in.SID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	if err := sharedapi.CheckPageBounds(in.Offset, in.Limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	// scope gate BEFORE revealing the timeline: the host's covens come from the registry (SelectHistory does not
	// carry them). host not-found and not-found due to scope both give a single 404.
	s, err := soul.SelectBySID(ctx, h.pool, in.SID)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+in.SID+" not found")}
		}
		h.logger.Error("soul.history: scope select failed", slog.String("sid", in.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soul history failed")}
	}
	if !h.inScope(claims, s) {
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+in.SID+" not found")}
	}

	filter := soul.HistoryFilter{SID: in.SID, Since: in.Since}
	for _, t := range in.Types {
		ht := soul.HistoryType(t)
		if !soul.ValidHistoryType(ht) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "query 'type' must be one of scenario/errand")}
		}
		filter.Types = append(filter.Types, ht)
	}

	items, total, err := soul.SelectHistory(ctx, h.pool, filter, in.Offset, in.Limit)
	if err != nil {
		h.logger.Error("soul.history: select failed", slog.String("sid", in.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soul history failed")}
	}

	dtos := make([]SoulHistoryItemView, 0, len(items))
	for _, it := range items {
		dtos = append(dtos, toSoulHistoryItemView(it))
	}
	return SoulHistoryView{SID: in.SID, Items: dtos, Offset: in.Offset, Limit: in.Limit, Total: total}, nil
}

// SoulCovenAssignInput — the NATIVE request shape of POST /v1/souls/coven (handler-native T5d).
// Replaces SoulCovenAssignRequest: the huma input (package api) binds the body by its fields,
// then calls AssignCovenTyped with this flat model.
//
// `label` (one label) and `labels` (a set) are XOR by mode:
//   - mode=append/remove → label required, labels forbidden;
//   - mode=replace → labels required (may be empty = "clear all"), label forbidden.
//
// Selector — a subset of the soul.* targeting vocabulary (all/sids/coven/incarnation/status).
// A free CEL predicate is deliberately NOT supported (it breaks the provability of the scope check).
type SoulCovenAssignInput struct {
	Mode     string
	Label    string
	Labels   []string
	DryRun   bool
	Selector SoulCovenAssignSelectorInput
}

// SoulCovenAssignSelectorInput — the NATIVE shape of the coven-assign selector (handler-native T5d).
type SoulCovenAssignSelectorInput struct {
	All         bool
	SIDs        []string
	Coven       string
	Incarnation string
	Status      string
}

// covenAssignFields — a value snapshot of the coven-assign request fields (nil → zero), keeping
// the former XOR/validation logic without scattered nil checks.
type covenAssignFields struct {
	Mode     string
	Label    string
	Labels   []string
	DryRun   bool
	Selector covenAssignSelectorFields
}

// covenAssignSelectorFields — a value snapshot of the selector's pointer-optional fields.
type covenAssignSelectorFields struct {
	All         bool
	SIDs        []string
	Coven       string
	Incarnation string
	Status      string
}

func derefCovenAssign(req SoulCovenAssignInput) covenAssignFields {
	return covenAssignFields{
		Mode:   req.Mode,
		Label:  req.Label,
		Labels: req.Labels,
		DryRun: req.DryRun,
		Selector: covenAssignSelectorFields{
			All:         req.Selector.All,
			SIDs:        req.Selector.SIDs,
			Coven:       req.Selector.Coven,
			Incarnation: req.Selector.Incarnation,
			Status:      req.Selector.Status,
		},
	}
}

// soulCovenAssignResponse — the 200 body. status ∈ completed | partial. For
// mode=replace `label` is absent, `labels` reflects the applied set
// (including an empty [] for "clear all"). MarshalJSON resolves the XOR at serialization:
// `omitempty` on []string will not do (an empty set for replace must be returned as
// `[]`, not omitted). The json tags document the shape (Marshal
// builds a map; Unmarshal is not used server-side).
type soulCovenAssignResponse struct {
	Mode    string   `json:"mode"`
	Label   string   `json:"label,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	HasSet  bool     `json:"-"`
	Matched int      `json:"matched"`
	Changed int      `json:"changed"`
	Status  string   `json:"status"`
	DryRun  bool     `json:"dry_run"`
}

// SoulCovenAssignResponse — the exported alias of the internal wire type (custom MarshalJSON
// XOR label↔labels), through which the huma route (package api) types the output without forking the wire
// shape (huma builds the schema from the fields, serializes via the same type; the schema name is aligned by the
// alias soulCovenAssignReply in huma_soul_envelope.go).
type SoulCovenAssignResponse = soulCovenAssignResponse

// MarshalJSON assembles the fields with XOR serialization of label↔labels by HasSet.
func (r soulCovenAssignResponse) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"mode":    r.Mode,
		"matched": r.Matched,
		"changed": r.Changed,
		"status":  r.Status,
		"dry_run": r.DryRun,
	}
	if r.HasSet {
		labels := r.Labels
		if labels == nil {
			labels = []string{}
		}
		out["labels"] = labels
	} else {
		out["label"] = r.Label
	}
	return json.Marshal(out)
}

// AssignCoven — POST /v1/souls/coven.
//
// Bulk-adds (append) / removes (remove) ONE Coven label or
// REPLACES (replace) the set of Coven labels on hosts under selector ∩ the operator's
// scope. Coven is a cold PG label: a plain UPDATE souls, no Redis.
// The permission gate (`soul.coven-assign`) is applied by middleware; the scope intersection
// (target hosts ⊆ scope + the assigned label / each of the replace set ∈
// scope) — here, BEFORE UPDATE: without it bulk = privilege-escalation.
//
// Body XOR shape: append/remove require `label`, `labels` forbidden;
// replace requires `labels` (may be empty = clear all), `label`
// forbidden.
//
// dry_run (body or `?dry_run=true`) — return matched under selector ∩ scope
// without UPDATE.
//
// Contract:
//   - 200 + {mode, label?, labels?, matched, changed, status, dry_run}.
//   - 400 — invalid JSON.
//   - 422 — invalid mode / label(s) / status / incarnation / empty
//     selector / label outside the operator's scope / XOR violation (label+labels).
//   - 500 — scoper not configured or a DB error.
//
// partial semantics (some chunks committed, then a failure) is returned as 200
// with `status: partial` — committed changes are idempotently re-applied
// by the operator; rolling them back is unsafe. A PG error BEFORE the first commit (count
// or the first chunk) — 500.

// SoulCovenAssignReply — the result of [SoulHandler.AssignCovenTyped] (handler-native).
// Carries the 200 body ([soulCovenAssignResponse] with custom MarshalJSON XOR label↔labels) +
// audit-payload.
type SoulCovenAssignReply struct {
	Body         soulCovenAssignResponse
	AuditPayload middleware.AuditPayload
}

// AssignCovenTyped — the domain function of POST /v1/souls/coven (handler-native): bulk
// coven-assign with scope intersection. rawReq — the native input; dryRunQuery — the flag from
// `?dry_run=true` (OR with body.dry_run). Errors — *problemError (422 invalid mode/label(s)/
// selector / XOR violation / label outside scope; 500 scoper nil / PG); success —
// [SoulCovenAssignReply] (200 body + audit-payload, including partial semantics → 200).
func (h *SoulHandler) AssignCovenTyped(ctx context.Context, claims *jwt.Claims, rawReq SoulCovenAssignInput, dryRunQuery bool) (SoulCovenAssignReply, error) {
	var zero SoulCovenAssignReply
	if h.scoper == nil {
		h.logger.Error("soul.coven-assign: scoper not configured")
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "coven-assign unavailable")}
	}
	req := derefCovenAssign(rawReq)

	mode := soul.CovenMode(req.Mode)
	if !soul.ValidCovenMode(mode) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'mode' must be one of: append, remove, replace")}
	}

	// XOR label↔labels by mode. append/remove operate on ONE label
	// (array_append/array_remove over a scalar); replace takes the WHOLE
	// set. Mixing the fields is a caller programming error, rejected
	// before semantic validation.
	switch mode {
	case soul.CovenAppend, soul.CovenRemove:
		if len(req.Labels) > 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'labels' is allowed only for mode=replace; use 'label' for append/remove")}
		}
		if !soul.ValidCoven(req.Label) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'label' must match "+soul.CovenPattern)}
		}
		if err := h.covenLabelValidator().Validate(req.Label); err != nil {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
	case soul.CovenReplace:
		if req.Label != "" {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'label' is allowed only for mode=append/remove; use 'labels' for replace")}
		}
		// An empty set is allowed (= clear all labels); we validate the format and
		// run each label through the label validator.
		for _, l := range req.Labels {
			if !soul.ValidCoven(l) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "labels entry "+l+" must match "+soul.CovenPattern)}
			}
			if err := h.covenLabelValidator().Validate(l); err != nil {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
			}
		}
	}

	sel := soul.BulkSelector{
		All:         req.Selector.All,
		SIDs:        req.Selector.SIDs,
		Coven:       req.Selector.Coven,
		Incarnation: req.Selector.Incarnation,
	}
	if req.Selector.Status != "" {
		st := soul.Status(req.Selector.Status)
		if !soul.ValidStatus(st) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"selector 'status' must be one of pending/connected/disconnected/revoked/expired/destroyed")}
		}
		sel.Status = st
	}
	for _, s := range req.Selector.SIDs {
		if !soul.ValidSID(s) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'sids' entry "+s+" must match "+soul.SIDPattern)}
		}
	}
	if req.Selector.Coven != "" && !soul.ValidCoven(req.Selector.Coven) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'coven' must match "+soul.CovenPattern)}
	}
	if req.Selector.Incarnation != "" && !incarnation.ValidName(req.Selector.Incarnation) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'incarnation' must match "+incarnation.NamePattern)}
	}

	// Bulk coven-assign targets hosts by coven → project the operator's boolean
	// scope onto its coven dimension ([rbac.Enforcer.CovenScope], NIM-128): only
	// coven-pure disjuncts contribute, host/trait-only predicates yield an empty
	// coven set (fail-closed). The shared PurviewResolver only exposes
	// ResolvePurview; the concrete resolver (rbac.Holder/Enforcer) also implements
	// CovenScope, reached via a narrow assertion (nil/absent → fail-closed 500).
	scoper, ok := h.scoper.(covenScoper)
	if !ok {
		h.logger.Error("soul.coven-assign: resolver lacks CovenScope")
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "coven-assign unavailable")}
	}
	covens, unrestricted := scoper.CovenScope(claims.Subject, "soul", "coven-assign")
	scope := soul.BulkScope{Covens: covens, Unrestricted: unrestricted}

	dryRun := req.DryRun || dryRunQuery

	// For replace we apply gate (b) explicitly before the dry_run COUNT (as BulkAssignCoven
	// does for append): an out-of-scope label must be rejected BEFORE any
	// DB access, otherwise COUNT would give a misleading matched.
	if mode == soul.CovenReplace && !unrestricted {
		for _, l := range req.Labels {
			if !slices.Contains(covens, l) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "label is outside operator coven-scope")}
			}
		}
	}

	if dryRun {
		matched, err := soul.CountBulkMatched(ctx, h.pool, sel, scope)
		if err != nil {
			return zero, h.bulkErrorToProblem(err)
		}
		return h.buildCovenAssignReply(req, mode, scope, soul.Report{Matched: matched, Status: soul.BulkCompleted}, true), nil
	}

	var (
		rep soul.Report
		err error
	)
	if mode == soul.CovenReplace {
		rep, err = soul.BulkReplaceCoven(ctx, h.pool, sel, scope, req.Labels)
	} else {
		rep, err = soul.BulkAssignCoven(ctx, h.pool, sel, scope, req.Label, mode)
	}
	if err != nil {
		// partial: some chunks committed — we return 200 + status:partial, so the
		// operator sees what was done and can re-apply (idempotently).
		if rep.Status == soul.BulkPartial {
			h.logger.Warn("soul.coven-assign: partial",
				slog.String("label", req.Label),
				slog.Any("labels", req.Labels),
				slog.String("mode", req.Mode),
				slog.Int("changed", rep.Changed),
				slog.Int("chunks", rep.ChunksCommitted),
				slog.Any("error", err),
			)
			return h.buildCovenAssignReply(req, mode, scope, rep, false), nil
		}
		return zero, h.bulkErrorToProblem(err)
	}

	return h.buildCovenAssignReply(req, mode, scope, rep, false), nil
}

// bulkErrorToProblem maps bulk-layer errors into *problemError.
func (h *SoulHandler) bulkErrorToProblem(err error) error {
	switch {
	case errors.Is(err, soul.ErrBulkEmptySelector):
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"selector matches no hosts: set one of all/sids/coven/status")}
	case errors.Is(err, soul.ErrBulkLabelOutOfScope):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "label is outside operator coven-scope")}
	default:
		h.logger.Error("soul.coven-assign: failed", slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "coven-assign failed")}
	}
}

// respondCovenAssign writes the audit-payload + the 200 response.
//
// The `label`/`labels` fields reflect the applied mode shape: for append/remove —
// `label`; for replace — `labels` (always an array, including empty on "clear
// all"). The audit-payload is symmetric to the response.
func (h *SoulHandler) buildCovenAssignReply(req covenAssignFields, mode soul.CovenMode, scope soul.BulkScope, rep soul.Report, dryRun bool) SoulCovenAssignReply {
	scopeApplied := !scope.Unrestricted
	payload := middleware.AuditPayload{
		"mode":          string(mode),
		"selector":      normalizeCovenSelector(req.Selector),
		"matched":       rep.Matched,
		"changed":       rep.Changed,
		"status":        string(rep.Status),
		"scope_applied": scopeApplied,
		"dry_run":       dryRun,
		"source":        "api",
	}
	resp := soulCovenAssignResponse{
		Mode:    string(mode),
		Matched: rep.Matched,
		Changed: rep.Changed,
		Status:  string(rep.Status),
		DryRun:  dryRun,
	}
	if mode == soul.CovenReplace {
		// nil → []string{} for stable JSON `[]` (replace with an empty set is
		// a valid "clear all labels" operation).
		labels := req.Labels
		if labels == nil {
			labels = []string{}
		}
		payload["labels"] = labels
		resp.Labels = labels
		resp.HasSet = true
	} else {
		payload["label"] = req.Label
		resp.Label = req.Label
	}
	return SoulCovenAssignReply{Body: resp, AuditPayload: payload}
}

// normalizeCovenSelector — the normalized selector form for the audit-payload.
func normalizeCovenSelector(s covenAssignSelectorFields) map[string]any {
	out := map[string]any{"all": s.All}
	if len(s.SIDs) > 0 {
		out["sids"] = s.SIDs
	}
	if s.Coven != "" {
		out["coven"] = s.Coven
	}
	if s.Incarnation != "" {
		out["incarnation"] = s.Incarnation
	}
	if s.Status != "" {
		out["status"] = s.Status
	}
	return out
}

// covenLabelValidator returns the active CovenLabelValidator. In the pilot —
// a format-only no-op (the format is already checked by ValidCoven); a hook for a future
// environments catalog (Q1) without new API fields.
func (h *SoulHandler) covenLabelValidator() soul.CovenLabelValidator {
	return soul.NoopCovenLabelValidator{}
}

// SoulCovenLabelSelector — a middleware helper for RBAC bulk coven-assign
// (`POST /v1/souls/coven`): extracts the assigned label from the JSON body and
// returns `{"coven": label}` for the permission check (rbac.md § the `coven=`
// selector). This is gate (b) — a coven-scoped operator passes the middleware only
// for a label within their scope; bare/`*` — for any.
//
// The body is read (under the already-applied MaxBytesReader limit on /v1/*) and
// restored for the handler via io.NopCloser over a buffer — the handler
// decodes the body again. An invalid/empty body → a nil selector: then
// a permission with a `coven=` selector will not match (deny at the middleware), while
// bare/`*` — passes, and the handler returns 400 on broken JSON. This is safe:
// an under-privileged operator without a matching scope does not pass further.
//
// Mode=replace returns `labels[]` instead of a single `label`. Enforcer.Matches does not
// support a multi-value selector → we return the first label of the set as a
// claim "the operator has the right for at least one of the labels"; EACH label of the
// set is re-checked by the handler-side gate (b) before the DB — the middleware
// here is only a coarse filter (deny an under-privileged operator without a matching
// scope), the service level covers the rest.
func SoulCovenLabelSelector(r *http.Request) map[string]string {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	// Restore the body for the handler in any case (even on error/empty).
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) == 0 {
		return nil
	}
	var probe struct {
		Label  string   `json:"label"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	if probe.Label != "" {
		return map[string]string{"coven": probe.Label}
	}
	if len(probe.Labels) > 0 {
		return map[string]string{"coven": probe.Labels[0]}
	}
	// Empty label/labels: for replace with empty labels = "clear all" — we let it
	// through to bare permission. A coven-scoped operator without a scope match falls off at the
	// service gate (BulkReplaceCoven checks that the target hosts ⊆ scope).
	return nil
}

// === POST /v1/souls/traits (traits-assign) — bulk operator-set trait labels (ADR-060) ===

// SoulTraitsAssignInput — the NATIVE request shape of POST /v1/souls/traits (handler-native).
// Traits is a map (key → scalar|list), a separate axis alongside the flat Coven (the read/target
// pilot is already in HEAD: souls.traits jsonb, soulprint.self.traits). Mode semantics:
//   - merge (default): set/overwrite keys from Traits, keep the rest;
//   - replace: replace the WHOLE traits map with Traits (empty = clear);
//   - remove: delete the keys in Keys (a list of names).
//
// XOR Traits↔Keys by mode: merge/replace take `traits` (a map), remove — `keys`
// (a list of names). Selector — the same subset of the soul.* targeting vocabulary (all/sids/coven/
// incarnation/status) as coven-assign.
type SoulTraitsAssignInput struct {
	Mode     string
	Traits   map[string]any
	Keys     []string
	DryRun   bool
	Selector SoulCovenAssignSelectorInput
}

// soulTraitsAssignResponse — the 200 body. status ∈ completed | partial. `keys` — the list of
// affected trait KEYS (merge/replace → keys of the given set; remove → deleted ones).
// trait values are NOT echoed in the response (symmetric to the audit-payload: we record the operation shape,
// not the content — trait values may carry a host's infrastructure data).
type soulTraitsAssignResponse struct {
	Mode    string   `json:"mode"`
	Keys    []string `json:"keys"`
	Matched int      `json:"matched"`
	Changed int      `json:"changed"`
	Status  string   `json:"status"`
	DryRun  bool     `json:"dry_run"`
}

// SoulTraitsAssignResponse — the exported alias of the internal wire type (the huma
// route types the output through it; the schema name is aligned by the alias soulTraitsAssignReply in
// huma_soul_envelope.go).
type SoulTraitsAssignResponse = soulTraitsAssignResponse

// SoulTraitsAssignReply — the result of [SoulHandler.AssignTraitsTyped] (handler-native):
// the 200 body + audit-payload.
type SoulTraitsAssignReply struct {
	Body         soulTraitsAssignResponse
	AuditPayload middleware.AuditPayload
}

// checkTraitPairsInScope is gate (b) of the per-soul trait write (NIM-281): every
// pair the operator attaches must lie inside its own trait-scope, the mirror of
// "the assigned coven label ∈ the operator's coven scope". A list value is checked
// element-wise — each element is a pair in its own right, and one out-of-scope
// element would grant just as much as a whole out-of-scope key.
//
// An unrestricted operator passes. Every other case resolves against
// [rbac.Enforcer.TraitScope]; a resolver without that projection is fail-closed
// 500, not a silently skipped gate.
//
// The pairs are read off the payload AS POSTGRES WILL STORE IT
// ([soul.CanonicalTraitPayload] → [rbac.TraitPairTexts]), never off the decoded
// map: the scope dimension this gate measures against is answered by `->>` over
// the stored jsonb, and only jsonb knows the text it will yield. Rendering the
// value in Go printed `1e+06` for a plain `1000000` and refused an operator the
// pair it plainly holds (NIM-529). The MCP twin
// ([mcp.Handler] soul.traits-assign) projects through the SAME two functions —
// one fixed copy and one forgotten copy is exactly the defect that ticket names.
func (h *SoulHandler) checkTraitPairsInScope(ctx context.Context, claims *jwt.Claims, traits map[string]any) error {
	scoper, ok := h.scoper.(traitScoper)
	if !ok {
		h.logger.Error("soul.traits-assign: resolver lacks TraitScope")
		return &problemError{problem.New(problem.TypeInternalError, "", "traits-assign unavailable")}
	}
	allowed, unrestricted := scoper.TraitScope(claims.Subject, "soul", "traits-assign")
	if unrestricted {
		return nil
	}
	raw, err := soul.CanonicalTraitPayload(ctx, h.pool, traits)
	if err != nil {
		h.logger.Error("soul.traits-assign: canonicalize trait payload", "error", err)
		return &problemError{problem.New(problem.TypeInternalError, "", "traits-assign unavailable")}
	}
	pairs := rbac.TraitPairTexts(raw)
	for _, key := range sortedMapKeys(pairs) {
		for _, elem := range pairs[key] {
			if !slices.Contains(allowed[key], elem) {
				return &problemError{problem.New(problem.TypeValidationFailed, "",
					"trait "+key+"="+elem+" is outside operator trait-scope")}
			}
		}
	}
	return nil
}

// sortedMapKeys — deterministic iteration so the rejected pair reported to the
// operator is stable across calls.
func sortedMapKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// AssignTraitsTyped — the domain function of POST /v1/souls/traits (handler-native): bulk
// trait-assign, the ONLY write path for labels attached to a HOST (NIM-281; the
// deprecation from the ADR-060 relocation is lifted — nothing overwrites this write and
// nothing else can produce a host trait, because nothing projects onto hosts).
// rawReq — the native input; dryRunQuery — the flag from `?dry_run=true` (OR with body.dry_run).
//
// SECURITY — two gates, the same pair as coven-assign:
//   - gate (a): target hosts ⊆ the operator's coven-scope (the shared [soul.BulkScope],
//     enforced in the WHERE predicate);
//   - gate (b): every pair being written ⊆ the operator's own trait-scope. Required
//     since a host-attached trait GRANTS visibility permanently (NIM-281) and
//     `trait.<key>=v` is a scope dimension (NIM-128) — without it any holder of this
//     permission could hand a host to a foreign role by stamping its pair. Applied to
//     merge/replace only, mirroring coven-assign, where `remove` is likewise ungated:
//     clearing a label off a host already inside gate (a) grants nothing.
//
// Errors — *problemError (422 invalid mode / key / value / nested / XOR violation /
// empty selector / pair outside scope; 500 scoper nil / PG); success —
// [SoulTraitsAssignReply] (200 body + audit-payload, including partial semantics → 200).
func (h *SoulHandler) AssignTraitsTyped(ctx context.Context, claims *jwt.Claims, rawReq SoulTraitsAssignInput, dryRunQuery bool) (SoulTraitsAssignReply, error) {
	var zero SoulTraitsAssignReply
	if h.scoper == nil {
		h.logger.Error("soul.traits-assign: scoper not configured")
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "traits-assign unavailable")}
	}

	mode := soul.TraitMode(rawReq.Mode)
	if mode == "" {
		mode = soul.TraitMerge // default.
	}
	if !soul.ValidTraitMode(mode) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'mode' must be one of: merge, replace, remove")}
	}

	// XOR traits↔keys by mode + format/value validation. merge/replace operate on a
	// key→value map; remove — on a list of key names.
	switch mode {
	case soul.TraitMerge, soul.TraitReplace:
		if len(rawReq.Keys) > 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'keys' is allowed only for mode=remove; use 'traits' for merge/replace")}
		}
		if err := soul.ValidateTraitDelta(rawReq.Traits); err != nil {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
	case soul.TraitRemove:
		if len(rawReq.Traits) > 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'traits' is allowed only for mode=merge/replace; use 'keys' for remove")}
		}
		if len(rawReq.Keys) == 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'keys' is required and must be non-empty for mode=remove")}
		}
		if err := soul.ValidateTraitKeys(rawReq.Keys); err != nil {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
	}

	sel := soul.BulkSelector{
		All:         rawReq.Selector.All,
		SIDs:        rawReq.Selector.SIDs,
		Coven:       rawReq.Selector.Coven,
		Incarnation: rawReq.Selector.Incarnation,
	}
	if rawReq.Selector.Status != "" {
		st := soul.Status(rawReq.Selector.Status)
		if !soul.ValidStatus(st) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"selector 'status' must be one of pending/connected/disconnected/revoked/expired/destroyed")}
		}
		sel.Status = st
	}
	for _, s := range rawReq.Selector.SIDs {
		if !soul.ValidSID(s) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'sids' entry "+s+" must match "+soul.SIDPattern)}
		}
	}
	if rawReq.Selector.Coven != "" && !soul.ValidCoven(rawReq.Selector.Coven) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'coven' must match "+soul.CovenPattern)}
	}
	if rawReq.Selector.Incarnation != "" && !incarnation.ValidName(rawReq.Selector.Incarnation) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'incarnation' must match "+incarnation.NamePattern)}
	}

	// Bulk traits-assign narrows target hosts by coven scope (gate a) → project
	// the operator's boolean scope onto its coven dimension via CovenScope
	// (NIM-128), reached through the same narrow assertion as coven-assign.
	scoper, ok := h.scoper.(covenScoper)
	if !ok {
		h.logger.Error("soul.traits-assign: resolver lacks CovenScope")
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "traits-assign unavailable")}
	}
	covens, unrestricted := scoper.CovenScope(claims.Subject, "soul", "traits-assign")
	scope := soul.BulkScope{Covens: covens, Unrestricted: unrestricted}

	// Gate (b), NIM-281: the pairs being attached must lie inside the operator's
	// own trait-scope. Checked BEFORE any WRITE, including on the dry-run path, so
	// a rejected write never reports a misleading `matched`. It is not before any
	// DB access — canonicalizing the payload is one `SELECT $1::jsonb` — but that
	// query reads no row and opens no transaction, so the dry-run promise stands.
	if mode == soul.TraitMerge || mode == soul.TraitReplace {
		if err := h.checkTraitPairsInScope(ctx, claims, rawReq.Traits); err != nil {
			return zero, err
		}
	}

	dryRun := rawReq.DryRun || dryRunQuery

	if dryRun {
		matched, err := soul.CountBulkMatched(ctx, h.pool, sel, scope)
		if err != nil {
			return zero, h.bulkErrorToProblem(err)
		}
		return h.buildTraitsAssignReply(rawReq, mode, scope, soul.Report{Matched: matched, Status: soul.BulkCompleted}, true), nil
	}

	var (
		rep soul.Report
		err error
	)
	if mode == soul.TraitReplace {
		rep, err = soul.BulkReplaceTraits(ctx, h.pool, sel, scope, rawReq.Traits)
	} else {
		rep, err = soul.BulkAssignTraits(ctx, h.pool, sel, scope, mode, rawReq.Traits, rawReq.Keys)
	}
	if err != nil {
		// partial: some chunks committed — 200 + status:partial (idempotently
		// re-applied by the operator, rolling back is unsafe — parity with coven).
		if rep.Status == soul.BulkPartial {
			h.logger.Warn("soul.traits-assign: partial",
				slog.String("mode", rawReq.Mode),
				slog.Int("changed", rep.Changed),
				slog.Int("chunks", rep.ChunksCommitted),
				slog.Any("error", err),
			)
			return h.buildTraitsAssignReply(rawReq, mode, scope, rep, false), nil
		}
		return zero, h.bulkErrorToProblem(err)
	}

	return h.buildTraitsAssignReply(rawReq, mode, scope, rep, false), nil
}

// buildTraitsAssignReply assembles the 200 response + audit-payload. `keys` — the sorted
// list of affected keys (for merge/replace — keys of the given set; for remove —
// deleted ones). trait values are put neither in the response nor in the audit (secret hygiene).
func (h *SoulHandler) buildTraitsAssignReply(req SoulTraitsAssignInput, mode soul.TraitMode, scope soul.BulkScope, rep soul.Report, dryRun bool) SoulTraitsAssignReply {
	keys := affectedTraitKeys(mode, req.Traits, req.Keys)
	payload := middleware.AuditPayload{
		"mode":          string(mode),
		"selector":      normalizeCovenSelector(covenAssignSelectorFields(req.Selector)),
		"keys":          keys,
		"matched":       rep.Matched,
		"changed":       rep.Changed,
		"status":        string(rep.Status),
		"scope_applied": !scope.Unrestricted,
		"dry_run":       dryRun,
		"source":        "api",
	}
	resp := soulTraitsAssignResponse{
		Mode:    string(mode),
		Keys:    keys,
		Matched: rep.Matched,
		Changed: rep.Changed,
		Status:  string(rep.Status),
		DryRun:  dryRun,
	}
	return SoulTraitsAssignReply{Body: resp, AuditPayload: payload}
}

// affectedTraitKeys — the sorted set of affected trait keys: for merge/replace —
// the keys of the Traits map, for remove — the Keys list. nil → []string{} (stable JSON `[]`).
func affectedTraitKeys(mode soul.TraitMode, traits map[string]any, keys []string) []string {
	var out []string
	if mode == soul.TraitRemove {
		out = append([]string(nil), keys...)
	} else {
		out = make([]string, 0, len(traits))
		for k := range traits {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// SoulSIDSelector — a middleware helper for RBAC: extracts the SID from the path param
// for the permission check. Uses the selector key `host` (rbac.md §
// Selector grammar — `host` for per-Soul targeting).
func SoulSIDSelector(r *http.Request) map[string]string {
	sid := chi.URLParam(r, "sid")
	if sid == "" {
		return nil
	}
	return map[string]string{"host": sid}
}

// SoulSshTargetInput — the NATIVE request shape of PUT /v1/souls/{sid}/ssh-target (handler-native
// T5d). Replaces SoulSSHTargetRequest: the huma input (package api) binds the body by its fields,
// then calls UpdateSshTargetTyped with this flat model.
//
// All fields required except `ssh_provider` (P2 W-1, ADR-032 amendment 2026-05-27): the operation
// is "update the SSH credentials", not "partially amend". `ssh_provider` (empty → nil → routing
// goes via coven_default → cluster_default; kebab-case, validated by the handler before writing).
type SoulSshTargetInput struct {
	SSHPort     int
	SSHUser     string
	SoulPath    string
	SSHProvider string
}

// SoulSshTargetView — the FLAT domain projection of the 200 body of PUT /v1/souls/{sid}/ssh-target
// (handler-native T5d). Package api projects it into the native schema SoulSshTargetReply (nested
// ssh_target — class-A reuse of the native SoulSshTarget). A snapshot of the saved state.
// SSHProvider empty → key omitted by the native type (omitempty).
type SoulSshTargetView struct {
	SID         string
	SSHPort     int
	SSHUser     string
	SoulPath    string
	SSHProvider string
}

// SoulSshTargetReply — the result of [SoulHandler.UpdateSshTargetTyped] (handler-native). Carries
// the domain projection of the 200 body (SoulSshTargetView: a snapshot) + audit-payload.
type SoulSshTargetReply struct {
	Body         SoulSshTargetView
	AuditPayload middleware.AuditPayload
}

// UpdateSshTargetTyped — the domain function of PUT /v1/souls/{sid}/ssh-target (handler-native):
// updating per-host SSH credentials for the push-flow. req — the native input. Errors — *problemError
// (422 invalid sid/ssh_port/ssh_user/soul_path/ssh_provider; 404 no soul; 500 PG); success —
// [SoulSshTargetReply] (the domain projection of the 200 body + audit-payload).
func (h *SoulHandler) UpdateSshTargetTyped(ctx context.Context, sid string, req SoulSshTargetInput) (SoulSshTargetReply, error) {
	var zero SoulSshTargetReply
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	if req.SSHPort < 1 || req.SSHPort > 65535 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'ssh_port' must be in [1..65535]")}
	}
	if req.SSHUser == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'ssh_user' is required")}
	}
	if req.SoulPath == "" || req.SoulPath[0] != '/' {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'soul_path' must be an absolute Unix path (start with '/')")}
	}
	// P2 W-1: optional `ssh_provider` — the kebab-case plugin name. Empty → routing
	// goes to the coven_default/cluster_default levels.
	provider := req.SSHProvider
	if provider != "" && !pushprovider.ValidName(provider) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'ssh_provider' must match "+pushprovider.NamePattern)}
	}

	target := &soul.SSHTarget{SSHPort: req.SSHPort, SSHUser: req.SSHUser, SoulPath: req.SoulPath}
	if provider != "" {
		sp := provider
		target.SSHProvider = &sp
	}
	if err := soul.UpdateSshTarget(ctx, h.pool, sid, target); err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.ssh-target.update: failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update ssh_target failed")}
	}

	auditPayload := middleware.AuditPayload{
		"sid":       sid,
		"ssh_port":  req.SSHPort,
		"ssh_user":  req.SSHUser,
		"soul_path": req.SoulPath,
	}
	if provider != "" {
		auditPayload["ssh_provider"] = provider
	}
	return SoulSshTargetReply{
		Body: SoulSshTargetView{
			SID:         sid,
			SSHPort:     req.SSHPort,
			SSHUser:     req.SSHUser,
			SoulPath:    req.SoulPath,
			SSHProvider: provider,
		},
		AuditPayload: auditPayload,
	}, nil
}

// parseTransport maps the JSON transport string to [soul.Transport]. Returns
// ok=false for an empty/unknown string (→ 422 on the handler side).
func parseTransport(v string) (soul.Transport, bool) {
	switch soul.Transport(v) {
	case soul.TransportAgent:
		return soul.TransportAgent, true
	case soul.TransportSSH:
		return soul.TransportSSH, true
	default:
		return "", false
	}
}

// coalesceCoven normalizes a nil slice to empty — for JSON `[]` instead of `null`
// (covens is declared non-nullable in proto/OpenAPI).
func coalesceCoven(c []string) []string {
	if c == nil {
		return []string{}
	}
	return c
}

// coalesceTraits normalizes a nil map to empty — for JSON `{}` instead of `null`
// (traits is declared non-nullable in OpenAPI, symmetric to coalesceCoven). A bare soul
// without operator-set labels is returned as `{}`, not `null` — the UI can render an
// empty set without a nil check (ADR-060 read-path).
func coalesceTraits(t map[string]any) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	return t
}
