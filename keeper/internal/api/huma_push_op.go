package api

// FULL-TYPED форма PUSH-домена (code-first источник OpenAPI, ADR-054 §Pattern).
// ТИРАЖ-БАТЧ-2e (push целиком на huma по эталонам operator issue-token + audit-endpoint):
// apply — WRITE+AUDIT (вариант B, event push.applied; 202+body async — apply_id, симметрия
// с operator issue-token 200+body, отличие лишь Status=202); get — read-with-path; push-runs —
// read-with-typed-query (offset/limit→400, status enum→422, ssh_provider string). Go-типы —
// единственный источник правды.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === POST /v1/push/apply (apply) — WRITE+AUDIT push.applied (202 async) ===

// pushApplyInput — huma-input POST /v1/push/apply (FULL-TYPED). Body — типизированное тело.
type pushApplyInput struct {
	Body PushApplyRequest
}

// PushApplyRequest — Go-форма тела POST /v1/push/apply (code-first источник схемы И
// валидации). inventory (SID[] target-хостов) + destiny (<name>@<ref>) + опц.
// input/ssh_provider/cleanup_stale_versions. Пустой inventory / пустой destiny —
// доменная валидация (422 в ApplyTyped). additionalProperties:false (huma-дефолт) →
// unknown поле тела → 400. Имя структуры = контрактное имя схемы в OpenAPI (huma
// DefaultSchemaNamer берёт reflect.Type.Name() напрямую) — выровнено под committed-
// рукопись (тираж N3). Register-func проецирует в native handlers.PushApplyInput
// (toPushApplyInput).
type PushApplyRequest struct {
	Inventory            []string       `json:"inventory" required:"true" doc:"список SID (FQDN) target-хостов (transport: ssh)"`
	Destiny              string         `json:"destiny" required:"true" doc:"ссылка на Destiny в форме <name>@<ref>"`
	Input                map[string]any `json:"input,omitempty" doc:"input для destiny"`
	SSHProvider          string         `json:"ssh_provider,omitempty" doc:"имя SshProvider; по умолчанию первый зарегистрированный"`
	CleanupStaleVersions bool           `json:"cleanup_stale_versions,omitempty" doc:"удалить устаревшие версии soul-бинаря/модулей в той же SSH-сессии"`
}

// pushApplyOutput — huma-output POST /v1/push/apply (FULL-TYPED). Status=202 (async
// Accepted); Body — native PushApplyReply (apply_id). Клиент опрашивает
// GET /v1/push/{apply_id}.
type pushApplyOutput struct {
	Status int `json:"-"`
	Body   PushApplyReply
}

// pushApplyOperation — метаданные POST /v1/push/apply. Path = "/apply" относительно
// chi-группы /v1/push. DefaultStatus=202. Permission push.apply + audit push.applied.
// Toll DegradedMiddleware (503 при cluster:degraded) — на chi-группе ДО huma (router.go).
// Errors: 400 unknown/malformed, 403 RBAC, 422 пустой inventory/битый destiny-ref, 500.
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

// === GET /v1/push/{apply_id} (get) — READ-with-path (БЕЗ audit) ===

// pushGetInput — huma-input GET /v1/push/{apply_id}. ApplyID — path (ULID; пустой → 422).
type pushGetInput struct {
	ApplyID string `path:"apply_id" doc:"ULID push-прогона"`
}

// pushGetOutput — huma-output GET /v1/push/{apply_id} (FULL-TYPED). Body — native 200-тело
// (PushApplyView).
type pushGetOutput struct {
	Body PushApplyView
}

// pushGetOperation — метаданные GET /v1/push/{apply_id}. DefaultStatus=200. READ-роут:
// audit НЕ навешан. Permission push.read. Errors: 403, 404 (нет apply_id), 422 пустой id, 500.
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

// === GET /v1/push-runs (list) — READ-with-typed-query (БЕЗ audit) ===

// pushRunsListInput — huma-input GET /v1/push-runs (FULL-TYPED typed-query). Statuses —
// multi-value (?status=X&status=Y) exact-match OR; enum-набор = ПОЛНЫЙ домен
// pushorch.PushRunStatus (значение вне набора → 422). explode:true ОБЯЗАТЕЛЕН (huma-дефолт
// query-array — explode=false: читал бы comma-separated как одно значение → сломанный OR).
// SSHProvider — exact-match string. offset/limit — int32 с default; диапазон enforce-ит
// CheckPageBounds в ListRunsTyped → 400 (НЕ huma min/max — parity ParsePage); bad-int → 400.
type pushRunsListInput struct {
	Statuses    []string `query:"status,explode" enum:"pending,running,success,partial_failed,failed,cancelled" doc:"multi-value ?status=X&status=Y — exact-match OR; значение вне enum → 422"`
	SSHProvider string   `query:"ssh_provider" doc:"exact-match по push_runs.ssh_provider"`
	Offset      int32    `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit       int32    `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// pushRunsListOutput — huma-output GET /v1/push-runs (FULL-TYPED). Body — native
// 200-envelope (PushRunListReply: items/offset/limit/total). Wire-форма зафиксирована
// golden-тестом.
type pushRunsListOutput struct {
	Body PushRunListReply
}

// pushRunsListOperation — метаданные GET /v1/push-runs. Path = "/push-runs" относительно
// chi-группы /v1 (полный под-/v1 путь — distinct-path для spec-dump). DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission incarnation.history. Errors: 400 (out-of-range
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

// toPushApplyInput — конверт typed huma-body → NATIVE request push-домена
// (handlers.PushApplyInput). Доменный handler разыменовывает pointer-optional поля;
// huma-форма — value/slice. Пустые → nil (handler трактует nil как «не задано», parity легаси).
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
