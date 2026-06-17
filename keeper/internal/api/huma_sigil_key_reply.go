package api

// HUMA-NATIVE reply-DTO SIGIL-KEY-домена (ротация trust-anchor-ключей подписи Sigil;
// Teardown T5b по эталону T5a huma_incarnation_reply.go). Reply/output Body huma-
// операций — native Go-struct в пакете api, НЕ генерёный legacy-генерата. Teardown сносит oapi/
// + рукопись: reply-Body должен стать code-first native.
//
// ИНВАРИАНТЫ (★ wire byte-exact + ★ имя схемы стабильно): форма байт-в-байт = прежний
// legacy-генерата (те же json-теги; introduced_at — наносекундный time-wire значения handler-слоя
// (.UTC().Truncate(Second))). Имя EXPORTED-struct = контрактное (SigilKeyIntroduceReply /
// SigilKeyListReply / SigilKeyView) → huma DefaultSchemaNamer даёт ту же схему.
// SigilKeyListReply — НЕ paged-envelope (только items[]). Проекция доменных
// handlers.SigilKey*-result-ов в эти типы — register-func (huma_sigil_key.go).
//
// STATUS-ПОЛЯ — native enum-типы SigilKeyIntroduceReplyStatus / SigilKeyViewStatus
// (huma_enums.go; per-field string-enum, рукопись инлайнит string+enum). Handler отдаёт
// status plain-string-ом, register-func кастует в native enum (тот же underlying string).
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). key_id — машинно hex(sha256) SPKI-DER
// публичного ключа (keyIDFromPublic, keyservice.go:287; hex.EncodeToString → lowercase
// 64 chars). Формат для клиент-кодогена; pattern не влияет на json.Marshal (golden цел).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с прежним legacy-генерата) ===

// SigilKeyIntroduceReply — native 201-тело POST /v1/sigil/keys (форма 1:1 с прежним
// SigilKeyIntroduceReply). БЕЗ приватника. status — native enum
// SigilKeyIntroduceReplyStatus (wire — строка); introduced_at — наносекундный time-wire.
type SigilKeyIntroduceReply struct {
	IntroducedAt time.Time                    `json:"introduced_at"`
	IsPrimary    bool                         `json:"is_primary"`
	KeyID        string                       `json:"key_id" pattern:"^[0-9a-f]{64}$"` // hex(sha256) SPKI-DER
	PubkeyPEM    string                       `json:"pubkey_pem"`
	Status       SigilKeyIntroduceReplyStatus `json:"status"`
}

// SigilKeyListReply — native 200-тело GET /v1/sigil/keys (форма 1:1 с прежним
// SigilKeyListReply). Только items[] — НЕ paged-envelope.
type SigilKeyListReply struct {
	Items []SigilKeyView `json:"items"`
}

// === nested reply-DTO ===

// SigilKeyView — native проекция active-ключа (форма 1:1 с прежним SigilKeyView).
// БЕЗ vault_ref. status — native enum SigilKeyViewStatus (wire — строка); introduced_at —
// наносекундный time-wire (значение усекает handler-слой до секунд).
type SigilKeyView struct {
	IntroducedAt time.Time          `json:"introduced_at"`
	IsPrimary    bool               `json:"is_primary"`
	KeyID        string             `json:"key_id" pattern:"^[0-9a-f]{64}$"` // hex(sha256) SPKI-DER
	Status       SigilKeyViewStatus `json:"status"`
}

// === проекция доменных handlers.SigilKey*-result-ов → native wire-DTO ===

// newSigilKeyIntroduceReply проецирует плоскую доменную handlers.SigilKeyIntroduceView в
// native. Status — native enum-каст (тот же underlying string); introduced_at handler
// отдаёт как есть (byte-exact с легаси-wire).
func newSigilKeyIntroduceReply(v handlers.SigilKeyIntroduceView) SigilKeyIntroduceReply {
	return SigilKeyIntroduceReply{
		IntroducedAt: v.IntroducedAt,
		IsPrimary:    v.IsPrimary,
		KeyID:        v.KeyID,
		PubkeyPEM:    v.PubkeyPEM,
		Status:       SigilKeyIntroduceReplyStatus(v.Status),
	}
}

// newSigilKeyView проецирует плоскую доменную handlers.SigilKeyView в native.
// introduced_at handler уже усёк до секунд (byte-exact с легаси-wire).
func newSigilKeyView(v handlers.SigilKeyView) SigilKeyView {
	return SigilKeyView{
		IntroducedAt: v.IntroducedAt,
		IsPrimary:    v.IsPrimary,
		KeyID:        v.KeyID,
		Status:       SigilKeyViewStatus(v.Status),
	}
}

// newSigilKeyListReply проецирует доменный handlers.SigilKeyListPage в native. Items
// сохраняют nil-vs-empty 1:1 (nil → null, [] → []) ради byte-exact — ListTyped даёт
// non-nil [] (пустой реестр → `[]`).
func newSigilKeyListReply(p handlers.SigilKeyListPage) SigilKeyListReply {
	var items []SigilKeyView
	if p.Items != nil {
		items = make([]SigilKeyView, len(p.Items))
		for i := range p.Items {
			items[i] = newSigilKeyView(p.Items[i])
		}
	}
	return SigilKeyListReply{Items: items}
}
