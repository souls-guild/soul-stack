package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modFile declares core.file.
var modFile = schema.Module{
	Name:         "file",
	Capabilities: []schema.Capability{schema.FSWriteRoot},
	States: map[string]schema.State{
		"absent": {
			Description: "File is removed.",
			Input: schema.Input{
				"path": {Type: schema.String, Required: true, Description: "Path of the file to remove."},
			},
		},
		"present": {
			Description: "File exists with the given content/mode/owner/group (ADR-015).",
			Input: schema.Input{
				"content": {Type: schema.String, Description: "File content (inline). Mutually exclusive with src; if neither is given, an empty file."},
				"group":   {Type: schema.String, Description: "Owning group (group name)."},
				"mode":    {Type: schema.String, Description: "Permissions in octal form, e.g. \"0640\"."},
				"owner":   {Type: schema.String, Description: "Owner (username)."},
				"path":    {Type: schema.String, Required: true, Description: "Target file path."},
				"src":     {Type: schema.String, Description: "Absolute path of a regular file on the host, its content is copied to path (typically the result of core.archive.extracted). Sets only the content, not the source's attributes. Mutually exclusive with content."},
			},
		},
		"rendered": {
			Description: "File = result of rendering a text/template template (ADR-010).",
			Input: schema.Input{
				"group":    {Type: schema.String, Description: "Owning group (group name)."},
				"mode":     {Type: schema.String, Description: "Permissions in octal form, e.g. \"0644\"."},
				"owner":    {Type: schema.String, Description: "Owner (username)."},
				"path":     {Type: schema.String, Required: true, Description: "Target path of the rendered file."},
				"template": {Type: schema.String, Required: true, Description: "Path to the .tmpl template (resolved locally, then service-level)."},
				"vars":     {Type: schema.Map, Items: &schema.Param{Type: schema.String}, Description: "Render variables (visible to the template as .vars.*)."},
			},
		},
	},
}
