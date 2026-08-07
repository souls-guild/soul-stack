package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modGit declares core.git.
var modGit = schema.Module{
	Name:         "git",
	Capabilities: []schema.Capability{schema.ExecSubprocess, schema.NetworkOutbound},
	States: map[string]schema.State{
		"cloned": {
			Description: "Path contains a git repo (cloned if absent); ADR-015.",
			Input: schema.Input{
				"branch": {Type: schema.String, Description: "Branch (default main)."},
				"depth":  {Type: schema.Int, Description: "Shallow clone depth (--depth)."},
				"path":   {Type: schema.String, Required: true, Description: "Target clone directory."},
				"repo":   {Type: schema.String, Required: true, Description: "Repository URL."},
			},
		},
		"pulled": {
			Description: "Clone-if-missing + git pull --ff-only. Changed only when HEAD changes.",
			Input: schema.Input{
				"branch": {Type: schema.String, Description: "Branch (default main)."},
				"depth":  {Type: schema.Int, Description: "Shallow clone depth (--depth)."},
				"path":   {Type: schema.String, Required: true, Description: "Target clone directory."},
				"repo":   {Type: schema.String, Required: true, Description: "Repository URL."},
			},
		},
	},
}
