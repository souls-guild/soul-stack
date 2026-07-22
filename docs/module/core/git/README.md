# core.git

Cloning and updating the git repository on the host. **Soul-side**, static
is built into the `soul` binary. Implementation - [`soul/internal/coremod/git/git.go`](../../../../soul/internal/coremod/git/git.go).

Calls system `git` as a subprocess (clone / pull / rev-parse); own
the module does not contain a git client. MVP deliberately **doesn't** cover remote URL changes,
submodule, lfs and sparse-checkout - too many forks for the first version.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `cloned` | There is a git repo along the path `path`. | `changed=true` if `path/.git` was missing and the repository was cloned. If `path/.git` already exists - `changed=false` (the contents are not touched, a new pull is not performed). |
| `pulled` | Along the path `path` there is a git repo, pulled up to remote (`git pull --ff-only`). | If `path/.git` is missing, clone, `changed=true`. If there is a repo - `git pull --ff-only`; `changed=true` only when `HEAD` has moved (checking `rev-parse HEAD` before and after). |

## cloned — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `repo` | string | required | Repository URL/address. Passed to `git clone` after `--` (repo starting with `-` will not be parsed as an option - argument-injection guard, security). |
| `path` | string | required | Clone target directory. The presence of `path/.git` is a criterion for idempotency. |
| `branch` | string | optional (default `main`) | Branch for `--branch`. If not specified - `main`. |
| `depth` | int | optional | Shallow clone depth (`--depth`). Applies only to `depth > 0`; not specified → full clone. |

## pulled — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `repo` | string | required | Repository URL/address. Used for clone-if-missing (same `--`-guard). |
| `path` | string | required | Repo directory. If `path/.git` is missing, clone first, then the "updated" semantics. |
| `branch` | string | optional (default `main`) | Branch for clone-if-missing. `git pull --ff-only` itself is not transmitted. |
| `depth` | int | optional | Shallow clone depth with clone-if-missing (`--depth`, only with `depth > 0`). |

## Capabilities / side-effects

- **Executes subprocesses:** `git clone` / `git pull --ff-only` / `git rev-parse HEAD`.
- **Changes the file system:** creates/updates the `path` directory. For system
paths requires appropriate permissions.
- **Network access:** clone/pull go to remote `repo`. Transport and Authentication -
on the system side `git` (ssh-agent, credential helper, `~/.netrc`, etc.);
the module does not configure them.
- **`pull` - fast-forward only** (`--ff-only`): divergent local history is not
blinks with force, the step drops (protection against the loss of local commits on the host).

## Output / register

`cloned`/`pulled` give `{ path, cloned: true, head }`, where `head` is the current
`HEAD` (sha from `git rev-parse HEAD`). `head` — best-effort: if `rev-parse` is not
gave sha, the field is empty (this does not affect the main flow).

## Example

`cloned` — upload the repository to the host (minimal example):

```yaml
- name: Clone deploy repo
  module: core.git.cloned
  params:
    repo: https://github.com/example/deploy.git
    path: /opt/deploy
    branch: main
    depth: 1
```

`pulled` - keep the working copy synchronous with the remote; `register` - to
restart the service only when shifting `HEAD`:

```yaml
- name: Keep deploy repo up to date
  module: core.git.pulled
  register: deploy_repo
  params:
    repo: https://github.com/example/deploy.git
    path: /opt/deploy
    branch: main
```

(in [`examples/`](../../../../examples/) there are no tasks with `core.git` yet - the example is minimal.)

## Security

- **`clone`/`pull` execute code from the repository - the main risk of the module.** System
`git` runs repository hooks during checkout (`.git/hooks/*`: `post-checkout`,
`post-merge`, etc.), and transport parameters can run an arbitrary command
on the host. The module calls `git clone` / `git pull --ff-only` through a subprocess
([`runClone` / `runPull`](../../../../soul/internal/coremod/git/git.go)) and **not**
disables hooks and does not sand git. Consequence: **`repo` must point to
trusted source** - clone untrusted repository = execute
the code of its author with the privileges of the process `soul`. Authentication and transport
(ssh-agent, credential helper, `~/.netrc`) - on the system side `git`, module
does not configure them.
- **Argument-injection guard `--` exists, but only covers `repo`/`path`.**
Positional arguments are preceded by the delimiter `--`
  (`args = append(args, "--", repo, path)`,
[`runClone`](../../../../soul/internal/coremod/git/git.go)): `repo`, starting
with `-` (for example `--upload-pack=<cmd>`), will not be parsed by git as an option. However
`branch` is substituted into `--branch <branch>` **before** `--` and without validation
(`OptStringParam`, default `main`): The untrusted value in `branch` may
change the meaning of the call. Keep `branch` under control by Destiny/scenario author,
like `repo`.
- **Dangerous vs. correct.** Substitution of untrusted source in `repo`:

  ```yaml
  # DANGER: repo from external input → someone else's repository is cloned, its
  # .git/hooks will be executed during checkout under the privileges of a soul agent.
  - name: Clone user-supplied repo
    module: core.git.cloned
    params:
      repo: "${ input.user_repo_url }"
      path: /opt/app
  ```

  ```yaml
  # SECURE: repo is a fixed trusted address, Destiny author replies
  # for its content; branch is also a literal.
  - name: Clone vetted deploy repo
    module: core.git.cloned
    params:
      repo: https://github.com/example/deploy.git
      path: /opt/deploy
      branch: main
  ```

- **Privileges.** The module **doesn't** declare `run_as_root` - in the manifest
([`git.yaml`](../../../../shared/coremanifest/git.yaml)) only
[`exec_subprocess`](../../../naming-rules.md#required_capabilities-enum) (call
`git`) and [`network_outbound`](../../../naming-rules.md#required_capabilities-enum)
(clone/pull go to remote). The file entry and `git` itself come with privileges
process `soul`-agent; writing to system paths (`/opt/...`, `/etc/...`) on
practice requires root - then the hooks of the untrusted repo will be executed under root, which
amplifies the trust cost of `repo` rather than softening it.
- **`pull` - fast-forward only** (`--ff-only`,
[`runPull`](../../../../soul/internal/coremod/git/git.go)): divergent local
history does not freeze with strength, the pace falls. This is protection against silent loss of local
commits on the host, not a security boundary against an untrusted remote.

## See also

- [README.md](../../README.md) - directory of core modules.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
