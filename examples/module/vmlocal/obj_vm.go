// The `vm` object — a virtual machine on a local libvirt/QEMU host, as address
// level 2 of `<alias>.vm.<action>`.
//
// ★ The object name and the four action names are `wbcloud`'s, verbatim, and
// that is the whole point of this artifact rather than an aesthetic choice. A
// plugin document carries no name of its own — address level 1 is the alias an
// operator registers it under — so registering THIS binary as `wbcloud` on a
// local stand makes the unmodified WB service scenario provision against libvirt.
// No fork, no `when:` on a mode, no test double. Renaming the object so that
// `vmlocal.vm.created` would not read with a stutter would throw that away.
//
// The param surface is `wbcloud`'s too, key for key, and
// TestParamSurfaceMatchesWBCloud holds it there against a vendored copy of that
// artifact's published document. Only the descriptions differ, because they are
// what an operator reads and a WB description would be a lie here.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// vmDef is the object's entry in the artifact's bundle.
//
// Side is [module.SideKeeper] for the same reason it is there: a machine is
// created when no hosts exist yet, so this runs on the Keeper against no host at
// all. [module.SideSoul] is the zero value, so losing this line fails at dispatch
// with nothing to point at.
//
// Capabilities are `wbcloud`'s as well, and honestly so: `endpoint` may name a
// remote libvirt (`qemu+ssh://`, `qemu+tcp://`), and everything this artifact
// writes it writes THROUGH libvirtd rather than to the filesystem itself.
func vmDef(m *VMLocal) module.Def {
	return module.Def{
		Name:         "vm",
		Description:  "A virtual machine on a local libvirt/QEMU host: provision a batch, tear one down, resize it, or read the inventory. Speaks the `wbcloud` vm contract so an unmodified cloud scenario runs against a workstation.",
		Side:         module.SideKeeper,
		Capabilities: []module.Capability{module.NetworkOutbound},
		Impl:         m.vm(),
		States: map[string]module.State{
			"created": {
				Description: "Provision `count` VMs and wait until every one of them is ready.\n" +
					"Idempotent on the batch identity: a rerun adopts the domains it already\n" +
					"made (matched by the domain metadata label `soulstack-run`, or by the\n" +
					"name `<name>-<seq>`) and tops up only what is missing, at the first free\n" +
					"indexes.\n" +
					"Each VM boots from a qcow2 overlay on the profile's image and a NoCloud\n" +
					"seed carrying `userdata`; ready means the guest took a DHCP lease and\n" +
					"reported a hostname, which is where sid and primary_ip come from.\n" +
					"Output.hosts is the provisioned batch, one entry per VM:\n" +
					"{sid, vm_id, primary_ip, external_ip, attributes}. It does NOT carry\n" +
					"bootstrap_token — minting one needs the Keeper's token store, which a\n" +
					"plugin has no access to, exactly as in the cloud artifact.\n" +
					"No dry-run preview.",
				Input: vmInput(module.Input{
					"count": {Type: module.Int, Default: 1,
						Description: "How many VMs the batch holds. The scan counts what already exists, so this is the TOTAL, not the number to add.",
					},
					"name": {Type: module.String,
						Description: "Batch identity: domains are named `<name>-<seq>` and labelled with it. Required unless profile.labels[\"soulstack-run\"] is set — without one of the two a rerun cannot recognise its own domains and would spawn orphans.",
					},
					"profile": {Type: module.Map, Required: true,
						Description: "The VM spec, same field names as the cloud artifact: namespace XOR namespace_id, network_id (UUID of a libvirt network), cpu_size (cores), ram_size and boot_disk_size (BYTES), rm_external_id (accepted and recorded; inert here), and the image as EITHER image_id (exact UUID, derived from the volume name) OR image_name (a volume in the image pool, e.g. debian-12). Plus the optional boot_disk_name / deletion_protection / labels. set_external_ip, external_ip_id, anti_affinity and cluster are REFUSED — see the README for why a silent no-op would be worse.",
					},
					"userdata": {Type: module.String,
						Description: "cloud-init user-data, at most 32 KiB. The cap is the cloud artifact's, mirrored deliberately: a seed ISO would hold more, and accepting more here would green a scenario that the cloud then refuses.",
					},
				}),
			},
			"destroyed": {
				Description: "Delete every VM in `vm_ids` and CONFIRM the teardown by polling.\n" +
					"A domain that is still being defined can refuse to go; the delete is\n" +
					"re-issued until the domain and its volumes are gone. A teardown that\n" +
					"could not be confirmed is reported failed, with the vm_id, never as\n" +
					"success. A domain carrying deletion_protection is refused.\n" +
					"No dry-run preview.",
				Input: vmInput(module.Input{
					"vm_ids": {Type: module.List, Required: true,
						Description: "Domain UUIDs to tear down. A VM that is already gone is an idempotent success.",
					},
				}),
			},
			"resized": {
				Description: "Resize every VM in `vm_ids` to an ABSOLUTE target.\n" +
					"Disk grows online through a block resize (never shrinks — a target at or\n" +
					"below the current size is an idempotent skip); the guest filesystem\n" +
					"follows only if it runs growpart, exactly as in the cloud. cpu/ram go\n" +
					"through stop → update → start and therefore need allow_downtime. libvirt\n" +
					"could hot-plug some of that, and this does not: a local run that passed\n" +
					"without allow_downtime would red in the cloud.\n" +
					"The host's free memory and the disk pool's free space are prechecked\n" +
					"against the batch's summed positive delta: it fails closed, before any\n" +
					"VM is touched.\n" +
					"No dry-run preview.",
				Input: vmInput(module.Input{
					"vm_ids": {Type: module.List, Required: true,
						Description: "Domain UUIDs to resize. Every VM in the batch gets the same target.",
					},
					"cpu_cores": {Type: module.Int, Default: 0,
						Description: "Target vCPU count. 0 leaves cpu alone.",
					},
					"ram_mb": {Type: module.Int, Default: 0,
						Description: "Target RAM in MiB. 0 leaves ram alone.",
					},
					"disk_gb": {Type: module.Int, Default: 0,
						Description: "Target boot-disk size in GiB. 0 leaves the disk alone. Shrinking is refused as an idempotent skip.",
					},
					"allow_downtime": {Type: module.Bool, Default: false,
						Description: "EXPLICIT consent to stop and start the VM. Required for a cpu/ram change and refused up front without it, so a batch never ends up half-stopped. A disk-only resize is online and does not need it.",
					},
				}),
			},
			"probed": {
				Description: "Read the VM inventory of a namespace. Read-only, changed=false by\n" +
					"design (a probe, not a mutation): it creates, deletes and modifies\n" +
					"nothing. Only domains this artifact made are reported — the namespace\n" +
					"label is the scope, so an unrelated VM on the same workstation is\n" +
					"invisible here and untouchable by `destroyed`. Output.hosts has the same\n" +
					"shape `created` produces.\n" +
					"No dry-run preview.",
				Input: vmInput(module.Input{
					"vm_ids": {Type: module.List,
						Description: "Report only these domain UUIDs. Omitted, the whole namespace is listed (optionally narrowed by run_label).",
					},
					"run_label": {Type: module.String,
						Description: "Report only VMs carrying this value in the domain metadata label `soulstack-run` — the batch identity `created` stamps. Ignored when vm_ids is set.",
					},
				}),
			},
		},
	}
}

// vmInput merges an action's own params with the connection params every action
// needs. Declared once rather than restated four times: param-level strictness
// reads `states.<action>.input` and nothing else (ADR-0076, NIM-204), so a key
// missing from one state's declaration is refused as `module.unknown_param` on a
// call that is perfectly legitimate.
//
// ★ `key_id` and `secret` are declared, required, and NOT USED. A local libvirtd
// over its unix socket authenticates by the permissions on that socket; there is
// no API key to present. They are here because the contract has them and dropping
// them would refuse the very scenario this artifact exists to run — and they are
// required rather than optional because the cloud requires them, and a surface
// laxer than the cloud's turns a local green into a cloud red. TestCredentials-
// AreAcceptedAndUnused pins the fact so it stays a documented no-op rather than
// a silent one.
func vmInput(own module.Input) module.Input {
	out := module.Input{
		"key_id": {Type: module.String, Required: true,
			Description: "Accepted and unused: a local libvirtd authenticates by socket permissions, not an API key. Declared because the cloud contract declares it.",
		},
		"secret": {Type: module.String, Required: true, Secret: true, Pattern: "^vault:.*",
			Description: "Accepted and unused (see key_id). Still declared secret so that a stand which does put a real credential here never has it logged.",
		},
		"endpoint": {Type: module.String, Required: true,
			Description: "libvirt connection URI — e.g. qemu:///system. This is the one connection param that carries a local meaning: it is where the hypervisor lives, as the cloud endpoint is where the compute API lives.",
		},
		"namespace": {Type: module.String,
			Description: "Scope label stamped into domain metadata. It is what keeps this artifact away from VMs it did not create: `probed` and `destroyed` see nothing outside it. One of namespace / namespace_id is required.",
		},
		"namespace_id": {Type: module.String,
			Description: "The alternative spelling of `namespace`.",
		},
		"ca_cert_pem": {Type: module.String, Secret: true, Pattern: "^vault:.*",
			Description: "Accepted and unused: TLS to a remote libvirt is configured in libvirt's own client files, not here. Declared because the cloud contract declares it.",
		},
		"client_cert_pem": {Type: module.String, Secret: true, Pattern: "^vault:.*",
			Description: "Accepted and unused (see ca_cert_pem).",
		},
		"client_key_pem": {Type: module.String, Secret: true, Pattern: "^vault:.*",
			Description: "Accepted and unused (see ca_cert_pem). Declared secret so it never reaches events or errors.",
		},
	}
	for k, v := range own {
		out[k] = v
	}
	return out
}
