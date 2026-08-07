package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modExec declares core.exec.
var modExec = schema.Module{
	Name:         "exec",
	Capabilities: []schema.Capability{schema.ExecSubprocess},
	States: map[string]schema.State{
		"run": {
			Description: "Run a process with argv without a shell (ADR-015).",
			Input: schema.Input{
				"args":    {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "argv[1:] arguments."},
				"cmd":     {Type: schema.String, Required: true, Example: "/usr/local/bin/myscript", Description: "Executable file name (argv[0], without shell)."},
				"creates": {Type: schema.String, Description: "Idempotency: if the file exists — the step is skipped (changed=false)."},
				"cwd":     {Type: schema.String, Description: "Process working directory."},
				"env":     {Type: schema.Map, Items: &schema.Param{Type: schema.String}, Description: "Additional environment variables (KEY: VALUE)."},
				"onlyif":  {Type: schema.String, Description: "Idempotency: shell command; exit≠0 → skip."},
				"unless":  {Type: schema.String, Description: "Idempotency: shell command; exit=0 → skip."},
			},
		},
	},
}
