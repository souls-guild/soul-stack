package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modTask declares core.task. It touches nothing on the host, so it declares no
// capabilities.
//
// `message` is optional HERE on purpose: whether it is required depends on where
// the task stands — inside a rescue unit it defaults to the original error, outside
// one it is required — and a module schema cannot see that. shared/config enforces
// the outside half (`missing_required_field`, ADR-0091 amendment NIM-993, rule 8).
var modTask = schema.Module{
	Name: "task",
	States: map[string]schema.State{
		"failed": {
			Description: "Always fails its task. Inside a rescue unit it is the explicit \"error not handled\"; elsewhere it fails the task as Ansible's `fail` does. ADR-0091 amendment NIM-993.",
			Input: schema.Input{
				"message": {Type: schema.String, Description: "The failure message. Optional inside a rescue unit, where it defaults to the original error; required anywhere else."},
			},
		},
	},
}
