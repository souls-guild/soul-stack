package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modService declares core.service.
var modService = schema.Module{
	Name:         "service",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.ExecSubprocess},
	States: map[string]schema.State{
		"disabled": {
			Description: "Service autostart on system boot is off (orthogonal to the running/stopped runtime state — a disabled unit may still be running).",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Service/unit name."},
			},
		},
		"enabled": {
			Description: "Service autostart on system boot.",
			Input: schema.Input{
				"daemon_reload": {Type: schema.String, Default: "auto", Enum: []any{"auto", "always", "never"}, Description: "systemctl daemon-reload before enable (systemd): auto = on NeedDaemonReload, always = unconditionally, never = opt-out. openrc/sysv — no-op (ADR-015)."},
				"name":          {Type: schema.String, Required: true, Description: "Service/unit name."},
			},
		},
		"masked": {
			Description: "The unit is masked (systemctl mask, symlink → /dev/null) — start is impossible manually or as a dependency. systemd-only; openrc/sysv is a fatal error. Apply disables the unit before masking (ADR-015).",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Service/unit name."},
			},
		},
		"restarted": {
			Description: "Unconditional restart (changed is always true).",
			Input: schema.Input{
				"daemon_reload": {Type: schema.String, Default: "auto", Enum: []any{"auto", "always", "never"}, Description: "systemctl daemon-reload before restart (systemd): auto = on NeedDaemonReload, always = unconditionally, never = opt-out. openrc/sysv — no-op (ADR-015)."},
				"name":          {Type: schema.String, Required: true, Description: "Service/unit name."},
			},
		},
		"running": {
			Description: "The service is running. The optional enabled controls autostart in one step (ADR-015).",
			Input: schema.Input{
				"daemon_reload": {Type: schema.String, Default: "auto", Enum: []any{"auto", "always", "never"}, Description: "systemctl daemon-reload before start (systemd): auto = on NeedDaemonReload, always = unconditionally, never = opt-out. openrc/sysv — no-op (ADR-015)."},
				"enabled":       {Type: schema.Bool, Description: "true → additionally enable, false → disable, omitted → leave autostart untouched."},
				"name":          {Type: schema.String, Required: true, Description: "Service/unit name."},
			},
		},
		"stopped": {
			Description: "The service is stopped.",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Service/unit name."},
			},
		},
	},
}
