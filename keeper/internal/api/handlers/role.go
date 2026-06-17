// Role-handler-ы Operator API (RBAC Фаза 2, Slice 2a) — доменный слой над
// [rbac.Service]. *Typed-функции несут бизнес-логику без http.ResponseWriter/
// *http.Request; HTTP обслуживает huma full-typed (api/huma_role.go), MCP зовёт
// rbac.Service напрямую (мимо handler).
//
// T5d (handler-native): домен role отвязан от legacy-генерата. *Typed-функции принимают
// NATIVE request-типы (огранизованы huma-input-ом в пакете api) и возвращают
// доменные result-ы с ПЛОСКИМИ wire-полями — native wire-DTO (схему OpenAPI)
// строит пакет api из этих полей. (w,r)-оболочки сняты.
//
// Бизнес-логика (builtin-граница, self-lockout, валидация name/permission) —
// в [rbac.Service]; handler только маппит sentinel-ошибки в RFC 7807. RBAC-
// проверка — в middleware (см. api/router.go), здесь её нет.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// RoleHandler — шесть endpoint-ов RBAC-CRUD (роли / permissions / membership).
// Делегирует бизнес-логику в [rbac.Service].
//
// Все зависимости immutable; safe for concurrent use — состояние между
// запросами не держит.
type RoleHandler struct {
	svc    *rbac.Service
	logger *slog.Logger
}

// NewRoleHandler создаёт handler. svc обязателен (паника при nil —
// единственная точка misconfiguration, caller обязан передать non-nil).
func NewRoleHandler(svc *rbac.Service, logger *slog.Logger) *RoleHandler {
	if svc == nil {
		panic("handlers.NewRoleHandler: rbac.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &RoleHandler{svc: svc, logger: logger}
}

// RoleCreateInput — NATIVE request-форма POST /v1/roles (handler-native T5d).
// Заменяет RoleCreateRequest: huma-input (пакет api) биндит и валидирует
// тело по своим полям, затем зовёт CreateTyped с этой плоской моделью.
// default_scope опционален (ADR-047 S1): nil → роль без scope-ограничения
// (bare-perms unrestricted). Description/Permissions — value/slice (пустые
// трактуются как «не задано», parity легаси-декода).
type RoleCreateInput struct {
	Name         string
	Description  string
	Permissions  []string
	DefaultScope *string
}

// RoleView — ПЛОСКАЯ доменная проекция роли (GET /v1/roles items[]), handler-
// native T5d. Пакет api проецирует её в native-схему RoleView (register-func).
// DefaultScope/Description — RAW string из домена (пустая = NULL/без значения);
// nullable-форму wire (omitempty) держит native-тип в api.
type RoleView struct {
	Name         string
	Description  string
	Builtin      bool
	Permissions  []string
	Operators    []string
	DefaultScope string
}

// RoleListPage — доменный список ролей GET /v1/roles (handler-native T5d). Пакет
// api проецирует Items → native RoleListReply (БЕЗ пагинации, role.list отдаёт
// весь каталог).
type RoleListPage struct {
	Items []RoleView
}

// RoleSpecStub — непустой *RoleHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaRoleSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [CadenceSpecStub]).
func RoleSpecStub() *RoleHandler {
	return &RoleHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// RoleCreateReply — результат успешного [RoleHandler.CreateTyped] (handler-native
// T5d). 201-тело role.create ПУСТОЕ (легаси-контракт: openapi.yaml `POST /v1/roles`
// отдаёт 201 без `content`), поэтому reply несёт не wire-поля ответа, а МЕТАДАННЫЕ
// для audit-payload (имя роли, набор permissions, AID создателя) — huma-обёртка
// кладёт их на huma-ctx через [middleware.SetHumaAuditPayload], а humaAuditMiddleware
// пишет в audit-event после успешного next (вариант B, см. api/huma_audit.go).
type RoleCreateReply struct {
	Name         string
	Permissions  []string
	CreatedByAID string
}

// AuditPayload собирает audit-payload create-роута (parity легаси SetAuditPayload).
// ЕДИНЫЙ источник для huma-варианта B.
func (r RoleCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"permissions":    r.Permissions,
		"created_by_aid": r.CreatedByAID,
	}
}

// CreateTyped — доменная функция POST /v1/roles (handler-native T5d): бизнес-логика
// без http.ResponseWriter/*http.Request. claims и req приходят аргументами; ошибки —
// *problemError (доставляются huma-обёрткой через [AsProblemDetails]), успех —
// [RoleCreateReply].
//
// Шаги: required name → svc.CreateRole (валидация name/permission/default_scope +
// RBAC subset-check + persist) → sentinel→problem. Audit-payload НЕ пишется здесь —
// его несёт reply; запись делает huma-audit-middleware. 201-тело пустое.
func (h *RoleHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req RoleCreateInput) (RoleCreateReply, error) {
	var zero RoleCreateReply
	if req.Name == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'name' is required")}
	}

	perms := req.Permissions
	err := h.svc.CreateRole(ctx, rbac.CreateRoleInput{
		Name:         req.Name,
		Description:  req.Description,
		Permissions:  perms,
		CallerAID:    claims.Subject,
		DefaultScope: req.DefaultScope,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeRoleExists, "", "role "+req.Name+" already exists")}
	case errors.Is(err, rbac.ErrInvalidRoleName):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a permission you do not hold yourself")}
	case isInvalidPermission(err) || isInvalidDefaultScope(err):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("role.create: service failed",
			slog.String("name", req.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create role failed")}
	}

	return RoleCreateReply{
		Name:         req.Name,
		Permissions:  perms,
		CreatedByAID: claims.Subject,
	}, nil
}

// ListTyped — доменная функция GET /v1/roles (handler-native T5d, READ без audit):
// читает каталог ролей и собирает [RoleListPage] (плоские RoleView) без http.
// ResponseWriter/*http.Request. Ошибка чтения каталога → *problemError (500);
// huma-обёртка доставляет её через [AsProblemDetails]. Wire-форму items (Description
// всегда, DefaultScope nil→пропуск, []-vs-null) строит native-проекция в api.
func (h *RoleHandler) ListTyped(ctx context.Context) (RoleListPage, error) {
	views, err := h.svc.ListRoles(ctx)
	if err != nil {
		h.logger.Error("role.list: service failed", slog.Any("error", err))
		return RoleListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list roles failed")}
	}

	items := make([]RoleView, 0, len(views))
	for _, v := range views {
		items = append(items, toRoleView(v))
	}
	return RoleListPage{Items: items}, nil
}

// RoleNameReply — результат write-операций, чей audit-payload несёт лишь имя роли
// (delete). 204-тело пустое; reply — МЕТАДАННЫЕ для audit (huma-обёртка кладёт на
// huma-ctx, middleware пишет после успеха; (w,r)-оболочка — через SetAuditPayload).
type RoleNameReply struct {
	Name string
}

// DeleteTyped — извлечённая доменная функция DELETE /v1/roles/{name} (FULL-TYPED
// разворот ADR-054 §Pattern (б)): бизнес-логика без http.ResponseWriter/*http.
// Request. name приходит аргументом (path-извлечение — на вызывающем слое); ошибки
// — *problemError, успех — [RoleNameReply] (audit-payload). 204-тело пустое.
func (h *RoleHandler) DeleteTyped(ctx context.Context, name string) (RoleNameReply, error) {
	var zero RoleNameReply
	err := h.svc.DeleteRole(ctx, name)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+name+" not found")}
	case errors.Is(err, rbac.ErrRoleBuiltin):
		return zero, &problemError{problem.New(problem.TypeRoleBuiltin, "", "role "+name+" is builtin and cannot be deleted")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "deleting role "+name+" would lock out the cluster")}
	default:
		h.logger.Error("role.delete: service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete role failed")}
	}
	return RoleNameReply{Name: name}, nil
}

// UpdatePermissionsInput — параметры [RoleHandler.UpdatePermissionsTyped]
// (FULL-TYPED разворот ADR-054 §Pattern). SetDefaultScope несёт presence-флаг
// ключа default_scope (omitted vs explicit null → разная PATCH-семантика): true →
// заменить scope значением DefaultScope (nil снимает); false → scope не трогать.
// Вычисление presence — на вызывающем слое (huma-конверт по raw body, (w,r)-
// оболочка по jsonHasKey).
type UpdatePermissionsInput struct {
	Name            string
	Permissions     []string
	SetDefaultScope bool
	DefaultScope    *string
}

// RolePermissionsReply — результат [RoleHandler.UpdatePermissionsTyped]:
// МЕТАДАННЫЕ для audit-payload (имя роли + новый набор permissions). 204-тело пустое.
type RolePermissionsReply struct {
	Name        string
	Permissions []string
}

// UpdatePermissionsTyped — извлечённая доменная функция PATCH /v1/roles/{name}/
// permissions (FULL-TYPED разворот ADR-054 §Pattern (б)): replace-семантика
// permissions + опц. замена default_scope, без http.ResponseWriter/*http.Request.
// claims/in приходят аргументами (декод/presence-детект/auth — на вызывающем слое);
// ошибки — *problemError, успех — [RolePermissionsReply] (audit-payload).
func (h *RoleHandler) UpdatePermissionsTyped(ctx context.Context, claims *jwt.Claims, in UpdatePermissionsInput) (RolePermissionsReply, error) {
	var zero RolePermissionsReply
	err := h.svc.UpdateRolePermissions(ctx, rbac.UpdateRolePermissionsInput{
		Name:            in.Name,
		Permissions:     in.Permissions,
		CallerAID:       claims.Subject,
		SetDefaultScope: in.SetDefaultScope,
		DefaultScope:    in.DefaultScope,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+in.Name+" not found")}
	case errors.Is(err, rbac.ErrRoleBuiltin):
		return zero, &problemError{problem.New(problem.TypeRoleBuiltin, "", "role "+in.Name+" is builtin and cannot be updated")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "updating role "+in.Name+" would lock out the cluster")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a permission you do not hold yourself")}
	case isInvalidPermission(err) || isInvalidDefaultScope(err):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("role.update: service failed",
			slog.String("name", in.Name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update role permissions failed")}
	}
	return RolePermissionsReply{Name: in.Name, Permissions: in.Permissions}, nil
}

// RoleOperatorReply — результат grant/revoke-operator: МЕТАДАННЫЕ для audit-payload
// (имя роли + AID; grant дополнительно несёт GrantedByAID). 204-тело пустое.
type RoleOperatorReply struct {
	Name         string
	AID          string
	GrantedByAID string
}

// GrantOperatorTyped — извлечённая доменная функция POST /v1/roles/{name}/operators
// (FULL-TYPED разворот ADR-054 §Pattern (б)): валидация AID (required + формат) +
// привязка оператора к роли, без http.ResponseWriter/*http.Request. CallerAID
// (granted_by_aid) — из claims. claims/name/aid приходят аргументами; ошибки —
// *problemError, успех — [RoleOperatorReply] (audit-payload). Идемпотентно (повтор —
// no-op в service).
func (h *RoleHandler) GrantOperatorTyped(ctx context.Context, claims *jwt.Claims, name, aid string) (RoleOperatorReply, error) {
	var zero RoleOperatorReply
	if aid == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' is required")}
	}
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' must match "+operator.AIDPattern)}
	}

	callerAID := claims.Subject
	err := h.svc.GrantOperator(ctx, rbac.GrantOperatorInput{
		RoleName:  name,
		AID:       aid,
		CallerAID: &callerAID,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+name+" not found")}
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" not found")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a role holding a permission you do not hold yourself")}
	default:
		h.logger.Error("role.grant-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.String("by_aid", callerAID),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "grant operator failed")}
	}
	return RoleOperatorReply{Name: name, AID: aid, GrantedByAID: callerAID}, nil
}

// RevokeOperatorTyped — извлечённая доменная функция DELETE /v1/roles/{name}/
// operators/{aid} (FULL-TYPED разворот ADR-054 §Pattern (б)): валидация path-AID +
// снятие membership-строки, без http.ResponseWriter/*http.Request. name/aid
// приходят аргументами; ошибки — *problemError, успех — [RoleOperatorReply]
// (audit-payload; GrantedByAID пуст — revoke его не несёт).
func (h *RoleHandler) RevokeOperatorTyped(ctx context.Context, name, aid string) (RoleOperatorReply, error) {
	var zero RoleOperatorReply
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.RevokeOperator(ctx, rbac.RevokeOperatorInput{
		RoleName: name,
		AID:      aid,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" is not a member of role "+name)}
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+name+" not found")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "revoking operator "+aid+" from role "+name+" would lock out the cluster")}
	default:
		h.logger.Error("role.revoke-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke operator failed")}
	}
	return RoleOperatorReply{Name: name, AID: aid}, nil
}

// isInvalidPermission — true, если err — ошибка [rbac.ParsePermission]
// (битый permission в CreateRole / UpdateRolePermissions). Sentinel-а у
// неё нет (диагностику несёт текст), поэтому распознаём по wrapped-обёртке
// «invalid permission» из service-а. Маппится в 422.
func isInvalidPermission(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid permission")
}

// isInvalidDefaultScope — true, если err — ошибка [rbac.ParseDefaultScope]
// (битый default_scope в CreateRole / UpdateRolePermissions). Sentinel-а нет
// (диагностику несёт текст ParseDefaultScope), распознаём по wrapped-обёртке
// «invalid default_scope». Маппится в 422.
func isInvalidDefaultScope(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid default_scope")
}

// toRoleView переводит [rbac.RoleView] в ПЛОСКУЮ доменную [RoleView] (handler-
// native T5d): поле-в-поле passthrough; nullable/omitempty wire-форму
// (Description всегда, DefaultScope ""→пропуск, []-vs-null) строит native-проекция
// в api (newRoleView). Permissions/Operators — non-nil слайс (`[]`, не `null`).
func toRoleView(v rbac.RoleView) RoleView {
	return RoleView{
		Name:         v.Name,
		Description:  v.Description,
		Builtin:      v.Builtin,
		Permissions:  emptyIfNil(v.Permissions),
		Operators:    emptyIfNil(v.Operators),
		DefaultScope: v.DefaultScope,
	}
}

// emptyIfNil гарантирует non-nil slice для JSON (`[]` вместо `null`) —
// permissions/operators роли без записей сериализуются пустым массивом. Общий
// helper role/synod-доменов (synod.toSynodResponse тоже использует).
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
