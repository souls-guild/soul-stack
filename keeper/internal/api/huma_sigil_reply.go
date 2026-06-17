package api

// HUMA-NATIVE reply-DTO SIGIL-домена (plugins/sigils allow-list; Teardown T5b по эталону
// T5a huma_incarnation_reply.go). Reply/output Body huma-операций — native Go-struct в
// пакете api, НЕ генерёный legacy-генерата. Teardown сносит oapi/ + рукопись: reply-Body должен
// стать code-first native.
//
// ИНВАРИАНТЫ (★ wire byte-exact + ★ имя схемы стабильно): форма байт-в-байт = прежний
// legacy-генерата (те же json-теги; revoked_at — `*time.Time` С omitempty → ключ опущен при nil,
// категория C; allowed_at — наносекундный time-wire значения handler-слоя). Имя
// EXPORTED-struct = контрактное (PluginSigilAllowReply / PluginSigilListReply /
// PluginSigilView) → huma DefaultSchemaNamer даёт ту же схему. PluginSigilListReply — НЕ
// paged-envelope (только items[]). Проекция доменных handlers.Sigil*-result-ов в эти типы
// — register-func (huma_sigil.go); handler отдаёт плоские поля (handler-native T5d).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с прежним legacy-генерата) ===

// PluginSigilAllowReply — native 201-тело POST /v1/plugins/sigils (форма 1:1 с прежним
// PluginSigilAllowReply). namespace/name/ref + sha256 (посчитан Keeper-ом).
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). sha256 — машинно hex(sha256) бинаря
// (hex.EncodeToString, lowercase 64 chars, pluginhost/slot.go:173); allowed_by_aid ←
// operator.AIDPattern. ref НЕ тегируется: это git-ref (tag/branch по ADR-007),
// произвольная строка, НЕ hash.
type PluginSigilAllowReply struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Ref       string `json:"ref"`
	SHA256    string `json:"sha256" pattern:"^[0-9a-f]{64}$"` // hex(sha256) бинаря
}

// PluginSigilListReply — native 200-тело GET /v1/plugins/sigils (форма 1:1 с прежним
// PluginSigilListReply). Только items[] — НЕ paged-envelope.
type PluginSigilListReply struct {
	Items []PluginSigilView `json:"items"`
}

// === nested reply-DTO ===

// PluginSigilView — native элемент items[] (форма 1:1 с прежним PluginSigilView).
// revoked_at — `*time.Time` С omitempty (nil у активных → ключ опущен); allowed_at —
// наносекундный time-wire (значение усекает handler-слой до секунд).
type PluginSigilView struct {
	AllowedAt    time.Time  `json:"allowed_at"`
	AllowedByAID string     `json:"allowed_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Name         string     `json:"name"`
	Namespace    string     `json:"namespace"`
	Ref          string     `json:"ref"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	SHA256       string     `json:"sha256" pattern:"^[0-9a-f]{64}$"` // hex(sha256) бинаря
}

// === проекция доменных handlers.Sigil*-result-ов → native wire-DTO ===

// newPluginSigilAllowReply проецирует плоскую доменную handlers.SigilAllowView в native.
func newPluginSigilAllowReply(v handlers.SigilAllowView) PluginSigilAllowReply {
	return PluginSigilAllowReply{
		Name:      v.Name,
		Namespace: v.Namespace,
		Ref:       v.Ref,
		SHA256:    v.SHA256,
	}
}

// newPluginSigilView проецирует плоскую доменную handlers.SigilView в native. RevokedAt
// и AllowedAt handler уже усёк до секунд (byte-exact с легаси-wire).
func newPluginSigilView(v handlers.SigilView) PluginSigilView {
	return PluginSigilView{
		AllowedAt:    v.AllowedAt,
		AllowedByAID: v.AllowedByAID,
		Name:         v.Name,
		Namespace:    v.Namespace,
		Ref:          v.Ref,
		RevokedAt:    v.RevokedAt,
		SHA256:       v.SHA256,
	}
}

// newPluginSigilListReply проецирует доменный handlers.SigilListPage в native. Items
// сохраняют nil-vs-empty 1:1 (nil → null, [] → []) ради byte-exact — ListTyped даёт
// non-nil [] (пустой реестр → `[]`).
func newPluginSigilListReply(p handlers.SigilListPage) PluginSigilListReply {
	var items []PluginSigilView
	if p.Items != nil {
		items = make([]PluginSigilView, len(p.Items))
		for i := range p.Items {
			items[i] = newPluginSigilView(p.Items[i])
		}
	}
	return PluginSigilListReply{Items: items}
}
