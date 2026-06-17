package api

// HUMA-NATIVE wire-DTO SYNOD-домена (handler-native T5d). Reply/output Body huma-
// операций — native Go-struct в пакете api, БЕЗ legacy-генерата. Handler (handlers/synod.go)
// возвращает доменные result-ы с плоскими полями; register-func (huma_synod.go)
// проецирует их В ЭТИ типы напрямую — конвертеров legacy-генерата → native больше нет.
//
//   - ИМЯ СХЕМЫ = контрактное (SynodListReply / SynodView): huma DefaultSchemaNamer
//     берёт reflect.Type.Name() → схема под тем же именем, что давал legacy-генерата.
//   - Единственный reply домена с телом — GET /v1/synods (SynodListReply.Items []SynodView).
//     create/update/delete/add/remove-operator/grant/revoke-role — 201/204 БЕЗ тела.
//   - description — `*string` С omitempty (nil → ключ опущен); operators/roles — `[]string`
//     БЕЗ omitempty (handler даёт non-nil пустой массив → `[]`).
//   - ФОРМА wire (категории A-D ADR-051) — golden byte-exact фиксирует huma_synod_reply_test.go.
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). operators[] — per-element AID (член
// группы) ← operator.AIDPattern; huma кладёт pattern в items[]. Формат для клиент-
// кодогена; pattern не влияет на json.Marshal (golden byte-exact цел).
//
// OUTPUT-PATTERN ИМЁН (батч 5): name + roles[] (per-element) ← rbac.RoleNamePattern
// (синод-имя единым reRoleName с role-name по решению synod.go; roles[] — имена ролей).
// huma кладёт per-element pattern в items[]. SynodView output-only (synod.list — отдельный
// *Request на create) → input-422-риска нет.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// SynodListReply — native 200-тело GET /v1/synods (items под `items`, БЕЗ
// offset/limit/total). items — native SynodView. Форма 1:1 с прежним SynodListReply.
type SynodListReply struct {
	Items []SynodView `json:"items"`
}

// SynodView — native проекция Synod-группы (element SynodListReply.items). Форма 1:1 с
// прежним SynodView: builtin (bool), description — `*string` С omitempty (nil → ключ
// опущен), operators/roles — `[]string` БЕЗ omitempty (пустой массив, не nil).
type SynodView struct {
	Builtin     bool     `json:"builtin"`
	Description *string  `json:"description,omitempty"`
	Name        string   `json:"name" pattern:"^[a-z][a-z0-9-]*$"`                  // ← rbac.RoleNamePattern (reRoleName)
	Operators   []string `json:"operators" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern (per-element)
	Roles       []string `json:"roles" pattern:"^[a-z][a-z0-9-]*$"`                 // ← rbac.RoleNamePattern (per-element)
}

// === проекция доменного handlers.SynodView (плоские поля) → native wire-DTO ===

// newSynodView проецирует плоскую доменную handlers.SynodView в native SynodView.
// Description отдаётся всегда (даже пустой "" — поле без omitempty в прежнем wire),
// поэтому указатель безусловный.
func newSynodView(v handlers.SynodView) SynodView {
	desc := v.Description
	return SynodView{
		Builtin:     v.Builtin,
		Description: &desc,
		Name:        v.Name,
		Operators:   v.Operators,
		Roles:       v.Roles,
	}
}

// newSynodListReply проецирует доменный handlers.SynodListPage в native SynodListReply.
// Сохраняет nil-vs-empty input 1:1 (nil → null, [] → []) ради byte-exact wire каталога
// (категория B ADR-051).
func newSynodListReply(p handlers.SynodListPage) SynodListReply {
	if p.Items == nil {
		return SynodListReply{Items: nil}
	}
	items := make([]SynodView, len(p.Items))
	for i := range p.Items {
		items[i] = newSynodView(p.Items[i])
	}
	return SynodListReply{Items: items}
}
