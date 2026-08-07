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
				"onlyif":  {Type: schema.String, Description: "Idempotency: shell command; exit≠0 → skip."},
				"unless":  {Type: schema.String, Description: "Idempotency: shell command; exit=0 → skip."},
			},
		},
	},
}
