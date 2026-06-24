// Provisioning-policy-handler Operator API (ADR-058 Часть B) — runtime-чтение и
// смена политики `provisioning_allowed_methods` (keeper_settings): список
// разрешённых способов СОЗДАНИЯ оператора ({user,ldap,oidc}). GET — read (БЕЗ
// audit, permission provisioning.read); PUT — write+audit (event
// provisioning.policy_changed, permission provisioning.update).
//
// Доменный слой над serviceregistry: GET читает текущий снимок политики через
// [ProvisioningPolicyReader] (Holder, cluster-консистентный atomic-снимок); PUT
// валидирует список → CSV → [serviceregistry.Service.SetSetting] (upsert +
// cluster-wide Redis-invalidate, тот же канал service-invalidate — Holder.refresh
// перечитает политику на всех нодах). RBAC — в middleware (router.go); здесь —
// маппинг ошибок в RFC 7807 + сборка audit-payload.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// ProvisioningPolicyReader — узкая read-поверхность текущей политики из снимка
// (cluster-консистентный, atomic). Реализуется *serviceregistry.Holder; объявлена
// интерфейсом, чтобы handler тестировался без подъёма Holder. ProvisioningPolicy
// возвращает отсортированный список разрешённых методов + флаг set (задана ли
// политика; set=false → дефолт «всё разрешено», methods=nil).
type ProvisioningPolicyReader interface {
	ProvisioningPolicy() (methods []string, set bool)
}

// ProvisioningPolicyHandler — GET/PUT /v1/provisioning-policy. reader читает
// снимок политики (GET), svc пишет её (PUT через SetSetting + invalidate). Обе
// зависимости обязательны (handler монтируется только при non-nil обеих, см.
// router.go). Состояние не держит; safe for concurrent use.
type ProvisioningPolicyHandler struct {
	reader ProvisioningPolicyReader
	svc    *serviceregistry.Service
	logger *slog.Logger
}

// NewProvisioningPolicyHandler создаёт handler. reader/svc обязательны (паника при
// nil — единственная точка misconfiguration, caller обязан передать non-nil).
func NewProvisioningPolicyHandler(reader ProvisioningPolicyReader, svc *serviceregistry.Service, logger *slog.Logger) *ProvisioningPolicyHandler {
	if reader == nil {
		panic("handlers.NewProvisioningPolicyHandler: reader is nil")
	}
	if svc == nil {
		panic("handlers.NewProvisioningPolicyHandler: serviceregistry.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ProvisioningPolicyHandler{reader: reader, svc: svc, logger: logger}
}

// ProvisioningPolicySpecStub — непустой заглушка для генерации huma-OpenAPI-
// фрагмента (handler при dump не вызывается, нужен лишь non-nil для no-op-проверки
// register-функций; reader/svc nil — handler в spec-режиме не исполняется, parity
// ServiceSpecStub).
func ProvisioningPolicySpecStub() *ProvisioningPolicyHandler {
	return &ProvisioningPolicyHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ProvisioningPolicyView — ПЛОСКОЕ доменное тело GET/PUT-ответа (handler-native).
// AllowedMethods — отсортированный список разрешённых методов (при PolicySet=false
// — nil, пакет api проецирует в `[]`); PolicySet=false → политика не задана (дефолт
// «всё разрешено»).
type ProvisioningPolicyView struct {
	AllowedMethods []string
	PolicySet      bool
}

// GetTyped — доменная функция GET /v1/provisioning-policy (READ без audit): читает
// текущую политику из снимка reader-а. Ошибок нет (snapshot-чтение).
func (h *ProvisioningPolicyHandler) GetTyped() ProvisioningPolicyView {
	methods, set := h.reader.ProvisioningPolicy()
	return ProvisioningPolicyView{AllowedMethods: methods, PolicySet: set}
}

// ProvisioningPolicyUpdateInput — NATIVE request-форма PUT /v1/provisioning-policy.
type ProvisioningPolicyUpdateInput struct {
	AllowedMethods []string
}

// ProvisioningPolicyUpdateReply — результат PutTyped: 200-тело (новая политика) +
// audit-поля (новый список + прежний, если был).
type ProvisioningPolicyUpdateReply struct {
	Body           ProvisioningPolicyView
	AllowedMethods []string
	Previous       []string
	PreviousSet    bool
}

// AuditPayload собирает audit-payload PUT-роута (provisioning.policy_changed):
// новый список allowed_methods + previous (прежний список, если политика была
// задана). Без секретов (имена методов публичны).
func (r ProvisioningPolicyUpdateReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{"allowed_methods": r.AllowedMethods}
	if r.PreviousSet {
		p["previous"] = r.Previous
	}
	return p
}

// PutTyped — доменная функция PUT /v1/provisioning-policy (WRITE+AUDIT): валидирует
// список (непустой, каждый ∈ {user,ldap,oidc}), соединяет в CSV, пишет через
// serviceregistry.Service.SetSetting (upsert + cluster-wide invalidate). claims —
// callerAID для updated_by_aid. Ошибки — *problemError (422 anti-lockout/невалидный
// метод, 404 caller-not-found, 500), успех — [ProvisioningPolicyUpdateReply].
//
// Anti-lockout: пустой список → 422 (нельзя запретить ВСЕ методы). Валидация и
// нормализация делегируется serviceregistry.ParseProvisioningMethods (единый
// источник домена методов и семантики), чтобы PUT и PoolSource.Load не разъехались.
func (h *ProvisioningPolicyHandler) PutTyped(ctx context.Context, claims *jwt.Claims, req ProvisioningPolicyUpdateInput) (ProvisioningPolicyUpdateReply, error) {
	var zero ProvisioningPolicyUpdateReply

	// Нормализация + валидация через единый парсер (lowercase/trim/dedup + домен
	// {user,ldap,oidc} + anti-lockout «непустой»). CSV из входного списка — тот же
	// формат, что хранит keeper_settings и читает PoolSource.Load.
	csv := joinMethodsCSV(req.AllowedMethods)
	methods, err := serviceregistry.ParseProvisioningMethods(csv)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrEmptyProvisioningMethods):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"allowed_methods must be non-empty (anti-lockout): cannot disable operator provisioning by all methods")}
	case errors.Is(err, serviceregistry.ErrInvalidProvisioningMethod):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("provisioning.policy: parse methods failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update provisioning policy failed")}
	}

	// Прежнее значение для audit-payload (best-effort; снимок reader-а — cluster-
	// консистентный текущий статус).
	prev, prevSet := h.reader.ProvisioningPolicy()

	// Каноническая нормализованная форма для записи: отсортированный set → CSV.
	normalized := sortedMethods(methods)
	callerAID := claims.Subject
	if _, err := h.svc.SetSetting(ctx, serviceregistry.SetSettingInput{
		Key:       serviceregistry.SettingProvisioningAllowedMethods,
		Value:     joinMethodsCSV(normalized),
		CallerAID: &callerAID,
	}); err != nil {
		switch {
		case errors.Is(err, serviceregistry.ErrOperatorNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "",
				"caller AID "+callerAID+" not found in operators registry")}
		case errors.Is(err, serviceregistry.ErrInvalidSettingKey):
			// Недостижимо (ключ — well-known const), defensive.
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		default:
			h.logger.Error("provisioning.policy: set setting failed",
				slog.String("by_aid", callerAID), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "update provisioning policy failed")}
		}
	}

	return ProvisioningPolicyUpdateReply{
		Body:           ProvisioningPolicyView{AllowedMethods: normalized, PolicySet: true},
		AllowedMethods: normalized,
		Previous:       prev,
		PreviousSet:    prevSet,
	}, nil
}

// sortedMethods переводит set разрешённых методов в отсортированный список.
func sortedMethods(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// joinMethodsCSV соединяет список методов в CSV-форму keeper_settings (через ',').
func joinMethodsCSV(methods []string) string {
	return strings.Join(methods, ",")
}
