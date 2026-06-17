// Operator API handler-ы ротации trust-anchor-ключей подписи Sigil (ADR-026(h),
// R3-S7) — тонкая HTTP-обёртка над [sigil.KeyService]. Тот же service вызывает
// MCP-tool-handler (keeper.sigil.key.*), один источник правды.
//
// Бизнес-логика (key-gen, Vault-write, CRUD реестра, publish anchors-changed) —
// в [sigil.KeyService]; handler декодирует request → service-call → маппит
// sentinel-ы в RFC 7807 и кодирует 2xx. RBAC — в middleware (router.go).
//
// БЕЗОПАСНОСТЬ: приватник НИКОГДА не в ответе (KeyService его не возвращает) и
// не в логах (handler логирует только key_id / by_aid).
//
// T5d (handler-native): домен sigil-key отвязан от legacy-генерата. *Typed-функции возвращают
// доменные result-ы с ПЛОСКИМИ wire-полями — native wire-DTO (схему OpenAPI) строит
// пакет api из этих полей. (w,r)-оболочки сняты; HTTP обслуживает huma full-typed
// (api/huma_sigil_key.go), MCP зовёт sigil.KeyService напрямую (мимо handler).
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

// reSigilKeyID — формат key_id: ровно 64 нижних hex-символа (SHA-256(SPKI), hex).
// Совпадает с конвенцией миграции 037 / [sigil.keyIDFromPublic]. path-сегмент
// без слешей/`..` — безопасен от traversal.
var reSigilKeyID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SigilKeyHandler — четыре endpoint-а ротации ключей подписи (introduce / list /
// set-primary / retire). Делегирует в [sigil.KeyService].
type SigilKeyHandler struct {
	svc    *sigil.KeyService
	logger *slog.Logger
}

// NewSigilKeyHandler создаёт handler. svc обязателен (паника при nil — единственная
// точка misconfiguration; caller обязан передать non-nil).
func NewSigilKeyHandler(svc *sigil.KeyService, logger *slog.Logger) *SigilKeyHandler {
	if svc == nil {
		panic("handlers.NewSigilKeyHandler: sigil.KeyService is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SigilKeyHandler{svc: svc, logger: logger}
}

// SigilKeySpecStub — непустой *SigilKeyHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaSigilKeySpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [RoleSpecStub]).
func SigilKeySpecStub() *SigilKeyHandler {
	return &SigilKeyHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// SigilKeyIntroduceView — ПЛОСКАЯ доменная проекция 201-тела POST /v1/sigil/keys
// (handler-native T5d). Пакет api проецирует её в native-схему SigilKeyIntroduceReply.
// Status — plain string домена (active/retired); native-тип в api держит enum-форму.
// БЕЗ приватника (KeyService его не возвращает).
type SigilKeyIntroduceView struct {
	KeyID        string
	PubkeyPEM    string
	IsPrimary    bool
	Status       string
	IntroducedAt time.Time
}

// SigilKeyView — ПЛОСКАЯ доменная проекция active-ключа (element SigilKeyListPage.Items),
// handler-native T5d. Пакет api проецирует её в native-схему SigilKeyView. БЕЗ vault_ref;
// Status — plain string домена; IntroducedAt уже усечён до секунд (parity легаси-wire).
type SigilKeyView struct {
	KeyID        string
	IsPrimary    bool
	Status       string
	IntroducedAt time.Time
}

// SigilKeyListPage — доменный результат GET /v1/sigil/keys (handler-native T5d). Пакет
// api проецирует Items → native SigilKeyListReply (items non-nil → `[]`).
type SigilKeyListPage struct {
	Items []SigilKeyView
}

// SigilKeyIntroduceReply — результат [SigilKeyHandler.IntroduceTyped] (handler-native).
// Несёт доменную проекцию 201-тела (SigilKeyIntroduceView, БЕЗ приватника) + caller AID.
type SigilKeyIntroduceReply struct {
	View      SigilKeyIntroduceView
	CallerAID string
}

// AuditPayload собирает audit-payload introduce-роута (parity легаси: key_id +
// is_primary + introduced_by_aid; БЕЗ приватника).
func (r SigilKeyIntroduceReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"key_id":            r.View.KeyID,
		"is_primary":        r.View.IsPrimary,
		"introduced_by_aid": r.CallerAID,
	}
}

// IntroduceTyped — доменная функция POST /v1/sigil/keys (handler-native): svc.Introduce
// (key-gen + Vault-write + register) + sentinel→problem. Ошибки — *problemError; успех —
// [SigilKeyIntroduceReply] (доменная проекция 201-тела + audit-поля). БЕЗОПАСНОСТЬ:
// приватник никогда не покидает KeyService.
func (h *SigilKeyHandler) IntroduceTyped(ctx context.Context, claims *jwt.Claims, makePrimary bool) (SigilKeyIntroduceReply, error) {
	var zero SigilKeyIntroduceReply
	res, err := h.svc.Introduce(ctx, makePrimary, claims.Subject)
	switch {
	case err == nil:
	case errors.Is(err, sigil.ErrConcurrentPrimary):
		return zero, &problemError{problem.New(problem.TypeSigilKeyConcurrentChange, "",
			"concurrent primary-key change; retry")}
	default:
		// БЕЗОПАСНОСТЬ: error может содержать обёрнутые Vault/PG-детали — в лог
		// (не в ответ), и без приватника (KeyService его в err не кладёт).
		h.logger.Error("sigil.key.introduce: service failed",
			slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "introduce signing key failed")}
	}

	return SigilKeyIntroduceReply{
		View: SigilKeyIntroduceView{
			KeyID:        res.KeyID,
			PubkeyPEM:    res.PubkeyPEM,
			IsPrimary:    res.IsPrimary,
			Status:       res.Status,
			IntroducedAt: res.IntroducedAt,
		},
		CallerAID: claims.Subject,
	}, nil
}

// ListTyped — доменная функция GET /v1/sigil/keys (handler-native, READ без audit):
// active-ключи (primary первым) → [SigilKeyListPage] (items non-nil). Ошибка чтения →
// *problemError (500). vault_ref опущен; introduced_at → UTC+Truncate(Second).
func (h *SigilKeyHandler) ListTyped(ctx context.Context) (SigilKeyListPage, error) {
	keys, err := h.svc.List(ctx)
	if err != nil {
		h.logger.Error("sigil.key.list: service failed", slog.Any("error", err))
		return SigilKeyListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list signing keys failed")}
	}
	items := make([]SigilKeyView, 0, len(keys))
	for _, k := range keys {
		items = append(items, SigilKeyView{
			KeyID:        k.KeyID,
			IsPrimary:    k.IsPrimary,
			Status:       k.Status,
			IntroducedAt: k.IntroducedAt.UTC().Truncate(time.Second),
		})
	}
	return SigilKeyListPage{Items: items}, nil
}

// SigilKeySetPrimaryReply — извлечённый результат [SigilKeyHandler.SetPrimaryTyped]
// (FULL-TYPED). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type SigilKeySetPrimaryReply struct {
	KeyID     string
	CallerAID string
}

// AuditPayload собирает audit-payload set-primary-роута (parity легаси: key_id +
// set_by_aid). Общий для (w,r) и huma-B.
func (r SigilKeySetPrimaryReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"key_id":     r.KeyID,
		"set_by_aid": r.CallerAID,
	}
}

// SetPrimaryTyped — извлечённая доменная функция POST /v1/sigil/keys/{key_id}/primary
// (FULL-TYPED ADR-054 §Pattern (б)): валидация key_id + svc.SetPrimary + sentinel→
// problem. Ошибки — *problemError; успех — [SigilKeySetPrimaryReply].
func (h *SigilKeyHandler) SetPrimaryTyped(ctx context.Context, claims *jwt.Claims, keyID string) (SigilKeySetPrimaryReply, error) {
	var zero SigilKeySetPrimaryReply
	if !reSigilKeyID.MatchString(keyID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'key_id' must match "+reSigilKeyID.String())}
	}

	err := h.svc.SetPrimary(ctx, keyID, claims.Subject)
	switch {
	case err == nil:
	case errors.Is(err, sigil.ErrKeyNotFound):
		return zero, &problemError{problem.New(problem.TypeSigilKeyNotFound, "",
			"no signing key with key_id="+keyID)}
	case errors.Is(err, sigil.ErrKeyRetired):
		return zero, &problemError{problem.New(problem.TypeSigilKeyConcurrentChange, "",
			"signing key "+keyID+" is retired; cannot become primary")}
	case errors.Is(err, sigil.ErrConcurrentPrimary):
		return zero, &problemError{problem.New(problem.TypeSigilKeyConcurrentChange, "",
			"concurrent primary-key change; retry")}
	default:
		h.logger.Error("sigil.key.set-primary: service failed",
			slog.String("key_id", keyID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "set primary signing key failed")}
	}

	return SigilKeySetPrimaryReply{KeyID: keyID, CallerAID: claims.Subject}, nil
}

// SigilKeyRetireReply — извлечённый результат [SigilKeyHandler.RetireTyped]
// (FULL-TYPED). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type SigilKeyRetireReply struct {
	KeyID     string
	CallerAID string
}

// AuditPayload собирает audit-payload retire-роута (parity легаси: key_id +
// retired_by_aid). Общий для (w,r) и huma-B.
func (r SigilKeyRetireReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"key_id":         r.KeyID,
		"retired_by_aid": r.CallerAID,
	}
}

// RetireTyped — извлечённая доменная функция DELETE /v1/sigil/keys/{key_id}
// (FULL-TYPED ADR-054 §Pattern (б)): валидация key_id + svc.Retire + sentinel→
// problem (last-active/primary → 409). Ошибки — *problemError; успех —
// [SigilKeyRetireReply].
func (h *SigilKeyHandler) RetireTyped(ctx context.Context, claims *jwt.Claims, keyID string) (SigilKeyRetireReply, error) {
	var zero SigilKeyRetireReply
	if !reSigilKeyID.MatchString(keyID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'key_id' must match "+reSigilKeyID.String())}
	}

	err := h.svc.Retire(ctx, keyID, claims.Subject)
	switch {
	case err == nil:
	case errors.Is(err, sigil.ErrKeyNotFound):
		return zero, &problemError{problem.New(problem.TypeSigilKeyNotFound, "",
			"no active signing key with key_id="+keyID)}
	case errors.Is(err, sigil.ErrLastActiveKey):
		return zero, &problemError{problem.New(problem.TypeSigilKeyLastActive, "",
			"cannot retire the last active signing key")}
	case errors.Is(err, sigil.ErrRetirePrimary):
		return zero, &problemError{problem.New(problem.TypeSigilKeyPrimary, "",
			"cannot retire the primary key; set another key primary first")}
	default:
		h.logger.Error("sigil.key.retire: service failed",
			slog.String("key_id", keyID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "retire signing key failed")}
	}

	return SigilKeyRetireReply{KeyID: keyID, CallerAID: claims.Subject}, nil
}
