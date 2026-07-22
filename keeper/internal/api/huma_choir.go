package api

// Registration and spec-dump of the CHOIR/VOICE domain on huma full-typed (BATCH-2f
// WRITE-SELF-AUDIT following the cadence-pilot + soul/synod multi-resource template,
// ADR-054 §Pattern). create/delete/add-voice/remove-voice are WRITE-SELF-AUDIT: choir/
// voice write audit INSIDE the extracted *Typed functions (handlers/choir.go
// writeAuditCtx), with no audit middleware wired (unlike the middleware-audit domains
// role/operator). list/list-voices are read (no audit). ALL routes are mounted via
// newHumaCadenceAPI (self-audit does NOT need audit middleware). There is no MCP for
// the choir domain (known limitation).

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaChoirCreate mounts POST /v1/incarnations/{name}/choirs via huma
// (WRITE-SELF-AUDIT: choir.created is written by the handler ITSELF inside CreateTyped).
// choirH nil → no-op. Handler: claims → convert the typed body → CreateTyped (create +
// self-audit) → 201 WITH BODY.
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

// registerHumaChoirList mounts GET /v1/incarnations/{name}/choirs via huma (READ,
// no audit). choirH nil → no-op.
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

// registerHumaChoirDelete mounts DELETE /v1/incarnations/{name}/choirs/{choir} via
// huma (WRITE-SELF-AUDIT: choir.deleted is written by the handler ITSELF inside DeleteTyped).
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

// registerHumaVoiceAdd mounts POST /v1/incarnations/{name}/choirs/{choir}/voices via
// huma (WRITE-SELF-AUDIT: choir.voice_added is written by the handler ITSELF inside AddVoiceTyped).
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

// registerHumaVoiceList mounts GET /v1/incarnations/{name}/choirs/{choir}/voices via
// huma (READ, no audit).
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

// registerHumaVoiceRemove mounts DELETE /v1/incarnations/{name}/choirs/{choir}/
// voices/{sid} via huma (WRITE-SELF-AUDIT: choir.voice_removed is written by the handler ITSELF).
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

// choirMissingClaims — a defensive response for missing claims (unreachable: RequireJWT
// puts claims in place before huma). problem+json (parity with cadenceMissingClaims).
func choirMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// choirProblem delivers a *Typed function's error through huma as problem+json. The
// domain *handlers.problemError → humaProblemError; non-problem → 500 (parity with cadenceProblem).
func choirProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaChoirSpecYAML assembles the OpenAPI fragment of ALL huma-migrated choir/voice
// routes as a YAML string, WITHOUT mounting on a real router. A hook for the rollout's
// spec-merge target and the guard test. Delegates to the generic [humaDumpSpec]. Returns 3.1.0.
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
