package api

// FULL-TYPED form of the PUSH domain (code-first source of OpenAPI, ADR-054 §Pattern).
// ROLLOUT BATCH 2e (push entirely on huma following the operator issue-token + audit-endpoint patterns):
// apply — WRITE+AUDIT (variant B, event push.applied; 202+body async — apply_id, symmetric
// with operator issue-token 200+body, differing only in Status=202); get — read-with-path; push-runs —
// read-with-typed-query (offset/limit→400, status enum→422, ssh_provider string). Go types —
// the single source of truth.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === POST /v1/push/apply (apply) — WRITE+AUDIT push.applied (202 async) ===

// pushApplyInput — huma input POST /v1/push/apply (FULL-TYPED). Body — the typed body.
type pushApplyInput struct {
	Body PushApplyRequest
}

// PushApplyRequest — the Go form of the POST /v1/push/apply body (code-first source of the schema AND
// validation). inventory (SID[] target hosts) + destiny (<name>@<ref>) + optional
// input/ssh_provider/cleanup_stale_versions. Empty inventory / empty destiny is
// domain validation (422 in ApplyTyped). additionalProperties:false (huma default) →
// unknown body field → 400. The struct name = the contract schema name in OpenAPI (huma
// DefaultSchemaNamer takes reflect.Type.Name() directly) — aligned with the committed
// hand-written spec (rollout N3). The register func projects into native handlers.PushApplyInput
// (toPushApplyInput).
type PushApplyRequest struct {
	Inventory            []string       `json:"inventory" required:"true" doc:"list of target SID (FQDN) hosts (transport: ssh)"`
	Destiny              string         `json:"destiny" required:"true" doc:"reference to Destiny in the form <name>@<ref>"`
	Input                map[string]any `json:"input,omitempty" doc:"input for destiny"`
	SSHProvider          string         `json:"ssh_provider,omitempty" doc:"SshProvider name; defaults to the first registered one"`
	CleanupStaleVersions bool           `json:"cleanup_stale_versions,omitempty" doc:"remove stale soul-binary/module versions in the same SSH session"`
}

// pushApplyOutput — huma output POST /v1/push/apply (FULL-TYPED). Status=202 (async
// Accepted); Body — native PushApplyReply (apply_id). The client polls
// GET /v1/push/{apply_id}.
type pushApplyOutput struct {
	Status int `json:"-"`
	Body   PushApplyReply
}

// pushApplyOperation — metadata for POST /v1/push/apply. Path = "/apply" relative to
// the chi group /v1/push. DefaultStatus=202. Permission push.apply + audit push.applied.
// Toll DegradedMiddleware (503 on cluster:degraded) — on the chi group BEFORE huma (router.go).
// Errors: 400 unknown/malformed, 403 RBAC, 422 empty inventory/broken destiny-ref, 500.
func pushApplyOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "pushApply",
		Method:        http.MethodPost,
		Path:          "/apply",
		Summary:       "Run a Destiny push over SSH",
		Description:   "Async push-orchestrator (Variant C, ADR-004 push-flow). 202 + apply_id, then poll GET /v1/push/{apply_id}. Permission push.apply. Blocked by Toll on cluster:degraded (503).",
		Tags:          []string{"push"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/push/{apply_id} (get) — READ with path (no audit) ===

// pushGetInput — huma input GET /v1/push/{apply_id}. ApplyID — path (ULID; empty → 422).
type pushGetInput struct {
	ApplyID string `path:"apply_id" doc:"ULID of the push run"`
}

// pushGetOutput — huma output GET /v1/push/{apply_id} (FULL-TYPED). Body — the native 200 body
// (PushApplyView).
type pushGetOutput struct {
	Body PushApplyView
}

// pushGetOperation — metadata for GET /v1/push/{apply_id}. DefaultStatus=200. READ route:
// audit not wired. Permission push.read. Errors: 403, 404 (no apply_id), 422 empty id, 500.
func pushGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "pushGet",
		Method:        http.MethodGet,
		Path:          "/{apply_id}",
		Summary:       "Push run status",
		Description:   "Current state of a push run by apply_id (ADR-004 push-flow). Permission push.read. Read-only, no audit (recovery-friendly when degraded).",
		Tags:          []string{"push"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/push-runs (list) — READ with typed query (no audit) ===

// pushRunsListInput — huma input GET /v1/push-runs (FULL-TYPED typed query). Statuses —
// multi-value (?status=X&status=Y) exact-match OR; the enum set = the FULL domain
// pushorch.PushRunStatus (a value outside the set → 422). explode:true is MANDATORY (the huma default
// for a query array is explode=false: it would read comma-separated as one value → a broken OR).
// SSHProvider — exact-match string. offset/limit — int32 with a default; CheckPageBounds
// enforces the range in ListRunsTyped → 400 (NOT huma min/max — parity with ParsePage); bad-int → 400.
type pushRunsListInput struct {
	Statuses    []string `query:"status,explode" enum:"pending,running,success,partial_failed,failed,cancelled" doc:"multi-value ?status=X&status=Y — exact-match OR; value outside enum → 422"`
	SSHProvider string   `query:"ssh_provider" doc:"exact-match on push_runs.ssh_provider"`
	Offset      int32    `query:"offset" default:"0" doc:"offset from start of set, ≥0 (matches shared/api.ParsePage; out-of-range → 400)"`
	Limit       int32    `query:"limit" default:"50" doc:"page size 1..1000 (matches shared/api.ParsePage; out-of-range → 400)"`
}

// pushRunsListOutput — huma output GET /v1/push-runs (FULL-TYPED). Body — the native
// 200 envelope (PushRunListReply: items/offset/limit/total). The wire shape is pinned by a
// golden test.
type pushRunsListOutput struct {
	Body PushRunListReply
}

// pushRunsListOperation — metadata for GET /v1/push-runs. Path = "/push-runs" relative to
// the chi group /v1 (the full sub-/v1 path — a distinct path for the spec dump). DefaultStatus=200.
// READ route: audit not wired. Permission incarnation.history. Errors: 400 (out-of-range
// pagination), 403 RBAC, 422 (bad status enum), 500.
func pushRunsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listPushRuns",
		Method:        http.MethodGet,
		Path:          "/push-runs",
		Summary:       "List push runs (paged)",
		Description:   "Global registry of push runs with status/ssh_provider filters and pagination (UI-4). Permission incarnation.history. Read-only, no audit.",
		Tags:          []string{"push"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// toPushApplyInput — converts the typed huma body → NATIVE request of the push domain
// (handlers.PushApplyInput). The domain handler dereferences pointer-optional fields;
// the huma form is value/slice. Empty → nil (the handler treats nil as "not set", legacy parity).
func toPushApplyInput(b PushApplyRequest) handlers.PushApplyInput {
	out := handlers.PushApplyInput{
		Inventory: b.Inventory,
		Destiny:   b.Destiny,
	}
	if b.Input != nil {
		in := b.Input
		out.Input = &in
	}
	if b.SSHProvider != "" {
		v := b.SSHProvider
		out.SSHProvider = &v
	}
	if b.CleanupStaleVersions {
		v := true
		out.CleanupStaleVersions = &v
	}
	return out
}
