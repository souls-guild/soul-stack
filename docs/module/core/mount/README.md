# core.mount

Manage mount points and `/etc/fstab` entries. **Soul-side**,
is statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/mount/mount.go`](../../../../soul/internal/coremod/mount/mount.go).

The current mount status is determined via `findmnt --target <path>`
(util-linux/busybox). fstab entry - **preserve-by-default**
(`util.AtomicWritePreserving`, pilot pattern `core.line`): fstab being corrected
in-place, its existing mode and owner are preserved; the module does not reset them
in `0644`/process owner. owner/group fstab parameters are not accepted.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The entry is in `/etc/fstab` **and** FS is mounted. | `changed=true`, if fstab has changed (entry added/updated) **or** FS was not mounted and is mounted now. Both matched - `changed=false`. fstab is matched against the entire line if `target` is matched. |
| `absent` | The file system was unmounted **and** the entry was deleted from `/etc/fstab`. | `changed=true` if the entry was in fstab and deleted **or** FS was mounted and unmounted. None of this is `changed=false`. |
| `mounted` | The FS is mounted "as is", **without** editing fstab (runtime-mount). | `changed=true`, if the FS was not mounted and is mounted now. Already mounted - `changed=false`. |
| `unmounted` | The FS is unmounted, **without** deleting the entry from fstab (the entry remains). | `changed=true`, if the file system was mounted and unmounted. Already unmounted - `changed=false`. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Mount-point (target). Created during mounting if there is no directory (`mkdir -p`, mode `0755`). |
| `source` | string | required | Source - device/NFS-share/label(`/dev/sdb1`, `nfs-host:/export`). |
| `fstype` | string | required | FS type (`ext4`, `xfs`, `nfs`, `tmpfs`). |
| `opts` | string | optional (default `defaults`) | Mounting options (4th fstab field), they are also passed to `mount -o`. |

The `dump` and `pass` fields of the fstab entry are fixed as `0 0` (no parameters
are controlled). fstab entry: `<source> <path> <fstype> <opts> 0 0`.

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Mount-point: it is used to search for an entry in fstab and check the mount status. |

## mounted — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Mount-point (target). |
| `source` | string | required | Mount source. |
| `fstype` | string | required | FS type. |
| `opts` | string | optional (default `defaults`) | Options for `mount -o`. fstab is not correct. |

## unmounted — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Mount-point for unmounting. The fstab entry is saved. |

## Capabilities / side-effects

- **Requires root** (`run_as_root`): `mount` / `umount`, edit `/etc/fstab`.
- **Executes subprocesses** (`exec_subprocess`): `findmnt` (status probe),
`mount`, `umount`. Positional `source`/`path` are separated `--` from options
(protects against arguments starting with `-`).
- **Writes outside `/var/lib/soul-stack`** (`fs_write_root`): edited by `/etc/fstab`
(atomically, preserve mode and owner), creates mount-point when
  `present`/`mounted`.
- **Changes the system:** mounts/unmounts the FS. fstab only writes
when the contents have actually changed (idempotent no-op fstab does not touch).

## Output / register

- `present` — `{ path, source, fstype, mounted: true, in_fstab: true }`.
- `absent` — `{ path, mounted: false, in_fstab: false }`.
- `mounted` — `{ path, mounted: true }`.
- `unmounted` — `{ path, mounted: false }`.

## Example

```yaml
- name: Mount data volume persistently
  module: core.mount.present
  params:
    path: /data
    source: /dev/sdb1
    fstype: ext4
    opts: "defaults,noatime"
```

(minimum valid example - there are no tasks for `core.mount` in `examples/` yet)

## Security

- **Mounting an untrusted source (especially network NFS/SMB) - main
module risk.** `present`/`mounted` execute `mount -t <fstype> -o <opts> -- <source> <path>`
([`ensureMounted`](../../../../soul/internal/coremod/mount/mount.go)) and
trust the contents of the mounted file system. Network `source`
(`nfs-host:/export`, SMB-share) under the control of an attacker can slip
files with setuid bit, symbolic links beyond mount-point or just
malicious content that other steps/processes will then read.
`source`, `path`, `fstype`, `opts` must come from the Destiny/scenario author,
and not from untrusted input. Protection against setuid/devices on an untrusted FS
is set by the author himself via `opts` (`nosuid,nodev,noexec`) - their module is **not**
imposes (default - `defaults`).
- **DoS via options and source.** `opts` is passed to `mount -o` verbatim;
aggressive network parameters (for example, hard `hard` NFS-mount on
unreachable host) can cause the operation to hang and block processes,
accessing mount-point. `tmpfs` unlimited `size=` capable
running out of memory. This is the property of the passed `opts`/`source`, their module
does not validate semantics.
- **Argument injection is closed `--`.** Positional `source`/`path` are separated
`--` from options when calling `mount`/`umount` (security review L1,
  [`ensureMounted`/`ensureUnmounted`](../../../../soul/internal/coremod/mount/mount.go)):
`source`/`path` starting with `-` are treated as paths, not flags
`mount`. Shell is not involved in this - the values come as separate argv tokens.
- **Idempotency and editing `/etc/fstab`.** The current status is determined
`findmnt --target <path>`; fstab entry - preserve-by-default
(`util.AtomicWritePreserving`): fstab is edited atomically (temp+rename), it
existing mode and owner are preserved, the module does not reset them. Record
happens **only** when the content actually changes (idempotent no-op
fstab does not touch), reconciliation - according to the entire entry line if `target` matches.
Erroneous `present` writes a persistent entry that will try to mount
source on every boot - the cost of the error in `source`/`opts` is repeated on
each boot, as opposed to the one-time `mounted`.
- **Privileges.** Manifest
[`mount.yaml`](../../../../shared/coremanifest/mount.yaml) announces
  `required_capabilities: [run_as_root, exec_subprocess, fs_write_root]` —
`mount`/`umount` require UID 0, executed as subprocesses
(`findmnt`/`mount`/`umount`), and edit `/etc/fstab` is an out-of-bounds write
`/var/lib/soul-stack`. This is a **declaration** for static reconciliation of `soul-lint` with
`allowed_capabilities` host (see [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
and **not** runtime elevation: the operation is executed with the privileges of the process
`soul` agent (under root), there is no elevation of rights inside the module.

## See also

- [README.md](../../README.md) - directory of core modules.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
