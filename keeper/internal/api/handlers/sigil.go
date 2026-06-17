// Sigil-handler-ы Operator API (Sigil S4a) — доменный слой над [sigil.Service].
// Тот же service вызывает MCP-tool-handler (S4b), что гарантирует один источник
// правды для plugin.allow/revoke/list.
//
// T5d (handler-native): домен sigil отвязан от legacy-генерата. *Typed-функции принимают
// NATIVE input-типы (организованы huma-input-ом в пакете api) и возвращают
// доменные result-ы с ПЛОСКИМИ wire-полями — native wire-DTO (схему OpenAPI)
// строит пакет api из этих полей. (w,r)-оболочки сняты; HTTP обслуживает huma
// full-typed (api/huma_sigil.go), MCP зовёт sigil.Service напрямую (мимо handler).
//
// Бизнес-логика (чтение слота кеша, подпись, CRUD реестра) — в [sigil.Service];
// handler только маппит sentinel-ошибки в RFC 7807. RBAC-проверка — в middleware
// (см. api/router.go), здесь её нет.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// reSigilSegment — closed-charset path-сегментов Sigil (namespace / name / ref).
// kebab-case + точки (теги вида v1.0.0) + подчёркивание; БЕЗ слешей и `..`.
//
// ref как одиночный path-сегмент (а не body / catch-all): tag-ref (`v1.2.3`)
// помещается в сегмент без экранирования, как SID=FQDN с точками
// (operator-api.md → ID в path). Branch-ref со слешем (`feature/x`) в MVP через
// path-DELETE НЕ поддерживается (плагины пинят к тегу-метке, не к движущейся
// ветке; вариант C: ref — стабильная метка допуска). Слеш в ref → 422; catch-
// all-сегмент отвергнут (ломает drift-test {ref}↔chi и допускает path-traversal).
var reSigilSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// SigilHandler — три endpoint-а Sigil allow-list-а (allow / list / revoke).
// Делегирует бизнес-логику в [sigil.Service].
//
// Все зависимости immutable; safe for concurrent use — состояние между
// запросами не держит.
type SigilHandler struct {
	svc    *sigil.Service
	logger *slog.Logger
}

// NewSigilHandler создаёт handler. svc обязателен (паника при nil —
// единственная точка misconfiguration, caller обязан передать non-nil).
func NewSigilHandler(svc *sigil.Service, logger *slog.Logger) *SigilHandler {
	if svc == nil {
		panic("handlers.NewSigilHandler: sigil.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SigilHandler{svc: svc, logger: logger}
}

// SigilSpecStub — непустой *SigilHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaSigilSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [RoleSpecStub]).
func SigilSpecStub() *SigilHandler {
	return &SigilHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// SigilAllowInput — NATIVE request-форма POST /v1/plugins/sigils (handler-native
// T5d). Заменяет PluginSigilAllowRequest: huma-input (пакет api) биндит/
// валидирует тело по своим полям, затем зовёт AllowTyped с этой плоской моделью.
// Формат сегментов (reSigilSegment) — доменная валидация в AllowTyped (422).
type SigilAllowInput struct {
	Namespace string
	Name      string
	Ref       string
}

// SigilAllowView — ПЛОСКАЯ доменная проекция 201-тела POST /v1/plugins/sigils
// (handler-native T5d). Пакет api проецирует её в native-схему PluginSigilAllowReply
// (register-func). namespace/name/ref (echo тройки) + sha256 (посчитан Keeper-ом).
type SigilAllowView struct {
	Namespace string
	Name      string
	Ref       string
	SHA256    string
}

// SigilView — ПЛОСКАЯ доменная проекция одной записи allow-list-а (element
// SigilListPage.Items), handler-native T5d. Пакет api проецирует её в native-схему
// PluginSigilView. AllowedAt/RevokedAt уже усечены до секунд (parity легаси-wire);
// RevokedAt nil у активных → ключ опускается native-типом. БЕЗ signature/manifest.
type SigilView struct {
	Namespace    string
	Name         string
	Ref          string
	SHA256       string
	AllowedByAID string
	AllowedAt    time.Time
	RevokedAt    *time.Time
}

// SigilListPage — доменный результат GET /v1/plugins/sigils (handler-native T5d).
// Пакет api проецирует Items → native PluginSigilListReply (items non-nil → `[]`).
type SigilListPage struct {
	Items []SigilView
}

// SigilAllowReply — результат [SigilHandler.AllowTyped] (handler-native). Несёт
// доменную проекцию 201-тела (SigilAllowView) + caller AID (для audit-payload).
type SigilAllowReply struct {
	View      SigilAllowView
	CallerAID string
}

// AuditPayload собирает audit-payload allow-роута (parity легаси: namespace/name/
// ref/sha256/allowed_by_aid; без signature/manifest).
func (r SigilAllowReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"namespace":      r.View.Namespace,
		"name":           r.View.Name,
		"ref":            r.View.Ref,
		"sha256":         r.View.SHA256,
		"allowed_by_aid": r.CallerAID,
	}
}

// AllowTyped — доменная функция POST /v1/plugins/sigils (handler-native): валидация
// тройки + svc.Allow + sentinel→problem. Ошибки — *problemError; успех —
// [SigilAllowReply] (доменная проекция 201-тела + audit-поля).
func (h *SigilHandler) AllowTyped(ctx context.Context, claims *jwt.Claims, in SigilAllowInput) (SigilAllowReply, error) {
	var zero SigilAllowReply
	if msg, valid := validateSigilTriple(in.Namespace, in.Name, in.Ref); !valid {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", msg)}
	}

	sha256, err := h.svc.Allow(ctx, sigil.AllowInput{
		Namespace: in.Namespace,
		Name:      in.Name,
		Ref:       in.Ref,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, sigil.ErrPluginNotInCache):
		return zero, &problemError{problem.New(problem.TypePluginNotInCache, "",
			"plugin "+in.Namespace+"-"+in.Name+" not found in host cache")}
	case errors.Is(err, sigil.ErrSigilAlreadyActive):
		return zero, &problemError{problem.New(problem.TypeSigilActive, "",
			"an active sigil already exists for "+in.Namespace+"/"+in.Name+"/"+in.Ref)}
	default:
		h.logger.Error("plugin.allow: service failed",
			slog.String("namespace", in.Namespace),
			slog.String("name", in.Name),
			slog.String("ref", in.Ref),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "allow plugin failed")}
	}

	return SigilAllowReply{
		View: SigilAllowView{
			Namespace: in.Namespace,
			Name:      in.Name,
			Ref:       in.Ref,
			SHA256:    sha256,
		},
		CallerAID: claims.Subject,
	}, nil
}

// ListTyped — доменная функция GET /v1/plugins/sigils (handler-native, READ без
// audit): читает реестр активных допусков и собирает [SigilListPage] (items non-nil).
// Ошибка чтения → *problemError (500). date-time → UTC+Truncate(Second) (наносекунды
// не уезжают в wire); RevokedAt nil у активных → ключ опускается native-типом.
func (h *SigilHandler) ListTyped(ctx context.Context) (SigilListPage, error) {
	views, err := h.svc.List(ctx)
	if err != nil {
		h.logger.Error("plugin.list: service failed", slog.Any("error", err))
		return SigilListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list sigils failed")}
	}

	items := make([]SigilView, 0, len(views))
	for _, v := range views {
		it := SigilView{
			Namespace:    v.Namespace,
			Name:         v.Name,
			Ref:          v.Ref,
			SHA256:       v.SHA256,
			AllowedByAID: v.AllowedByAID,
			AllowedAt:    v.AllowedAt.UTC().Truncate(time.Second),
		}
		if v.RevokedAt != nil {
			t := v.RevokedAt.UTC().Truncate(time.Second)
			it.RevokedAt = &t
		}
		items = append(items, it)
	}
	return SigilListPage{Items: items}, nil
}

// SigilRevokeReply — извлечённый результат [SigilHandler.RevokeTyped] (FULL-TYPED).
// Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type SigilRevokeReply struct {
	Namespace string
	Name      string
	Ref       string
}

// AuditPayload собирает audit-payload revoke-роута (parity легаси: namespace/name/
// ref). Общий для (w,r) и huma-B.
func (r SigilRevokeReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"namespace": r.Namespace,
		"name":      r.Name,
		"ref":       r.Ref,
	}
}

// RevokeTyped — извлечённая доменная функция DELETE /v1/plugins/sigils/{namespace}/
// {name}/{ref} (FULL-TYPED ADR-054 §Pattern (б)): валидация тройки path-сегментов +
// svc.Revoke + sentinel→problem. Ошибки — *problemError; успех — [SigilRevokeReply].
func (h *SigilHandler) RevokeTyped(ctx context.Context, claims *jwt.Claims, namespace, name, ref string) (SigilRevokeReply, error) {
	var zero SigilRevokeReply
	if msg, valid := validateSigilTriple(namespace, name, ref); !valid {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", msg)}
	}

	err := h.svc.Revoke(ctx, namespace, name, ref, claims.Subject)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, sigil.ErrSigilNotFound):
		return zero, &problemError{problem.New(problem.TypeSigilNotFound, "",
			"no active sigil for "+namespace+"/"+name+"/"+ref)}
	default:
		h.logger.Error("plugin.revoke: service failed",
			slog.String("namespace", namespace),
			slog.String("name", name),
			slog.String("ref", ref),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke plugin failed")}
	}

	return SigilRevokeReply{Namespace: namespace, Name: name, Ref: ref}, nil
}

// validateSigilTriple проверяет тройку (namespace, name, ref) против
// [reSigilSegment]. Возвращает (human-readable msg, false) на первой
// невалидной части, ("", true) если все валидны.
func validateSigilTriple(namespace, name, ref string) (string, bool) {
	switch {
	case namespace == "":
		return "field 'namespace' is required", false
	case !reSigilSegment.MatchString(namespace):
		return "field 'namespace' must match " + reSigilSegment.String(), false
	case name == "":
		return "field 'name' is required", false
	case !reSigilSegment.MatchString(name):
		return "field 'name' must match " + reSigilSegment.String(), false
	case ref == "":
		return "field 'ref' is required", false
	case !reSigilSegment.MatchString(ref):
		return "field 'ref' must match " + reSigilSegment.String() + " (branch-refs with '/' are not supported via path in MVP)", false
	}
	return "", true
}
