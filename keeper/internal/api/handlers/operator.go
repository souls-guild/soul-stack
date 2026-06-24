// Package handlers — HTTP-handler-ы Operator API (M0.6b).
//
// M0.7: бизнес-логика вынесена в [operator.Service]; handler-ы — тонкая
// HTTP-обёртка (декодирование request → service-call → encoding 4xx/2xx).
// Тот же service вызывается MCP-tool-handler-ом (keeper/internal/mcp), что
// гарантирует один источник правды для трёх endpoint-ов (PM-decision
// M0.7 #6, ТЗ delegation.md).
//
// T5d (handler-native PILOT): домен operator полностью отвязан от legacy-генерата.
// *Typed-функции принимают NATIVE request-типы (огранизованы huma-input-ом в
// пакете api) и возвращают доменные result-ы с ПЛОСКИМИ wire-полями — НЕ
// legacy-генерата-Body. Native wire-DTO (схему OpenAPI) строит пакет api из этих полей
// (register-func huma_operator.go), oapi-генерёные типы в operator-домене не
// участвуют. (w,r)-оболочки сняты: HTTP обслуживает huma full-typed, MCP зовёт
// operator.Service напрямую (мимо handler).
//
// RBAC-проверка делается в middleware (см. api/router.go и
// api/middleware/rbac.go), handler выполняет только маппинг ошибок в
// RFC 7807.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// OperatorDB — узкий интерфейс над pgxpool.Pool, который нужен handler-у
// для не-транзакционных endpoint-ов.
type OperatorDB = operator.ExecQueryRower

// OperatorPool — расширение [OperatorDB] с BeginTx, нужное Revoke-handler-у
// для атомарной self-lockout-проверки.
type OperatorPool interface {
	OperatorDB
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// JWTIssuer — узкий интерфейс над `*keeper/internal/jwt.Issuer`.
// Сужение нужно для unit-тестов (mock без подгрузки signing-key).
type JWTIssuer interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// RBACSource — узкая поверхность rbac-сервиса для handler-side helper-ов.
// Нужен только RolesOf (передаётся в operator.Service для выпуска JWT);
// lockout-probe берёт admin-set из БД (Slice 3), не из in-memory снимка.
type RBACSource interface {
	RolesOf(aid string) []string
}

// ProvisioningGate — узкая поверхность политики provisioning_allowed_methods
// (ADR-058 Часть B): гейтит ветку СОЗДАНИЯ оператора. Реализуется
// *serviceregistry.Holder; объявлена локально, чтобы handlers не тянул
// serviceregistry. nil → гейт выключен (политика не сконфигурирована /
// тесты, back-compat — CreateTyped пропускает).
type ProvisioningGate interface {
	ProvisioningMethodAllowed(method string) bool
}

// OperatorHandler — три endpoint-а Operator API. Делегирует бизнес-логику
// в [operator.Service].
//
// Все зависимости immutable; safe for concurrent use, потому что не
// держит состояния между запросами.
type OperatorHandler struct {
	svc    *operator.Service
	logger *slog.Logger

	// gate — политика provisioning_allowed_methods (ADR-058 Часть B), гейтит
	// CreateTyped (метод "user"). nil → гейт выключен (back-compat). Инжектится
	// через [SetProvisioningGate] late-binding-ом: Holder в `keeper run`
	// поднимается отдельным setup-шагом, конструктор сигнатуру не меняет.
	gate ProvisioningGate
}

// SetProvisioningGate late-binding-ом подключает политику provisioning_allowed_methods
// (ADR-058 Часть B). nil — снять гейт (back-compat: создание любым методом).
// Вызывается из `keeper run` после подъёма serviceregistry.Holder. Идемпотентен;
// потокобезопасность не требуется — вызов до старта HTTP-сервера.
func (h *OperatorHandler) SetProvisioningGate(gate ProvisioningGate) {
	h.gate = gate
}

// NewOperatorHandler создаёт handler. ttlDefault — TTL JWT-токенов.
// Внутри собирает [operator.Service] (один на handler).
//
// Сохраняем старую сигнатуру (pool / issuer / rbacSrc / ttlDefault / logger)
// для бинарной совместимости с keeper/cmd/keeper wire-up и unit-тестами
// (handlers/operator_test.go) — service-объект создаётся скрыто.
func NewOperatorHandler(pool OperatorPool, issuer JWTIssuer, rbacSrc RBACSource, ttlDefault time.Duration, logger *slog.Logger) *OperatorHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       pool,
		Issuer:     issuer,
		RBAC:       rbacSrc,
		TTLDefault: ttlDefault,
		Logger:     logger,
	})
	if err != nil {
		// Single point of misconfiguration — caller (NewServer) уже
		// валидирует non-nil deps; реальный path сюда не должен
		// дойти, но panic-ить здесь хуже, чем зафиксировать в логах.
		panic(fmt.Sprintf("handlers.NewOperatorHandler: %v", err))
	}
	return &OperatorHandler{svc: svc, logger: logger}
}

// Service возвращает inner [operator.Service]. Используется wire-up-ом MCP-
// сервера в keeper/cmd/keeper, чтобы переиспользовать тот же экземпляр
// (single source of truth, см. delegation.md PM-decision #6).
func (h *OperatorHandler) Service() *operator.Service { return h.svc }

// OperatorSpecStub — непустой *OperatorHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaOperatorSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [RoleSpecStub]).
func OperatorSpecStub() *OperatorHandler {
	return &OperatorHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// maxDisplayNameLen — верхняя граница `display_name` Архонта. Пустой
// display_name легитимен (service подставит AID); слишком длинный — мусор/DoS
// в UI-списках. 200 символов — с запасом для «Имя Фамилия (команда)».
const maxDisplayNameLen = 200

// OperatorCreateInput — NATIVE request-форма POST /v1/operators (handler-native
// PILOT T5d). Заменяет OperatorCreateRequest: huma-input (пакет api) биндит
// и валидирует тело по этим полям, затем зовёт CreateTyped. Roles — плоский
// []string (huma omitempty: пустой/опущенный → nil → «без roles», parity легаси).
type OperatorCreateInput struct {
	AID         string
	DisplayName string
	Roles       []string
}

// OperatorCreateReply — извлечённый результат [OperatorHandler.CreateTyped]
// (handler-native PILOT). Несёт ПЛОСКИЕ wire-поля 201-тела (api строит из них
// native-схему OperatorCreateReply) + audit-payload-поля (выставляются
// middleware: huma-вариант B). GrantedRoles служит обоим: wire-полю roles
// (omitempty) и audit-payload.
type OperatorCreateReply struct {
	AID          string
	DisplayName  string
	AuthMethod   string
	CreatedAt    time.Time
	CreatedByAID string
	JWT          string
	GrantedRoles []string
}

// CreateTyped — доменная функция POST /v1/operators (handler-native PILOT):
// бизнес-логика без http.ResponseWriter/*http.Request. claims и req приходят
// аргументами; ошибки — *problemError (доставляются huma-обёрткой через
// [AsProblemDetails]), успех — [OperatorCreateReply] (плоские wire-поля + audit).
func (h *OperatorHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req OperatorCreateInput) (OperatorCreateReply, error) {
	var zero OperatorCreateReply
	if req.AID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' is required")}
	}
	if !operator.ValidAID(req.AID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'aid' must match "+operator.AIDPattern)}
	}
	// display_name опционален (пустой → service подставит AID); ограничиваем
	// только верхнюю длину, чтобы не пускать мусор в реестр / UI.
	if len(req.DisplayName) > maxDisplayNameLen {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("field 'display_name' must be at most %d characters", maxDisplayNameLen))}
	}

	// Гейт политики provisioning_allowed_methods (ADR-058 Часть B): создание
	// оператора через Operator API — метод "user" (created_via=user). gate==nil →
	// пропускаем (политика не сконфигурирована, back-compat). bootstrap-путь
	// (`keeper init`) сюда НЕ заходит — он не вызывает CreateTyped.
	if h.gate != nil && !h.gate.ProvisioningMethodAllowed("user") {
		return zero, &problemError{problem.New(problem.TypeProvisioningMethodDisabled, "",
			"operator provisioning via 'user' method is disabled by policy")}
	}

	res, err := h.svc.Create(ctx, operator.CreateInput{
		AID:         req.AID,
		DisplayName: req.DisplayName,
		CallerAID:   claims.Subject,
		Roles:       req.Roles,
	})
	if err != nil {
		switch {
		case errors.Is(err, operator.ErrOperatorAlreadyExists):
			return zero, &problemError{problem.New(problem.TypeOperatorExists, "",
				"operator with this AID already exists")}
		// roles[]: несуществующая роль — validation-failed (422) с указанием,
		// какая именно роль не найдена. Atomic create+grant уже откатил tx —
		// оператор НЕ создан.
		case errors.Is(err, rbac.ErrRoleNotFound):
			return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", err.Error())}
		// FK-violation на role-grant aid → operator не существует. На пути
		// create+grant это означало бы рассинхрон INSERT/grant в одной tx —
		// невозможно по конструкции, но защищаемся явным маппингом 404.
		case errors.Is(err, rbac.ErrOperatorNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "", err.Error())}
		// invalid role name из pre-валидации — validation-failed.
		case strings.Contains(err.Error(), "invalid role name"):
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
		h.logger.Error("operator.create: service failed",
			slog.String("aid", req.AID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create operator failed")}
	}

	return OperatorCreateReply{
		AID:          res.AID,
		DisplayName:  res.DisplayName,
		AuthMethod:   string(res.AuthMethod),
		CreatedAt:    res.CreatedAt.UTC().Truncate(time.Second),
		CreatedByAID: res.CreatedByAID,
		JWT:          res.JWT,
		GrantedRoles: res.GrantedRoles,
	}, nil
}

// AuditPayload собирает audit-payload create-роута (parity легаси SetAuditPayload).
// ЕДИНЫЙ источник для huma-варианта B (единообразно с OperatorRevokeReply/
// OperatorIssueTokenReply.AuditPayload()).
func (r OperatorCreateReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{
		"aid":            r.AID,
		"display_name":   r.DisplayName,
		"auth_method":    r.AuthMethod,
		"created_by_aid": r.CreatedByAID,
	}
	if len(r.GrantedRoles) > 0 {
		p["roles"] = r.GrantedRoles
	}
	return p
}

// OperatorRevokeReply — извлечённый результат [OperatorHandler.RevokeTyped]
// (handler-native PILOT). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type OperatorRevokeReply struct {
	AID    string
	Reason string
}

// AuditPayload собирает audit-payload revoke-роута (parity легаси: aid +
// опц. reason). Источник для huma-варианта B.
func (r OperatorRevokeReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{"aid": r.AID}
	if r.Reason != "" {
		p["reason"] = r.Reason
	}
	return p
}

// RevokeTyped — доменная функция POST /v1/operators/{aid}/revoke (handler-native
// PILOT): валидация path-AID + svc.Revoke + sentinel→problem. Ошибки —
// *problemError; успех — [OperatorRevokeReply] (audit-поля).
func (h *OperatorHandler) RevokeTyped(ctx context.Context, claims *jwt.Claims, targetAID, reason string) (OperatorRevokeReply, error) {
	var zero OperatorRevokeReply
	if !operator.ValidAID(targetAID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.Revoke(ctx, operator.RevokeInput{
		AID:       targetAID,
		Reason:    reason,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, operator.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "",
			"target is the last active cluster-admin; revoking would lock out the cluster")}
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"operator "+targetAID+" not found")}
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return zero, &problemError{problem.New(problem.TypeOperatorRevoked, "",
			"operator "+targetAID+" is already revoked")}
	default:
		h.logger.Error("operator.revoke: service failed",
			slog.String("aid", targetAID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke failed")}
	}

	return OperatorRevokeReply{AID: targetAID, Reason: reason}, nil
}

// OperatorIssueTokenReply — извлечённый результат [OperatorHandler.IssueTokenTyped]
// (handler-native PILOT). Несёт ПЛОСКИЕ wire-поля 200-тела (api строит native-
// схему IssueTokenReply) + audit-поля.
type OperatorIssueTokenReply struct {
	AID       string
	JWT       string
	ExpiresAt time.Time
}

// AuditPayload собирает audit-payload issue-token-роута (parity легаси: aid +
// expires_at RFC3339). БЕЗ самого JWT (SENSITIVE). Источник для huma-B.
func (r OperatorIssueTokenReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"aid":        r.AID,
		"expires_at": r.ExpiresAt.Format(time.RFC3339),
	}
}

// IssueTokenTyped — доменная функция POST /v1/operators/{aid}/issue-token
// (handler-native PILOT): валидация path-AID + svc.IssueToken + sentinel→problem.
// Ошибки — *problemError; успех — [OperatorIssueTokenReply] (wire-поля + audit).
func (h *OperatorHandler) IssueTokenTyped(ctx context.Context, claims *jwt.Claims, targetAID string) (OperatorIssueTokenReply, error) {
	var zero OperatorIssueTokenReply
	if !operator.ValidAID(targetAID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}
	res, err := h.svc.IssueToken(ctx, operator.IssueTokenInput{
		AID:       targetAID,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through.
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"operator "+targetAID+" not found")}
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return zero, &problemError{problem.New(problem.TypeOperatorRevoked, "",
			"operator "+targetAID+" is revoked")}
	default:
		h.logger.Error("operator.issue-token: service failed",
			slog.String("aid", targetAID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue JWT failed")}
	}

	return OperatorIssueTokenReply{
		AID:       res.AID,
		JWT:       res.JWT,
		ExpiresAt: res.ExpiresAt.UTC().Truncate(time.Second),
	}, nil
}

// OperatorView — ПЛОСКАЯ wire-форма Operator-а (list-item / get-200), handler-
// native PILOT (заменяет Operator-алиас). Nullable-поля отражают NULL в БД:
// created_by_aid (NULL у bootstrap/system/federated — ADR-058(d) легализовал
// NULL для не-bootstrap-строк), revoked_at (NULL у активного). bootstrap_initial —
// derived-флаг `op.IsBootstrap()` (created_via='bootstrap') для UI: отдельной
// колонки в БД нет, единственность первого Архонта гарантирует partial unique
// index `WHERE created_via='bootstrap'` (ADR-013/ADR-014 amendment 2026-06-23,
// миграция 085). Признак перенесён с прежнего `created_by_aid IS NULL` —
// иначе federated/system-операторы с NULL-родителем давали ложный bootstrap-флаг.
// Пакет api проецирует OperatorView → native-схему Operator (register-func),
// wire-форма (UTC + Truncate(Second) на date-time) фиксируется здесь.
type OperatorView struct {
	AID              string
	AuthMethod       string
	BootstrapInitial bool
	CreatedAt        time.Time
	CreatedByAID     *string
	CreatedVia       string
	DisplayName      string
	Metadata         map[string]any
	RevokedAt        *time.Time
}

func toOperatorView(op *operator.Operator) OperatorView {
	out := OperatorView{
		AID:              op.AID,
		DisplayName:      op.DisplayName,
		AuthMethod:       string(op.AuthMethod),
		CreatedAt:        op.CreatedAt.UTC().Truncate(time.Second),
		CreatedByAID:     op.CreatedByAID,
		CreatedVia:       op.CreatedVia,
		BootstrapInitial: op.IsBootstrap(),
		Metadata:         op.Metadata,
	}
	if op.RevokedAt != nil {
		t := op.RevokedAt.UTC().Truncate(time.Second)
		out.RevokedAt = &t
	}
	return out
}

// OperatorListPage — доменный paged-результат GET /v1/operators (handler-native
// PILOT). Плоские offset/limit/total + срез OperatorView; пакет api проецирует
// его в native-envelope (PagedResponse[api.Operator] → схема OperatorListReply).
type OperatorListPage struct {
	Items  []OperatorView
	Offset int
	Limit  int
	Total  int
}

// ListTyped — доменная функция GET /v1/operators (handler-native PILOT, read-
// with-typed-query, БЕЗ audit). filter/offset/limit приходят уже
// провалидированными (huma-bind: auth_method enum→422, revoked bool→400,
// pagination int32; диапазон offset/limit enforce-ит этот слой через
// CheckPageBounds). Ошибка чтения → *problemError (500).
func (h *OperatorHandler) ListTyped(ctx context.Context, filter operator.ListFilter, offset, limit int) (OperatorListPage, error) {
	var zero OperatorListPage

	// Диапазон пагинации (offset≥0, limit∈[1,1000]) — ЕДИНЫЙ источник границ
	// sharedapi.CheckPageBounds (тот же, что у ParsePage). Out-of-range → 400
	// TypeMalformedRequest (контракт-инвариант: huma typed-int НЕ несёт schema-
	// minimum/maximum, иначе вернул бы 422 — wire-change против легаси/strict 400).
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	ops, total, err := h.svc.List(ctx, filter, offset, limit)
	if err != nil {
		h.logger.Error("operator.list: service failed",
			slog.Int("offset", offset),
			slog.Int("limit", limit),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list operators failed")}
	}

	items := make([]OperatorView, 0, len(ops))
	for _, op := range ops {
		items = append(items, toOperatorView(op))
	}
	return OperatorListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// GetTyped — доменная функция GET /v1/operators/{aid} (handler-native PILOT,
// READ-вариант без audit): валидация path-AID + svc.Get + sentinel→problem
// (404/500). Ошибки — *problemError; успех — [OperatorView] (200-тело).
func (h *OperatorHandler) GetTyped(ctx context.Context, targetAID string) (OperatorView, error) {
	var zero OperatorView
	if !operator.ValidAID(targetAID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}
	op, err := h.svc.Get(ctx, targetAID)
	switch {
	case err == nil:
		return toOperatorView(op), nil
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"operator "+targetAID+" not found")}
	default:
		h.logger.Error("operator.get: service failed",
			slog.String("aid", targetAID),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get operator failed")}
	}
}
