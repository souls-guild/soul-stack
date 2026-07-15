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
	Inventory            []string       `json:"inventory" required:"true" doc:"список SID (FQDN) target-хостов (transport: ssh)"`
	Destiny              string         `json:"destiny" required:"true" doc:"ссылка на Destiny в форме <name>@<ref>"`
	Input                map[string]any `json:"input,omitempty" doc:"input для destiny"`
	SSHProvider          string         `json:"ssh_provider,omitempty" doc:"имя SshProvider; по умолчанию первый зарегистрированный"`
	CleanupStaleVersions bool           `json:"cleanup_stale_versions,omitempty" doc:"удалить устаревшие версии soul-бинаря/модулей в той же SSH-сессии"`
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
		Summary:       "Запустить push-прогон Destiny по SSH",
		Description:   "Async push-orchestrator (Variant C, ADR-004 push-flow). 202 + apply_id, далее опрос GET /v1/push/{apply_id}. Permission push.apply. Блокируется Toll при cluster:degraded (503).",
		Tags:          []string{"push"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/push/{apply_id} (get) — READ with path (no audit) ===

// pushGetInput — huma input GET /v1/push/{apply_id}. ApplyID — path (ULID; empty → 422).
type pushGetInput struct {
	ApplyID string `path:"apply_id" doc:"ULID push-прогона"`
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
		Summary:       "Состояние push-прогона",
		Description:   "Текущее состояние push-прогона по apply_id (ADR-004 push-flow). Permission push.read. Read-only, без audit (recovery-friendly при degraded).",
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
	Statuses    []string `query:"status,explode" enum:"pending,running,success,partial_failed,failed,cancelled" doc:"multi-value ?status=X&status=Y — exact-match OR; значение вне enum → 422"`
	SSHProvider string   `query:"ssh_provider" doc:"exact-match по push_runs.ssh_provider"`
	Offset      int32    `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit       int32    `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
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
		Summary:       "Список push-прогонов (paged)",
		Description:   "Глобальный реестр push-прогонов с фильтрами status/ssh_provider и пагинацией (UI-4). Permission incarnation.history. Read-only, без audit.",
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
