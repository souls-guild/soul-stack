# core.directory

Directory management: declarative creation with the given permissions/owner
(`present`) and removal (`absent`). **Soul-side**, is statically built into the
`soul` binary. Implementation -
[`soul/internal/coremod/directory/directory.go`](../../../../soul/internal/coremod/directory/directory.go).

`core.directory` is split out of `core.file` ([ADR-015 Amendment
2026-07-17](../../../adr/0015-core-modules-mvp.md); hard rename, no back-compat).
The former `core.file.directory` state is removed - its create/drift-fix logic
moved 1:1 into `core.directory.present`, and `absent` (safe removal) is new. Manage
files with [`core.file`](../file/README.md), directories with `core.directory`.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The directory exists with `mode` / `owner` / `group` specified (opt. `parents` = `mkdir -p`). | `changed=true` if the directory was not created (created) or `mode` or owner/group is drifting (fixed - chmod/chown). Everything coincided - `changed=false`. The path is occupied by a file/symlink - error, no overwriting. |
| `absent` | The directory has been removed. | `changed=true` if the directory was removed. There is no path - `changed=false` (idempotent). A **non-empty** directory is removed only with `recursive: true` (otherwise error, nothing removed). |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Target directory path (**absolute** - a relative path is a runtime error). |
| `mode` | string | optional | Rights in octal form (`"0755"`, `"0750"`). If set, it is applied during creation and verified/repaired during drift; if not specified, mode when created depends on umask and is not checked. |
| `owner` | string | optional | Owner (username). Resolves through `/etc/passwd`, drift is repaired `chown`. |
| `group` | string | optional | Group (name). Resolves through `/etc/group`, drift is repaired `chown`. |
| `parents` | bool | optional (default `false`) | `true` — create intermediate directories (semantics `mkdir -p`). `false` - a missing parent results in an error. |

Idempotency: the directory exists and `mode`/`owner`/`group` are the same → `changed=false`;
there is no directory → created (`changed=true`, in `output` - `created: true`);
the directory exists but the attributes are drifting → `chmod`/`chown` fixes,
`changed=true`; the path is occupied by a **file/symlink** (not a directory) → error,
existing entry **not** overwritten. `recurse` (recursive content rights setting on
the directory contents) in the MVP **not** supported - only the directory itself is
managed. `parents` is `present`-only (creation semantics); removing empty parent
directories upward is deferred.

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Path of the directory to remove (**absolute**). |
| `recursive` | bool | optional (default `false`) | `false` - remove only an empty directory; a non-empty one → error. `true` - recursively remove the contents (`rm -r` semantics). |

Removal behavior (safety-first, fail-closed by default):

- path is missing → `changed=false` (idempotent, like `core.file.absent`);
- path is an **empty** directory → removed (`os.Remove`) → `changed=true`;
- path is a **non-empty** directory and `recursive: false` → **error**
  (`directory … is not empty (set recursive: true to remove its contents)`) -
  nothing is removed;
- path is a non-empty directory and `recursive: true` → recursively removed
  (`os.RemoveAll`) → `changed=true`.

This is a deliberate reversal of the Salt/Ansible default. There `state=absent` /
`file.absent` on a directory is an unconditional, silent recursive delete (`rm -rf`)
with no safety flag - a long-standing footgun, especially when a templated variable
in `path` collapses to empty and takes out a parent. Here a non-empty directory is
never removed without explicit consent (`recursive: true`) - absent the flag, the
step fails loudly instead of destroying data.

### Guards

- **Protected system roots.** After `filepath.Clean`, `absent` refuses to remove a
  protected root - `/`, `/etc`, `/usr`, `/var`, `/home`, `/root`, `/boot`, `/bin`,
  `/sbin`, `/lib`, `/lib64`, `/proc`, `/sys`, `/dev`, `/opt`, `/mnt` (error `refusing
  to remove protected system path …`). A child of such a root (e.g. `/etc/myapp`) is
  allowed - only the root itself is denied. This is a fail-closed guard against a
  templated empty variable collapsing to a system root; it is not a substitute for
  a trusted `path` (see Security).
- **Symlink-safety.** `absent` stats the path with `os.Lstat` (does not follow the
  link). A symlink at `path` → error `path … is a symlink, not a directory` - the
  link is never traversed and the directory it points to is never removed. In the
  recursive case `os.RemoveAll` also removes in-tree symlinks as links (it never
  follows them into another tree), so recursion cannot escape the target subtree.
- **Type-conflict.** A file (not a directory) at `path` → error `path … exists and
  is not a directory` (`present` reports the same for a non-directory) - directories
  are this module's responsibility, files are `core.file`'s.

### Plan / Scry

Both states support declarative dry-run
([ADR-031](../../../adr/0031-scry-drift.md)) - pure read, no mutation:

- `present`: missing → `changed=true` (Apply would create); a `mode`/`owner`/`group`
  drift → `changed=true`; all matched → `changed=false`; a type-conflict (path is not
  a directory) → **plan error** (not a false-clean).
- `absent`: missing → `changed=false`; empty or (`recursive: true` and non-empty) →
  `changed=true`; **non-empty and `recursive: false` → plan error**
  (`directory_not_empty`) - honestly reports that Apply would fail rather than
  reporting a clean plan; a symlink/file/protected-root at `path` → plan error.

### Difference from `core.exec.run install -d`

`install -d` - an imperative shell call: executed every run, `changed` is not
defined by the module (`core.exec.run` itself always "executed the command"), there
is no owner/mode drift verification/repair, and dry-run (Scry) is impossible.
`core.directory.present` - declarative: idempotent by `mode`/`owner`/`group`, fixes
drift, returns an honest `changed`, does not mask a conflict with a file, and
supports Plan/Scry - the plan reports the same `changed` that Apply would, without
touching the host.

## Capabilities / side-effects

- **Changes the file system:** creates a directory (`present`), removes a directory
  (`absent`), applies mode and owner. For system paths (`/etc/...`) requires the
  relevant rights - in practice `run_as_root`.
- **Does not execute subprocesses** - `mkdir`/`chmod`/`chown`/`remove` are in-process,
  without a shell.

## Output / register

`present` returns `{ path, mode, created }`, where `created` is whether the directory
was created in this run (`false` if the attributes already existed and were just
repaired). `absent` returns `{ path, removed }`, where `removed` is whether a
directory was removed in this run (`false` on an already-missing path).

## Examples

`present` (replacement `core.exec.run install -d`); `parents: true` creates the whole
directory chain:

```yaml
- name: Ensure exporter data directory
  module: core.directory.present
  params:
    path: /var/lib/node_exporter/textfile
    parents: true
    mode: "0755"
    owner: node_exporter
    group: node_exporter
```

`absent` (remove a non-empty directory) - `recursive: true` is the explicit consent
to delete the contents:

```yaml
- name: Remove the stale build cache
  module: core.directory.absent
  params:
    path: /var/cache/myapp/build
    recursive: true
```

Without `recursive: true` the same step removes the directory only if it is empty,
and fails loudly (`directory … is not empty`) otherwise - nothing is deleted.

## Security

- **Removal is the main risk of the module.** `absent` with `recursive: true`
  deletes a directory subtree; an untrusted `path` = deleting an arbitrary system
  subtree. `path` must come from Destiny/scenario, not from untrusted input. The
  protected-root guard and the empty-directory-only default are fail-closed safety
  nets, not a license to feed untrusted paths - a child of a protected root (e.g.
  `/etc/anything`) is still removable.
- **Never traverses a symlink.** `absent` reject-s a symlink at `path` (via
  `os.Lstat`, without following it), and recursive removal deletes in-tree symlinks
  as links without following them - a symlink cannot be used to escape the target
  subtree into another part of the file system.
- **Direct writing to an arbitrary path, including system (`/etc/...`).** `present`
  creates directories and fixes `mode`/`owner`/`group` at `path`; an untrusted `path`
  or `mode` can create a world-accessible directory in a sensitive location. `path`,
  `mode`, `owner`, `group` must come from Destiny/scenario. For directories with
  sensitive contents set `mode` explicitly (`"0700"`/`"0750"`) and `owner`/`group`.
- **Privileges.** Manifest
  [`directory.yaml`](../../../../shared/coremanifest/directory.yaml) announces
  [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum) (writing
  outside `/var/lib/soul-stack/`). Writing to / removing system paths in practice
  requires root - the module is executed with the privileges of the `soul` agent
  process, without elevation of rights inside. Subprocesses are **not** run
  (in-process `mkdir`/`chmod`/`chown`/`remove`, without a shell).

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/file/README.md](../file/README.md) - `core.file` (file management; `core.directory` was split out of it).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs; **Amendment 2026-07-17** - `core.directory` split out of `core.file`, `absent` with `recursive`.
- [ADR-031](../../../adr/0031-scry-drift.md) - Scry / drift-detection (declarative dry-run).
