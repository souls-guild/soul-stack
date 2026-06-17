package api

// HUMA-NATIVE reply-DTO PUSH-PROVIDER-домена (handler-native T5d-2c-full). Reply/output Body
// huma-операций — native Go-struct в пакете api, БЕЗ legacy-генерата. Register-func
// (huma_pushprovider.go) проецирует плоские доменные view-ы handler-а (PushProviderView)
// напрямую В ЭТИ типы — конвертеров legacy-генерата→native больше нет. Ключевое для push-provider:
//
//   - ФОРМА байт-в-байт = прежняя legacy-генерата (json-теги/omitempty/date-time/nullable категории A-D).
//   - ИМЯ СХЕМЫ = контрактное (PushProvider / PushProviderListReply): huma
//     DefaultSchemaNamer берёт reflect.Type.Name() → схема под тем же именем, что давал
//     прежний legacy-генерата.
//   - params — `map[string]interface{}` БЕЗ omitempty: handler нормализует nil→{}
//     (toPushProviderView), на wire всегда объект. updated_by_aid — `*string` С omitempty
//     (nil → ключ опущен). created_at/updated_at — наносекундный time-wire (.UTC() БЕЗ
//     Truncate, значение даёт handler-слой, не форма).
//   - PushProviderListReply — НЕ generic-envelope (не sharedapi.PagedResponse), а обычный
//     reply с полем items[]PushProvider + offset/limit/total (int) → `type: integer` без
//     format (assertOffsetEnvelopeNoFormat). Top-level reply-DTO, не через alias.

import (
	"time"
)

// === top-level reply-DTO (форма 1:1 с прежней legacy-генерата-формой) ===

// PushProvider — native запись реестра push_providers (POST 201 / GET 200 / PUT 200 /
// list-element). Форма 1:1 с PushProvider: params — `map` БЕЗ omitempty (handler даёт
// {} при nil); updated_by_aid — `*string` С omitempty; created_at/updated_at —
// наносекундный time-wire.
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). created_by_aid/updated_by_aid ←
// operator.AIDPattern (формат для клиент-кодогена); pattern не влияет на json.Marshal.
type PushProvider struct {
	CreatedAt    time.Time              `json:"created_at"`
	CreatedByAID string                 `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Name         string                 `json:"name"`
	Params       map[string]interface{} `json:"params"`
	UpdatedAt    time.Time              `json:"updated_at"`
	UpdatedByAID *string                `json:"updated_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
}

// PushProviderListReply — native 200-тело GET /v1/push-providers (offset-envelope:
// items/offset/limit/total). items — native PushProvider; offset/limit/total — int (parity
// legacy-генерата). Форма 1:1 с PushProviderListReply.
type PushProviderListReply struct {
	Items  []PushProvider `json:"items"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
	Total  int            `json:"total"`
}
