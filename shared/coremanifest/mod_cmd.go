package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modCmd declares core.cmd.
var modCmd = schema.Module{
	Name:         "cmd",
	Capabilities: []schema.Capability{schema.ExecSubprocess},
	States: map[string]schema.State{
		"shell": {
			Description: "Run a shell string via `sh -c` (pipes/redirects/glob). TRUSTED-ONLY (ADR-015).",
			Input: schema.Input{
				"cmd":     {Type: schema.String, Required: true, Multiline: true, Example: "systemctl daemon-reload\nsystemctl restart nginx", Description: "Shell string, executed via `sh -c`."},
				"creates": {Type: schema.String, Description: "Idempotency: if the file exists — the step is skipped (changed=false)."},
				"cwd":     {Type: schema.String, Description: "Process working directory."},
				"env":     {Type: schema.Map, Items: &schema.Param{Type: schema.String}, Description: "Additional environment variables (KEY: VALUE)."},
				// List with no Items — see the note on the same param in mod_exec.go.
				// The two verb-shell modules share one contract (coremanifest.verbShellModules);
				// the knob has to exist on both or it splits. The two descriptions differ
				// on purpose — only this one needs the `sh -c` pipeline sentence.
				"exit_codes": {Type: schema.List, Description: "Exit codes accepted as success: integers and/or \"lo-hi\" ranges, e.g. [0, 1] or [0, \"2-5\"]. Defaults to [0] — any other code fails the task (NIM-687). `sh -c` reports the LAST command of a pipeline."},
				"onlyif":     {Type: schema.String, Description: "Idempotency: shell command; exit≠0 → skip."},
				"unless":     {Type: schema.String, Description: "Idempotency: shell command; exit=0 → skip."},
			},
		},
	},
}
