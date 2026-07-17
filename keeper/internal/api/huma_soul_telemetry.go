package api

// GET /v1/souls/{sid}/telemetry - host-vitals of one Soul (NIM-86, ADR-006).
// READ-with-path, NO audit (soulprint pattern). Reply types are handlers.*
// (the handler returns them directly, huma emits schemas by Go type name).

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// soulTelemetryInput — huma-input GET /v1/souls/{sid}/telemetry. SID — path.
type soulTelemetryInput struct {
	SID string `path:"sid" doc:"SID (FQDN) of the Soul"`
}

// soulTelemetryOutput — huma-output: Body — [handlers.SoulTelemetryReply]
// (latest+window+freshness).
type soulTelemetryOutput struct {
	Body handlers.SoulTelemetryReply
}

// soulTelemetryOperation - metadata for GET /v1/souls/{sid}/telemetry. DefaultStatus=
// 200. READ route: audit is NOT attached. Permission soul.list (same read-tier as
// get/soulprint). Errors: 403, 404 (no soul / out of scope), 422 bad sid, 500.
func soulTelemetryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulTelemetry",
		Method:        http.MethodGet,
		Path:          "/{sid}/telemetry",
		Summary:       "Host-vitals of a Soul",
		Description:   "Latest host utilization snapshot (CPU/load/mem/disk) + a window for sparklines from Redis (NIM-86, ADR-006), with a scope gate. Permission soul.list. stale=true if the snapshot is stale or there is no data (old agent -> graceful). Read-only, no audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaSoulTelemetry mounts GET /v1/souls/{sid}/telemetry via huma
// (READ-with-path, NO audit). nil telemetryH -> not registered.
func registerHumaSoulTelemetry(humaAPI huma.API, telemetryH *handlers.TelemetryHandler) {
	if telemetryH == nil {
		return
	}
	huma.Register(humaAPI, soulTelemetryOperation(), func(ctx context.Context, in *soulTelemetryInput) (*soulTelemetryOutput, error) {
		reply, err := telemetryH.GetTelemetry(ctx, claimsOrNil(ctx), in.SID)
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulTelemetryOutput{Body: reply}, nil
	})
}
