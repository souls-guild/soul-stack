package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modModule declares core.module.
var modModule = schema.Module{
	Name: "module",
	// Fetch travels over the existing mTLS EventStream ClientConn to Keeper (ADR-065(a)).
	Capabilities: []schema.Capability{schema.NetworkOutbound},
	States: map[string]schema.State{
		"installed": {
			Description: "The SoulModule plugin from the active Sigil grant is pulled from Keeper (FetchModule), verified, and atomically installed into a catalog slot of the module cache. Idempotent by the binary's sha256 (ADR-065).",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Full plugin name \"<namespace>.<name>\" (e.g. community.redis)."},
				"ref":  {Type: schema.String, Description: "Pin check (NOT a version selector): the active Sigil grant must be on this ref, otherwise module_not_allowed."},
			},
		},
	},
}
