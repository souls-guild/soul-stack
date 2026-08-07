package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modCloud declares core.cloud.
var modCloud = schema.Module{
	Name: "cloud",
	States: map[string]schema.State{
		"created": {
			Description: "Keeper-side (on:keeper). Cloud VMs created through a CloudDriver plugin (ADR-017).",
			Input: schema.Input{
				"count":             {Type: schema.Int, Description: "How many VMs to create (default 1, >= 1)."},
				"generate_userdata": {Type: schema.Bool, Description: "Generate userdata from keeper.yml cloud_init (ADR-017(h))."},
				"name":              {Type: schema.String, Description: "Base name for the VM batch (self-onboard Variant T): keeper predicts FQDN=<name>-<index>.<suffix>. Required with self_onboard."},
				"profile":           {Type: schema.Map, Description: "VM profile parameters (backend-specific)."},
				"provider":          {Type: schema.String, Required: true, Description: "Name of the cloud Provider in the registry (driver + credentials + region)."},
				"self_onboard":      {Type: schema.Bool, Description: "The VM onboards itself from cloud-init (Variant T, ADR-017(h)): keeper predicts the FQDN and bakes per-VM tokens into userdata. Requires name + providers.fqdn_suffix."},
				"userdata":          {Type: schema.String, Description: "Cloud-init userdata (mutually exclusive with generate_userdata and self_onboard)."},
			},
		},
		"destroyed": {
			Description: "Keeper-side (on:keeper). Cloud VMs destroyed, registries cascade-updated (ADR-017).",
			Input: schema.Input{
				"provider": {Type: schema.String, Required: true, Description: "Name of the cloud Provider in the registry."},
				"sids":     {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "SIDs for cascade-updating registries (souls/seeds/tokens)."},
				"vm_ids":   {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.String}, Description: "List of provider-vm-ids to destroy."},
			},
		},
		"resized": {
			Description: "Keeper-side (on:keeper). Cloud VM resources changed to an absolute target size via CloudDriver.Resize (capability Resizable).",
			Input: schema.Input{
				"allow_downtime": {Type: schema.Bool, Description: "Allow downtime (stop/start) for cpu/ram resize. Required when changing cpu/ram; disk-only is online (default false)."},
				"desired":        {Type: schema.Map, Required: true, Description: "Target resources in our units: cpu_cores (cores) / ram_mb (MB) / disk_gb (GB). At least one > 0; fields with 0 are unchanged."},
				"provider":       {Type: schema.String, Required: true, Description: "Name of the cloud Provider in the registry."},
				"vm_ids":         {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.String}, Description: "provider-vm-ids to resize (one target for the whole batch)."},
			},
		},
	},
}
