# vmlocal — a machine provider over libvirt/QEMU, speaking the `wbcloud` contract

`vmlocal` provisions virtual machines on a local libvirt host. It exists so that
bootstrap work can be debugged against a workstation instead of costing a billed
cloud VM per cycle.

## The property this is built around

A plugin document carries **no name of its own**. Address level 1 —
the `wbcloud` in `wbcloud.vm.created` — is the alias an operator writes in
`keeper.yml::plugins.soul_modules[].name`, and it appears nowhere in the artifact's
bytes.

So registering this binary under the alias `wbcloud` on a local stand makes a
cloud service scenario provision against libvirt **with its YAML unchanged**. No
fork, no `when:` on a mode, no test double. In production the substitution is
closed by the grant: the sigil pins a sha256.

The claim is bounded, and the bound is worth stating rather than discovering:
**a scenario that asks for something a single libvirt host cannot do is refused,
not quietly served.** That is `set_external_ip`, `external_ip_id`,
`anti_affinity`, `cluster` and `image_version` — see the profile table below. A
scenario using none of them runs unchanged; one using any of them is told which
field and why. The vault values change too (`endpoint` becomes a libvirt URI);
what does not change is the scenario.

That only holds while the object, the actions and the parameter surface are
identical, because param-level strictness (ADR-0076, NIM-204) refuses a call
carrying a key the state does not declare. So:

- the object is **`vm`**, the actions are **`created` / `destroyed` / `probed` /
  `resized`**;
- the parameter surface matches key for key, type for type, including
  `required`, `secret`, `pattern` and defaults;
- `testdata/wbcloud.schema.json` is a vendored copy of what the cloud artifact
  publishes, and `TestParamSurfaceMatchesWBCloud` compares the two. When the
  cloud contract moves, that test reddens. Re-copy the fixture and decide whether
  `vmlocal` follows — do not delete the test.

There are **no extra params**. Local knobs are environment variables, because a
param this artifact has and the cloud does not would make a scenario using it
unrunnable in the cloud.

## The rule the refusals follow

> **Inert is accepted. A promise that cannot be kept is refused.**

A field that affects nothing observable here is accepted and recorded
(`rm_external_id`). A field that promises a property of the machine which a single
libvirt host cannot provide is refused in `Validate`. A silent no-op there is
exactly what would turn a second implementation from a check on the contract into
a way around it.

And in the other direction:

> **Mirror the cloud's validation, or be stricter — never laxer.**

`rm_external_id` is inert here and still **required**; `userdata` is capped at the
cloud's 32 KiB even though an ISO would hold more; a cpu/ram change demands
`allow_downtime` even though libvirt could hot-plug it. A surface laxer than the
cloud's would green a scenario the cloud then refuses.

## Profile fields

| field | local meaning |
|---|---|
| `namespace` / `namespace_id` | scope label in the domain metadata — what keeps `probed` and `destroyed` away from machines this artifact did not create |
| `network_id` | the **UUID of a libvirt network**; libvirt networks carry real UUIDs, so this is 1:1 |
| `image_name` | a volume in the image pool, e.g. `debian-12` |
| `image_id` | a UUIDv5 derived from the image NAME (the volume filename without its extension), so an operator can compute it without asking this host; a miss lists the catalogue |
| `image_version` | **refused** — the catalogue is a pool with one volume per name, so there is no set of versions to pin |
| `cpu_size` | `<vcpu>`, in cores |
| `ram_size` | `<memory>`, in **BYTES** |
| `boot_disk_size` | virtual size of the qcow2 overlay, in **BYTES** |
| `boot_disk_name` | volume name in the disk pool |
| `labels` | domain metadata; `soulstack-run` carries the batch identity and is load-bearing for idempotency |
| `deletion_protection` | honoured: `destroyed` refuses a domain carrying it |
| `rm_external_id` | **accepted and required, inert** — recorded in metadata, affects nothing |
| `set_external_ip`, `external_ip_id` | **refused** — there is no external-address pool |
| `anti_affinity` | **refused** — one hypervisor, so machines cannot be spread across hosts |
| `cluster` | **refused** — no cluster placement |

### Connection params

`endpoint` is the one that carries a local meaning: the **libvirt connection URI**
(`qemu:///system`), as the cloud endpoint is where the compute API lives. Put it
in the same Vault key the scenario already reads.

`key_id`, `secret`, `ca_cert_pem`, `client_cert_pem`, `client_key_pem` are
**accepted and unused**: a local libvirtd over its unix socket authenticates by
the permissions on that socket, and TLS to a remote libvirt is configured in
libvirt's own client files. This is documented rather than silent, and
`TestCredentialsAreAcceptedAndUnused` pins it.

## Where `sid` and `primary_ip` come from

Both are read from the **DHCP lease** (`virNetworkGetDhcpLeases`), matched by the
domain's interface MAC. The guest announces its hostname over DHCP because the
NoCloud seed sets `local-hostname`, so the answer is the machine's own account of
itself — the same kind of fact the cloud reports from `vm.hostname`, not an echo
of what was asked for.

Readiness is therefore the cloud's predicate exactly: a lease with both an address
and a hostname.

`external_ip` is always empty here.

## Setting up a host

```bash
sudo apt-get install -y qemu-system-x86 qemu-utils libvirt-daemon-system \
  libvirt-clients dnsmasq-base nftables
sudo usermod -aG libvirt,kvm "$USER"   # needs a fresh login, or use `sg libvirt -c ...`

# Two dir-backed pools: the image catalogue and the VM disks.
for p in vmlocal-images vmlocal-disks; do
  virsh -c qemu:///system pool-define-as "$p" dir --target "/var/lib/libvirt/images/$p"
  virsh -c qemu:///system pool-build "$p"
  virsh -c qemu:///system pool-start "$p"
  virsh -c qemu:///system pool-autostart "$p"
done

# Stage a base image into the catalogue.
curl -sSLO https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
virsh -c qemu:///system vol-create-as vmlocal-images debian-12.qcow2 3G --format qcow2
virsh -c qemu:///system vol-upload --pool vmlocal-images debian-12.qcow2 \
  debian-12-genericcloud-amd64.qcow2
```

The pool names are overridable with `VMLOCAL_IMAGE_POOL` and `VMLOCAL_DISK_POOL`.
The plugin inherits them from the Keeper process that spawns it.

### Networking

A Keeper reaching a machine, and a machine reaching back, both go over libvirt's
NAT network (`virbr0`, `192.168.122.0/24`). On the WSL2 dev stand this works
because the Keeper runs as a **host process in the same network namespace**, and
binds `bootstrap`/`event_stream` on `0.0.0.0` — so the guest reaches it at
`192.168.122.1`. Verified both directions on 2026-09-12.

It would **not** work from a Keeper inside a Docker Desktop container, which lives
in a different VM: nothing routes to `192.168.122.0/24` from there.

## Three traps, each found by a live run

1. **The seed is a virtio disk, not a SATA cdrom.** The Debian/Ubuntu *cloud*
   kernels are built for virtio and carry no AHCI or `sr` driver, so a cdrom never
   appears in the guest: cloud-init finds no datasource, the machine boots as
   `localhost` with `ssh.service` failed, and the symptom surfaces three steps
   later as an SSH timeout. cloud-init matches on the filesystem **label**
   (`cidata`), not the device type.
2. **The MAC is pinned in the domain XML.** libvirt generates a fresh one on every
   redefine when the element is absent, which silently breaks the lease lookup
   that `sid` and `primary_ip` depend on — on a machine that is otherwise running
   fine.
3. **Each creation mints a fresh machine identity, and the MAC follows from it.**
   dnsmasq keeps a lease for its full TTL after a machine is gone. While the MAC
   was derived from `namespace/name`, destroying a batch and creating it again
   under the same name **inherited the old lease** — and `created` reported the new
   machine ready, at the old address, about a second after defining it, before it
   had booted at all. Two consecutive live runs both "came up" at
   `192.168.122.198`, the second in 1s. A recreated machine is a different machine
   and gets a different `vm_id`, which is the cloud's semantics too.

A fourth, less a trap than a fact worth knowing: **stock cloud images do not act on
the ACPI power button**, so the graceful stop before a cpu/ram resize always times
out and the machine is stopped the hard way. The window is 30s and the forced stop
is reported in an event rather than done silently.

## Tests

```bash
GOWORK=off go test ./...                       # the contract, no hypervisor needed
sg libvirt -c 'GOWORK=off go test -tags libvirt -timeout 20m -run TestLive .'
```

The default lane covers everything above the hypervisor seam. The `libvirt`-tagged
lane drives the whole lifecycle against a real host and is the only thing that
exercises `libvirt.go`; it skips with a named reason when a prerequisite is
missing.

## What each action publishes

`created` and `probed` publish `output.hosts`; `resized` publishes
`output.results` with one `{vm_id, caused_downtime, changed, error?}` per machine
— the same keys and shapes the cloud artifact publishes, so a scenario that
registers any of these steps reads the same thing from either provider.

`TestParamSurfaceMatchesWBCloud` compares only the INPUT surface, because that is
what the published document declares. The output shape is not in the document at
all, which makes it the dimension where the two implementations can diverge with
every test still green — the `resized` results block was exactly that, found by
review rather than by a test.

A machine that never came up appears in `hosts` as a bare `{vm_id}` and the step
reports **failed**. It deliberately carries no `sid`: the fallback would
manufacture `<name>.<namespace>` for a machine that never announced one, and that
is the field the downstream bootstrap guard keys on — filling it in disables the
check that would have caught the failure.

## Where this is deliberately MORE permissive than the cloud

One divergence, stated rather than left implicit, because everywhere else this
artifact refuses what it cannot match:

**A member of the batch that is powered down is adopted and STARTED.** The cloud
excludes non-live VMs from its adoption scan, reuses the index, and fails loudly
with `AlreadyExists`. Here, a workstation that rebooted leaves every machine
stopped and nothing autostarts, so the cloud's behaviour would build a sibling on
every rerun and leave the dead one behind for the next rerun to ignore as well.
`created` is a state, not an imperative, and converging a stopped member onto
"running" is what the state says.

The cost is the direction that normally gets refused here: a scenario that relies
on it is green locally and red in the cloud. It is on this list for that reason.

## Known limits

- **`hosts[]` carries no `bootstrap_token`**, and cannot: minting one needs the
  Keeper's token store, which a plugin has no access to. `vmlocal` hits the
  identical wall the cloud artifact hits — which is itself useful information, in
  that the gap is in the contract rather than in one provider.
- `resized` grows the block device online; the guest filesystem follows only if it
  runs `growpart`. That is the cloud's behaviour too.

## What a second implementation found out about the contract

- **`key_id` / `secret` are `required: true` on all four states, and that is the
  WB-shaped part of the contract.** `endpoint` generalises; a key-and-secret pair
  does not — a local hypervisor authenticates by socket permissions. Any second
  implementation is forced to invent a dummy. A contract-level fix would make them
  optional and let each implementation refuse when it needs them.
- **`profile` is declared `map`, so nothing inside it is type-checked**, which
  puts the ten fields that decide what machine gets built outside NIM-778's reach.
  The cloud artifact's reader answers `0` for a mistyped number and the resulting
  message reports a supplied field as *missing*. `vmlocal` names the type instead.
- **`run_label` is an input filter with no output counterpart**: the cloud's
  `attributes` carries no labels, so a scenario cannot read back the batch
  identity it filtered by. `vmlocal` echoes `run_label` in `attributes`.
