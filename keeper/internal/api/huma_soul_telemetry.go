package api

// GET /v1/souls/{sid}/telemetry — host-vitals одного Soul-а (NIM-86, ADR-006).
// READ-with-path, БЕЗ audit (эталон soulprint). Reply-типы — handlers.*
// (handler возвращает их напрямую, huma эмитит схемы по имени Go-типа).

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// soulTelemetryInput — huma-input GET /v1/souls/{sid}/telemetry. SID — path.
type soulTelemetryInput struct {
	SID string `path:"sid" doc:"SID (FQDN) Soul-а"`
}

// soulTelemetryOutput — huma-output: Body — [handlers.SoulTelemetryReply]
// (latest+window+freshness).
type soulTelemetryOutput struct {
	Body handlers.SoulTelemetryReply
}

// soulTelemetryOperation — метаданные GET /v1/souls/{sid}/telemetry. DefaultStatus=
// 200. READ-роут: audit НЕ навешан. Permission soul.list (тот же read-tier, что
// get/soulprint). Errors: 403, 404 (нет soul / вне scope), 422 bad sid, 500.
func soulTelemetryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulTelemetry",
		Method:        http.MethodGet,
		Path:          "/{sid}/telemetry",
		Summary:       "Host-vitals Soul-а",
		Description:   "Последний снимок утилизации хоста (CPU/load/mem/disk) + окно для спарклайнов из Redis (NIM-86, ADR-006), со scope-гейтом. Permission soul.list. stale=true если снимок протух или данных нет (старый агент → graceful). Read-only, без audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaSoulTelemetry монтирует GET /v1/souls/{sid}/telemetry через huma
// (READ-with-path, БЕЗ audit). nil telemetryH → не подключается.
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
