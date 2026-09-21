package api

// HUMA-NATIVE wire-DTO of the SETTINGS domain (ADR-0073). The handler
// (handlers/settings.go) returns the FLAT domain SettingView; the register func
// projects it into these schemas. SCHEMA NAME = the contract one (huma
// DefaultSchemaNamer takes reflect.Type.Name()).

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// SettingReply — one entry of the settings catalog. `value` and `default` are
// typed JSON scalars (a number for a float/int field, a duration string like
// "30s" for a duration one, a boolean for a flag), `source` says which layer the
// effective value came from, in precedence order `default` < `pg` < `file`.
//
// `cluster_value` + `overridden_locally` appear only when this instance's
// keeper.yml shadows an existing cluster override: without them a PUT would read
// as accepted while this host quietly kept its own value.
type SettingReply struct {
	Key         string `json:"key"`
	YAMLPath    string `json:"yaml_path"`
	Type        string `json:"type" enum:"float,int,duration,bool,string"`
	Bounds      string `json:"bounds" doc:"accepted range, e.g. (0, 1] or [30s, 1h]; an enum (\"debug | info | warn | error\") or \"true | false\" where a range makes no sense"`
	Default     any    `json:"default"`
	Value       any    `json:"value" doc:"effective value on this instance"`
	Source      string `json:"source" enum:"default,file,pg"`
	Description string `json:"description"`

	ClusterValue      any  `json:"cluster_value,omitempty" doc:"value stored cluster-wide, present only when this instance's file shadows it"`
	OverriddenLocally bool `json:"overridden_locally,omitempty" doc:"true when keeper.yml on this instance wins over the cluster value"`
}

// SettingsCatalogReply — the native 200 body of GET /v1/settings.
type SettingsCatalogReply struct {
	Items []SettingReply `json:"items"`
}

func newSettingReply(v handlers.SettingView) SettingReply {
	return SettingReply{
		Key:         v.Key,
		YAMLPath:    v.YAMLPath,
		Type:        v.Type,
		Bounds:      v.Bounds,
		Default:     v.Default,
		Value:       v.Value,
		Source:      v.Source,
		Description: v.Description,

		ClusterValue:      v.ClusterValue,
		OverriddenLocally: v.OverriddenLocally,
	}
}

// newSettingsCatalogReply projects the flat domain list into native. items is
// non-nil `[]` (an empty catalog is an empty array in the wire, not null).
func newSettingsCatalogReply(views []handlers.SettingView) SettingsCatalogReply {
	items := make([]SettingReply, 0, len(views))
	for _, v := range views {
		items = append(items, newSettingReply(v))
	}
	return SettingsCatalogReply{Items: items}
}
