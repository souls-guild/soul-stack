package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modState declares core.state — the keeper-side write point of a service state
// field that carries declared secrets ([ADR-0083] §4).
var modState = schema.Module{
	Name: "state",
	States: map[string]schema.State{
		"present": {
			Description: "Keeper-side (on:keeper). The state field named by key holds set, with every property the service state_schema declares `type: secret` resolved present-not-set: an existing Vault value is kept, a missing one is minted from a generate_secret() request. Returns the effective state with each secret property replaced by its `vault:` reference ([ADR-0083] §4).",
			Input: schema.Input{
				"key": {
					Type: schema.String, Required: true,
					Description: "Top-level property name of the service state_schema to write. The Vault path of each declared secret is DERIVED from (service, incarnation, this field, the element's key) and is never authored.",
				},
				"set": {
					Type: schema.List, Required: true,
					// Declared `list` because a field carrying declared secrets is a
					// collection keyed by the schema's `key:` sibling — the only shape
					// resolveCollection accepts. A scalar secret field is written as a
					// CEL expression (generate_secret() has no literal form), and a
					// CEL-wrapped value is exempt from the literal type check anyway.
					Description: "The field's value: a list of elements, each an object carrying the declared key property. A secret property is written as generate_secret({'length': …, 'charset': …}); omitting it requires the value to already exist.",
				},
			},
		},
	},
}
