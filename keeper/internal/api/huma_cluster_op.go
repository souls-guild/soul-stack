package api

// FULL-TYPED форма CLUSTER-домена (GET /v1/cluster, code-first источник OpenAPI).
// READ-роут (БЕЗ audit): HA-список Keeper-инстансов из Conclave (ADR-006 amend) +
// self-health текущего инстанса. Permission soul.list (то же право чтения реестра;
// нового права не заводим). Тонкий конверт: register-handler зовёт извлечённую
// доменную ClusterHandler.GetTyped, проецирует reply → native wire-DTO.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// clusterGetInput — huma-input GET /v1/cluster. Параметров нет: view cluster-wide
// (без per-SID scope), self определяется по конфигу инстанса.
type clusterGetInput struct{}

// clusterGetOutput — huma-output GET /v1/cluster (FULL-TYPED). Body — native DTO
// (clusterReply: instances[] + self_kid + self_health).
type clusterGetOutput struct {
	Body clusterReply
}

// clusterGetOperation — метаданные GET /v1/cluster. Path = "/cluster" относительно
// группы /v1. DefaultStatus=200. READ-роут: audit НЕ навешан. Permission soul.list.
// Errors: 403 RBAC, 500.
func clusterGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getCluster",
		Method:        http.MethodGet,
		Path:          "/cluster",
		Summary:       "HA-топология Keeper-кластера",
		Description:   "Живые Keeper-инстансы из Conclave-реестра (kid + started_at + alive + is_reaper_leader) + self_kid + self_health (postgres/redis/vault текущего инстанса). Permission soul.list. Read-only, без audit. Версия агента (soul) НЕ включается.",
		Tags:          []string{"cluster"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}
