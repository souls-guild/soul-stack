package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modLine declares core.line.
var modLine = schema.Module{
	Name:         "line",
	Capabilities: []schema.Capability{schema.FSWriteRoot},
	States: map[string]schema.State{
		"absent": {
			Description: "Removes matching lines (by regexp — all; otherwise by exact line).",
			Input: schema.Input{
				"create": {Type: schema.Bool, Description: "Create the file if it doesn't exist."},
				"group":  {Type: schema.String, Description: "Owner group on create (group name)."},
				"line":   {Type: schema.String, Description: "Exact line to remove (either line or regexp is required)."},
				"mode":   {Type: schema.String, Description: "Permissions on create, octal form."},
				"owner":  {Type: schema.String, Description: "Owner on create (username)."},
				"path":   {Type: schema.String, Required: true, Description: "Target file."},
				"regexp": {Type: schema.String, Description: "RE2 pattern: all matching lines are removed."},
			},
		},
		"present": {
			Description: "The line is present in the file (with regexp — replaces the first matching line); ADR-015.",
			Input: schema.Input{
				"create":       {Type: schema.Bool, Description: "Create the file if it doesn't exist."},
				"group":        {Type: schema.String, Description: "Owner group on create (group name)."},
				"insertafter":  {Type: schema.String, Description: "Literal anchor or EOF — where to insert (mutually exclusive with insertbefore)."},
				"insertbefore": {Type: schema.String, Description: "Literal anchor or BOF — where to insert (mutually exclusive with insertafter)."},
				// line is required for present, but not for absent — the cross-field invariant
				// is checked by Module.Validate (not expressible as per-state schema-required).
				"line":   {Type: schema.String, Description: "Exact line being managed (required for present)."},
				"mode":   {Type: schema.String, Description: "Permissions on create, octal form, e.g. \"0644\"."},
				"owner":  {Type: schema.String, Description: "Owner on create (username)."},
				"path":   {Type: schema.String, Required: true, Description: "Target file."},
				"regexp": {Type: schema.String, Description: "RE2 pattern: the first matching line is replaced with line."},
			},
		},
	},
}
