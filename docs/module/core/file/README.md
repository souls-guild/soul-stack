# core.file

File and directory management: creation with specified content/permissions/owner,
deletion, rendering from text/template template, creating a directory. **Soul-side**,
is statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/file/file.go`](../../../../soul/internal/coremod/file/file.go)
(present/absent), [`soul/internal/coremod/file/rendered.go`](../../../../soul/internal/coremod/file/rendered.go)
(rendered) and [`soul/internal/coremod/file/directory.go`](../../../../soul/internal/coremod/file/directory.go)
(directory).

`core.file` intentionally covers multiple roles: `present` with inline-`content`
replaces `core.copy`, `rendered` replaces `core.template`
([ADR-015](../../../adr/0015-core-modules-mvp.md): these modules
are not separately allocated), `directory` declaratively creates a directory instead
`core.exec.run` with `install -d`.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The file exists with `content` (inline) **or** `src` (host copy) given, plus `mode` / `owner` / `group`. | `changed=true`, if there was no file or the contents are different (verification according to SHA-256 - for `src` this is `sha256(src bytes)`), `mode` or owner/group. Everything coincided - `changed=false`. |
| `absent` | The file has been deleted. | `changed=true`, if the file was deleted. There is no file - `changed=false`. |
| `rendered` | File = text/template template rendering result ([ADR-010](../../../adr/0010-templating.md)). | Render to memory → SHA-256 is checked against existing file → write only when diff. `changed=true` if at least one of the content/mode/owner has changed. The entry is atomic (temp + rename in the same directory). |
| `directory` | The directory exists with `mode` / `owner` / `group` specified. | `changed=true` if the directory was not created (created) or `mode` or owner/group is drifting (fixed - chmod/chown). Everything coincided - `changed=false`. The path is occupied by a file - error, no overwriting. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Target file path. |
| `content` | string | optional | Contents of the file inline. Mutually exclusive with `src`. Neither `content` nor `src` are specified - an empty file is created (legacy behavior). |
| `src` | string | optional | **Absolute** path of the regular file on the host; its contents are copied to `path` (typically the result of `core.archive.extracted`). Sets **only** the content, not the source attributes. Mutually exclusive with `content`. |
| `mode` | string | optional | Rights in octal form (`"0640"`, `"0755"`). If not specified, the default mode `os.WriteFile` is taken when creating; the existing mode is not checked or corrected. |
| `owner` | string | optional | Owner (username). Resolved via `/etc/passwd`. |
| `group` | string | optional | Group (name). Resolved via `/etc/group`. |

### content vs src

`present` specifies the contents of the file in **exactly one** of two ways:

- **`content`** — inline content, right in the task.
- **`src`** - an absolute copy of the contents of a regular file already on the host
paths (typically the result is `core.archive.extracted`: unpacked the archive → put
one of the extracted files into place).

Mutual exclusion rules:

- **`content` and `src` together → error** (`content and src are mutually exclusive`).
The conflict is caught by the **presence of the key**, and not by the empty string: `content: ""`
together with `src:` is also a conflict.
- **Neither `content` nor `src` → empty file** (legacy behavior of `present`, reverse
compatibility; not an error).
- `src` specifies **content only**. `mode`/`owner`/`group` of the target file are taken
from explicit params `present`, the attributes of the `src` file are NOT inherited.

`src`-border (MVP - regular file only):

- path must be **absolute** (relative → `src must be absolute`);
- type is checked via `os.Lstat` + `IsRegular()` - **exactly Lstat**: symlink
reject-is, but should not be (protection against source substitution by symlink on
sensitive file); directory/symlink/device/socket/fifo →
  `src %s is not a regular file`;
- missing `src` → `read src %s: no such file`; unreadable - permission error
is forwarded as is.

Idempotency with `src`: desired hash = `sha256(src content)`, `changed=true`
if `path` is missing, either `sha256(path) != sha256(src)` or `mode` drifts /
owner/group. `src` is read into memory once - the same buffer is hashed and
is written (TOCTOU protection between reconciliation and write).

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Path of the file to be deleted. |

## directory — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Target directory path. |
| `mode` | string | optional | Rights in octal form (`"0755"`, `"0750"`). If set, it is applied during creation and verified/repaired during drift; if not specified, mode when created depends on umask and is not checked. |
| `owner` | string | optional | Owner (username). Resolves through `/etc/passwd`, drift is repaired `chown`. |
| `group` | string | optional | Group (name). Resolves through `/etc/group`, drift is repaired `chown`. |
| `parents` | bool | optional (default `false`) | `true` — create intermediate directories (semantics `mkdir -p`). `false` - A missing parent results in an error. |

Idempotency: the directory exists and `mode`/`owner`/`group` are the same → `changed=false`;
there is no directory → created (`changed=true`, in `output` - `created: true`); catalog
there is, but the attributes are drifting → `chmod`/`chown` fixes, `changed=true`; the way is busy
**file** (not directory) → error, existing file **not** overwritten.
`recurse` (recursive content rights setting) in MVP **not** supported -
only the directory itself is managed.

**Difference from `core.exec.run install -d`.** `install -d` - imperative
shell call: executed every run, `changed` is not defined by the module
(`core.exec.run` itself always "executed the command"), drift verification/repair
there is no owner and mode, dry-run (Scry) is impossible. `core.file.directory` —
declarative: idempotent by `mode`/`owner`/`group`, fixes drift, returns
honest `changed`, the conflict with the file does not mask, and supports Plan/Scry
([ADR-031](../../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) - `planDirectory` reports the same
`changed` what Apply would do without touching the host.

## rendered — params

Param-contract **destiny level** (as the author of the problem writes) differs from
wire form that Soul sees: the author specifies `template:` (path to `.tmpl`) and
`vars:`, and Keeper after the CEL phase translates them into `template_content` (literal body
template) and `render_context` (context root §3.2) inside `ApplyRequest.params`
- see [ADR-018](../../../adr/0018-soulprint-typed.md) and
[templating.md §3.2](../../../templating.md). Below is what the author of destiny writes.

Wire Keeper is **obliged** to deliver BOTH `template_content` AND `render_context`:
without any of them state `rendered` crashes (`template_content` is missing →
nothing to render; `render_context` is missing → templates with `.self.*` / `.vars.*`
strict-mode fall). This is a prod-invariant of golden-path, not optional - handoff
Keeper→Soul without both fields is considered a product blocker (see comments
[`soul/internal/coremod/file/rendered.go`](../../../../soul/internal/coremod/file/rendered.go)).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Target path of the rendered file. |
| `template` | string | required | Path to the `.tmpl` template (resolve scenario-local → service-level, [ADR-009](../../../adr/0009-scenario-dsl.md)). Keeper reads the body and delivers as `template_content`. |
| `vars` | map | optional | DERIVATIVE template variables (calculated by CEL); in text/template available as `.vars.*`. Placed by Keeper in `render_context.vars`. To forward operator-input as is, `vars:` is NOT needed - the template reads `.input.*` directly (Option B). |
| `mode` | string | optional | Rights in octal form, like `present`. |
| `owner` | string | optional | Owner as `present`. |
| `group` | string | optional | Group like `present`. |

Template sees context root `{ vars, self, role, essence }` + **conditional**
`input`: `.vars.*` - from `vars:` (derived values), `.input.*` - resolved
operator-input pass (**Option B**: the template reads the input fields directly, without
passthrough via `vars:`; key `input` Keeper puts **only if the template is real
refers to `.input.*`** - detector by bypassing parse-AST `.tmpl`, not string-search;
templates on some `.vars` `input` are not received), `.self.*` - soulprint projection
([ADR-018](../../../adr/0018-soulprint-typed.md)), `.role` —
declared-host role, `.essence.*` - effective essence. Missing variable in
template - rendering error (text/template strict-mode, `missingkey=error`).

> **`.input.*` and secrets.** The secret operator-input field read by the template via
> `.input.<name>`, masked in monitored channels (status_details/error/logs) by
> passage diagram (seal mechanism S-1, [templating.md §7.4](../../../templating.md)).
> On wire (`ApplyRequest.params.render_context`) the value is real - Soul renders
> file is the actual secret, masking affects only the observability output.

**Keys `.self.*` — snake_case** (proto field names, canon ADR-018), symmetrically
CEL `soulprint.self.*` - single point of truth. Composite keys via `_`:
`.self.os.pkg_mgr`, `.self.os.init_system`, `.self.network.primary_ip` - literally
as `soulprint.self.os.pkg_mgr` in YAML expressions. camelCase(`.self.os.pkgMgr`)
will not work (there is no value under this key).

## Capabilities / side-effects

- **Changes the file system:** creates / overwrites / deletes a file, creates
directory, ruled by mode and owner. For system paths (`/etc/...`) requires
relevant rights - in practice `run_as_root`.
- **Does not execute subprocesses** for present/absent/rendered/directory (render,
entry, `mkdir`/`chmod`/`chown` - in-process, without shell).
- `rendered` uses the built-in text/template engine (`shared/tmpl`,
sprig-allowlist; no access to FS/network/environment - three sandbox barriers
[ADR-010](../../../adr/0010-templating.md)).

## Output / register

`present` and `rendered` give `{ path, sha256, mode, installed: true }`, where
`sha256` is a hash of the recorded content. `absent` - `{ path, installed: false }`.
`directory` - `{ path, mode, created }`, where `created` - whether the directory was created in
this run (`false`, if the attributes already existed and were just being repaired).
`register:` on a rendered task is typically used as an anchor `onchanges:` for
service restart when the config changes.

## Examples

`present` with inline-content (role `core.copy`):

```yaml
- name: Drop a static marker file
  module: core.file.present
  params:
    path: /etc/soul-stack/marker
    content: "managed by soul-stack"
    mode: "0644"
    owner: root
    group: root
```

`directory` (replacement `core.exec.run install -d`); `parents: true` creates all
directory chain:

```yaml
- name: Ensure exporter data directory
  module: core.file.directory
  params:
    path: /var/lib/node_exporter/textfile
    parents: true
    mode: "0755"
    owner: node_exporter
    group: node_exporter
```

`rendered` from template (role `core.template`); `register` - to restart the service
only when changing the config:

```yaml
- name: Render redis.conf
  module: core.file.rendered
  register: redis_conf
  params:
    path: /etc/redis/redis.conf
    template: templates/redis.conf.tmpl
    mode: "0640"
    owner: redis
    group: redis
    vars:
      password: "${ input.password }"
      config:   "${ has(input.config) ? input.config : {} }"
```

(from [`examples/destiny/redis/tasks/server.yml`](../../../../examples/destiny/redis/tasks/server.yml); the complete merged config comes with one map `config` - maxmemory/persistence/passthrough merge scenario via `merge()` even before render)

## Security

- **Direct writing to an arbitrary file system path, including system
(`/etc/...`), is the main risk of the module.** `present`/`rendered` are created and
overwrite the file at `path`, and `absent` deletes it; target path module not
limits it to the sandbox. Untrusted `path` = write/delete arbitrary
system file (for example, replacing `/etc/passwd`, `/etc/sudoers`,
unit file). `path`, `content`, `mode`, `owner`, `group` must come from
by Destiny/scenario, not from untrusted input.
- **Record atomicity differs by state and by branch `present`.** `rendered`
writes atomically - render to memory, then temp + rename in the same directory
  (`util.AtomicWrite`, [`rendered.go`](../../../../soul/internal/coremod/file/rendered.go)),
so the observer does not see the partially written config. At `present`
([`file.go`](../../../../soul/internal/coremod/file/file.go)) branch differs:
**`src`-branch writes atomically** (`util.AtomicWrite`, temp + rename - copy of the config
from the host can be read by a competitive daemon), and the **`content`-branch is written via
`os.WriteFile` directly** - **without** temp+rename: if there is a failure while writing the file
may remain truncated. For configs with inline-`content`, read competitively
running daemon, prefer `rendered`/`src` (or guarantee restart
consumer via `onchanges:`).
- **`src` - copy from host, not from untrusted input.** `src` indicates
absolute path of the regular file on the host itself; `present` reject-it symlink via
`os.Lstat` (the source does not follow a symbolic link - protection against spoofing),
directory/device/socket/fifo and relative paths. It limits the shape
source, but does not make it trusted: `src`, like `path`, must come from
by Destiny/scenario, not from untrusted input - otherwise the contents
any host file will be copied to `path`.
- **`mode`/`owner` is the author's responsibility, not a module default.** If `mode` is not
is specified, when creating, the default mode `os.WriteFile` is taken (depending on umask),
and the existing mode is **not** checked and corrected - a secret written without
explicit `mode` may turn out to be world-readable. For files with sensitive
contents (keys, passwords in the config) set `mode` explicitly (`"0600"`/`"0640"`)
and `owner`/`group`. `owner`/`group` are resolved via `/etc/passwd` and `/etc/group`
on the host.
- **`rendered`: render in sandbox, secret in context.** Template body and context
come from Keeper as `template_content` + `render_context` after the CEL phase
([ADR-010](../../../adr/0010-templating.md),
[ADR-012](../../../adr/0012-keeper-soul-grpc.md)); Soul side
renders itself via `shared/tmpl` (sprig-allowlist; without access to FS/network/
environment - three sandbox barriers). This limits **what** the template can do,
but does not make the content secure: secrets (`${ vault(...) }`, passwords via
`vars:`) end up in `render_context` and in the final file - hence the mandatory
explicit `mode` for configs with secrets. Absence of a variable in the template -
render error (strict-mode `missingkey=error`), not silent empty
substitution.
- **Privileges.** Manifest [`file.yaml`](../../../../shared/coremanifest/file.yaml)
announces [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum)
(entry outside `/var/lib/soul-stack/`). Write to system paths on
practice requires root - the module is executed with process privileges
`soul`-agent, without elevation of rights inside. present/absent/rendered subprocesses
**not** run (in-process recording and rendering, without shell).

## See also

- [README.md](../../README.md) - directory of core modules.
- [templating.md](../../../templating.md) - standard template engine spec (CEL + text/template).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-010](../../../adr/0010-templating.md) — render `core.file.rendered`, security model.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs; why are `core.copy`/`core.template` not highlighted.
- [ADR-018](../../../adr/0018-soulprint-typed.md) - `render_context` and `self` soulprint projection.
