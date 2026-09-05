package api

// FULL-TYPED form of incarnation MEMBERSHIP (code-first OpenAPI source, ADR-054 §Pattern;
// ADR-008 amendment 2026-07-28, NIM-209). Membership is a sub-resource of an incarnation
// (/{name}/members[/{sid}]), the choir-voices layout: the huma op carries the FULL path
// relative to the /v1/incarnations group.
//
// Audit class: WRITE-SELF-AUDIT for bind/unbind (incarnation.member_bound /
// .member_unbound written by the handler ITSELF inside *Typed) — audit middleware is NOT
// wired on these routes. The roster read writes no audit.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/incarnations/{id}/members (bind) — WRITE-SELF-AUDIT incarnation.member_bound (200+body) ===

// memberBindInput — huma input POST .../members. Name — path; Body — typed body.
type memberBindInput struct {
	ID   string `path:"id" doc:"incarnation id"`
	Body IncarnationMemberBindRequest
}

// memberBindOutput — huma output POST .../members (FULL-TYPED). Status=200.
type memberBindOutput struct {
	Body IncarnationMemberBindReply
}

// memberBindOperation — metadata for POST .../members. DefaultStatus=200 (not 201: the
// call is idempotent and may create nothing). Errors: 400 malformed, 403 (no permission,
// or a SID outside the caller's soul scope), 404 incarnation, 422 (unknown SID / host not
// connected / bad shape), 500.
func memberBindOperation() huma.Operation {
	return huma.Operation{
		OperationID: "bindIncarnationMembers",
		Method:      http.MethodPost,
		Path:        "/{id}/members",
		Summary:     "Bind hosts to an incarnation",
		Description: "Binds already-onboarded, connected Souls to the incarnation's roster so a scenario can subsequently roll onto them (ADR-008 amendment, NIM-209). Idempotent: re-binding a member is a no-op reported in already_member. Permission incarnation.bind-member; EVERY target SID must also be inside the caller's soul scope (all-or-nothing) - otherwise 403. 422 - unknown SID or a host that is not connected.",
		Tags:        []string{"incarnation"},
		Errors:      []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{id}/members (roster read) — READ (no audit) ===

// memberListInput — huma input GET .../members. Name — path.
type memberListInput struct {
	ID string `path:"id" doc:"incarnation id"`
}

// memberListOutput — huma output GET .../members (FULL-TYPED).
type memberListOutput struct {
	Body IncarnationMemberListReply
}

// memberListOperation — metadata for GET .../members. DefaultStatus=200. READ: audit not
// wired. Permission incarnation.get (the roster needs no right of its own).
func memberListOperation() huma.Operation {
	return huma.Operation{
		OperationID: "listIncarnationMembers",
		Method:      http.MethodGet,
		Path:        "/{id}/members",
		Summary:     "List the incarnation's roster",
		Description: "Member hosts of the incarnation (incarnation_membership, ADR-008 amendment / NIM-124), with bound_at / bound_by_aid. Permission incarnation.get. Narrowed to the hosts inside the caller's soul scope. Read-only, no audit.",
		Tags:        []string{"incarnation"},
		Errors:      []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/incarnations/{id}/members/{sid} (unbind) — WRITE-SELF-AUDIT incarnation.member_unbound (204) ===

// memberUnbindInput — huma input DELETE .../members/{sid}. Name/SID — path.
type memberUnbindInput struct {
	ID  string `path:"id" doc:"incarnation id"`
	SID string `path:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$" doc:"SID (FQDN) of the host to unbind"`
}

// memberUnbindOutput — huma output DELETE .../members/{sid} (FULL-TYPED). Status=204.
type memberUnbindOutput struct {
	Status int `json:"-"`
}

// memberUnbindOperation — metadata for DELETE .../members/{sid}. DefaultStatus=204.
// Errors: 403 (no permission, or the SID is outside the caller's soul scope), 404
// incarnation, 422 bad path, 500.
func memberUnbindOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "unbindIncarnationMember",
		Method:        http.MethodDelete,
		Path:          "/{id}/members/{sid}",
		Summary:       "Unbind a host from an incarnation",
		Description:   "Removes the host from the incarnation's roster - it stops being a target of every FUTURE run (ADR-008 amendment, NIM-209). Idempotent: unbinding a non-member succeeds unchanged. Permission incarnation.unbind-member; the SID must also be inside the caller's soul scope.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
