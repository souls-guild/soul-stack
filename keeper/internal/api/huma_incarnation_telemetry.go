package api

// GET /v1/incarnations/{name}/telemetry — агрегат host-vitals по хостам инкарнации
// (NIM-86). READ, БЕЗ audit. Existence-gate incarnation.get (как incarnation-read);
// видимость хостов сужает soul-read-scope. Reply — [handlers.IncarnationTelemetryReply].

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// incarnationTelemetryInput — huma-input GET /v1/incarnations/{name}/telemetry.
type incarnationTelemetryInput struct {
	Name string `path:"name" doc:"имя инкарнации (корневой Coven-label хостов)"`
}

// incarnationTelemetryOutput — huma-output: Body — [handlers.IncarnationTelemetryReply]
// (latest+stale на хост, без окна).
type incarnationTelemetryOutput struct {
	Body handlers.IncarnationTelemetryReply
}

// incarnationTelemetryOperation — метаданные GET /v1/incarnations/{name}/telemetry.
// DefaultStatus=200. READ-роут: audit НЕ навешан. Permission incarnation.get.
// Пустой флот / вне scope → hosts:[] (не ошибка). Errors: 403, 422 bad name, 500.
func incarnationTelemetryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationTelemetry",
		Method:        http.MethodGet,
		Path:          "/{name}/telemetry",
		Summary:       "Host-vitals хостов инкарнации",
		Description:   "Агрегат последних снимков утилизации по хостам инкарнации (latest+stale на хост, без окна) из Redis (NIM-86). Permission incarnation.get; видимость хостов — soul-read-scope. Пустой флот / вне scope → hosts:[]. Read-only, без audit.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationTelemetry монтирует GET /v1/incarnations/{name}/telemetry
// через huma (READ, БЕЗ audit). nil telemetryH → не подключается.
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
