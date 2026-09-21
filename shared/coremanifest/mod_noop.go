package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modNoop declares core.noop. It performs no actions, so it declares no capabilities.
var modNoop = schema.Module{
	Name: "noop",
	States: map[string]schema.State{
		"run": {
			Description: "No-op. Does nothing, always succeeds with no change (changed=false). barrier anchor / placeholder; ADR-015.",
		},
	},
}
