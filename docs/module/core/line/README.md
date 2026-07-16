# core.line

In-place line-by-line editing of an existing file (lineinfile equivalent).
**Soul-side**, statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/line/line.go`](../../../../soul/internal/coremod/line/line.go)
(dispatcher + validation),
[`soul/internal/coremod/line/apply.go`](../../../../soul/internal/coremod/line/apply.go)
(read params, write file) and
[`soul/internal/coremod/line/edit.go`](../../../../soul/internal/coremod/line/edit.go)
(pure string editing functions).

This is the first core module that **doesn't** overwrite the entire file (like
`core.file`), but changes individual lines. Recording is atomic
(`util.AtomicWrite`: temp + rename), not in-place truncate. Default
mode/owner/group of existing file **saved** ([ADR-015](../../../adr/0015-core-modules-mvp.md)),
explicit `mode`/`owner`/`group` for `present` - override.

## Be careful with `regexp` - the main source of drift

`core.line` is a deliberately stripped-down secure MVP for precisely the reason that
which lineinfile was deferred: **"regexp matches not what you think"**. What
exactly matches:

- `regexp` applies to **each logical line of the file without the `\n`** terminator
via Go `regexp.MatchString` - that is, a **partial** match on a string (not
"entire line" unless you set the anchor `^…$` yourself).
- CRLF **not** normalized: `\r` remains part of the string and participates in matching
and compare as is (predictability - the module does not guess the intent).
- **Backrefs are not supported**: you cannot substitute groups from `regexp` into `line`.
- `regexp` is validated against `Validate` (`regexp.Compile`); invalid pattern -
error before launch.

Behavior of `present` with `regexp` on **multiple** matches: replaced
**only the first** matching line, the rest are left untouched, goes into output
`warning` with the number of matches. This is a deliberate safe choice, not a bug.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The line `line` is present in the file. **With `regexp`:** the first matching line is replaced with `line` (if there are no matches - `line` is added according to the insertion rules; if the first match is already equal to `line` - no-op). **Without `regexp`:** The exact line `line` is added if it is not already there. | `changed=true` if a line is added or replaced. Match/presence → `changed=false`. |
| `absent` | **With `regexp`:** **all** matching lines are removed. **Without `regexp`:** **all** exact matches of `line` are removed. | `changed=true` if at least one row was deleted. There is nothing to delete (or there is no file) → `changed=false`. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Target file. |
| `line` | string | required | The exact string value being manipulated is added (without `regexp`) or substituted in place of the first match (with `regexp`). |
| `regexp` | string | optional | Go-regexp; **partial** match on string. Without anchors, `^…$` matches the substring. Backrefs are not supported. |
| `insertafter` | string | optional (`""` \| `EOF` \| literal) | Insertion position when adding: after the first line, exactly equal to the literal; `EOF` or unfound anchor → end of file. Mutually exclusive with `insertbefore`. |
| `insertbefore` | string | optional (`""` \| `BOF` \| literal) | Insertion position: before the first line, exactly equal to the literal; `BOF` → start of file; unfound anchor → end of file. Mutually exclusive with `insertafter`. |
| `create` | bool | optional (default `false`) | If the file does not exist: `create:true` creates it with the single line `line`; otherwise the step drops (`file not found, set create:true to create it`). |
| `mode` | string | optional | Rights in octal form (`"0644"`). They are used only with `create:true` (new file) or as an override when editing. |
| `owner` | string | optional | Owner (username); resolves via `/etc/passwd`. |
| `group` | string | optional | Group(name); resolves via `/etc/group`. |

Anchors `insertafter`/`insertbefore` (except `EOF`/`BOF`) are an **exact match
strings**, not regexp: insertion position is predictable. Anchor not found → fallback
on EOF.

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Target file. If there is no file, no-op (`changed=false`, `create` is ignored). |
| `line` | string | requires `line` **or** `regexp` | Without `regexp` - exact match criterion to remove all such rows. |
| `regexp` | string | requires `line` **or** `regexp` | Removes **all** matching strings (partial match, see warning above). |

`absent` does not accept `mode`/`owner`/`group` - always saves when editing
current mode/owner/group of the file.

## Capabilities / side-effects

- **Changes the file system** (`fs_write_root` for system paths): edit /
creates a file by atomic writing (temp + rename), can change owner/mode.
For paths, `/etc/...` in practice requires `run_as_root`.
- **Does not execute subprocesses** - read, edit and write in-process,
without shell.
- The final line feed of the source file is preserved; empty file is not
becomes a file with an empty line.

## Output / register

- `present` (edit): `{ path, changed, matched, replaced }`; `warning` —
is added only for multiple `regexp` matches.
- `present` (creation, `create:true`): `{ path, changed: true, created: true }`.
- `present`/`absent` unchanged: `{ path, changed: false, matched }` or
  `{ path, changed: false, removed: 0 }`.
- `absent` (delete): `{ path, changed: true, removed: <N> }`.

## Examples

`present` - guarantee customization by replacing the existing regexp string:

```yaml
- name: Ensure PasswordAuthentication is off in sshd_config
  module: core.line.present
  params:
    path: /etc/ssh/sshd_config
    regexp: '^#?PasswordAuthentication'
    line: 'PasswordAuthentication no'
```

`absent` — remove all lines that match the pattern:

```yaml
- name: Remove legacy include directives
  module: core.line.absent
  params:
    path: /etc/app/app.conf
    regexp: '^include\s+/etc/app/legacy/'
```

(minimum valid example - there are no tasks for `core.line` in `examples/` yet)

## Security

- **`regexp` edits someone else's file with a partial match - the main source of dangerous
edits.** `regexp` is applied to each logical line via
`regexp.MatchString` without implicit anchors
  ([`presentRegexp` / `absentEdit`](../../../../soul/internal/coremod/line/edit.go)):
without `^…$` it matches **substring**, and in `absent` it deletes **all** matched strings.
Too wide a pattern (especially from untrusted interpolation) can destroy
more lines than intended in the system config. See a detailed analysis of semantics
matches in the section "Be careful with `regexp`"
above - it is also the main security invariant of the module. Anchors
`insertafter`/`insertbefore` is an **exact** string match (literal), not
  regexp ([`appendByPosition`](../../../../soul/internal/coremod/line/edit.go)),
so the insertion position is predictable and independent of the pattern.
- **ReDoS is not a threat, backrefs are prohibited.** Engine - Go `regexp` (RE2):
compilation via `regexp.Compile` to `Validate` and to `readParams`
  ([`line.go`](../../../../soul/internal/coremod/line/line.go),
[`apply.go`](../../../../soul/internal/coremod/line/apply.go)) - invalid
pattern is rejected before writing. RE2 guarantees linear matching time, so
catastrophic backtracking / ReDoS on untrusted `regexp` is not possible (unlike
from PCRE). Backrefs (group substitution `regexp` to `line`) **not supported** in MVP
([ADR-015](../../../adr/0015-core-modules-mvp.md)): this
removes a whole class of errors "they wrote something into the file that was not what they thought."
- **Write is atomic (temp + rename), no partially written file.** Module
never does in-place truncate: new content is written to a temporary file
in the same directory and atomically renamed
  (`util.AtomicWrite` / `util.AtomicWritePreserving`,
[`apply.go`](../../../../soul/internal/coremod/line/apply.go)). Aborting a run
(crash, OOM) does not leave the config half rewritten. When editing in-place
mode/owner/group **saved** by default
([ADR-015](../../../adr/0015-core-modules-mvp.md)) —
the module does not silently demote the rights of an existing file.
- **Dangerous vs. correct.** Untrusted pattern in `absent`:

  ```yaml
  # DANGER: regexp from external input without anchors. value = "" → matches each
  # line → absent will delete the ENTIRE file; a wide pattern will remove the excess.
  - name: Remove user-supplied lines
    module: core.line.absent
    params:
      path: /etc/app/app.conf
      regexp: "${ input.user_pattern }"
  ```

  ```yaml
  # SAFE: regexp is a fixed anchor pattern from the author of the task,
  # matches exactly the required lines.
  - name: Remove legacy include directives
    module: core.line.absent
    params:
      path: /etc/app/app.conf
      regexp: '^include\s+/etc/app/legacy/'
  ```

- **Privileges.** The module **doesn't** declare `run_as_root` - in the manifest
([`line.yaml`](../../../../shared/coremanifest/line.yaml)) only
[`fs_write_root`](../../../naming-rules.md#required_capabilities-enum) (record for
limits `/var/lib/soul-stack/`). Editing occurs with process privileges
`soul`-agent; for paths `/etc/...` in practice requires root. Subprocesses
module **doesn't** run - reading, editing and writing in-process, without shell
  ([`apply.go`](../../../../soul/internal/coremod/line/apply.go)).

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/file/README.md](../file/README.md) - manage the entire file (present/absent/rendered).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs; why `core.line` is cut in MVP.
