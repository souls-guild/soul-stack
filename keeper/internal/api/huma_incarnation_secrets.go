package api

// FULL-TYPED reveal-секретов инкарнации (NIM-74, code-first источник OpenAPI). Два
// роута под правом incarnation.view-secrets:
//   - POST /v1/incarnations/{name}/secrets/reveal — раскрытие plaintext (SELF-AUDIT
//     incarnation.secret_revealed внутри RevealSecretTyped; newHumaCadenceAPI, без
//     middleware-навески);
//   - GET /v1/incarnations/{name}/secrets/revealable — discovery (READ, без audit).
// Go-типы — единственный источник правды схемы.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// --- POST .../secrets/reveal (SELF-AUDIT incarnation.secret_revealed) ---

// incRevealSecretInput — huma-input POST .../secrets/reveal. Name — path; тело —
// {secret_id, key}. Версию сервиса клиент НЕ задаёт (берётся inc.ServiceVersion).
type incRevealSecretInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationRevealSecretRequest
}

// IncarnationRevealSecretRequest — тело POST .../secrets/reveal.
type IncarnationRevealSecretRequest struct {
	SecretID string `json:"secret_id" doc:"id раскрываемого секрета (revealable_secrets манифеста сервиса)"`
	Key      string `json:"key" doc:"ключ элемента enumerate-массива текущего state (element.name)"`
}

// IncarnationRevealSecretReply — native 200-тело POST .../secrets/reveal. Value —
// plaintext (санкционированное раскрытие: НЕ прогоняется через MaskSecrets).
type IncarnationRevealSecretReply struct {
	Value string `json:"value" doc:"plaintext-значение секрета"`
}

// incRevealSecretOutput — huma-output POST .../secrets/reveal.
type incRevealSecretOutput struct {
	Body IncarnationRevealSecretReply
}

// incRevealSecretOperation — метаданные POST .../secrets/reveal. DefaultStatus=200.
// Permission incarnation.view-secrets (RBAC-гейт — middleware до handler-а). Errors:
// 403 нет права, 404 вне scope | нет secret_id | key не в state | нет значения,
// 422 невалидный name/secret_id/key, 500.
func incRevealSecretOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "incarnationRevealSecret",
		Method:        http.MethodPost,
		Path:          "/{name}/secrets/reveal",
		Summary:       "Раскрыть plaintext секрета инкарнации",
		Description:   "Резолв plaintext секрета, объявленного revealable_secrets сервиса, из Vault. Permission incarnation.view-secrets (снятие маски, строго привилегированнее incarnation.get). key обязан ∈ enumerate-массива текущего state. Audit incarnation.secret_revealed (без значения). Вне scope → 404.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationRevealSecret монтирует POST .../secrets/reveal (SELF-AUDIT:
// handler пишет incarnation.secret_revealed внутри RevealSecretTyped). incH nil → no-op.
func registerHumaIncarnationRevealSecret(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRevealSecretOperation(), func(ctx context.Context, in *incRevealSecretInput) (*incRevealSecretOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		res, err := incH.RevealSecretTyped(ctx, claims, in.Name, in.Body.SecretID, in.Body.Key)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incRevealSecretOutput{Body: IncarnationRevealSecretReply{Value: res.Value}}, nil
	})
}

// --- GET .../secrets/revealable (READ, без audit) ---

// incRevealableSecretsInput — huma-input GET .../secrets/revealable.
type incRevealableSecretsInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
}

// IncarnationRevealableSecretItem — элемент discovery-ответа.
type IncarnationRevealableSecretItem struct {
	SecretID  string   `json:"secret_id" doc:"id секрета (передаётся в secret_id при reveal)"`
	Label     string   `json:"label" doc:"подпись для UI"`
	StatePath string   `json:"state_path" doc:"state-путь массива (tail enumerate, напр. redis_users)"`
	Keys      []string `json:"keys" doc:"допустимые ключи (element.name текущего state)"`
}

// IncarnationRevealableSecretsReply — native 200-тело GET .../secrets/revealable.
type IncarnationRevealableSecretsReply struct {
	Items []IncarnationRevealableSecretItem `json:"items" doc:"раскрываемые секреты инкарнации"`
}

// incRevealableSecretsOutput — huma-output GET .../secrets/revealable.
type incRevealableSecretsOutput struct {
	Body IncarnationRevealableSecretsReply
}

// incRevealableSecretsOperation — метаданные GET .../secrets/revealable.
// DefaultStatus=200. READ (без audit). Permission incarnation.view-secrets
// (existence-gate). Errors: 403, 404 вне scope, 422 невалидный name, 500.
func incRevealableSecretsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "incarnationRevealableSecrets",
		Method:        http.MethodGet,
		Path:          "/{name}/secrets/revealable",
		Summary:       "Список раскрываемых секретов инкарнации",
		Description:   "Discovery revealable_secrets сервиса + keys из enumerate-массива текущего state. Read-only, без audit. Permission incarnation.view-secrets (existence-gate). Вне scope → 404.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationRevealableSecrets монтирует GET .../secrets/revealable
// (READ, без audit). incH nil → no-op.
func registerHumaIncarnationRevealableSecrets(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRevealableSecretsOperation(), func(ctx context.Context, in *incRevealableSecretsInput) (*incRevealableSecretsOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		res, err := incH.RevealableSecretsTyped(ctx, claims, in.Name)
		if err != nil {
			return nil, incProblem(err)
		}
		items := make([]IncarnationRevealableSecretItem, 0, len(res.Items))
		for _, it := range res.Items {
			keys := it.Keys
			if keys == nil {
				keys = []string{}
			}
			items = append(items, IncarnationRevealableSecretItem{
				SecretID:  it.SecretID,
				Label:     it.Label,
				StatePath: it.StatePath,
				Keys:      keys,
			})
		}
		return &incRevealableSecretsOutput{Body: IncarnationRevealableSecretsReply{Items: items}}, nil
	})
}
