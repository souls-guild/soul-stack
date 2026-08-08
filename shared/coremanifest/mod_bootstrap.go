package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modBootstrap declares the keeper-side ready-made VM onboarding contract.
var modBootstrap = schema.Module{
	Name: "bootstrap",
	States: map[string]schema.State{
		"issued": {
			Description: "Keeper-side (on:keeper). Atomically issue per-host bootstrap tokens for ready-made VM SIDs, independent of core.cloud.created.",
			Input: schema.Input{
				"sids": {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.String}, Description: "Non-empty unique list of ready-made VM FQDN/SIDs."},
			},
		},
		"delivered": {
			Description: "Keeper-side (on:keeper). Install/deliver and redeem per-host bootstrap tokens over direct SSH or Teleport (ADR-063).",
			Input: schema.Input{
				"hosts":             {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.Map}, Description: "Per-host issued-token objects. primary_ip is required only for direct transport; Teleport addresses hosts by sid."},
				"install":           {Type: schema.Bool, Description: "Install the full Soul setup before token delivery (Teleport only)."},
				"join_wait_timeout": {Type: schema.String, Description: "Teleport join wait as a duration string (integer seconds is also accepted at runtime)."},
				"ssh_port":          {Type: schema.Int, Description: "SSH port (default 22)."},
				"ssh_provider":      {Type: schema.String, Required: true, Description: "SshProvider name; retained as audit metadata in Teleport mode."},
				"ssh_user":          {Type: schema.String, Description: "SSH user (default root)."},
				"start_soul":        {Type: schema.Bool, Description: "Activate soul.service after redeem (default true)."},
				"token_path":        {Type: schema.String, Description: "Remote token path (default /etc/soul/token)."},
			},
		},
	},
}
