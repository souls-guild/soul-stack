package api

// GET /v1/incarnations/{name}/telemetry — aggregate host-vitals across incarnation hosts
// (NIM-86). READ, WITHOUT audit. Existence-gate incarnation.get (like incarnation-read);
// host visibility is narrowed by soul-read-scope. Reply — [handlers.IncarnationTelemetryReply].

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// incarnationTelemetryInput — huma-input GET /v1/incarnations/{name}/telemetry.
type incarnationTelemetryInput struct {
	Name string `path:"name" doc:"incarnation name (root Coven-label of hosts)"`
}

// incarnationTelemetryOutput — huma-output: Body — [handlers.IncarnationTelemetryReply]
// (latest+stale per host, without a window).
type incarnationTelemetryOutput struct {
	Body handlers.IncarnationTelemetryReply
}

// incarnationTelemetryOperation — metadata for GET /v1/incarnations/{name}/telemetry.
// DefaultStatus=200. READ-route: audit is NOT attached. Permission incarnation.get.
// Empty souls / out of scope → hosts:[] (not an error). Errors: 403, 422 bad name, 500.
func incarnationTelemetryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationTelemetry",
		Method:        http.MethodGet,
		Path:          "/{name}/telemetry",
		Summary:       "Host-vitals of incarnation hosts",
		Description:   "Aggregate of the latest utilization snapshots across incarnation hosts (latest+stale per host, without a window) from Redis (NIM-86). Permission incarnation.get; host visibility - soul-read-scope. Empty souls / out of scope -> hosts:[]. Read-only, without audit.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationTelemetry mounts GET /v1/incarnations/{name}/telemetry
// via huma (READ, WITHOUT audit). nil telemetryH → not registered.
func registerHumaIncarnationTelemetry(humaAPI huma.API, telemetryH *handlers.TelemetryHandler) {
	if telemetryH == nil {
		return
	}
	huma.Register(humaAPI, incarnationTelemetryOperation(), func(ctx context.Context, in *incarnationTelemetryInput) (*incarnationTelemetryOutput, error) {
		reply, err := telemetryH.AggregateByIncarnation(ctx, claimsOrNil(ctx), in.Name)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incarnationTelemetryOutput{Body: reply}, nil
	})
}
