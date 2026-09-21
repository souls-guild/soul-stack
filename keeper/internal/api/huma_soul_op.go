package api

// FULL-TYPED form of the SOUL domain (code-first OpenAPI source, ADR-054 §Pattern).
// ROLLOUT-BATCH-2e (soul read+write on huma per the role/operator references + audit endpoint):
// create — WRITE+AUDIT (soul.created, 201+body); coven-assign — WRITE+AUDIT (soul.coven-changed,
// 200+body custom XOR); issue-token — WRITE+AUDIT (soul.token-issued, 200+body like operator
// issue-token); ssh-target — WRITE+AUDIT (soul.ssh-target.updated, 200+body); list — read-with-
// typed-query (coven/status/transport + offset/limit/cursor); get/soulprint — read-with-path;
// history — read-with-typed-query (type[]/since/offset/limit, paginated → CheckPageBounds).
//
// POST /v1/souls/{sid}/exec (ErrandExec) — WRITE+AUDIT (errand.invoked) with TWO
// success codes: 200 sync ErrandResult (terminal up to server-cap) / 202 async
// ErrandAccepted + Location header (escalation). Body is pre-marshaled into
// json.RawMessage (errand GET shape), Status/Location — huma field convention.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// huma-output Body aliases for the domain wire types (handlers). Through them huma builds the
// schema of the 200 bodies and serializes the values already assembled by the handler (custom
// MarshalJSON / paged envelope / byte-passthrough typed_facts are preserved).
type (
	soulCovenAssignReplyBody = handlers.SoulCovenAssignResponse
	soulSoulprintReplyBody   = handlers.SoulprintReadReply
)

// === POST /v1/souls (create) — WRITE+AUDIT soul.created (201+body) ===

// soulCreateInput — huma input POST /v1/souls (FULL-TYPED). Body — a typed body.
type soulCreateInput struct {
	Body SoulCreateRequest
}

// soulCreateOutput — huma output POST /v1/souls (FULL-TYPED). Status=201; Body — huma-native
// 201 body (SoulCreateReply, shape 1:1 with SoulCreateReply; bootstrap_token only for
// transport=agent). Wire shape is pinned by a golden-JSON byte-exact test
// (huma_soul_reply_test.go).
type soulCreateOutput struct {
	Status int `json:"-"`
	Body   SoulCreateReply
}

// soulCreateOperation — metadata of POST /v1/souls. Path = "/" relative to the chi group
// /v1/souls. DefaultStatus=201. Permission soul.create + audit soul.created. Errors: 400
// unknown/malformed, 403 RBAC, 409 soul-exists, 422 sid/transport/coven validation, 500.
func soulCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createSoul",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Register Soul",
		Description:   "Onboarding host to souls registry (status: pending). For transport=agent, bootstrap token is issued. Permission soul.create. 409 — SID taken.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/coven (coven-assign) — WRITE+AUDIT soul.coven-changed (200+body) ===

// soulCovenAssignInput — huma input POST /v1/souls/coven (FULL-TYPED). Body — typed body
// (mode + label/labels XOR + selector). DryRun also from query (?dry_run=true, OR with body).
type soulCovenAssignInput struct {
	Body   SoulCovenAssignRequest
	DryRun bool `query:"dry_run" doc:"count matched without UPDATE (OR with body.dry_run)"`
}

// soulCovenAssignOutput — huma output POST /v1/souls/coven (FULL-TYPED). Status=200; Body —
// typed 200 body (handlers.SoulCovenAssignBody; custom MarshalJSON XOR label↔labels).
type soulCovenAssignOutput struct {
	Status int `json:"-"`
	Body   soulCovenAssignReplyBody
}

// soulCovenAssignOperation — metadata of POST /v1/souls/coven. DefaultStatus=200. Permission
// soul.coven-assign + audit soul.coven-changed. Errors: 400 unknown/malformed, 403 RBAC,
// 422 mode/label(s)/selector/scope validation, 500.
func soulCovenAssignOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "assignSoulCoven",
		Method:        http.MethodPost,
		Path:          "/coven",
		Summary:       "Bulk assign Coven tags",
		Description:   "Bulk append/remove single label or replace set on hosts under selector ∩ scope (ADR-008). Permission soul.coven-assign. partial → 200 status:partial.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/traits (traits-assign) — WRITE+AUDIT soul.traits-changed (200+body) ===

// soulTraitsAssignInput — huma input POST /v1/souls/traits (FULL-TYPED). Body — typed body
// (mode + traits/keys XOR + selector). DryRun also from query (?dry_run=true, OR with body).
type soulTraitsAssignInput struct {
	Body   SoulTraitsAssignRequest
	DryRun bool `query:"dry_run" doc:"count matched without UPDATE (OR with body.dry_run)"`
}

// soulTraitsAssignOutput — huma output POST /v1/souls/traits (FULL-TYPED). Status=200; Body —
// typed 200 body (handlers.SoulTraitsAssignResponse).
type soulTraitsAssignOutput struct {
	Status int `json:"-"`
	Body   handlers.SoulTraitsAssignResponse
}

// soulTraitsAssignOperation — metadata of POST /v1/souls/traits. DefaultStatus=200. Permission
// soul.traits-assign + audit soul.traits-changed. Errors: 400 unknown/malformed, 403 RBAC,
// 422 mode/traits/keys/selector validation, 500.
func soulTraitsAssignOperation() huma.Operation {
	return huma.Operation{
		OperationID: "assignSoulTraits",
		Method:      http.MethodPost,
		Path:        "/traits",
		Summary:     "Bulk assignment of trait-tags to hosts",
		// The ONLY way a host acquires a trait (NIM-281): a label lives where an
		// operator attached it, so labelling an incarnation never reaches its
		// hosts and this endpoint has no incarnation-side shortcut.
		Description:   "Bulk merge/replace/remove of operator-set trait-tags attached to HOSTS (souls.traits jsonb) on hosts under selector \u2229 coven-scope. These are a host's whole set of traits: belonging to an incarnation attaches nothing (NIM-281), so a host carries exactly the pairs an operator put on it. Permission soul.traits-assign; merge/replace additionally require every pair to lie inside the operator's own trait-scope (gate b). partial -> 200 status:partial.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/{sid}/issue-token (issue-token) — WRITE+AUDIT soul.token-issued (200+body) ===

// soulIssueTokenInput — huma input POST /v1/souls/{sid}/issue-token. SID — path; Force —
// query (?force=true). No Body.
type soulIssueTokenInput struct {
	SID   string `path:"sid" doc:"SID (FQDN) of Soul"`
	Force bool   `query:"force" doc:"expire the active token and issue a new one"`
}

// soulIssueTokenOutput — huma output POST /v1/souls/{sid}/issue-token (FULL-TYPED). Status=200;
// Body — huma-native 200 body (SoulIssueTokenReply: sid/bootstrap_token/expires_at). Unlike the
// 204 write routes, issue-token returns the issued token (parity operator issue-token).
type soulIssueTokenOutput struct {
	Status int `json:"-"`
	Body   SoulIssueTokenReply
}

// soulIssueTokenOperation — metadata of POST /v1/souls/{sid}/issue-token. DefaultStatus=200.
// Permission soul.issue-token + audit soul.token-issued. Errors: 403 RBAC, 404 no soul,
// 409 active token without force, 422 invalid sid / transport=ssh, 500.
func soulIssueTokenOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "issueSoulToken",
		Method:        http.MethodPost,
		Path:          "/{sid}/issue-token",
		Summary:       "Reissue bootstrap token",
		Description:   "Reissue of the bootstrap token for transport=agent (?force=true expires the active one). Permission soul.issue-token. 409 - active token without force.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/souls/{sid} (forget) — WRITE+AUDIT soul.forgotten (200+body) ===

// soulForgetInput — huma input DELETE /v1/souls/{sid}. SID — path. No Body, no
// query: there is deliberately no `force` (NIM-386). A single verb must not be
// able to degrade from "release the host" to "drop its row" — if the release
// cannot happen the call fails with 503 having deleted nothing.
type soulForgetInput struct {
	SID string `path:"sid" doc:"SID (FQDN) of Soul"`
}

// soulForgetOutput — huma output DELETE /v1/souls/{sid} (FULL-TYPED). Status=200
// WITH BODY, not the 204 a DELETE usually answers: the operation fires four ON
// DELETE CASCADE edges and two of them reach objects other operators own
// (incarnation rosters, Choir Voices), so the counts have to be on screen. The
// release outcome rides along — a host erased but not released is not a plain
// success.
type soulForgetOutput struct {
	Status int `json:"-"`
	Body   SoulForgetReply
}

// soulForgetOperation — metadata of DELETE /v1/souls/{sid}. DefaultStatus=200.
// Permission soul.forget + audit soul.forgotten. Errors: 403 RBAC, INCLUDING a
// host outside the operator's scope (this is a scope-aware
// RequirePermissionMulti with SoulSIDScopeSelector, so the host and its covens
// are resolved before the handler runs — out-of-scope is answered 403 and never
// reaches the 404 branch, unlike the read routes where narrowing happens in the
// handler); 404 no soul;
// 422 invalid sid; 503 the cluster could not be told (NOTHING was deleted —
// retryable, problem type `teardown-unavailable`); 500.
//
// No state precondition and therefore no 409: a host may be forgotten in any
// state, including `connected` (the call tears the stream down) and one this
// cluster has never heard from. What keeps a forgotten host gone is not a state
// check but the allowlist — seed auth resolves `soul_seeds.fingerprint`, and the
// seeds cascade away with the row.
func soulForgetOperation() huma.Operation {
	return huma.Operation{
		OperationID: "forgetSoul",
		Method:      http.MethodDelete,
		Path:        "/{sid}",
		Summary:     "Forget a host",
		Description: "Erase a host from the registry and release what it held: revoke its seeds, burn its unused bootstrap tokens, " +
			"close its EventStream across the cluster and purge its Redis keys. The souls row goes, and with it — through ON DELETE CASCADE — " +
			"its seeds, its unburnt bootstrap tokens, its incarnation memberships and its Choir Voices; all four counts are in the reply. " +
			"IRREVERSIBLE, and legal in any state (including connected). A forgotten host cannot reconnect: seed auth is an allowlist and its " +
			"allowlist entry is gone. Permission soul.forget. 503 - the cluster-wide teardown notice could not be sent and NOTHING was deleted.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors: []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity,
			http.StatusInternalServerError, http.StatusServiceUnavailable},
	}
}

// === PUT /v1/souls/{sid}/ssh-target (ssh-target) — WRITE+AUDIT soul.ssh-target.updated (200+body) ===

// soulSshTargetInput — huma input PUT /v1/souls/{sid}/ssh-target. SID — path; Body — typed body.
type soulSshTargetInput struct {
	SID  string `path:"sid" doc:"SID (FQDN) of Soul"`
	Body SoulSshTarget
}

// soulSshTargetOutput — huma output PUT /v1/souls/{sid}/ssh-target (FULL-TYPED). Status=200;
// Body — huma-native 200 body (SoulSshTargetReply: snapshot of the saved target; nested
// ssh_target — class-A reuse of native SoulSshTarget). Wire shape is pinned by a golden-JSON
// byte-exact test (huma_soul_reply_test.go).
type soulSshTargetOutput struct {
	Status int `json:"-"`
	Body   SoulSshTargetReply
}

// soulSshTargetOperation — metadata of PUT /v1/souls/{sid}/ssh-target. DefaultStatus=200.
// Permission soul.ssh-target-update + audit soul.ssh-target.updated. Errors: 400 unknown/
// malformed, 403 RBAC, 404 no soul, 422 sid/port/user/path/provider validation, 500.
func soulSshTargetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateSoulSSHTarget",
		Method:        http.MethodPut,
		Path:          "/{sid}/ssh-target",
		Summary:       "Update Soul SSH requisites",
		Description:   "Per-host SSH requisites for push-flow (ADR-032 S7-1). Replace semantics (full set). Permission soul.ssh-target-update. 404 - no soul.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls (list) — READ with typed query (no audit) ===

// soulListInput — huma input GET /v1/souls (FULL-TYPED typed query). coven/transport — string
// filters; status — a closed-set enum (out of set → 422). cursor — a keyset cursor (string).
// offset/limit — int32 with default; bad-int → 400; range / offset+cursor conflict / broken
// cursor are resolved in the (w,r) wrapper via ParsePageWithCursor (the huma route calls ListTyped with
// already-parsed page/cursor). Here offset/limit/cursor are bound for the schema;
// the business pagination parse is done by the register handler via ParsePageWithCursor over the same
// query values.
type soulListInput struct {
	// Coven is REPEATABLE (`?coven=a&coven=b`) and matches ANY of the labels given —
	// the create-form roster picker asks for hosts across the set of covens an
	// incarnation declares (NIM-371), and one label per request would make it union
	// pages client-side over totals that each mean something else. One value behaves
	// exactly as the single-valued parameter did.
	Coven      []string `query:"coven" doc:"filter by Coven label the host carries itself; belonging to an incarnation attaches no label (NIM-281); repeatable — matches ANY of the labels; AND within scope"`
	Status     string   `query:"status" enum:"pending,connected,disconnected,revoked,expired,destroyed" doc:"filter by status; outside enum -> 422"`
	Transport  string   `query:"transport" enum:"agent,ssh" doc:"filter by transport; outside enum -> 422"`
	Unassigned bool     `query:"unassigned" doc:"only hosts belonging to NO incarnation (incarnation_membership, NIM-124) — the free souls a create scenario can be rolled onto"`
	SIDPrefix  string   `query:"sid_prefix" maxLength:"254" doc:"only SIDs starting with this prefix (autocomplete); matched literally, LIKE metacharacters included"`
	Cursor     string   `query:"cursor" doc:"keyset continuation cursor (regex-mode scope)"`
	Offset     int32    `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400; offset+cursor → 422)"`
	Limit      int32    `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// soulListOutput — huma output GET /v1/souls (FULL-TYPED). Body — a TAGGED native envelope
// SoulListReply (CURSOR, 6 fields: items.$ref to native SoulListEntry with json tags +
// next_cursor/total_approximate omitempty). Body used to be handlers.SoulListReply (=
// PagedResponse[SoulListView]) — untagged View → PascalCase wire (contract bug #7).
// The register func projects reply.Items through newSoulListEntry and CARRIES the cursor fields
// (next_cursor/total_approximate) byte-exact. The OpenAPI schema doesn't change (same alias target
// SoulListReply).
type soulListOutput struct {
	Body SoulListReply
}

// soulListOperation — metadata of GET /v1/souls. Path = "/" relative to the chi group /v1/souls.
// DefaultStatus=200. READ route: audit not wired. Permission soul.list. Errors: 400 (bad
// pagination / broken cursor), 403 RBAC, 422 (bad status/transport enum / offset+cursor), 500.
func soulListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listSouls",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "List of Souls (paged, scoped)",
		Description:   "Registry of souls with scoped visibility (ADR-047) and coven/status/transport filters. offset-fast-path or keyset (mode chosen server-side from Purview). Permission soul.list. Read-only, no audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/stats (stats) — READ aggregate (no audit) ===

// soulStatsInput — huma input GET /v1/souls/stats. No parameters: the aggregate is
// computed over the operator's entire visible scope (the boundary comes from Purview, NOT the query).
type soulStatsInput struct{}

// soulStatsOutput — huma output GET /v1/souls/stats (FULL-TYPED). Body — a native
// aggregate DTO (soulStatsReply: by_status/by_transport/by_coven/total/stale_count).
type soulStatsOutput struct {
	Body soulStatsReply
}

// soulStatsOperation — metadata of GET /v1/souls/stats. Path = "/stats" relative to
// the chi group /v1/souls. DefaultStatus=200. READ route: audit not wired. Permission
// soul.list (the same registry-read right as list/get). Errors: 403 RBAC, 500.
func soulStatsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulsStats",
		Method:        http.MethodGet,
		Path:          "/stats",
		Summary:       "Souls registry aggregate (Overview)",
		Description:   "Summary by status/transport/coven + total + stale_count for Souls Overview with scoped visibility (ADR-047). transport - agent/ssh (UI maps to pull/push). stale_count - by mark_disconnected.stale_after. Permission soul.list. Read-only, no audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/{sid} (get) — READ with path (no audit) ===

// soulGetInput — huma input GET /v1/souls/{sid}. SID — path.
type soulGetInput struct {
	SID string `path:"sid" doc:"SID (FQDN) of Soul"`
}

// soulGetOutput — huma output GET /v1/souls/{sid} (FULL-TYPED). Body — huma-native 200 body
// (SoulListEntry — the same projection as the list-envelope element; shared get Body + envelope element).
type soulGetOutput struct {
	Body SoulListEntry
}

// soulGetOperation — metadata of GET /v1/souls/{sid}. DefaultStatus=200. READ route: audit not
// wired. Permission soul.list. Errors: 403, 404 (no soul / out of scope), 422 bad sid, 500.
func soulGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoul",
		Method:        http.MethodGet,
		Path:          "/{sid}",
		Summary:       "Soul card",
		Description:   "One registry row of souls for the detail page (ADR-047 scoped). Permission soul.list. Outside scope -> 404. Read-only, no audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/{sid}/soulprint (soulprint) — READ with path (no audit) ===

// soulSoulprintInput — huma input GET /v1/souls/{sid}/soulprint. SID — path.
type soulSoulprintInput struct {
	SID string `path:"sid" doc:"SID (FQDN) of Soul"`
}

// soulSoulprintOutput — huma output GET /v1/souls/{sid}/soulprint (FULL-TYPED). Body — typed
// 200 body (handlers.SoulprintReadReply: sid/typed_facts/collected_at/received_at).
type soulSoulprintOutput struct {
	Body soulSoulprintReplyBody
}

// soulSoulprintOperation — metadata of GET /v1/souls/{sid}/soulprint. DefaultStatus=200. READ
// route: audit not wired. Permission soul.list. Errors: 403, 404 (no soul / out of scope),
// 410 (soulprint not received), 422 bad sid, 500.
func soulSoulprintOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulprint",
		Method:        http.MethodGet,
		Path:          "/{sid}/soulprint",
		Summary:       "Soul soulprint",
		Description:   "The latest typed SoulprintReport (ADR-018) with a scope gate. Permission soul.list. 410 - soulprint has never arrived. Read-only, no audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/{sid}/history (history) — READ with typed query (no audit) ===

// soulHistoryInput — huma input GET /v1/souls/{sid}/history (FULL-TYPED typed query). SID —
// path. Types — multi-value (?type=X&type=Y) OR (explode:true is MANDATORY). Since — date-time
// (bad value → 400). offset/limit — int32 with default; range → CheckPageBounds 400.
type soulHistoryInput struct {
	SID    string    `path:"sid" doc:"SID (FQDN) of Soul"`
	Types  []string  `query:"type,explode" enum:"scenario,errand" doc:"multi-value ?type=X&type=Y - OR by source; outside enum -> 422"`
	Since  time.Time `query:"since" doc:"started_at > since (RFC3339); bad value → 400"`
	Offset int32     `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32     `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// soulHistoryOutput — huma output GET /v1/souls/{sid}/history (FULL-TYPED). Body — a huma-native
// 200 envelope (SoulHistoryReply: sid/items/offset/limit/total + nested SoulHistoryItem;
// a standalone envelope, NOT generic PagedResponse).
type soulHistoryOutput struct {
	Body SoulHistoryReply
}

// soulHistoryOperation — metadata of GET /v1/souls/{sid}/history. DefaultStatus=200. READ route:
// audit not wired. Permission soul.list. Errors: 400 (out-of-range pagination / bad since),
// 403, 404 (no soul / out of scope), 422 (bad sid / type enum), 500.
func soulHistoryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulHistory",
		Method:        http.MethodGet,
		Path:          "/{sid}/history",
		Summary:       "Soul run history (paged)",
		Description:   "Per-host timeline (scenario apply_runs + ad-hoc errands) with a scope gate. Permission soul.list. Read-only, no audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/{sid}/exec (exec) — WRITE+AUDIT errand.invoked (200 sync / 202 async) ===

// errandExecInput — huma input POST /v1/souls/{sid}/exec. SID — path; Body — a typed
// body (module required + opt. input/timeout_seconds/dry_run). additionalProperties:false
// (huma default) → unknown body field → 400.
type errandExecInput struct {
	SID  string `path:"sid" doc:"SID (FQDN) of the target Soul"`
	Body ErrandRunRequest
}

// errandExecOutput — huma output POST /v1/souls/{sid}/exec with TWO success codes under
// one OperationID (200 sync ErrandResult / 202 async ErrandAccepted — different bodies +
// Location only on 202). Status — huma field convention (response-code override).
// Location — a header field: an empty string is NOT written (huma native omitempty), set
// ONLY on 202. Body — json.RawMessage: the handler pre-marshals the chosen body (errand GET
// shape; the schema in the fragment = `{}`, committed openapi.yaml carries the typed
// 200/ErrandResult + 202/ErrandAccepted — authoritative). The wire body bytes are identical to legacy.
type errandExecOutput struct {
	Status   int             `json:"-"`
	Location string          `header:"Location" json:"-"`
	Body     json.RawMessage `json:"body"`
}

// errandExecOperation — metadata of POST /v1/souls/{sid}/exec. DefaultStatus=200 (sync
// terminal). 202 (async escalation) — an additional success code (the handler sets
// Status=202 + Location itself). Permission errand.run + audit errand.invoked. Errors: 202
// async, 400 unknown/malformed + dry_run on a verb-shell module (malformed-request,
// NIM-489 — impossible on every host, so not the 409 axis), 403 RBAC, 404
// soul-not-connected, 409 the target did not announce dry_run
// (soul-capability-unsupported, NIM-456), 422 invalid sid/module/timeout, 500.
func errandExecOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "ErrandExec",
		Method:        http.MethodPost,
		Path:          "/{sid}/exec",
		Summary:       "Run Errand on a Soul",
		Description:   "Pull ad-hoc module exec on a single host (ADR-033). 200 sync (terminal up to server-cap 30s) or 202 + Location async-escalation. Permission errand.run. 404 - Soul not connected. 400 - dry_run requested for a verb-shell module (no pure-read Plan exists on any host, so no upgrade fixes it). 409 - dry_run requested and the target Soul did not announce the dry_run capability (it would apply for real).",
		Tags:          []string{"Errand"},
		DefaultStatus: http.StatusOK,
		Errors: []int{http.StatusAccepted, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
			http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
