package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modBootstrap declares the keeper-side ready-made VM onboarding contract.
//
// The state `delivered` was removed in NIM-834 and is deliberately not declared
// as a deprecated stub: a scenario still carrying `core.bootstrap.delivered`
// must be refused offline by soul-lint, not accepted and then silently do
// nothing. Installing a host is site-specific — see the ADR-063 amendment
// 2026-09-09 for the requirements the installer inherited.
var modBootstrap = schema.Module{
	Name: "bootstrap",
	States: map[string]schema.State{
		"issued": {
			Description: "Keeper-side (on:keeper). Atomically issue per-host bootstrap tokens for ready-made VM SIDs.",
			Input: schema.Input{
				"sids": {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.String}, Description: "Non-empty unique list of ready-made VM FQDN/SIDs."},
			},
		},
	},
}
