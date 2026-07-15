package api

// FULL-TYPED reveal of incarnation secrets (NIM-74, code-first OpenAPI source). Two
// routes under the incarnation.view-secrets right:
//   - POST /v1/incarnations/{name}/secrets/reveal — plaintext reveal (SELF-AUDIT
//     incarnation.secret_revealed inside RevealSecretTyped; newHumaCadenceAPI,
//     without middleware wiring);
//   - GET /v1/incarnations/{name}/secrets/revealable — discovery (READ, no audit).
// The Go types are the single source of truth for the schema.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// --- POST .../secrets/reveal (SELF-AUDIT incarnation.secret_revealed) ---

// incRevealSecretInput — huma input for POST .../secrets/reveal. Name — path; body —
// {secret_id, key}. The client does NOT set the service version (taken from
// inc.ServiceVersion).
type incRevealSecretInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationRevealSecretRequest
}

// IncarnationRevealSecretRequest — the body of POST .../secrets/reveal.
type IncarnationRevealSecretRequest struct {
	SecretID string `json:"secret_id" doc:"id раскрываемого секрета (revealable_secrets манифеста сервиса)"`
	Key      string `json:"key" doc:"ключ элемента enumerate-массива текущего state (element.name)"`
}

// IncarnationRevealSecretReply — the native 200 body of POST .../secrets/reveal.
// Value — plaintext (a sanctioned reveal: NOT run through MaskSecrets).
type IncarnationRevealSecretReply struct {
	Value string `json:"value" doc:"plaintext-значение секрета"`
}

// incRevealSecretOutput — huma-output POST .../secrets/reveal.
type incRevealSecretOutput struct {
	Body IncarnationRevealSecretReply
}

// incRevealSecretOperation — metadata for POST .../secrets/reveal.
// DefaultStatus=200. Permission incarnation.view-secrets (RBAC gate — middleware
// before the handler). Errors: 403 no right, 404 out of scope | no secret_id | key
// not in state | no value, 422 invalid name/secret_id/key, 500.
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

// registerHumaIncarnationRevealSecret mounts POST .../secrets/reveal (SELF-AUDIT:
// the handler writes incarnation.secret_revealed inside RevealSecretTyped). incH nil
// → no-op.
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

// --- GET .../secrets/revealable (READ, no audit) ---

// incRevealableSecretsInput — huma-input GET .../secrets/revealable.
type incRevealableSecretsInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
}

// IncarnationRevealableSecretItem — one item of the discovery response.
type IncarnationRevealableSecretItem struct {
	SecretID  string   `json:"secret_id" doc:"id секрета (передаётся в secret_id при reveal)"`
	Label     string   `json:"label" doc:"подпись для UI"`
	StatePath string   `json:"state_path" doc:"state-путь массива (tail enumerate, напр. redis_users)"`
	Keys      []string `json:"keys" doc:"допустимые ключи (element.name текущего state)"`
}

// IncarnationRevealableSecretsReply — the native 200 body of GET .../secrets/revealable.
type IncarnationRevealableSecretsReply struct {
	Items []IncarnationRevealableSecretItem `json:"items" doc:"раскрываемые секреты инкарнации"`
}

// incRevealableSecretsOutput — huma-output GET .../secrets/revealable.
type incRevealableSecretsOutput struct {
	Body IncarnationRevealableSecretsReply
}

// incRevealableSecretsOperation — metadata for GET .../secrets/revealable.
// DefaultStatus=200. READ (no audit). Permission incarnation.view-secrets
// (existence gate). Errors: 403, 404 out of scope, 422 invalid name, 500.
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

// registerHumaIncarnationRevealableSecrets mounts GET .../secrets/revealable
// (READ, no audit). incH nil → no-op.
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
