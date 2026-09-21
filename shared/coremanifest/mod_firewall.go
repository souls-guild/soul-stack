package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modFirewall declares core.firewall.
var modFirewall = schema.Module{
	Name:         "firewall",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.ExecSubprocess},
	States: map[string]schema.State{
		"absent": {
			Description: "Rule removed.",
			Input: schema.Input{
				"action": {Type: schema.String, Enum: []any{"allow", "deny"}, Description: "Action allow|deny (default allow)."},
				"port":   {Type: schema.Int, Required: true, Description: "Port 1..65535."},
				"proto":  {Type: schema.String, Enum: []any{"tcp", "udp"}, Description: "Protocol tcp|udp (default tcp)."},
				"source": {Type: schema.String, Description: "IPv4 CIDR or a single IPv4."},
				"zone":   {Type: schema.String, Description: "firewalld zone."},
			},
		},
		"present": {
			Description: "Firewall rule exists (port/proto/source/action); ufw/firewalld. ADR-015/ADR-016.",
			// port is read as int OR string (for ${...} interpolation). The schema type is
			// int (numeric literal); the string form passes as a CEL-wrapped value.
			Input: schema.Input{
				"action": {Type: schema.String, Enum: []any{"allow", "deny"}, Description: "Action allow|deny (default allow)."},
				"port":   {Type: schema.Int, Required: true, Description: "Port 1..65535."},
				"proto":  {Type: schema.String, Enum: []any{"tcp", "udp"}, Description: "Protocol tcp|udp (default tcp)."},
				"source": {Type: schema.String, Description: "IPv4 CIDR or a single IPv4 (default any). IPv6 not supported in MVP."},
				"zone":   {Type: schema.String, Description: "firewalld zone (default - default zone)."},
			},
		},
	},
}
