package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationDestroyArgs — arguments tool-а keeper.incarnation.destroy
// (schemaIncarnationDestroyInput): name + allow_destroy обязательны.
// Operator-facing allow_destroy маппится в internal force (унификация
// force↔allow_destroy): false — destroy через teardown-сценарий `destroy`;
// true — снос без teardown (force-DELETE).
type incarnationDestroyArgs struct {
	Name         string `json:"name"`
	AllowDestroy *bool  `json:"allow_destroy"`
}

// incarnationDestroyOutput — output keeper.incarnation.destroy
// (schemaApplyIDOutput): apply_id одной destroy-операции.
type incarnationDestroyOutput struct {
	ApplyID string `json:"_apply_id"`
}

// destroyForceDeleteTimeout — таймаут detached-ctx force-DELETE-а (паритет REST
// Destroy и run.go S-D3). Снос переживает возврат результата tool-а.
const destroyForceDeleteTimeout = 5 * time.Second

// callIncarnationDestroy — mutating async-tool keeper.incarnation.destroy.
// Паритет REST IncarnationHandler.Destroy (S-D4): резолв incarnation →
// PrepareDestroy (S-D2a) → Destroy (S-D1, source=mcp) → force? DeleteAfterTeardown
// (S-D3 force-путь) : StartDestroy (S-D2b teardown).
//
// allow_destroy — required bool (отсутствует/не-bool → malformed-request,
// strictUnmarshal). false и нет scenario `destroy` → validation-failed
// (PrepareDestroy). RBAC-context — {"incarnation": name} (name-bound).
//
// audit destroy_started пишет сам [incarnation.Destroy] (source=mcp, archonAID
// из JWT) — НЕ дублируем через h.writeAudit; destroy_completed (force-путь)
// пишет [incarnation.DeleteAfterTeardown].
func (h *Handler) callIncarnationDestroy(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.destroy"

	var a incarnationDestroyArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !incarnation.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+incarnation.NamePattern)
	}
	if a.AllowDestroy == nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'allow_destroy' is required (boolean confirmation flag)")
	}
	force := *a.AllowDestroy

	// destroyer / services / loader обязательны (паритет REST): без них endpoint
	// не функционален (нечем сделать pre-check снапшота и запустить teardown).
	if h.deps.ScenarioDestroyer == nil || h.deps.ServiceRegistry == nil || h.deps.ServiceLoader == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "destroy is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC при ненайденной/сбойной incarnation (паритет REST).
		if scopeErr := h.checkIncarnationScope(claims, "destroy", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.destroy")
		}
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.destroy select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// RBAC OR-Check по coven/service-scope incarnation (covens ∪ {name}) —
	// зеркало REST middleware, scope из inc.Service / inc.Covens.
	if err := h.checkIncarnationScope(claims, "destroy", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.destroy")
	}

	// S-D2a pre-check: резолв снапшота (force=true — scenario-missing-gate
	// применяем после чтения lifecycle.auto_destroy, как в REST Destroy).
	art, err := incarnation.PrepareDestroy(ctx, h.deps.ServiceRegistry, h.deps.ServiceLoader, inc, true)
	if err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.destroy prepare failed",
				slog.String("name", a.Name),
				slog.String("service", inc.Service),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// S3 enforcement: lifecycle.auto_destroy=false → удаление ВСЕГДА прямое
	// (приоритет над allow_destroy). effectiveForce = оператор форсит ИЛИ сервис
	// запретил teardown.
	autoDestroy := true
	if art != nil && art.Manifest != nil {
		autoDestroy = art.Manifest.Lifecycle.AutoDestroyEnabled()
	}
	effectiveForce := force || !autoDestroy

	// effectiveForce=false → нужен teardown: scenario `destroy` обязан быть в
	// снапшоте, иначе validation-failed ДО перехода в destroying.
	if !effectiveForce {
		hasScenario, herr := incarnation.HasDestroyScenario(h.deps.ServiceLoader, art)
		if herr != nil {
			h.deps.Logger.Error("mcp: incarnation.destroy scenario probe failed",
				slog.String("name", a.Name),
				slog.String("service", inc.Service),
				slog.Any("error", herr),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "prepare incarnation destroy failed")
		}
		if !hasScenario {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"service "+inc.Service+" has no `destroy` scenario — pass allow_destroy=true to force destroy without teardown")
		}
	}

	applyID := audit.NewULID()

	// S-D1: перевод в destroying + audit destroy_started (source=mcp). AuditWriter
	// сужен до mcp.AuditWriter, структурно = audit.Writer (Destroy примет его).
	if _, err := incarnation.Destroy(ctx, h.deps.IncarnationDB, h.deps.AuditWriter, a.Name, effectiveForce,
		audit.SourceMCP, claims.Subject, applyID, h.deps.Logger); err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.destroy transition failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// effectiveForce=true → S-D3 force-путь: снос строки напрямую (teardown
	// пропущен; allow_destroy=true ИЛИ lifecycle.auto_destroy=false). Detached-ctx
	// — снос переживает возврат tool-результата.
	if effectiveForce {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), destroyForceDeleteTimeout)
		defer cancel()
		if _, err := incarnation.DeleteAfterTeardown(dctx, h.deps.IncarnationDB, h.deps.AuditWriter, a.Name, effectiveForce, h.deps.Logger); err != nil {
			h.deps.Logger.Error("mcp: incarnation.destroy force delete failed",
				slog.String("name", a.Name),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "force-destroy delete failed")
		}
		return h.toolResult(req.ID, incarnationDestroyOutput{ApplyID: applyID})
	}

	// effectiveForce=false → S-D2b: async-teardown scenario `destroy` (TerminalDestroy).
	serviceRef, ok := h.deps.ServiceRegistry.Resolve(inc.Service)
	if !ok {
		// Гонка дерегистрации сервиса между pre-check и стартом teardown.
		h.deps.Logger.Error("mcp: incarnation.destroy service deregistered between prepare and teardown",
			slog.String("name", a.Name), slog.String("service", inc.Service))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "service "+inc.Service+" is not registered")
	}
	serviceRef.Ref = inc.ServiceVersion
	if err := h.deps.ScenarioDestroyer.StartDestroy(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: a.Name,
		ServiceRef:      serviceRef,
		StartedByAID:    claims.Subject,
	}); err != nil {
		h.deps.Logger.Error("mcp: incarnation.destroy teardown start failed",
			slog.String("name", a.Name),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "start destroy teardown failed")
	}

	return h.toolResult(req.ID, incarnationDestroyOutput{ApplyID: applyID})
}
