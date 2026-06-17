package api

// FULL-TYPED форма SIGIL-KEY-домена (ротация trust-anchor-ключей подписи Sigil,
// ADR-026(h) R3-S7; code-first источник OpenAPI, ADR-054 §Pattern). ТИРАЖ-БАТЧ-2a:
// introduce (WRITE+AUDIT sigil.key-introduced, 201+body), list (read-bare, БЕЗ audit),
// set-primary + retire (WRITE+AUDIT sigil.key-primary-set / sigil.key-retired, 204,
// path key_id). Go-типы — единственный источник правды.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/sigil/keys (introduce) — WRITE+AUDIT sigil.key-introduced ===

// sigilKeyIntroduceInput — huma-input POST /v1/sigil/keys (FULL-TYPED). Body —
// типизированное тело (опц. make_primary). Тело целиком опционально (пустое →
// make_primary=false): make_primary — `*bool omitempty` (parity легаси-контракта,
// presence-PATCH здесь НЕ нужен — нет различия omitted/null, только default false).
type sigilKeyIntroduceInput struct {
	Body SigilKeyIntroduceRequest
}

// SigilKeyIntroduceRequest — Go-форма тела POST /v1/sigil/keys. make_primary —
// опц. флаг «сделать новый ключ primary» (parity SigilKeyIntroduceRequest).
// additionalProperties:false → unknown→400. Имя структуры = контрактное имя схемы
// (huma DefaultSchemaNamer; рукопись SigilKeyIntroduceRequest, N4).
type SigilKeyIntroduceRequest struct {
	MakePrimary *bool `json:"make_primary,omitempty" doc:"сделать новый ключ primary (новые Sigil-ы подписываются им); default false"`
}

// sigilKeyIntroduceOutput — huma-output POST /v1/sigil/keys (FULL-TYPED). Status=201;
// Body — native 201-тело (SigilKeyIntroduceReply: key_id/pubkey_pem/is_primary/
// status/introduced_at). БЕЗ приватника (SENSITIVE никогда не покидает KeyService).
type sigilKeyIntroduceOutput struct {
	Status int `json:"-"`
	Body   SigilKeyIntroduceReply
}

// sigilKeyIntroduceOperation — метаданные POST /v1/sigil/keys. Path = "/"
// относительно chi-группы /v1/sigil/keys. DefaultStatus=201. Permission
// sigil.key-introduce + audit sigil.key-introduced. Errors: 400 unknown/malformed,
// 403 RBAC, 409 concurrent-primary-change, 500.
func sigilKeyIntroduceOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "introduceSigilKey",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Ввести ключ подписи Sigil",
		Description:   "Генерирует ed25519-пару, пишет приватник в Vault, вводит публичную часть в реестр trust-anchor-ов (ADR-026(h)). Permission sigil.key-introduce. Возвращает pubkey, НЕ приватник.",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusInternalServerError},
	}
}

// === GET /v1/sigil/keys (list) — READ-bare (БЕЗ audit) ===

// sigilKeyListInput — huma-input GET /v1/sigil/keys. Параметров нет (active-ключи
// без фильтров) — пустая структура (parity roleListInput).
type sigilKeyListInput struct{}

// sigilKeyListOutput — huma-output GET /v1/sigil/keys (FULL-TYPED). Body — native
// 200-тело (SigilKeyListReply: active-ключи, primary первым, БЕЗ vault_ref).
// Wire-форма (items non-nil [], introduced_at секундной точности, typed status-enum)
// зафиксирована golden-JSON snapshot-тестом.
type sigilKeyListOutput struct {
	Body SigilKeyListReply
}

// sigilKeyListOperation — метаданные GET /v1/sigil/keys. Path = "/" относительно
// chi-группы /v1/sigil/keys. DefaultStatus=200. READ-роут: audit НЕ навешан.
func sigilKeyListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listSigilKeys",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список active-ключей подписи Sigil",
		Description:   "Active trust-anchor-ключи подписи (primary первым, без vault_ref, ADR-026(h)). Permission sigil.key-list. Read-only, без audit.",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === POST /v1/sigil/keys/{key_id}/primary (set-primary) — WRITE+AUDIT sigil.key-primary-set ===

// sigilKeySetPrimaryInput — huma-input POST /v1/sigil/keys/{key_id}/primary. key_id —
// path-параметр (huma извлекает по `path:"key_id"`). Формат (reSigilKeyID, 64 hex) —
// доменная валидация в SetPrimaryTyped (422). Body нет.
type sigilKeySetPrimaryInput struct {
	KeyID string `path:"key_id" doc:"key_id ключа подписи (SHA-256(SPKI), 64 hex)"`
}

// sigilKeyNoContentOutput — huma-output 204-write-роутов set-primary/retire. БЕЗ Body
// (легаси-контракт: 204 No Content). huma на output без Body → SetStatus(204) → пусто.
type sigilKeyNoContentOutput struct {
	Status int `json:"-"`
}

// sigilKeySetPrimaryOperation — метаданные POST /v1/sigil/keys/{key_id}/primary.
// DefaultStatus=204. Permission sigil.key-set-primary + audit sigil.key-primary-set.
// Errors: 403 RBAC, 404 key-not-found, 409 retired/concurrent-change, 422 bad key_id, 500.
func sigilKeySetPrimaryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "setPrimarySigilKey",
		Method:        http.MethodPost,
		Path:          "/{key_id}/primary",
		Summary:       "Сделать ключ подписи primary",
		Description:   "Назначает active-ключ primary (новые Sigil-ы подписываются им, ADR-026(h)). Permission sigil.key-set-primary. 404 — ключа нет; 409 — retired/гонка.",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/sigil/keys/{key_id} (retire) — WRITE+AUDIT sigil.key-retired ===

// sigilKeyRetireInput — huma-input DELETE /v1/sigil/keys/{key_id}. key_id — path-
// параметр. Формат (reSigilKeyID) — доменная валидация в RetireTyped. Body нет.
type sigilKeyRetireInput struct {
	KeyID string `path:"key_id" doc:"key_id ключа подписи для вывода (SHA-256(SPKI), 64 hex)"`
}

// sigilKeyRetireOperation — метаданные DELETE /v1/sigil/keys/{key_id}.
// DefaultStatus=204. Permission sigil.key-retire + audit sigil.key-retired. Errors:
// 403 RBAC, 404 key-not-found, 409 last-active/primary, 422 bad key_id, 500.
func sigilKeyRetireOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "retireSigilKey",
		Method:        http.MethodDelete,
		Path:          "/{key_id}",
		Summary:       "Вывести ключ подписи из active",
		Description:   "Помечает ключ retired (ADR-026(h)). Permission sigil.key-retire. 404 — active-записи нет; 409 — последний active либо primary (сперва SetPrimary другому).",
		Tags:          []string{"sigil-key"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
