package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modMount declares core.mount.
var modMount = schema.Module{
	Name:         "mount",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.ExecSubprocess, schema.FSWriteRoot},
	States: map[string]schema.State{
		"absent": {
			Description: "Unmounted + removed from /etc/fstab.",
			Input: schema.Input{
				"path": {Type: schema.String, Required: true, Description: "Mount point (target)."},
			},
		},
		"mounted": {
			Description: "Only mounted \"as is\" (without editing fstab) - runtime-mount.",
			Input: schema.Input{
				"fstype": {Type: schema.String, Required: true, Description: "Filesystem type (ext4/nfs/...)."},
				"opts":   {Type: schema.String, Description: "Mount options (default \"defaults\")."},
				"path":   {Type: schema.String, Required: true, Description: "Mount point (target)."},
				"source": {Type: schema.String, Required: true, Description: "Source (device/UUID/network resource)."},
			},
		},
		"present": {
			Description: "Entry in /etc/fstab + mounted (ADR-015).",
			Input: schema.Input{
				"fstype": {Type: schema.String, Required: true, Description: "Filesystem type (ext4/nfs/...)."},
				"opts":   {Type: schema.String, Description: "Mount options (default \"defaults\")."},
				"path":   {Type: schema.String, Required: true, Description: "Mount point (target)."},
				"source": {Type: schema.String, Required: true, Description: "Source (device/UUID/network resource)."},
			},
		},
		"unmounted": {
			Description: "Only unmounted (entry in fstab remains).",
			Input: schema.Input{
				"path": {Type: schema.String, Required: true, Description: "Mount point (target)."},
			},
		},
	},
}
