package api

// PILOT-2 разворота OpenAPI spec-first → code-first на huma v2, FULL-TYPED форма
// (ADR-054 §Audit). Доказывает РОУТ POST /v1/roles поверх chi-mux через huma как
// ЭТАЛОН тиража ~30 middleware-audit-доменов: тот же FULL-TYPED-конверт, что pilot-1
// (cadence), ПЛЮС huma-native audit-middleware (вариант B, huma_audit.go) — потому
// что role писал audit через apimiddleware.Audit + SetAuditPayload, а full-typed
// huma САМ пишет ответ (StatusRecorder к нему неприменим). Pilot-1 cadence писал
// self-audit ВНУТРИ CreateTyped (emitWrite) и middleware-audit не имел — для тиража
// доменов с middleware-audit нужен именно вариант B.
//
// Граница та же, что pilot-1 (huma_cadence.go §FULL-TYPED PATTERN): typed input +
// извлечённая CreateTyped + тонкий конверт + typed output (без Body — 201 пустое,
// легаси-контракт). Отличие — payload кладётся на huma-ctx через
// SetHumaAuditPayload (а не на *http.Request через SetAuditPayload), middleware
// читает после next.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaRole монтирует POST /v1/roles через huma на переданный chi.Router
// (та группа, что уже несёт RequireJWT/RequirePermission(role.create) + huma-audit-
// middleware). roleH — доменный handler; nil → no-op (паттерн opt-in-домена
// router.go: роут подключается только при non-nil roleH).
//
// FULL-TYPED handler: huma валидирует typed Body → конверт в доменную модель →
// CreateTyped → audit-payload на huma-ctx (SetHumaAuditPayload, читает
// humaAuditMiddleware после next) → пустой typed output (201). Доменные problem-
// ошибки — через humaProblemError (тот же error-контракт, что huma-валидация).
func registerHumaRole(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleCreateOperation(), func(ctx context.Context, in *roleCreateInput) (*roleCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, roleMissingClaims()
		}
		reply, err := roleH.CreateTyped(ctx, claims, handlers.RoleCreateInput{
			Name:         in.Body.Name,
			Description:  in.Body.Description,
			Permissions:  in.Body.Permissions,
			DefaultScope: in.Body.DefaultScope,
		})
		if err != nil {
			return nil, roleProblem(err)
		}
		// Audit-payload на huma-ctx: humaAuditMiddleware (вариант B) seed-ит carrier
		// ДО next, читает payload ПОСЛЕ. Поля — parity легаси SetAuditPayload
		// (name + permissions + created_by_aid; без секретов, ADR-022).
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &roleCreateOutput{Status: http.StatusCreated}, nil
	})
}

// registerHumaRoleList монтирует GET /v1/roles через huma на chi-группе
// /v1/roles (READ-вариант pilot-1 — full-typed output, БЕЗ audit-middleware).
// roleH nil → no-op. Handler читает каталог (ListTyped) → конверт в typed output;
// ошибка чтения → roleProblem (500). RBAC role.list — на группе (huma наследует).
func registerHumaRoleList(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleListOperation(), func(ctx context.Context, _ *roleListInput) (*roleListOutput, error) {
		reply, err := roleH.ListTyped(ctx)
		if err != nil {
			return nil, roleProblem(err)
		}
		return &roleListOutput{Body: newRoleListReply(reply)}, nil
	})
}

// registerHumaRoleDelete монтирует DELETE /v1/roles/{name} через huma (WRITE+AUDIT
// вариант B — event role.deleted навешан newHumaAuditAPI на группе). roleH nil →
// no-op. Handler: DeleteTyped → audit-payload на huma-ctx → пустой 204-output.
func registerHumaRoleDelete(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleDeleteOperation(), func(ctx context.Context, in *roleDeleteInput) (*roleNoContentOutput, error) {
		reply, err := roleH.DeleteTyped(ctx, in.Name)
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{"name": reply.Name})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaRoleUpdatePermissions монтирует PATCH /v1/roles/{name}/permissions
// через huma (WRITE+AUDIT — event role.permissions-updated). roleH nil → no-op.
// Handler: claims → конверт presence default_scope из [Optional] в доменные
// SetDefaultScope/DefaultScope (omitted→Set=false не трогать; null→Set=true сброс;
// value→Set=true установка) → UpdatePermissionsTyped → audit-payload → 204.
func registerHumaRoleUpdatePermissions(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleUpdatePermissionsOperation(), func(ctx context.Context, in *roleUpdatePermissionsInput) (*roleNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, roleMissingClaims()
		}
		reply, err := roleH.UpdatePermissionsTyped(ctx, claims, handlers.UpdatePermissionsInput{
			Name:            in.Name,
			Permissions:     in.Body.Permissions,
			SetDefaultScope: in.Body.DefaultScope.Set,
			DefaultScope:    optionalToPtr(in.Body.DefaultScope),
		})
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{
			"name":        reply.Name,
			"permissions": reply.Permissions,
		})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaRoleGrantOperator монтирует POST /v1/roles/{name}/operators через
// huma (WRITE+AUDIT — event role.operator-granted). roleH nil → no-op. Handler:
// claims → GrantOperatorTyped (валидация AID + привязка) → audit-payload → 204.
func registerHumaRoleGrantOperator(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleGrantOperatorOperation(), func(ctx context.Context, in *roleGrantOperatorInput) (*roleNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, roleMissingClaims()
		}
		reply, err := roleH.GrantOperatorTyped(ctx, claims, in.Name, in.Body.AID)
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{
			"name":           reply.Name,
			"aid":            reply.AID,
			"granted_by_aid": reply.GrantedByAID,
		})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaRoleRevokeOperator монтирует DELETE /v1/roles/{name}/operators/{aid}
// через huma (WRITE+AUDIT — event role.operator-revoked). roleH nil → no-op.
// Handler: RevokeOperatorTyped (валидация path-AID + снятие) → audit-payload → 204.
func registerHumaRoleRevokeOperator(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleRevokeOperatorOperation(), func(ctx context.Context, in *roleRevokeOperatorInput) (*roleNoContentOutput, error) {
		reply, err := roleH.RevokeOperatorTyped(ctx, in.Name, in.AID)
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{
			"name": reply.Name,
			"aid":  reply.AID,
		})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// roleMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). Отдаётся как problem+json (а не huma.NewError),
// чтобы defensive-путь эталона нёс тот же error-контракт, что прочие ошибки роутов.
func roleMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// roleProblem доставляет ошибку CreateTyped через huma как problem+json. Доменная
// *handlers.problemError → humaProblemError (его Details, статус из таблицы). Не-
// problem (нештатный путь) → 500 internal (parity cadenceProblem).
func roleProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaRoleSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma role-роутов
// (create/list/delete/update-permissions/grant/revoke-operator) как YAML-строку, БЕЗ
// монтирования на реальный router. Хук для спека-мерж-таргета тиража и guard-теста.
// Делегирует generic [humaDumpSpec], регистрируя операции через те же register-
// функции (единый register-путь, нет дубля dump-vs-mount): handler-заглушка при dump
// не вызывается; audit-навеска не нужна (newHumaCadenceAPI без UseMiddleware
// достаточно для схемы). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaRoleSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.RoleSpecStub()
		registerHumaRole(api, stub)
		registerHumaRoleList(api, stub)
		registerHumaRoleDelete(api, stub)
		registerHumaRoleUpdatePermissions(api, stub)
		registerHumaRoleGrantOperator(api, stub)
		registerHumaRoleRevokeOperator(api, stub)
		return nil
	})
}

// newHumaRoleAPI собирает huma.API поверх chi-группы /v1/roles с huma-audit-
// middleware (вариант B) под переданный event-тип. Параллель newHumaCadenceAPI, но
// role пишет audit СНАРУЖИ *Typed (через middleware) — cadence писал self-audit
// внутри. evt параметризован: каждый write-роут role (create/delete/update/grant/
// revoke) монтируется на СВОЕЙ chi-группе с собственным event-типом.
func newHumaRoleAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}
