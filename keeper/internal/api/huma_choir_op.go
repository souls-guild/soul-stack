package api

// FULL-TYPED форма CHOIR/VOICE-домена (code-first источник OpenAPI, ADR-054 §Pattern).
// БАТЧ-2f WRITE-SELF-AUDIT (choir/voice пишут audit ВНУТРИ handler-а через writeAuditCtx,
// БЕЗ audit-middleware — отличие от middleware-audit-доменов role/operator):
// create — choir.created (201+body); delete — choir.deleted (204); add-voice — choir.
// voice_added (201+body); remove-voice — choir.voice_removed (204); list/list-voices —
// read (БЕЗ audit). Multi-resource: voices — sub-resource /choirs/{choir}/voices[/{sid}],
// huma-op несёт ПОЛНЫЙ путь /{name}/choirs[/...] относительно группы /v1/incarnations.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/incarnations/{name}/choirs (create) — WRITE-SELF-AUDIT choir.created (201+body) ===

// choirCreateInput — huma-input POST .../choirs. Name — path (incarnation-имя);
// Body — typed тело.
type choirCreateInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body ChoirCreateRequest
}

// ChoirCreateRequest — Go-форма тела POST .../choirs (code-first источник схемы И
// валидации). created_by_aid из тела НЕ принимается (берётся из JWT). Формат
// choir_name / size-bounds — доменная валидация (422 в CreateTyped).
// additionalProperties:false (huma-дефолт) → unknown поле → 400. Имя структуры =
// контрактное имя схемы (huma DefaultSchemaNamer; рукопись ChoirCreateRequest, N4).
type ChoirCreateRequest struct {
	ChoirName   string  `json:"choir_name" required:"true" pattern:"^[a-z][a-z0-9_-]*$" doc:"имя Choir-а (^[a-z][a-z0-9_-]*$)"`
	Description *string `json:"description,omitempty" doc:"человекочитаемое описание"`
	MinSize     *int    `json:"min_size,omitempty" doc:"нижний лимит размера партии (> 0)"`
	MaxSize     *int    `json:"max_size,omitempty" doc:"верхний лимит размера партии (≥ min_size)"`
}

// choirCreateOutput — huma-output POST .../choirs (FULL-TYPED). Status=201; Body —
// native 201-тело (Choir; nullable-поля без omitempty → null).
type choirCreateOutput struct {
	Status int `json:"-"`
	Body   Choir
}

// choirCreateOperation — метаданные POST .../choirs. DefaultStatus=201. WRITE-SELF-
// AUDIT choir.created (пишет handler). Errors: 400 unknown/malformed, 403 RBAC, 404
// incarnation, 409 choir-exists, 422 валидация choir_name/size, 500.
func choirCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createChoir",
		Method:        http.MethodPost,
		Path:          "/{name}/choirs",
		Summary:       "Создать Choir",
		Description:   "Declared-топология хостов внутри инкарнации (ADR-044). created_by_aid из JWT. Permission choir.create. 409 — имя занято.",
		Tags:          []string{"choir"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/choirs (list) — READ (БЕЗ audit) ===

// choirListInput — huma-input GET .../choirs. Name — path.
type choirListInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
}

// choirListOutput — huma-output GET .../choirs (FULL-TYPED). Body — native envelope
// (ChoirListReply: items/offset/limit/total).
type choirListOutput struct {
	Body ChoirListReply
}

// choirListOperation — метаданные GET .../choirs. DefaultStatus=200. READ: audit НЕ
// навешан. Permission choir.list. Errors: 403, 422 bad name, 500.
func choirListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listChoirs",
		Method:        http.MethodGet,
		Path:          "/{name}/choirs",
		Summary:       "Список Choir-ов инкарнации",
		Description:   "Топология Choir-ов инкарнации (ADR-044). Permission choir.list. Несуществующая incarnation → items=[]. Read-only, без audit.",
		Tags:          []string{"choir"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/incarnations/{name}/choirs/{choir} (delete) — WRITE-SELF-AUDIT choir.deleted (204) ===

// choirDeleteInput — huma-input DELETE .../choirs/{choir}. Name/Choir — path.
type choirDeleteInput struct {
	Name  string `path:"name" doc:"имя инкарнации"`
	Choir string `path:"choir" doc:"имя Choir-а"`
}

// choirDeleteOutput — huma-output DELETE .../choirs/{choir} (FULL-TYPED). Status=204
// (тела нет).
type choirDeleteOutput struct {
	Status int `json:"-"`
}

// choirDeleteOperation — метаданные DELETE .../choirs/{choir}. DefaultStatus=204.
// WRITE-SELF-AUDIT choir.deleted. Errors: 403, 404 choir, 422 bad path, 500.
func choirDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteChoir",
		Method:        http.MethodDelete,
		Path:          "/{name}/choirs/{choir}",
		Summary:       "Удалить Choir",
		Description:   "Удаляет Choir (каскадом его Voice-ы). Permission choir.delete.",
		Tags:          []string{"choir"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/choirs/{choir}/voices (add-voice) — WRITE-SELF-AUDIT choir.voice_added (201+body) ===

// voiceAddInput — huma-input POST .../voices. Name/Choir — path; Body — typed тело.
type voiceAddInput struct {
	Name  string `path:"name" doc:"имя инкарнации"`
	Choir string `path:"choir" doc:"имя Choir-а"`
	Body  VoiceAddRequest
}

// VoiceAddRequest — Go-форма тела POST .../voices. added_by_aid из тела НЕ
// принимается. Формат sid / role / position ≥0 — доменная валидация (422).
// additionalProperties:false → unknown поле → 400. Имя структуры = контрактное имя
// схемы (huma DefaultSchemaNamer; рукопись VoiceAddRequest, N4).
type VoiceAddRequest struct {
	SID      string  `json:"sid" required:"true" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$" doc:"SID (FQDN) хоста — член инкарнации"`
	Role     *string `json:"role,omitempty" doc:"declared-роль (kebab-case, 1..63)"`
	Position *int    `json:"position,omitempty" doc:"порядковый индекс в партии (≥ 0)"`
}

// voiceAddOutput — huma-output POST .../voices (FULL-TYPED). Status=201; Body —
// native 201-тело (Voice).
type voiceAddOutput struct {
	Status int `json:"-"`
	Body   Voice
}

// voiceAddOperation — метаданные POST .../voices. DefaultStatus=201. WRITE-SELF-AUDIT
// choir.voice_added. Errors: 400 unknown/malformed, 403, 404 choir, 409 voice-exists,
// 422 валидация sid/role/position / SID-не-член, 500.
func voiceAddOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "addVoice",
		Method:        http.MethodPost,
		Path:          "/{name}/choirs/{choir}/voices",
		Summary:       "Добавить Voice в Choir",
		Description:   "Членство SID в Choir-е (ADR-044). added_by_aid из JWT. Permission choir.add-voice. 409 — Voice уже есть; 422 — SID не член инкарнации.",
		Tags:          []string{"choir"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/choirs/{choir}/voices (list-voices) — READ (БЕЗ audit) ===

// voiceListInput — huma-input GET .../voices. Name/Choir — path.
type voiceListInput struct {
	Name  string `path:"name" doc:"имя инкарнации"`
	Choir string `path:"choir" doc:"имя Choir-а"`
}

// voiceListOutput — huma-output GET .../voices (FULL-TYPED). Body — native envelope
// (VoiceListReply).
type voiceListOutput struct {
	Body VoiceListReply
}

// voiceListOperation — метаданные GET .../voices. DefaultStatus=200. READ: audit НЕ
// навешан. Permission choir.list. Errors: 403, 422 bad path, 500.
func voiceListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listVoices",
		Method:        http.MethodGet,
		Path:          "/{name}/choirs/{choir}/voices",
		Summary:       "Список Voice-ов Choir-а",
		Description:   "Члены Choir-а (ADR-044). Permission choir.list. Несуществующий Choir → items=[]. Read-only, без audit.",
		Tags:          []string{"choir"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/incarnations/{name}/choirs/{choir}/voices/{sid} (remove-voice) — WRITE-SELF-AUDIT choir.voice_removed (204) ===

// voiceRemoveInput — huma-input DELETE .../voices/{sid}. Name/Choir/SID — path.
type voiceRemoveInput struct {
	Name  string `path:"name" doc:"имя инкарнации"`
	Choir string `path:"choir" doc:"имя Choir-а"`
	SID   string `path:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$" doc:"SID (FQDN) хоста"`
}

// voiceRemoveOutput — huma-output DELETE .../voices/{sid} (FULL-TYPED). Status=204.
type voiceRemoveOutput struct {
	Status int `json:"-"`
}

// voiceRemoveOperation — метаданные DELETE .../voices/{sid}. DefaultStatus=204.
// WRITE-SELF-AUDIT choir.voice_removed. Errors: 403, 404 voice, 422 bad path, 500.
func voiceRemoveOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "removeVoice",
		Method:        http.MethodDelete,
		Path:          "/{name}/choirs/{choir}/voices/{sid}",
		Summary:       "Убрать Voice из Choir-а",
		Description:   "Снимает членство SID в Choir-е (ADR-044). Permission choir.remove-voice.",
		Tags:          []string{"choir"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
