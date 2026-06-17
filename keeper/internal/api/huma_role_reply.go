package api

// HUMA-NATIVE wire-DTO ROLE-домена (handler-native T5d). Reply/output Body huma-
// операций — native Go-struct в пакете api, БЕЗ legacy-генерата. Handler (handlers/role.go)
// возвращает доменные result-ы с плоскими полями; register-func (huma_role.go)
// проецирует их В ЭТИ типы напрямую — конвертеров legacy-генерата → native больше нет.
// Ключевое:
//
//   - ИМЯ СХЕМЫ = контрактное (RoleView): huma DefaultSchemaNamer берёт
//     reflect.Type.Name() → схема под тем же именем, что давал RoleView.
//   - Единственный reply домена с телом — GET /v1/roles (RoleListReply.Items []RoleView).
//     create/delete/update-permissions/grant/revoke — 201/204 БЕЗ тела.
//   - default_scope/description — `*string` С omitempty (nil → ключ опущен); operators/
//     permissions — `[]string` БЕЗ omitempty (handler даёт non-nil пустой массив → `[]`).
//   - ФОРМА wire (json-теги/omitempty/[]-vs-null категории A-D ADR-051) — golden
//     byte-exact фиксирует huma_role_reply_test.go.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// OUTPUT-PATTERN ИМЁН (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). name ← rbac.RoleNamePattern. Формат для клиент-
// кодогена; pattern не влияет на json.Marshal (golden byte-exact цел). RoleView output-only
// (role.list — отдельный *Request на create) → input-422-риска нет. operators[] НЕ
// тегируется: это AID (operator.AIDPattern), вне name-скоупа этого батча.

// RoleView — native запись каталога ролей (element RoleListReply.items). Форма 1:1 с
// прежним RoleView: builtin (bool), default_scope/description — `*string` С
// omitempty (nil → ключ опущен), operators/permissions — `[]string` БЕЗ omitempty
// (пустой массив, не nil).
type RoleView struct {
	Builtin      bool     `json:"builtin"`
	DefaultScope *string  `json:"default_scope,omitempty"`
	Description  *string  `json:"description,omitempty"`
	Name         string   `json:"name" pattern:"^[a-z][a-z0-9-]*$"` // ← rbac.RoleNamePattern
	Operators    []string `json:"operators"`
	Permissions  []string `json:"permissions"`
}

// === проекция доменного handlers.RoleView (плоские поля) → native wire-DTO ===

// newRoleView проецирует плоскую доменную handlers.RoleView в native RoleView.
// Description отдаётся всегда (даже пустой "" — поле без omitempty в прежнем wire),
// поэтому указатель безусловный; DefaultScope пустой (NULL) → nil (omitempty опускает
// ключ — роль без scope).
func newRoleView(v handlers.RoleView) RoleView {
	desc := v.Description
	out := RoleView{
		Builtin:     v.Builtin,
		Description: &desc,
		Name:        v.Name,
		Operators:   v.Operators,
		Permissions: v.Permissions,
	}
	if v.DefaultScope != "" {
		out.DefaultScope = &v.DefaultScope
	}
	return out
}

// newRoleListReply проецирует доменный handlers.RoleListPage в native RoleListReply
// (items под `items`, БЕЗ пагинации). Сохраняет nil-vs-empty input 1:1 (nil → null,
// [] → []) ради byte-exact wire каталога (категория B ADR-051).
func newRoleListReply(p handlers.RoleListPage) RoleListReply {
	if p.Items == nil {
		return RoleListReply{Items: nil}
	}
	items := make([]RoleView, len(p.Items))
	for i := range p.Items {
		items[i] = newRoleView(p.Items[i])
	}
	return RoleListReply{Items: items}
}
