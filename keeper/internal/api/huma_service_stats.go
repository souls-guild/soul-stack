package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

type serviceStatsInput struct{}

type serviceStatsOutput struct {
	Body ServiceStatsReply
}

// ServiceStatsEntry — one service in GET /v1/stats/services.
type ServiceStatsEntry struct {
	Service      string `json:"service" doc:"service id"`
	Incarnations int    `json:"incarnations" doc:"incarnations of this service within the caller's scope"`
	Hosts        int    `json:"hosts" doc:"distinct member hosts of those incarnations that the caller may see as Souls"`
}

// ServiceStatsReply — the body of GET /v1/stats/services.
type ServiceStatsReply struct {
	ByService []ServiceStatsEntry `json:"by_service" doc:"one entry per service with at least one incarnation in scope, sorted by service"`
}

func serviceStatsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getServiceStats",
		Method:        http.MethodGet,
		Path:          "/services",
		Summary:       "Incarnations and hosts per service (Overview)",
		Description:   "Per service: how many incarnations the caller can list (scoped like GET /v1/incarnations) and how many distinct hosts are members of them, counting only hosts inside the caller's soul.list visibility (the soul half of GET /v1/incarnations/{id}/members; incarnation.get, which gates that roster read, is not asked for here). A host bound to two incarnations of one service counts once. Services without an incarnation in scope are omitted. Fail-closed: an empty incarnation scope -> empty by_service; an empty soul scope -> hosts 0. Permission incarnation.list. Read-only, no audit.",
		Tags:          []string{"stats"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// registerHumaServiceStats mounts GET /v1/stats/services (READ aggregate, no audit).
// incH nil → no-op.
func registerHumaServiceStats(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, serviceStatsOperation(), func(ctx context.Context, _ *serviceStatsInput) (*serviceStatsOutput, error) {
		stats, err := incH.ServiceStatsTyped(ctx, claimsOrNil(ctx))
		if err != nil {
			return nil, incProblem(err)
		}
		return &serviceStatsOutput{Body: newServiceStatsReply(stats)}, nil
	})
}

func newServiceStatsReply(stats []handlers.ServiceStatsView) ServiceStatsReply {
	entries := make([]ServiceStatsEntry, len(stats))
	for i, s := range stats {
		entries[i] = ServiceStatsEntry{Service: s.Service, Incarnations: s.Incarnations, Hosts: s.Hosts}
	}
	return ServiceStatsReply{ByService: entries}
}
