package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modGroup declares core.group.
var modGroup = schema.Module{
	Name:         "group",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.ExecSubprocess},
	States: map[string]schema.State{
		"absent": {
			Description: "Group is removed.",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Group name."},
			},
		},
		"present": {
			Description: "Group exists (present-or-create; ADR-015).",
			Input: schema.Input{
				"gid":    {Type: schema.Int, Description: "Explicit gid (groupadd -g). Only takes effect on creation."},
				"name":   {Type: schema.String, Required: true, Description: "Group name."},
				"system": {Type: schema.Bool, Description: "System group (groupadd -r), gid from the system range. Only on creation."},
			},
		},
	},
}
