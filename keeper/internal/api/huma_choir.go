package api

// Регистрация и spec-dump CHOIR/VOICE-домена на huma full-typed (БАТЧ-2f WRITE-SELF-
// AUDIT по эталону cadence pilot + soul/synod multi-resource, ADR-054 §Pattern).
// create/delete/add-voice/remove-voice — WRITE-SELF-AUDIT: choir/voice пишут audit
// ВНУТРИ извлечённых *Typed-функций (handlers/choir.go writeAuditCtx), audit-middleware
// НЕ навешан (отличие от middleware-audit-доменов role/operator). list/list-voices —
// read (БЕЗ audit). ВСЕ роуты монтируются через newHumaCadenceAPI (self-audit НЕ
// нужен audit-middleware). MCP choir-домена НЕТ (known-limitation).

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaChoirCreate монтирует POST /v1/incarnations/{name}/choirs через huma
// (WRITE-SELF-AUDIT: choir.created пишет САМ handler внутри CreateTyped). choirH nil →
// no-op. Handler: claims → конверт typed-body → CreateTyped (create + self-audit) →
// 201 С ТЕЛОМ.
func registerHumaChoirCreate(humaAPI huma.API, choirH *handlers.ChoirHandler) {
	if choirH == nil {
		return
	}
	huma.Register(humaAPI, choirCreateOperation(), func(ctx context.Context, in *choirCreateInput) (*choirCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, choirMissingClaims()
		}
		view, err := choirH.CreateTyped(ctx, claims, in.Name, handlers.ChoirCreateInput{
			ChoirName:   in.Body.ChoirName,
			Description: in.Body.Description,
			MinSize:     in.Body.MinSize,
			MaxSize:     in.Body.MaxSize,
		})
		if err != nil {
			return nil, choirProblem(err)
		}
		return &choirCreateOutput{Status: http.StatusCreated, Body: newChoir(view)}, nil
	})
}

// registerHumaChoirList монтирует GET /v1/incarnations/{name}/choirs через huma (READ,
// БЕЗ audit). choirH nil → no-op.
func registerHumaChoirList(humaAPI huma.API, choirH *handlers.ChoirHandler) {
	if choirH == nil {
		return
	}
	huma.Register(humaAPI, choirListOperation(), func(ctx context.Context, in *choirListInput) (*choirListOutput, error) {
		reply, err := choirH.ListChoirsTyped(ctx, in.Name)
		if err != nil {
			return nil, choirProblem(err)
		}
		return &choirListOutput{Body: newChoirListReply(reply)}, nil
	})
}

// registerHumaChoirDelete монтирует DELETE /v1/incarnations/{name}/choirs/{choir} через
// huma (WRITE-SELF-AUDIT: choir.deleted пишет САМ handler внутри DeleteTyped).
func registerHumaChoirDelete(humaAPI huma.API, choirH *handlers.ChoirHandler) {
	if choirH == nil {
		return
	}
	huma.Register(humaAPI, choirDeleteOperation(), func(ctx context.Context, in *choirDeleteInput) (*choirDeleteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, choirMissingClaims()
		}
		if err := choirH.DeleteTyped(ctx, claims, in.Name, in.Choir); err != nil {
			return nil, choirProblem(err)
		}
		return &choirDeleteOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaVoiceAdd монтирует POST /v1/incarnations/{name}/choirs/{choir}/voices
// через huma (WRITE-SELF-AUDIT: choir.voice_added пишет САМ handler внутри AddVoiceTyped).
func registerHumaVoiceAdd(humaAPI huma.API, choirH *handlers.ChoirHandler) {
	if choirH == nil {
		return
	}
	huma.Register(humaAPI, voiceAddOperation(), func(ctx context.Context, in *voiceAddInput) (*voiceAddOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, choirMissingClaims()
		}
		view, err := choirH.AddVoiceTyped(ctx, claims, in.Name, in.Choir, handlers.VoiceAddInput{
			SID:      in.Body.SID,
			Role:     in.Body.Role,
			Position: in.Body.Position,
		})
		if err != nil {
			return nil, choirProblem(err)
		}
		return &voiceAddOutput{Status: http.StatusCreated, Body: newVoice(view)}, nil
	})
}

// registerHumaVoiceList монтирует GET /v1/incarnations/{name}/choirs/{choir}/voices
// через huma (READ, БЕЗ audit).
func registerHumaVoiceList(humaAPI huma.API, choirH *handlers.ChoirHandler) {
	if choirH == nil {
		return
	}
	huma.Register(humaAPI, voiceListOperation(), func(ctx context.Context, in *voiceListInput) (*voiceListOutput, error) {
		reply, err := choirH.ListVoicesTyped(ctx, in.Name, in.Choir)
		if err != nil {
			return nil, choirProblem(err)
		}
		return &voiceListOutput{Body: newVoiceListReply(reply)}, nil
	})
}

// registerHumaVoiceRemove монтирует DELETE /v1/incarnations/{name}/choirs/{choir}/
// voices/{sid} через huma (WRITE-SELF-AUDIT: choir.voice_removed пишет САМ handler).
func registerHumaVoiceRemove(humaAPI huma.API, choirH *handlers.ChoirHandler) {
	if choirH == nil {
		return
	}
	huma.Register(humaAPI, voiceRemoveOperation(), func(ctx context.Context, in *voiceRemoveInput) (*voiceRemoveOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, choirMissingClaims()
		}
		if err := choirH.RemoveVoiceTyped(ctx, claims, in.Name, in.Choir, in.SID); err != nil {
			return nil, choirProblem(err)
		}
		return &voiceRemoveOutput{Status: http.StatusNoContent}, nil
	})
}

// choirMissingClaims — defensive-ответ при отсутствии claims (недостижим: RequireJWT
// кладёт claims до huma). problem+json (parity cadenceMissingClaims).
func choirMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// choirProblem доставляет ошибку *Typed-функции через huma как problem+json. Доменный
// *handlers.problemError → humaProblemError; не-problem → 500 (parity cadenceProblem).
func choirProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaChoirSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma choir/voice-
// роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-
// таргета тиража и guard-теста. Делегирует generic [humaDumpSpec]. Возвращает 3.1.0.
func HumaChoirSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ChoirSpecStub()
		registerHumaChoirCreate(api, stub)
		registerHumaChoirList(api, stub)
		registerHumaChoirDelete(api, stub)
		registerHumaVoiceAdd(api, stub)
		registerHumaVoiceList(api, stub)
		registerHumaVoiceRemove(api, stub)
		return nil
	})
}
