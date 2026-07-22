# core.cmd

Launching a shell line via `sh -c "<cmd>"` - with processing pipes, redirects, glob,
variables. **Soul-side**, statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/cmd/cmd.go`](../../../../soul/internal/coremod/cmd/cmd.go).

This is a verb module: the only state is `shell`. Unlike
[`core.exec`](../exec/README.md) (argv, no shell) here the string is interpreted
shell. **Module TRUSTED-ONLY**: `cmd`-line goes to `sh -c` without escape -
this is shell by design; any interpolation (CEL-render, register, soulprint) inside
`cmd` is executed by the shell as code, so the source of the string must be
trusted (by Destiny/scenario) rather than external input. Where shell semantics fail
needed - use `core.exec`.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `shell` | Execute `sh -c "<cmd>"`. | Defaults to `changed=true` (verb "execute"). You can downgrade to no-op using the guard parameters `creates` / `unless` / `onlyif` (check order: creates → unless → onlyif, the first one to fire wins): when triggered, the **not** command is launched, `changed=false`, output `{ skipped: true, reason, exit_code: 0 }`. |

## shell — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `cmd` | string | required | Shell string. Executed as `sh -c "<cmd>"`; pipes, redirects, glob, substitutions work. |
| `cwd` | string | optional | The working directory of the process is `sh`. |
| `env` | map&lt;string,string&gt; | optional | Process environment variables (`KEY=VALUE`). |
| `creates` | string | optional | Guard: if the file at this path **exists** - skip (`changed=false`, `reason: creates`). |
| `unless` | string | optional | Guard: execute `sh -c "<unless>"`; if its exit **= 0** - skip (`reason: unless`). |
| `onlyif` | string | optional | Guard: execute `sh -c "<onlyif>"`; if its exit **≠ 0** - skip (`reason: onlyif`). |

## Capabilities / side-effects

- **Executes subprocesses** ([`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum)):
main command (`sh -c "<cmd>"`), as well as guard commands `unless` / `onlyif`
(also via `sh -c`).
- **Changes the system exactly as much as the shell line changes** - the module itself
doesn't write anything on its own. Requires appropriate rights for system operations
(in practice - root, [`run_as_root`](../../../naming-rules.md#required_capabilities-enum)).
- `creates` uses `os.Stat` (path existence), `unless`/`onlyif` -
auxiliary shell calls.

## Output / register

`shell` returns `{ stdout, stderr, exit_code }` (exit_code is a number). When
when the guard is triggered - `{ skipped: true, reason, exit_code: 0 }` with `changed=false`.
Like `core.exec`, the main command's non-zero exit does not take a step by itself
failed - solves `failed_when:` in scenario.

## Examples

**`creates`-guard** — install is skipped if the result file is already in place (the simplest idempotency, but **doesn't** catch version upgrades: the path is the same → no-op step even when the contents are outdated):

```yaml
# Expand the binary from the unpacked directory: install -m 0755 - local shell,
# without network. Idempotency - through creates: binary in place → no-op.
- name: Install redis_exporter binary
  module: core.cmd.shell
  params:
    creates: "${ input.bin_dir + '/redis_exporter' }"
    cmd: >-
      install -m 0755
      '${ '/tmp/redis_exporter-' + input.redis_exporter_version + '/redis_exporter-v' + input.redis_exporter_version + '.linux-' + soulprint.self.os.arch + '/redis_exporter' }'
      '${ input.bin_dir + '/redis_exporter' }'
```

(from the redis_exporter inline block in [`examples/service/monitoring/scenario/create/main.yml`](../../../../examples/service/monitoring/scenario/create/main.yml). The reference `node-exporter` install step goes **not** through `creates`, but the version-aware `unless` - see below: `creates` does not catch the version upgrade, so It is better to install the binary for the pin version via `unless`)

**Version-aware `unless`-guard** — install is skipped ONLY when the binary of the required version is already in place. Unlike `creates`, this allows **upgrade**: another version → `unless` not satisfied → install is executed and overwrites the old binary. `unless` satisfied = exit 0 (`--version` - the output contains the expected version):

```yaml
# install is skipped only if node_exporter of the required version is already installed;
# any other version (or lack of binary) → unless not satisfied → reinstall.
# register: the install step is connected to the onchanges of the restart - the upgrade will restart the service.
- name: Install node_exporter binary
  module: core.cmd.shell
  register: node_exporter_bin
  params:
    unless: "${ 'test -x ' + input.bin_dir + '/node_exporter && ' + input.bin_dir + '/node_exporter --version 2>&1 | grep -qF ' + \"'version \" + input.version + \" '\" }"
    cmd: >-
      install -m 0755
      '${ '/tmp/node_exporter-' + input.version + '/node_exporter-' + input.version + '.linux-' + soulprint.self.os.arch + '/node_exporter' }'
      '${ input.bin_dir + '/node_exporter' }'
```

(from [`examples/destiny/node-exporter/tasks/service.yml`](../../../../examples/destiny/node-exporter/tasks/service.yml). `grep`-pattern `'version <X> '` - in single quotes, with spaces on both sides of the version. `node_exporter --version` prints the string `node_exporter, version <X> (...)` - there is always a space before `(`; a leading space separates the token `version`, **trailing space is required** so that the pattern `'version 1.9.0 '` does NOT give a false match on the output of `version 1.9.01 ` (without the trailing space `grep -qF 'version 1.9.0'` would also match `1.9.01` under semver-`pattern`, in quotes injection). is not possible. `arch` is taken from `soulprint.self.os.arch` - a stable host self-fact available in the destiny CEL pass.)

## Security

- **TRUSTED-ONLY is the main invariant of the module.** `cmd`-string goes to the shell as
`sh -c "<cmd>"` without any escape (`util.RunOpts{Name: "sh", Args:
  ["-c", shellCmd]}`, [`cmd.go`](../../../../soul/internal/coremod/cmd/cmd.go)).
This is shell by design: pipes/redirects/glob/substitutions are needed by the module itself.
Consequence - **any untrusted interpolation in `cmd` = shell-injection**.
Values from CEL-render, `register.*`, `soulprint.*`, `input.*` fall into the string
and are executed by the shell as code through the metacharacters `$`, `` ` ``, `|`, `&`, `;`,
`>`, `<`, `(`, `*`. The source of the `cmd`-string must be the author of Destiny/scenario,
and not external input. The same guard commands `unless` / `onlyif` also go through
`sh -c` - they are subject to exactly the same prohibition on untrusted
interpolation.
- **Privileges.** The module **doesn't** declare `run_as_root` - in the manifest
([`cmd.yaml`](../../../../shared/coremanifest/cmd.yaml)) only
  [`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum).
The command is executed with the privileges of the `soul`-agent process, without elevation
inside the module; for system operations the agent in practice runs as root, and
then `sh -c` also runs under root - this increases the price of injection, and does not soften it.
- **Dangerous vs. correct.** Substituting an untrusted value directly into the shell string:

  ```yaml
  # DANGER: filename from an untrusted source is interpreted by the shell.
  # filename = "x; rm -rf /var/lib/app" → rm will be executed.
  - name: Remove uploaded file
    module: core.cmd.shell
    params:
      cmd: "rm -f /srv/uploads/${ input.filename }"
  ```

If shell semantics (pipes/redirects/glob) are not needed, rewrite them to
[`core.exec.run`](../exec/README.md), where the argv form passes the value to individual
token and metacharacters are **not** interpreted (verified: `core.exec` runs
`exec.CommandContext(cmd, args...)` without `sh -c`):

  ```yaml
  # SAFE: filename is a separate argv token, the shell is not involved.
  - name: Remove uploaded file
    module: core.exec.run
    params:
      cmd: rm
      args: ["-f", "/srv/uploads/${ input.filename }"]
  ```

If shell semantics are really needed and the `cmd` part comes from CEL-render -
value must be quoted by helper `${ q(...) }` (quoting for shell,
**post-MVP**: not available yet - see package-doc
[`cmd.go`](../../../../soul/internal/coremod/cmd/cmd.go); before he appeared
keep such steps entirely under the control of the Destiny author).

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/exec/README.md](../exec/README.md) - argv option without shell (TRUSTED-ONLY is not needed); the same guard flags.
- [core/archive/README.md](../archive/README.md) - unpacking before the install step.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
