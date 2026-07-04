package api

// POST /v1/sse-token — минтинг короткоживущего SSE-транспорт-токена (ADR-068 §A0/A3,
// под-развилка A). browser-EventSource НЕ умеет слать `Authorization` → web дёргает
// этот authed-эндпоинт (Bearer) и открывает EventSource(…?access_token=<short-jwt>).
// Токен подписан ТЕМ ЖЕ ключом (issuer), что operator-JWT → middleware.RequireJWT
// верифицирует его без правок; НО TTL ~60s вместо 30 дней: 30-дневный operator-JWT в
// URL оседает в reverse-proxy/access-log = долгоживущая утечка (query-token —
// security-floor ниже header-а, ADR-068 §A3).

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// sseTokenTTL — время жизни короткоживущего SSE-транспорт-токена (~60s): окна на
// открытие EventSource-стрима хватает, а утечка токена из access-log истекает почти
// сразу. Единый источник для минтинга и wire-поля expires_in.
const sseTokenTTL = 60 * time.Second

// sseTokenHandler минтит короткоживущий JWT для SSE-транспорта (ADR-068 §A0).
type sseTokenHandler struct {
	issuer handlers.JWTIssuer
	ttl    time.Duration
	logger *slog.Logger
}

// newSseTokenHandler собирает handler. logger nil → discard.
func newSseTokenHandler(issuer handlers.JWTIssuer, logger *slog.Logger) *sseTokenHandler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &sseTokenHandler{issuer: issuer, ttl: sseTokenTTL, logger: logger}
}

// SseTokenReply — 200-тело POST /v1/sse-token.
type SseTokenReply struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // TTL в секундах (~60)
}

// sseTokenInput — пустой (тело/параметры не нужны: identity берётся из JWT).
type sseTokenInput struct{}

type sseTokenOutput struct {
	Body SseTokenReply
}

func sseTokenOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "issueSseToken",
		Method:        http.MethodPost,
		Path:          "/sse-token",
		Summary:       "Выпустить короткоживущий SSE-транспорт-токен",
		Description:   "Минтит JWT TTL~60s тем же signing key для открытия EventSource (browser не шлёт Authorization). Требует Bearer (сам эндпоинт — не SSE, query-token не принимает). Токен удостоверяет текущего оператора; RBAC доступа к прогону проверяет SSE-route.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusUnauthorized, http.StatusInternalServerError},
	}
}

// registerHumaSseToken монтирует POST /v1/sse-token. h nil → no-op (opt-in wire-up).
func registerHumaSseToken(humaAPI huma.API, h *sseTokenHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, sseTokenOperation(), func(ctx context.Context, _ *sseTokenInput) (*sseTokenOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok || claims == nil {
			return nil, incMissingClaims()
		}
		// Короткоживущий токен несёт ту же identity (sub+roles), что operator-JWT: RBAC
		// доступа к apply_id проверяет сам SSE-route — токен лишь удостоверяет оператора.
		tok, err := h.issuer.Issue(claims.Subject, claims.Roles, h.ttl, false)
		if err != nil {
			h.logger.Error("sse-token: issue failed", slog.Any("error", err))
			return nil, humaProblemError{Details: problemWithStatus(problem.TypeInternalError, http.StatusInternalServerError, "issue sse token failed")}
		}
		return &sseTokenOutput{Body: SseTokenReply{AccessToken: tok, ExpiresIn: int(h.ttl / time.Second)}}, nil
	})
}
