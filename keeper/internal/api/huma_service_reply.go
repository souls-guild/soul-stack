package api

// HUMA-NATIVE wire-DTO of the SERVICE domain (handler-native T5d). Reply/output Body of huma
// operations — native Go structs in package api, no legacy generator. Handler (handlers/service.go)
// returns domain FLAT results; register-func (huma_service.go) projects them
// directly INTO THESE types — no more legacy-generator → native converters. Key points:
//
//   - SCHEMA NAME = the contract (ServiceView / ServiceListReply / ServiceRefsListReply /
//     GitRef / ServiceStateSchemaReply / StateSchemaMigration / ServiceDependenciesReply /
//     ServiceDependency): huma DefaultSchemaNamer takes reflect.Type.Name().
//   - ENUM field GitRef.Type — native GitRefType (huma_enums.go, INLINE enum): huma
//     inlines the string-named type as `type: string` (schema byte-identical to legacy).
//   - ServiceListReply / ServiceRefsListReply / ServiceDependenciesReply — direct Body
//     types (items-only shape, no generic-envelope alias).
//   - wire SHAPE (json tags/omitempty/date-time/nullable categories A-D ADR-051) —
//     golden byte-exact pinned by huma_service_reply_test.go.
//
// Scenarios-list (handlers.ServiceScenariosReply) is NOT here: its element is the domain
// artifact.Scenario, schema-name alignment is done by huma_service_envelope.go.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// OUTPUT NAME-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). ServiceView.name ← serviceregistry.NamePattern.
// Format for client codegen; the pattern does not affect json.Marshal (golden byte-exact intact).
// ServiceView is output-only (register/update — separate *Request) → no input-422 risk.
// service echo fields (ServiceRefsListReply/ServiceStateSchemaReply/ServiceDependenciesReply.
// service) are NOT tagged: a duplicate of the {name} path parameter, its format is the input domain (outside this
// batch). git/ref — a git-ref, NOT a name (out of scope).

// ServiceView — native body of a Service registry record (POST 201 / GET / PATCH 200 /
// list-element). created_by_aid/refresh/updated_by_aid — `*string` WITH omitempty (nil →
// key omitted); created_at/updated_at — nanosecond time-wire (value is truncated by the
// handler layer, not the shape).
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

// ServiceListReply — native 200 body of GET /v1/services (items under `items`, without
// offset/limit/total). items — native ServiceView.
type ServiceListReply struct {
	Items []ServiceView `json:"items"`
}

// ServiceRefsListReply — native 200 body of GET /v1/services/{name}/refs (service + refs[]).
// refs — native GitRef.
type ServiceRefsListReply struct {
	Refs    []GitRef `json:"refs"`
	Service string   `json:"service"`
}

// ServiceStateSchemaReply — native 200 body of GET /v1/services/{name}/state-schema.
// schema — `*map` WITH omitempty (nil → key omitted); migrations — native StateSchema-
// Migration; state_schema_version — int.
type ServiceStateSchemaReply struct {
	Migrations         []StateSchemaMigration  `json:"migrations"`
	Ref                string                  `json:"ref"`
	Schema             *map[string]interface{} `json:"schema,omitempty"`
	Service            string                  `json:"service"`
	StateSchemaVersion int                     `json:"state_schema_version"`
}

// ServiceDependenciesReply — native 200 body of GET /v1/services/{name}/dependencies
// (service/ref + destiny[]/modules[]). destiny/modules — native ServiceDependency.
type ServiceDependenciesReply struct {
	Destiny []ServiceDependency `json:"destiny"`
	Modules []ServiceDependency `json:"modules"`
	Ref     string              `json:"ref"`
	Service string              `json:"service"`
}

// GitRef — native git-ref record (element ServiceRefsListReply.refs). is_default —
// `*bool` WITH omitempty (nil → key omitted); type — enum GitRefType (wire string,
// schema `type: string`).
type GitRef struct {
	Commit    string     `json:"commit"`
	IsDefault *bool      `json:"is_default,omitempty"`
	Name      string     `json:"name"`
	Type      GitRefType `json:"type"`
}

// StateSchemaMigration — native step of the migration chain (element ServiceStateSchemaReply.
// migrations). from/to — int.
type StateSchemaMigration struct {
	From int    `json:"from"`
	Path string `json:"path"`
	To   int    `json:"to"`
}

// ServiceDependency — native destiny[]/modules[] manifest record (element Service-
// DependenciesReply). git — `*string` WITH omitempty (nil → key omitted).
type ServiceDependency struct {
	Git  *string `json:"git,omitempty"`
	Name string  `json:"name"`
	Ref  string  `json:"ref"`
}

// === projection of domain FLAT handler results → native wire-DTO ===

// newServiceView projects the flat handlers.ServiceView into a native ServiceView (field-
// by-field; date-time truncated at the handler layer).
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

// newServiceListReply projects handlers.ServiceListPage into a native ServiceListReply.
// Preserves nil-vs-empty input 1:1 for byte-exact wire (category B ADR-051).
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

// newGitRef projects the flat handlers.GitRefView into a native GitRef. IsDefault false →
// nil (omitempty omits the key — parity legacy).
func newGitRef(v handlers.GitRefView) GitRef {
	out := GitRef{Commit: v.Commit, Name: v.Name, Type: GitRefType(v.Type)}
	if v.IsDefault {
		d := true
		out.IsDefault = &d
	}
	return out
}

// newServiceRefsListReply projects handlers.ServiceRefsList into native. refs — non-nil
// `[]` (handler returns non-nil, parity with the former contract).
func newServiceRefsListReply(l handlers.ServiceRefsList) ServiceRefsListReply {
	refs := make([]GitRef, 0, len(l.Refs))
	for i := range l.Refs {
		refs = append(refs, newGitRef(l.Refs[i]))
	}
	return ServiceRefsListReply{Refs: refs, Service: l.Service}
}

// newServiceStateSchemaReply projects handlers.ServiceStateSchema into native. schema —
// empty/nil map → nil (omitempty omits the key, parity legacy nullable-object).
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

// newServiceDependency projects the flat handlers.ServiceDependency into native.
// git "" → nil (omitempty omits the key).
func newServiceDependency(d handlers.ServiceDependency) ServiceDependency {
	out := ServiceDependency{Name: d.Name, Ref: d.Ref}
	if d.Git != "" {
		git := d.Git
		out.Git = &git
	}
	return out
}

// newServiceDependenciesReply projects handlers.ServiceDependenciesList into native.
// destiny/modules — non-nil `[]` (handler returns non-nil).
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
