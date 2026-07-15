package api

// FULL-TYPED shape of the CLUSTER domain (GET /v1/cluster, code-first OpenAPI
// source). READ route (no audit): HA list of Keeper instances from the Conclave
// (ADR-006 amend) + self-health of the current instance. Permission soul.list
// (the same registry-read right; no new right is introduced). Thin envelope: the
// register handler calls the extracted domain ClusterHandler.GetTyped and
// projects reply → native wire-DTO.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// clusterGetInput — huma input for GET /v1/cluster. No parameters: a cluster-wide
// view (no per-SID scope), self is determined by the instance config.
type clusterGetInput struct{}

// clusterGetOutput — huma output for GET /v1/cluster (FULL-TYPED). Body — native
// DTO (clusterReply: instances[] + self_kid + self_health).
type clusterGetOutput struct {
	Body clusterReply
}

// clusterGetOperation — metadata for GET /v1/cluster. Path = "/cluster" relative
// to the /v1 group. DefaultStatus=200. READ route: audit not wired. Permission
// soul.list. Errors: 403 RBAC, 500.
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
