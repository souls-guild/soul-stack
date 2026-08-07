package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modSysctl declares core.sysctl.
var modSysctl = schema.Module{
	Name:         "sysctl",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.ExecSubprocess, schema.FSWriteRoot},
	States: map[string]schema.State{
		"applied": {
			Description: "Bulk set of kernel parameters as one deterministic drop-in in /etc/sysctl.d (sorted keys) + a targeted sysctl -p <file> on change; ADR-015 amend (the module itself owns drop-in+reload+idempotency).",
			Input: schema.Input{
				"filename":        {Type: schema.String, Required: true, Description: "Drop-in file name in /etc/sysctl.d (e.g. 30-redis); the .conf suffix is added automatically."},
				"ignore_failures": {Type: schema.Bool, Default: false, Description: "sysctl -e -p (--ignore): suppresses read-only/nonexistent keys in containers. Explicit opt-in."},
				"reload":          {Type: schema.String, Default: "auto", Enum: []any{"auto", "always", "never"}, Description: "sysctl -p <file> after writing: auto = only when the file changed, always = unconditionally, never = opt-out. reload itself does not mark changed (reuses the core.service daemon_reload dictionary)."},
				"settings":        {Type: schema.Map, Required: true, Items: &schema.Param{Type: schema.String}, Description: "Map of kernel parameter→value. Keys are sorted → drop-in content is deterministic."},
			},
		},
		"present": {
			Description: "Kernel parameter name=value (runtime via sysctl -w + persist in /etc/sysctl.d); ADR-015.",
			Input: schema.Input{
				"filename": {Type: schema.String, Description: "Persist-file name in /etc/sysctl.d (default — name with '.'→'-' + .conf)."},
				"name":     {Type: schema.String, Required: true, Description: "Kernel parameter name (e.g. net.ipv4.ip_forward)."},
				"value":    {Type: schema.String, Required: true, Description: "Desired value."},
			},
		},
	},
}
