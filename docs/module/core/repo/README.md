# core.repo

OS package repository management (ansible idea `apt_repository`/`yum_repository`,
redesigned to be a secure declarative MVP). **Soul-side**, statically integrated
in `soul` binary. Implementation - [`soul/internal/coremod/repo/repo.go`](../../../../soul/internal/coremod/repo/repo.go)
(contract, validation, parameters) and [`soul/internal/coremod/repo/backends.go`](../../../../soul/internal/coremod/repo/backends.go)
(apt/dnf/yum/apk backends, file operations).

Backend is determined automatically (`util.DetectPkgMgr`); if no manager
found - step drops (`no supported package manager detected`). Target artifacts by
backend:

- **apt** → `/etc/apt/sources.list.d/<name>.list` (one-line format) + key in
`/etc/apt/keyrings/<name>.gpg`, which is referenced by `.list` through `signed-by=`
(modern format, **not** `apt-key` - that is deprecated and puts the key in the general
trust store without reference to the repository);
- **dnf/yum** → `/etc/yum.repos.d/<name>.repo` (ini format);
- **apk** → line in `/etc/apk/repositories`.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | The repository is declared: the description file (and for apt - the GPG key) is in place with the necessary content. | `changed=true` if the target file was missing/byte-by-byte different or (apt with `gpg_key`) the key was missing/different. Everything coincided - `changed=false`. |
| `absent` | The repository description has been removed. **The GPG key is not touched** - it can be used by other repositories (manually cleaning the key is a separate explicit step). | `changed=true` if the file/line was deleted. There was no - `changed=false`. |

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Repository name. Becomes the file name (`<name>.list`/`<name>.repo`), so it is validated: only `[A-Za-z0-9._-]`, without `/`, `\` and `..` (protection against path-traversal - writes outside the target directory). |
| `uri` | string | required (for `present`) | Base URL of the repository. Valid `http://` and `https://` (`file://`/`ftp://`/blank - error). `http://` is legitimate for the interior mirror, but gives a mandatory warning (see Security). |
| `gpg_key` | string | optional | Contents of the GPG key inline (ASCII-armored/PEM or binary keyring - written as is). For apt, it materializes in `/etc/apt/keyrings/<name>.gpg` (mode `0644`) and connects via `signed-by=`; for dnf/yum it is written to `gpgkey=`. Ключ **по URL в core.repo намеренно не качается** (ADR-071 §(g), вариант B): сеть/SSRF остаётся в [`core.url.fetched`](../url/README.md) (network_outbound + SSRF-guard + checksum), а сюда содержимое подаётся inline через `${ file(...) }`/`vault`. Critical for supply-chain. |
| `gpg_check` | bool | optional (default `true`) | Cryptographic packet verification. `false` - opt-out, allowed, but gives a mandatory warning (symmetry of checksum-opt-out in core.url). For dnf/yum it is written in `gpgcheck=`. |
| `suite` | string | optional | Suite/distribution (apt: `deb <uri> <suite> <components>`). It does not affect dnf/yum/apk (apk puts the full URL in `uri`). |
| `components` | list | optional | apt-string components (`main contrib …`). Apt only. |
| `arch` | list | optional | Архитектуры apt-строки → опция `[arch=amd64,arm64 …]` (после `signed-by=`). Токены санитизируются (`[a-z0-9]`, без пробелов/скобок — защита от инъекции в опции). Только apt; на dnf/yum/apk игнорируется (как `suite`/`components`). ADR-071. |
| `enabled` | bool | optional (default `true`) | Is the repository enabled? `false`: for apt/apk the line is commented out (`# …`), for dnf/yum - `enabled=0`. |

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Repository name (same character set). For apt/dnf/yum, defines the file to be deleted `<name>.list`/`<name>.repo`. |
| `uri` | string | required **for apk** | apk does not store the repo name in a file, so the deletion matches `uri`. For apt/yum `uri` is not needed (there is a file `<name>`). Without `uri` apk-absent crashes (otherwise guessing is a risk of deleting someone else's line). |

(`gpg_check`/`suite`/`components`/`enabled`/`gpg_key` for `absent` are not used.)

## Security

[ADR-016](../../../adr/0016-parity-license.md)
"safety comes first", but the balance with legitimate cases is softer than that
core.url, with mandatory warnings instead of prohibitions:

- **`gpg_key` is critical for supply-chain.** If specified, the key is real
materializes (apt: keyring + `signed-by=`) / is written as `gpgkey=`
(dnf/yum) and participates in idempotency comparison. apt uses modern
`signed-by=`-keyring (trust is tied to a specific repository, not to the global one
  trust store).
- **`gpg_check=false` allowed** (opt-out), but Apply returns required
warning in output: "packages will NOT be cryptographically verified."
- **`gpg_check=true` without `gpg_key`** - also warning, backend-specific: for
dnf/yum this will **break package installation** (`gpgcheck=1` without `gpgkey=`); apt/apk
rely on their trust stores.
- **`http://` in `uri` is acceptable** (internal mirror), but with a mandatory warning
  "traffic is unencrypted".
- **`name` is sanitized** against path-traversal (name becomes filename).

## Capabilities / side-effects

- **Executes subprocesses:** only for `util.DetectPkgMgr` (backend definition).
Writing repository descriptions - in-process, without shell.
- **Changes the file system:** writes/deletes the repository description file and (apt with
`gpg_key`) keyring. For system paths (`/etc/apt`, `/etc/yum.repos.d`,
`/etc/apk`) requires appropriate rights. Recording - preserve-by-default
(`util.AtomicWritePreserving`): The rights/ownership of the existing file is preserved.
- **Does not execute `apt-get update`/`dnf makecache`** - the module only declares
repository; index refresh does `core.pkg` when installed.

## Output / register

`present`/`absent` give `{ name, backend, path, changed }`, where `backend` -
`apt`/`yum`/`apk`, `path` is the affected file (`<name>.list`/`<name>.repo` or
`/etc/apk/repositories`). If there were warnings (opt-out gpg_check / http uri / no
key) - field `warnings: [...]` with a list of strings (they end up in the output and are not lost).

## Example

`present` (apt) with a GPG key - minimal example:

```yaml
- name: Declare internal apt repo
  module: core.repo.present
  params:
    name: example-internal
    uri: https://apt.example.com/debian
    suite: bookworm
    components: [main]
    gpg_key: "${ file('files/example.gpg.asc') }"
```

### Зеркало upstream в закрытом контуре (ADR-071, вариант B)

Подключение внутреннего apt-зеркала (напр. redis.io в Nexus) — двумя шагами: ключ
приносит `core.url.fetched` (там network_outbound + SSRF-guard + checksum), а
`core.repo` объявляет репо inline-ключом через `${ file(...) }` и задаёт `arch`.
core.repo сети НЕ касается (pure-FS) — ключ-по-URL остаётся вне модуля:

```yaml
# 1) ключ redis.io из внутреннего зеркала (allow_private — internal Nexus, ADR-067)
- name: Fetch redis.io signing key
  module: core.url.fetched
  params:
    url: https://nexus.internal/repository/redis-raw/redis.gpg
    path: /etc/soul-stack/keys/redis.asc
    checksum: "sha256:<hex>"
    allow_private: true

# 2) объявить зеркало apt (uri=Nexus, arch=amd64, ключ inline)
- name: Declare redis.io apt mirror
  module: core.repo.present
  params:
    name: redis
    uri: https://nexus.internal/repository/redis-apt
    suite: bookworm
    components: [main]
    arch: [amd64]
    gpg_key: "${ file('/etc/soul-stack/keys/redis.asc') }"
```

Результат — `/etc/apt/sources.list.d/redis.list`:

```
deb [signed-by=/etc/apt/keyrings/redis.gpg arch=amd64] https://nexus.internal/repository/redis-apt bookworm main
```

Ключ может быть ASCII-armored (`.asc`): apt ≥ 1.4 читает armored keyring по
`signed-by=` напрямую; если нужен бинарный keyring — зеркало отдаёт уже
dearmored-ключ (dearmor вне core.repo). Установку пакета из объявленного зеркала
делает `core.pkg.installed` с `=version`-пином (S3/NIM-105).

`absent` (apk) - deletion matches `uri`:

```yaml
- name: Drop old apk mirror
  module: core.repo.absent
  params:
    name: old-mirror
    uri: https://dl-cdn.alpinelinux.org/alpine/edge/testing
```

(there are no tasks with `core.repo` in [`examples/`](../../../../examples/) yet - the examples are minimal.)

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/pkg/README.md](../pkg/README.md) - install packages from the declared repository.
- [core/url/README.md](../url/README.md) - checksum-opt-out, symmetric gpg-opt-out here.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
- [ADR-016](../../../adr/0016-parity-license.md) - "safety comes first."
