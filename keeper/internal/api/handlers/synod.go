// Synod-handler-ы Operator API (RBAC Synod, ADR-049) — доменный слой над
// [rbac.Service]. *Typed-функции несут бизнес-логику без http.ResponseWriter/
// *http.Request; HTTP обслуживает huma full-typed (api/huma_synod.go), MCP зовёт
// rbac.Service напрямую (мимо handler).
//
// T5d (handler-native): домен synod отвязан от legacy-генерата. *Typed принимают NATIVE
// request-типы (огранизованы huma-input-ом в пакете api) и возвращают доменные
// result-ы с ПЛОСКИМИ wire-полями. (w,r)-оболочки сняты.
//
// Бизнес-логика (builtin-граница, self-lockout, least-privilege subset,
// валидация name/aid) — в [rbac.Service]; handler маппит sentinel-ошибки в RFC 7807.
// RBAC-проверка — в middleware (см. api/router.go), здесь её нет.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// SynodHandler — семь endpoint-ов Synod-CRUD (группы / membership / bundle).
// Делегирует бизнес-логику в [rbac.Service]. Все зависимости immutable; safe
// for concurrent use.
type SynodHandler struct {
	svc    *rbac.Service
	logger *slog.Logger
}

// NewSynodHandler создаёт handler. svc обязателен (паника при nil — caller
// обязан передать non-nil).
func NewSynodHandler(svc *rbac.Service, logger *slog.Logger) *SynodHandler {
	if svc == nil {
		panic("handlers.NewSynodHandler: rbac.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SynodHandler{svc: svc, logger: logger}
}

// SynodCreateInput — NATIVE request-форма POST /v1/synods (handler-native T5d).
// name обязателен; Description — `*string` (nil → без описания), parity легаси-декода.
type SynodCreateInput struct {
	Name        string
	Description *string
}

// SynodUpdateInput — NATIVE request-форма PATCH /v1/synods/{name} (handler-native
// T5d). Description обязателен (мутируется ТОЛЬКО он; name (PK) immutable — из path).
type SynodUpdateInput struct {
	Description string
}

// SynodView — ПЛОСКАЯ доменная проекция Synod-группы (GET /v1/synods items[]),
// handler-native T5d. Description — RAW string (пустая = без описания); nullable/
// []-vs-null wire-форму держит native-проекция в api (newSynodView).
type SynodView struct {
	Name        string
	Description string
	Builtin     bool
	Roles       []string
	Operators   []string
}

// SynodListPage — доменный список Synod-групп GET /v1/synods (handler-native T5d).
type SynodListPage struct {
	Items []SynodView
}

// SynodSpecStub — непустой *SynodHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaSynodSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [RoleSpecStub]).
func SynodSpecStub() *SynodHandler {
	return &SynodHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// toSynodResponse проецирует доменный [rbac.SynodView] в ПЛОСКУЮ [SynodView]
// (handler-native T5d): поле-в-поле passthrough; nullable/[]-vs-null wire-форму
// (Description всегда "", roles/operators `[]`) строит native-проекция в api.
// Roles/Operators — non-nil срез (`[]`, не `null`).
func toSynodResponse(v rbac.SynodView) SynodView {
	return SynodView{
		Name:        v.Name,
		Description: v.Description,
		Builtin:     v.Builtin,
		Roles:       emptyIfNil(v.Roles),
		Operators:   emptyIfNil(v.Operators),
	}
}

// SynodCreateReply — результат [SynodHandler.CreateTyped] (FULL-TYPED разворот
// ADR-054 §Pattern). 201-тело synod.create ПУСТОЕ (легаси-контракт: openapi.yaml
// `POST /v1/synods` отдаёт 201 без `content`), поэтому reply несёт не wire-поля
// ответа, а МЕТАДАННЫЕ для audit-payload (имя группы + AID создателя). 204/201-
// тело пустое.
type SynodCreateReply struct {
	Name         string
	CreatedByAID string
}

// AuditPayload собирает audit-payload create-роута (parity легаси SetAuditPayload).
// ЕДИНЫЙ источник для (w,r)-оболочки И huma-варианта B.
func (r SynodCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"created_by_aid": r.CreatedByAID,
	}
}

// CreateTyped — извлечённая доменная функция POST /v1/synods (FULL-TYPED разворот
// ADR-054 §Pattern (б)): бизнес-логика без http.ResponseWriter/*http.Request.
// claims и req приходят аргументами (декод/auth — на вызывающем слое); ошибки —
// *problemError (доставляются huma-обёрткой через [AsProblemDetails] либо
// (w,r)-оболочкой через [writeProblemError]), успех — [SynodCreateReply].
func (h *SynodHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req SynodCreateInput) (SynodCreateReply, error) {
	var zero SynodCreateReply
	if req.Name == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'name' is required")}
	}

	var description string
	if req.Description != nil {
		description = *req.Description
	}
	if len(description) > rbac.SynodDescriptionMaxLen {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'description' exceeds max length")}
	}
	err := h.svc.CreateSynod(ctx, rbac.CreateSynodInput{
		Name:        req.Name,
		Description: description,
		CallerAID:   claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeSynodExists, "", "synod "+req.Name+" already exists")}
	case errors.Is(err, rbac.ErrInvalidSynodName):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("synod.create: service failed",
			slog.String("name", req.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create synod failed")}
	}

	return SynodCreateReply{Name: req.Name, CreatedByAID: claims.Subject}, nil
}

// ListTyped — доменная функция GET /v1/synods (handler-native T5d, READ без audit):
// читает каталог групп и собирает [SynodListPage] (плоские SynodView) без http.
// ResponseWriter/*http.Request. Ошибка чтения → *problemError (500). Wire-форму
// items (toSynodResponse + native-проекция) строит api.
func (h *SynodHandler) ListTyped(ctx context.Context) (SynodListPage, error) {
	views, err := h.svc.ListSynods(ctx)
	if err != nil {
		h.logger.Error("synod.list: service failed", slog.Any("error", err))
		return SynodListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list synods failed")}
	}

	items := make([]SynodView, 0, len(views))
	for _, v := range views {
		items = append(items, toSynodResponse(v))
	}
	return SynodListPage{Items: items}, nil
}

// SynodNameReply — результат write-операций, чей audit-payload несёт лишь имя
// группы (delete). 204-тело пустое; reply — МЕТАДАННЫЕ для audit.
type SynodNameReply struct {
	Name string
}

// DeleteTyped — извлечённая доменная функция DELETE /v1/synods/{name} (FULL-TYPED
// разворот ADR-054 §Pattern (б)). name приходит аргументом; ошибки — *problemError,
// успех — [SynodNameReply] (audit-payload). 204-тело пустое.
func (h *SynodHandler) DeleteTyped(ctx context.Context, name string) (SynodNameReply, error) {
	var zero SynodNameReply
	err := h.svc.DeleteSynod(ctx, name)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	case errors.Is(err, rbac.ErrSynodBuiltin):
		return zero, &problemError{problem.New(problem.TypeSynodBuiltin, "", "synod "+name+" is builtin and cannot be deleted")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "deleting synod "+name+" would lock out the cluster")}
	default:
		h.logger.Error("synod.delete: service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete synod failed")}
	}
	return SynodNameReply{Name: name}, nil
}

// SynodUpdateReply — результат [SynodHandler.UpdateTyped]: МЕТАДАННЫЕ для audit-
// payload (имя группы + новое описание). 204-тело пустое.
type SynodUpdateReply struct {
	Name        string
	Description string
}

// AuditPayload собирает audit-payload update-роута. ЕДИНЫЙ источник для
// (w,r)-оболочки И huma-варианта B (parity [SynodCreateReply.AuditPayload]).
func (r SynodUpdateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":        r.Name,
		"description": r.Description,
	}
}

// UpdateTyped — извлечённая доменная функция PATCH /v1/synods/{name} (FULL-TYPED
// разворот ADR-054 §Pattern (б)): валидация description + замена, без
// http.ResponseWriter/*http.Request. claims/name/req приходят аргументами; ошибки
// — *problemError, успех — [SynodUpdateReply] (audit-payload).
func (h *SynodHandler) UpdateTyped(ctx context.Context, claims *jwt.Claims, name string, req SynodUpdateInput) (SynodUpdateReply, error) {
	var zero SynodUpdateReply
	if req.Description == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'description' is required")}
	}
	if len(req.Description) > rbac.SynodDescriptionMaxLen {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'description' exceeds max length")}
	}

	err := h.svc.UpdateSynodDescription(ctx, name, req.Description)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	default:
		h.logger.Error("synod.update: service failed",
			slog.String("name", name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update synod failed")}
	}

	return SynodUpdateReply{Name: name, Description: req.Description}, nil
}

// SynodOperatorReply — результат add/remove-operator: МЕТАДАННЫЕ для audit-payload
// (имя группы + AID; add дополнительно несёт AddedByAID). 204-тело пустое.
type SynodOperatorReply struct {
	Name       string
	AID        string
	AddedByAID string
}

// AddOperatorAuditPayload собирает audit-payload add-operator-роута (несёт
// added_by_aid). ЕДИНЫЙ источник для (w,r)-оболочки И huma-варианта B. Отдельно
// от [RemoveOperatorAuditPayload]: один reply-тип обслуживает оба роута, но их
// payload-наборы различаются (remove added_by_aid не несёт).
func (r SynodOperatorReply) AddOperatorAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":         r.Name,
		"aid":          r.AID,
		"added_by_aid": r.AddedByAID,
	}
}

// RemoveOperatorAuditPayload собирает audit-payload remove-operator-роута (БЕЗ
// added_by_aid). ЕДИНЫЙ источник для (w,r)-оболочки И huma-варианта B.
func (r SynodOperatorReply) RemoveOperatorAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name": r.Name,
		"aid":  r.AID,
	}
}

// AddOperatorTyped — извлечённая доменная функция POST /v1/synods/{name}/operators
// (FULL-TYPED разворот ADR-054 §Pattern (б)): валидация AID (required + формат) +
// привязка члена к группе, без http.ResponseWriter/*http.Request. AddedByAID — из
// claims. claims/name/aid приходят аргументами; ошибки — *problemError, успех —
// [SynodOperatorReply]. Идемпотентно (повтор — no-op в service).
func (h *SynodHandler) AddOperatorTyped(ctx context.Context, claims *jwt.Claims, name, aid string) (SynodOperatorReply, error) {
	var zero SynodOperatorReply
	if aid == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' is required")}
	}
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.AddOperator(ctx, rbac.AddOperatorInput{
		SynodName: name,
		AID:       aid,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" not found")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot add an operator to a synod bundling permissions you do not hold yourself")}
	default:
		h.logger.Error("synod.add-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "add operator failed")}
	}

	return SynodOperatorReply{Name: name, AID: aid, AddedByAID: claims.Subject}, nil
}

// RemoveOperatorTyped — извлечённая доменная функция DELETE /v1/synods/{name}/
// operators/{aid} (FULL-TYPED разворот ADR-054 §Pattern (б)): валидация path-AID +
// снятие membership-строки, без http.ResponseWriter/*http.Request. name/aid
// приходят аргументами; ошибки — *problemError, успех — [SynodOperatorReply]
// (audit-payload; AddedByAID пуст — remove его не несёт).
func (h *SynodHandler) RemoveOperatorTyped(ctx context.Context, name, aid string) (SynodOperatorReply, error) {
	var zero SynodOperatorReply
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.RemoveOperator(ctx, rbac.RemoveOperatorInput{
		SynodName: name,
		AID:       aid,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" is not a member of synod "+name)}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "removing operator "+aid+" from synod "+name+" would lock out the cluster")}
	default:
		h.logger.Error("synod.remove-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "remove operator failed")}
	}

	return SynodOperatorReply{Name: name, AID: aid}, nil
}

// SynodRoleReply — результат grant/revoke-role: МЕТАДАННЫЕ для audit-payload
// (имя группы + роль; grant дополнительно несёт GrantedByAID). 204-тело пустое.
type SynodRoleReply struct {
	Name         string
	Role         string
	GrantedByAID string
}

// GrantRoleAuditPayload собирает audit-payload grant-role-роута (несёт
// granted_by_aid). ЕДИНЫЙ источник для (w,r)-оболочки И huma-варианта B. Отдельно
// от [RevokeRoleAuditPayload]: один reply-тип обслуживает оба роута, но их
// payload-наборы различаются (revoke granted_by_aid не несёт).
func (r SynodRoleReply) GrantRoleAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"role":           r.Role,
		"granted_by_aid": r.GrantedByAID,
	}
}

// RevokeRoleAuditPayload собирает audit-payload revoke-role-роута (БЕЗ
// granted_by_aid). ЕДИНЫЙ источник для (w,r)-оболочки И huma-варианта B.
func (r SynodRoleReply) RevokeRoleAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name": r.Name,
		"role": r.Role,
	}
}

// GrantRoleTyped — извлечённая доменная функция POST /v1/synods/{name}/roles
// (FULL-TYPED разворот ADR-054 §Pattern (б)): валидация role (required) +
// добавление роли в bundle, без http.ResponseWriter/*http.Request. GrantedByAID —
// из claims. claims/name/role приходят аргументами; ошибки — *problemError, успех
// — [SynodRoleReply]. Идемпотентно.
func (h *SynodHandler) GrantRoleTyped(ctx context.Context, claims *jwt.Claims, name, role string) (SynodRoleReply, error) {
	var zero SynodRoleReply
	if role == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'role' is required")}
	}

	callerAID := claims.Subject
	err := h.svc.GrantRole(ctx, rbac.GrantRoleInput{
		SynodName: name,
		RoleName:  role,
		CallerAID: callerAID,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+role+" not found")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a role bundling a permission you do not hold yourself")}
	default:
		h.logger.Error("synod.grant-role: service failed",
			slog.String("name", name),
			slog.String("role", role),
			slog.String("by_aid", callerAID),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "grant role failed")}
	}

	return SynodRoleReply{Name: name, Role: role, GrantedByAID: callerAID}, nil
}

// RevokeRoleTyped — извлечённая доменная функция DELETE /v1/synods/{name}/roles/
// {role_name} (FULL-TYPED разворот ADR-054 §Pattern (б)): снятие роли из bundle,
// без http.ResponseWriter/*http.Request. name/role приходят аргументами; ошибки —
// *problemError, успех — [SynodRoleReply] (audit-payload; GrantedByAID пуст).
func (h *SynodHandler) RevokeRoleTyped(ctx context.Context, name, role string) (SynodRoleReply, error) {
	var zero SynodRoleReply
	err := h.svc.RevokeRole(ctx, rbac.RevokeRoleInput{
		SynodName: name,
		RoleName:  role,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "role "+role+" is not bundled in synod "+name)}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "revoking role "+role+" from synod "+name+" would lock out the cluster")}
	default:
		h.logger.Error("synod.revoke-role: service failed",
			slog.String("name", name),
			slog.String("role", role),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke role failed")}
	}

	return SynodRoleReply{Name: name, Role: role}, nil
}
