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
				// The first param in coremanifest declared as a list with NO Items, on
				// purpose: its elements are heterogeneous — an integer is one exact code,
				// a "lo-hi" string an inclusive range — so no single element type would be
				// true. Both readers of Items handle nil: sdk/schema validates elements
				// only under `if p.Items != nil`, and the catalog copies it through
				// (keeper/internal/api/handlers/modulecatalog.go) into an omitempty
				// field, so the key is simply absent from the JSON. Declaring Items: Int,
				// as core.http's status_codes does, would promise the form builder
				// something false and read the range form out of the contract.
				"exit_codes": {Type: schema.List, Description: "Exit codes accepted as success: integers and/or \"lo-hi\" ranges, e.g. [0, 1] or [0, \"2-5\"]. Defaults to [0] — any other code fails the task (NIM-687)."},
				"onlyif":     {Type: schema.String, Description: "Idempotency: shell command; exit≠0 → skip."},
				"unless":     {Type: schema.String, Description: "Idempotency: shell command; exit=0 → skip."},
			},
		},
	},
}
