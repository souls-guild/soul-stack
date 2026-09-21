# core.exec

Starting a process directly via `exec()` with argv - **no shell** (no pipes,
redirects, glob, variable substitutions). **Soul-side**, statically built into
`soul`-binary. Implementation -
[`soul/internal/coremod/exec/exec.go`](../../../../soul/internal/coremod/exec/exec.go).

This is a verb module: the only state is `run` (without declarative semantics
"lead to state"). For shell semantics (pipes/redirects) - [`core.cmd`](../cmd/README.md).
Which exit codes count as success is the `exit_codes` param, and it defaults to
`[0]`: a command that ran and exited non-zero fails the task. Codes that are an
answer rather than an error — `grep` exiting 1 — are declared per task.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `run` | Run `cmd` from `args` through `exec()`. | Defaults to `changed=true` (verb "execute"). You can downgrade to no-op using the guard parameters `creates` / `unless` / `onlyif` (check order: creates → unless → onlyif, the first one to fire wins): when triggered, the **not** command is launched, `changed=false`, output `{ skipped: true, reason, exit_code: 0 }`. For read-only probe, put `changed_when: false` in scenario. |

## run — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `cmd` | string | required | Name/path of the executable file (argv[0]). Runs directly, without `sh -c`. |
| `args` | list&lt;string&gt; | optional | Command arguments (argv[1:]). Each element is transmitted as a separate token, without shell parsing. |
| `cwd` | string | optional | The working directory of the process. |
| `env` | map&lt;string,string&gt; | optional | Process environment variables (`KEY=VALUE`). |
| `creates` | string | optional | Guard: if the file at this path **exists** - skip (`changed=false`, `reason: creates`). |
| `unless` | string | optional | Guard: execute `sh -c "<unless>"`; if its exit **= 0** - skip (`reason: unless`). |
| `onlyif` | string | optional | Guard: execute `sh -c "<onlyif>"`; if its exit **≠ 0** - skip (`reason: onlyif`). |
| `exit_codes` | list | optional, default `[0]` | Exit codes accepted as success. Elements are exact integers and/or inclusive `"lo-hi"` string ranges, mixed freely: `[0, 1, 2]`, `["2-5"]`, `[0, "2-5"]`. Any code outside the set fails the task. An empty list is rejected — it would accept nothing at all, not even 0. A process killed by a signal reports `-1`, which no `"lo-hi"` range can cover (the string form is non-negative by construction) — write negatives as bare integers: `[0, -1]`. Does not apply to the guards: `unless`/`onlyif` read their own exit code by their own rule, and a skipped task reports `exit_code: 0` and never fails. |

## Capabilities / side-effects

- **Executes subprocesses** ([`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum)):
main command (`cmd args`), as well as guard commands `unless` / `onlyif`
(the latest ones are via `sh -c`).
- **Changes the system exactly as much as the running command changes** —
the module is a wrapper over the process and does not write anything by itself. For system
operations require appropriate rights (in practice - root,
  [`run_as_root`](../../../naming-rules.md#required_capabilities-enum)).
- `creates` uses `os.Stat` (path existence), `unless`/`onlyif` -
auxiliary shell calls.

## Output / register

`run` returns `{ stdout, stderr, exit_code }` (exit_code is a number). When triggered
guard - `{ skipped: true, reason, exit_code: 0 }` with `changed=false`. Typical
using `register:` - read-only probe (`changed_when: false`) with reading
`register.<name>.stdout` in subsequent `where:` / `failed_when:` (`output:` is **not** a read site - the key is refused, `output_unsupported`).

**A code outside `exit_codes` fails the task, and the output still arrives.** The final
event carries `failed=true` **and** `{ stdout, stderr, exit_code }`, so `register.<name>.*`
is fully populated on the failure too — the stderr that explains it is right there, and a
predicate over `register.self.exit_code` is evaluable.

A stream can still go missing: a command's output is arbitrary bytes, and one that is not
valid UTF-8 (`printf '\xff'`) cannot be carried. Each field is decided on its own — an
unencodable one is left out and named in the failure message, an encodable one is kept
whatever its neighbours did, and a command that wrote binary to both streams loses both
and has both named. `exit_code` is a number, so it is never among them and the
`failed_when:` escape hatch always has something to read.

It is the only failure class that carries output. Everything that fails **before** the
command runs carries none, and there is no `register.self.exit_code` to test — read
`TaskEvent.error` instead: an unknown state, a bad param (`cmd`/`args`/`cwd`/`env`, and a
malformed `exit_codes`, which is rejected before the guards are even checked), a
`creates`/`unless`/`onlyif` guard that could not be evaluated (`os.Stat` denied, the guard
shell unavailable), and a process that would not start at all (`res.Err != nil`).

To accept a code the command legitimately returns, widen the set on the task:

```yaml
params:
  cmd: grep
  args: ["-q", "listen", "/etc/redis/redis.conf"]
  exit_codes: [0, 1]                  # 1 = no match, an answer
```

To tolerate **any** code, waive the verdict from the scenario with `failed_when: false`
([destiny/tasks.md](../../../destiny/tasks.md), `failed_when:`) — it is applied after Apply
and has the last word. Note that a waived task ends **OK, not CHANGED**, so `onchanges:`
dependents of it do not fire; when a handler must still run, widen `exit_codes` instead.

## Example

```yaml
# Read-only probe: run argv without shell, read stdout into register.
- name: Read kernel release
  module: core.exec.run
  register: kernel_release
  changed_when: false
  params:
    cmd: uname
    args: ["-r"]

# Running with guard creates: binary in place → no-op.
- name: Initialize data dir once
  module: core.exec.run
  params:
    cmd: /usr/local/bin/app
    args: ["init", "--data-dir", "/var/lib/app"]
    creates: /var/lib/app/.initialized
```

## Security

- **argv-form, without shell - the key difference from [`core.cmd`](../cmd/README.md).**
`cmd` is launched directly as `exec.CommandContext(cmd, args...)` without `sh -c`
([`exec.go`](../../../../soul/internal/coremod/exec/exec.go)). Metacharacters
(`$`, `` ` ``, `|`, `&`, `;`, `>`, `*`) in `cmd`/`args` **are not interpreted** —
each element of `args` is transmitted as a separate token without shell parsing. Therefore
there is no risk of shell-injection: the value `"x; rm -rf /"` in `args` is one
literal argument, not command.
- **Still TRUSTED-ONLY for the binary name.** The absence of a shell does not make the module
safe for arbitrary untrusted input: `cmd` (argv[0]) specifies which
the executable file will be launched, and `args` - with what arguments. Untrusted
value in `cmd` = run a custom binary; untrusted in `args` can
change the meaning of the operation (flags, paths). Values from `register.*` / `soulprint.*`
/ `input.*` in `cmd`/`args` are only valid if they are trusted by the author
  Destiny/scenario.
- **Guard `unless` / `onlyif` is a shell.** Unlike the main command,
auxiliary guard commands are executed via `sh -c "<unless|onlyif>"`
([`shouldSkip`](../../../../soul/internal/coremod/exec/exec.go)) - to their lines
is subject to the same prohibition against untrusted interpolation as
[`core.cmd`](../cmd/README.md). `creates` shell does not use - this is `os.Stat`
along the way.
- **Privileges.** The module **doesn't** declare `run_as_root` - in the manifest
([`exec` module](../../../../shared/coremanifest/mod_exec.go)) only
  [`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum).
The command is executed with the privileges of the `soul`-agent process, without elevation
inside the module; For system operations, the agent in practice runs under root.
- **Side-effect `creates:` (idempotency anchor).** If there is a file on the path
`creates` command **not** run (`changed=false`, `reason: creates`).
This reduces the side effects of repeated runs, but `creates` only checks
the existence of the path (`os.Stat`), not the content or success of the last run -
do not rely on it as a guarantee of correct condition, only as a
guard "already done."

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/cmd/README.md](../cmd/README.md) - shell option (`sh -c`, pipes/redirects); the same guard flags.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
- [ADR-008](../../../adr/0008-coven-stable-tags.md) - volatile role via inline-probe (`core.exec.run` + `register:` + `where:`).
