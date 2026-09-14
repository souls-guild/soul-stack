// The `vm` object — a virtual machine on a libvirt/QEMU host, as address level 2
// of `<alias>.vm.<action>`.
//
// The object is `vm` and the actions are the four states a machine can be asked
// to be in. A plugin document carries no name of its own: address level 1 is the
// alias an operator registers this binary under, so `vm` is the whole of what
// this artifact names, and it names a libvirt domain.
//
// ★ The parameter surface is this artifact's OWN, justified by what libvirt can
// do — see [profileVocabulary], which closes the one part of it the platform
// cannot police. It was formerly a key-for-key copy of a cloud provider's
// surface, carried so that one unmodified cloud scenario would run here; that is
// no longer the design (NIM-873), and the credentials, the second spelling of
// `namespace` and the external-address fields left with it.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// vmDef is the object's entry in the artifact's bundle.
//
// Side is [module.SideKeeper]: a machine is created when no hosts exist yet, so
// this runs on the Keeper against no host at all. [module.SideSoul] is the zero
// value, so losing this line fails at dispatch with nothing to point at.
//
// NetworkOutbound is honest rather than nominal: `endpoint` may name a remote
// libvirt (`qemu+ssh://`, `qemu+tcp://`), and everything this artifact writes it
// writes THROUGH libvirtd rather than to the filesystem itself.
func vmDef(m *VMLocal) module.Def {
	return module.Def{
		Name:         "vm",
		Description:  "A virtual machine on a libvirt/QEMU host: provision a batch, tear one down, resize it, or read the inventory.",
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
					"{vm_id, sid, primary_ip, state, attributes}, where attributes is\n" +
					"{namespace, name, cpu_size, ram_size, image_id, network_id, created_at, run_label}.\n" +
					"A VM that did not come up is a bare {vm_id} and the step reports failed.\n" +
					"It does NOT carry bootstrap_token — minting one needs the Keeper's token\n" +
					"store, which a plugin has no access to.\n" +
					"No dry-run preview.",
				Input: vmInput(module.Input{
					"count": {Type: module.Int, Default: 1,
						Description: "How many VMs the batch holds. The scan counts what already exists, so this is the TOTAL, not the number to add.",
					},
					"name": {Type: module.String,
						Description: "Batch identity: domains are named `<name>-<seq>` and labelled with it. Required unless profile.labels[\"soulstack-run\"] is set — without one of the two a rerun cannot recognise its own domains and would spawn orphans.",
					},
					"profile": {Type: module.Map, Required: true,
						Description: "The VM spec — a CLOSED set of fields; an unrecognised key is refused rather than ignored. namespace (the placement scope, winning over the step's), network_id (UUID of a libvirt network), cpu_size (cores), ram_size and boot_disk_size (BYTES), and the image as EITHER image_id (a UUID derived from the volume name) OR image_name (a volume in the image pool, e.g. debian-12). Optional: boot_disk_name, deletion_protection, labels.",
					},
					"userdata": {Type: module.String,
						Description: "cloud-init user-data, at most 32 KiB. The cap bounds what goes onto the NoCloud seed; a document that large is a rendering accident rather than an intention.",
					},
				}),
			},
			"destroyed": {
				Description: "Delete every VM in `vm_ids` and CONFIRM the teardown by polling.\n" +
					"A domain that is still being defined can refuse to go; the delete is\n" +
					"re-issued until the domain and its volumes are gone. A teardown that\n" +
					"could not be confirmed is reported failed, with the vm_id, never as\n" +
					"success. A domain carrying deletion_protection is refused.\n" +
					"Output.hosts is one {vm_id} per id addressed.\n" +
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
					"follows only if it runs growpart. cpu/ram go through\n" +
					"stop → update → start and therefore need allow_downtime; libvirt could\n" +
					"hot-plug some of that and this does not, so the consent the param asks\n" +
					"for is consent the operation actually spends.\n" +
					"The host's free memory and the disk pool's free space are prechecked\n" +
					"against the batch's summed positive delta: it fails closed, before any\n" +
					"VM is touched.\n" +
					"Output.results is one {vm_id, changed, caused_downtime, error?} per VM —\n" +
					"every VM is attempted, so a batch reports per-machine outcomes rather\n" +
					"than stopping at the first failure.\n" +
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
					"label is the scope, so an unrelated VM on the same host is invisible\n" +
					"here and untouchable by `destroyed`. Output.hosts has the same\n" +
					"{vm_id, sid, primary_ip, state, attributes} shape `created` produces,\n" +
					"attributes being\n" +
					"{namespace, name, cpu_size, ram_size, image_id, network_id, created_at, run_label}.\n" +
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
// ★ There are TWO, and no credential among them. A libvirtd over its unix socket
// authenticates by the permissions on that socket, and TLS to a remote one is
// configured in libvirt's own client files — so an artifact that declared
// `key_id` / `secret` / the three PEMs would be asking for five values it can do
// nothing with. It did, until NIM-873, because a cloud contract it mirrored asked
// for them; a param an artifact accepts and cannot use is a promise to the
// operator that nobody keeps.
func vmInput(own module.Input) module.Input {
	out := module.Input{
		"endpoint": {Type: module.String, Required: true,
			Description: "libvirt connection URI — e.g. qemu:///system, or qemu+ssh://host/system for a remote hypervisor.",
		},
		"namespace": {Type: module.String,
			Description: "Scope label stamped into domain metadata. It is what keeps this artifact away from VMs it did not create: `probed` and `destroyed` see nothing outside it. Required on every action but `created`, where profile.namespace may supply it.",
		},
	}
	for k, v := range own {
		out[k] = v
	}
	return out
}
