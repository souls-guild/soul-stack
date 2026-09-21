package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modDirectory declares core.directory.
var modDirectory = schema.Module{
	Name:         "directory",
	Capabilities: []schema.Capability{schema.FSWriteRoot},
	States: map[string]schema.State{
		"absent": {
			Description: "Directory is removed. An empty directory is always removed; a non-empty one only with recursive:true, otherwise an error (a deliberate divergence from a silent rm -rf).",
			Input: schema.Input{
				"path":      {Type: schema.String, Required: true, Description: "Path of the directory to remove."},
				"recursive": {Type: schema.Bool, Description: "Remove a non-empty directory's contents (rm -r semantics). Default false: a non-empty directory errors."},
			},
		},
		"present": {
			Description: "Directory exists with the given owner/group/mode (replacement for `core.exec.run install -d`, ADR-015).",
			Input: schema.Input{
				"group":   {Type: schema.String, Description: "Owning group (group name)."},
				"mode":    {Type: schema.String, Description: "Permissions in octal form, e.g. \"0755\"."},
				"owner":   {Type: schema.String, Description: "Owner (username)."},
				"parents": {Type: schema.Bool, Description: "Create intermediate directories (mkdir -p semantics). Default false."},
				"path":    {Type: schema.String, Required: true, Description: "Target directory path."},
			},
		},
	},
}
