package api

// HUMA-NATIVE wire-DTO SERVICE-домена (handler-native T5d). Reply/output Body huma-
// операций — native Go-struct в пакете api, БЕЗ legacy-генерата. Handler (handlers/service.go)
// возвращает доменные ПЛОСКИЕ result-ы; register-func (huma_service.go) проецирует их
// В ЭТИ типы напрямую — конвертеров legacy-генерата → native больше нет. Ключевое:
//
//   - ИМЯ СХЕМЫ = контрактное (ServiceView / ServiceListReply / ServiceRefsListReply /
//     GitRef / ServiceStateSchemaReply / StateSchemaMigration / ServiceDependenciesReply /
//     ServiceDependency): huma DefaultSchemaNamer берёт reflect.Type.Name().
//   - ENUM-поле GitRef.Type — native GitRefType (huma_enums.go, INLINE-enum): huma
//     инлайнит string-named-тип как `type: string` (схема byte-identical legacy).
//   - ServiceListReply / ServiceRefsListReply / ServiceDependenciesReply — direct Body-
//     типы (items-only форма, без generic-envelope-alias).
//   - ФОРМА wire (json-теги/omitempty/date-time/nullable категории A-D ADR-051) —
//     golden byte-exact фиксирует huma_service_reply_test.go.
//
// Scenarios-list (handlers.ServiceScenariosReply) НЕ здесь: его element — domain
// artifact.Scenario, выравнивание имени схемы делает huma_service_envelope.go.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// OUTPUT-PATTERN ИМЁН (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). ServiceView.name ← serviceregistry.NamePattern.
// Формат для клиент-кодогена; pattern не влияет на json.Marshal (golden byte-exact цел).
// ServiceView output-only (register/update — отдельный *Request) → input-422-риска нет.
// service-эхо-поля (ServiceRefsListReply/ServiceStateSchemaReply/ServiceDependenciesReply.
// service) НЕ тегируются: дубль path-параметра {name}, его формат — input-домен (вне этого
// батча). git/ref — git-ref, НЕ имя (вне скоупа).

// ServiceView — native тело записи реестра Service-а (POST 201 / GET / PATCH 200 /
// list-element). created_by_aid/refresh/updated_by_aid — `*string` С omitempty (nil →
// ключ опущен); created_at/updated_at — наносекундный time-wire (значение усекает
// handler-слой, не форма).
type ServiceView struct {
	CreatedAt    time.Time `json:"created_at"`
	CreatedByAID *string   `json:"created_by_aid,omitempty"`
	Git          string    `json:"git"`
	Name         string    `json:"name" pattern:"^[a-z][a-z0-9-]*$"` // ← serviceregistry.NamePattern
	Ref          string    `json:"ref"`
	Refresh      *string   `json:"refresh,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
	UpdatedByAID *string   `json:"updated_by_aid,omitempty"`
}

// ServiceListReply — native 200-тело GET /v1/services (items под `items`, БЕЗ
// offset/limit/total). items — native ServiceView.
type ServiceListReply struct {
	Items []ServiceView `json:"items"`
}

// ServiceRefsListReply — native 200-тело GET /v1/services/{name}/refs (service + refs[]).
// refs — native GitRef.
type ServiceRefsListReply struct {
	Refs    []GitRef `json:"refs"`
	Service string   `json:"service"`
}

// ServiceStateSchemaReply — native 200-тело GET /v1/services/{name}/state-schema.
// schema — `*map` С omitempty (nil → ключ опущен); migrations — native StateSchema-
// Migration; state_schema_version — int.
type ServiceStateSchemaReply struct {
	Migrations         []StateSchemaMigration  `json:"migrations"`
	Ref                string                  `json:"ref"`
	Schema             *map[string]interface{} `json:"schema,omitempty"`
	Service            string                  `json:"service"`
	StateSchemaVersion int                     `json:"state_schema_version"`
}

// ServiceDependenciesReply — native 200-тело GET /v1/services/{name}/dependencies
// (service/ref + destiny[]/modules[]). destiny/modules — native ServiceDependency.
type ServiceDependenciesReply struct {
	Destiny []ServiceDependency `json:"destiny"`
	Modules []ServiceDependency `json:"modules"`
	Ref     string              `json:"ref"`
	Service string              `json:"service"`
}

// GitRef — native запись git-ref (element ServiceRefsListReply.refs). is_default —
// `*bool` С omitempty (nil → ключ опущен); type — enum GitRefType (wire-строка,
// schema `type: string`).
type GitRef struct {
	Commit    string     `json:"commit"`
	IsDefault *bool      `json:"is_default,omitempty"`
	Name      string     `json:"name"`
	Type      GitRefType `json:"type"`
}

// StateSchemaMigration — native шаг цепочки миграций (element ServiceStateSchemaReply.
// migrations). from/to — int.
type StateSchemaMigration struct {
	From int    `json:"from"`
	Path string `json:"path"`
	To   int    `json:"to"`
}

// ServiceDependency — native запись destiny[]/modules[] манифеста (element Service-
// DependenciesReply). git — `*string` С omitempty (nil → ключ опущен).
type ServiceDependency struct {
	Git  *string `json:"git,omitempty"`
	Name string  `json:"name"`
	Ref  string  `json:"ref"`
}

// === проекция доменных ПЛОСКИХ handler-result-ов → native wire-DTO ===

// newServiceView проецирует плоскую handlers.ServiceView в native ServiceView (поле-
// в-поле; date-time усечён на handler-слое).
func newServiceView(v handlers.ServiceView) ServiceView {
	return ServiceView{
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Git:          v.Git,
		Name:         v.Name,
		Ref:          v.Ref,
		Refresh:      v.Refresh,
		UpdatedAt:    v.UpdatedAt,
		UpdatedByAID: v.UpdatedByAID,
	}
}

// newServiceListReply проецирует handlers.ServiceListPage в native ServiceListReply.
// Сохраняет nil-vs-empty input 1:1 ради byte-exact wire (категория B ADR-051).
func newServiceListReply(p handlers.ServiceListPage) ServiceListReply {
	if p.Items == nil {
		return ServiceListReply{Items: nil}
	}
	items := make([]ServiceView, len(p.Items))
	for i := range p.Items {
		items[i] = newServiceView(p.Items[i])
	}
	return ServiceListReply{Items: items}
}

// newGitRef проецирует плоскую handlers.GitRefView в native GitRef. IsDefault false →
// nil (omitempty опускает ключ — parity legacy).
func newGitRef(v handlers.GitRefView) GitRef {
	out := GitRef{Commit: v.Commit, Name: v.Name, Type: GitRefType(v.Type)}
	if v.IsDefault {
		d := true
		out.IsDefault = &d
	}
	return out
}

// newServiceRefsListReply проецирует handlers.ServiceRefsList в native. refs — non-nil
// `[]` (handler даёт non-nil, parity прежнего контракта).
func newServiceRefsListReply(l handlers.ServiceRefsList) ServiceRefsListReply {
	refs := make([]GitRef, 0, len(l.Refs))
	for i := range l.Refs {
		refs = append(refs, newGitRef(l.Refs[i]))
	}
	return ServiceRefsListReply{Refs: refs, Service: l.Service}
}

// newServiceStateSchemaReply проецирует handlers.ServiceStateSchema в native. schema —
// пустая/nil-карта → nil (omitempty опускает ключ, parity legacy nullable-object).
func newServiceStateSchemaReply(s handlers.ServiceStateSchema) ServiceStateSchemaReply {
	migrations := make([]StateSchemaMigration, 0, len(s.Migrations))
	for _, m := range s.Migrations {
		migrations = append(migrations, StateSchemaMigration{From: m.From, Path: m.Path, To: m.To})
	}
	out := ServiceStateSchemaReply{
		Migrations:         migrations,
		Ref:                s.Ref,
		Service:            s.Service,
		StateSchemaVersion: s.StateSchemaVersion,
	}
	if len(s.Schema) > 0 {
		m := map[string]interface{}(s.Schema)
		out.Schema = &m
	}
	return out
}

// newServiceDependency проецирует плоскую handlers.ServiceDependency в native.
// git "" → nil (omitempty опускает ключ).
func newServiceDependency(d handlers.ServiceDependency) ServiceDependency {
	out := ServiceDependency{Name: d.Name, Ref: d.Ref}
	if d.Git != "" {
		git := d.Git
		out.Git = &git
	}
	return out
}

// newServiceDependenciesReply проецирует handlers.ServiceDependenciesList в native.
// destiny/modules — non-nil `[]` (handler даёт non-nil).
func newServiceDependenciesReply(l handlers.ServiceDependenciesList) ServiceDependenciesReply {
	return ServiceDependenciesReply{
		Destiny: newServiceDependencySlice(l.Destiny),
		Modules: newServiceDependencySlice(l.Modules),
		Ref:     l.Ref,
		Service: l.Service,
	}
}

func newServiceDependencySlice(in []handlers.ServiceDependency) []ServiceDependency {
	out := make([]ServiceDependency, 0, len(in))
	for i := range in {
		out = append(out, newServiceDependency(in[i]))
	}
	return out
}
