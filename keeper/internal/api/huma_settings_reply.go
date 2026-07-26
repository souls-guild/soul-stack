package api

// HUMA-NATIVE wire-DTO of the SETTINGS domain (ADR-0073). The handler
// (handlers/settings.go) returns the FLAT domain SettingView; the register func
// projects it into these schemas. SCHEMA NAME = the contract one (huma
// DefaultSchemaNamer takes reflect.Type.Name()).

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// SettingReply — one entry of the settings catalog. `value` and `default` are
// typed JSON scalars (number for a float/int field), `source` says which layer
// the effective value came from: `default` < `file` < `pg`.
type SettingReply struct {
	Key         string `json:"key"`
	YAMLPath    string `json:"yaml_path"`
	Type        string `json:"type" enum:"float,int"`
	Bounds      string `json:"bounds" doc:"accepted range, e.g. (0, 1]"`
	Default     any    `json:"default"`
	Value       any    `json:"value" doc:"effective value on this instance"`
	Source      string `json:"source" enum:"default,file,pg"`
	Description string `json:"description"`
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
