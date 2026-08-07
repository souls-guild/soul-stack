package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modUser declares core.user.
var modUser = schema.Module{
	Name:         "user",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.ExecSubprocess},
	States: map[string]schema.State{
		"absent": {
			Description: "User removed.",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "User name."},
			},
		},
		"present": {
			Description: "User exists (present-or-create, no reconcile of an existing user; ADR-015).",
			Input: schema.Input{
				"group":  {Type: schema.String, Description: "Primary group (useradd -g); must already exist. Only on creation."},
				"groups": {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "Supplementary groups (useradd -G). Applies only on creation."},
				"home":   {Type: schema.String, Description: "Home directory (useradd -d). Applies only on creation."},
				"name":   {Type: schema.String, Required: true, Description: "User name."},
				"shell":  {Type: schema.String, Description: "Login shell (useradd -s). Applies only on creation."},
				"system": {Type: schema.Bool, Description: "System account (useradd -r) for service accounts. Only on creation."},
				"uid":    {Type: schema.Int, Description: "Explicit uid (useradd -u). Applies only on creation."},
			},
		},
	},
}
