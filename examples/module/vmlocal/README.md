# vmlocal — a machine provider over libvirt/QEMU

`vmlocal` provisions virtual machines on a libvirt host. It exists so that work
which needs real machines — bootstrap, onboarding, a service's `create` path —
can be done against a host you already have instead of a billed cloud VM per
cycle.

It is a machine provider in its own right, not a local stand-in for one. The
object is `vm`, the actions are `created` / `destroyed` / `probed` / `resized`,
and every parameter below is there because libvirt can answer for it.

> **History, because the code reads oddly without it.** Until NIM-873 this
> artifact was built as a key-for-key mirror of a cloud provider's published
> document, so that registering it under that provider's alias would run an
> unmodified cloud scenario against libvirt. That bought one property and cost
> the contract its shape: five credential params it could not use, two spellings
> of `namespace`, and a profile whose field list was a diff against someone
> else's. The mirror is gone. **A scenario written for a cloud provider no longer
> runs here unedited** — the ask changes, not just the vault values.

## The parameter surface, and what holds it still

Two connection params, because libvirt needs two:

| param | meaning |
|---|---|
| `endpoint` | the libvirt connection URI — `qemu:///system`, or `qemu+ssh://host/system` for a remote hypervisor |
| `namespace` | the scope label stamped into domain metadata. It is what keeps this artifact away from machines it did not create: `probed` and `destroyed` see nothing outside it |

**There is no credential.** A libvirtd over its unix socket authenticates by the
permissions on that socket, and TLS to a remote one is configured in libvirt's
own client files. `TestNoParamCarriesACredential` keeps it that way: a
`secret: true` param reappearing means this artifact has acquired something to
authenticate *with*, which is a design decision rather than a parameter
addition.

The declared surface is pinned by `ownParams` in `bundle_test.go` — a table of
what each action offers. That table is what replaced the guard that used to
compare this surface against a vendored copy of a foreign document. It is a
statement of what `vmlocal` provides, so the surface moves by an edit a reviewer
sees.

### Profile fields — a CLOSED set

`profile` is declared `map`, so param-level strictness (ADR-0076) type-checks
nothing inside it: the fields that decide what machine gets built are exactly the
ones the platform cannot see. So the artifact closes the set itself
(`profileVocabulary`), and **a key it does not read is refused rather than
ignored** — an ignored key in a machine spec looks exactly like an honoured one
to the operator who wrote it.

| field | local meaning |
|---|---|
| `namespace` | placement scope; wins over the step's `namespace` |
| `network_id` | the **UUID of a libvirt network** — libvirt networks carry real UUIDs |
| `image_name` | a volume in the image pool, e.g. `debian-12` |
| `image_id` | a UUIDv5 derived from the image NAME (the volume filename without its extension), so an operator can compute it without asking this host; a miss lists the catalogue |
| `cpu_size` | `<vcpu>`, in cores |
| `ram_size` | `<memory>`, in **BYTES** |
| `boot_disk_size` | virtual size of the qcow2 overlay, in **BYTES** |
| `boot_disk_name` | volume name in the disk pool |
| `labels` | domain metadata; `soulstack-run` carries the batch identity and is load-bearing for idempotency |
| `deletion_protection` | honoured: `destroyed` refuses a domain carrying it |

`image_id` XOR `image_name`: both at once is refused rather than silently
preferring one, because an author who wrote both had two intentions and only one
of them would happen.

`userdata` is cloud-init user-data, capped at 32 KiB — the bound on what goes
onto the NoCloud seed. The ISO would hold megabytes; a document that large is a
rendering accident.

## ★ The OUTPUT shape, which no document declares

A module document declares its **inputs** and nothing else. `register.<task>.*`
is therefore the one dimension of this contract the platform cannot police, and
where an implementation drifts from what its consumers read with every test still
green. It had already drifted when this was written: the state descriptions
promised an `external_ip` this artifact has never emitted, and omitted the
`state` it always has.

So the shape is declared in code (`hostEntryKeys`, `hostAttrKeys`,
`hostStubKeys`, `resizeKeys` in `apply.go`), quoted in the state descriptions an
operator reads, and held to the emitters by `TestOutputShapeIsDeclared`.

- **`created`, `probed` → `output.hosts[]`**, one entry per machine:
  `{vm_id, sid, primary_ip, state, attributes}`, with
  `attributes = {namespace, name, cpu_size, ram_size, image_id, network_id, created_at, run_label}`.
- **`destroyed` → `output.hosts[]`** of bare `{vm_id}`, one per id addressed.
- **`resized` → `output.results[]`**, one
  `{vm_id, changed, caused_downtime}` per machine, plus `error` only on a
  failure — a key holding `""` would make `has(r.error)` true for the whole
  batch, and that is the predicate a scenario writes.

`primary_ip` is **flat**. Nesting it under `network:` — which is the soulprint's
shape and a plausible thing to reach for — makes it invisible to the consumer
while every test that only checks `sid` stays green.

A machine that never came up appears in `hosts` as a bare `{vm_id}` and the step
reports **failed**. It deliberately carries no `sid`: the fallback would
manufacture `<name>.<namespace>` for a machine that never announced one, and that
is the field the downstream bootstrap guard keys on — filling it in disables the
check that would have caught the failure.

`hosts[]` carries **no `bootstrap_token`**, and cannot: minting one needs the
Keeper's token store, which a plugin has no access to.

## Where `sid` and `primary_ip` come from

Both are read from the **DHCP lease** (`virNetworkGetDhcpLeases`), matched by the
domain's interface MAC. The guest announces its hostname over DHCP because the
NoCloud seed sets `local-hostname`, so the answer is the machine's own account of
itself rather than an echo of what was asked for. Readiness is a lease with both
an address and a hostname.

## Resize semantics

Disk grows online through a block resize and never shrinks (a target at or below
the current size is an idempotent skip); the guest filesystem follows only if it
runs `growpart`. cpu and ram go through **stop → update → start**, so they need
`allow_downtime: true` — the consent the param asks for is consent the operation
actually spends. libvirt could hot-plug some of it; this does not.

The host's free memory and the disk pool's free space are prechecked against the
batch's summed positive delta, so a batch that will not fit fails **before any
machine is touched**.

Past the precheck the batch is not abandoned on the first per-machine error: a
machine stopped to have its cpu changed would be left stopped with nobody told
which one. Every machine is attempted and reports its own outcome in
`output.results`.

## Idempotency, and adopting a stopped member

`created` is idempotent on the batch identity: a rerun adopts the domains it
already made (matched by the metadata label `soulstack-run`, or by the name
`<name>-<seq>`) and tops up only what is missing, at the first free indexes.

**A member of the batch that is powered down is adopted and STARTED.** A
workstation that rebooted leaves every machine stopped and nothing autostarts;
building a sibling instead would leave the dead one behind for the next rerun to
ignore as well. `created` is a state, not an imperative, and converging a stopped
member onto "running" is what the state says.

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
   and gets a different `vm_id`.

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

Regenerate `schema.json` after any change to the bundle:

```bash
go run ./sdk/cmd/soul-mod stamp examples/module/vmlocal/dist/vmlocal
cp examples/module/vmlocal/dist/schema.json examples/module/vmlocal/schema.json
```
