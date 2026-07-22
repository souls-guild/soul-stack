# core.pkg

Managing OS packages via native package manager. **Soul-side**, static
is built into the `soul` binary. Implementation - [`soul/internal/coremod/pkg/pkg.go`](../../../../soul/internal/coremod/pkg/pkg.go).

Backend is taken from the soulprint fact `os.pkg_mgr` (**primary**, [ADR-018(b)](../../../adr/0018-soulprint-typed.md)); **fallback** for an empty/unknown fact - runtime detection `util.DetectPkgMgr`: **apt** (Debian/Ubuntu),
**dnf** (RHEL ≥ 8), **yum** (RHEL ≤ 7), **apk** (Alpine). If neither fact nor detection
given by the manager - the step is falling (`no supported package manager detected`).

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `installed` | The package is installed (optional exact version). | `changed=true`, if the package was not present or the installed version ≠ requested `version`. If the required version is already installed (or `version` is not specified and the package is available) - `changed=false`. |
| `absent` | The package has been removed. | `changed=true` if the package was removed. If there is no package - `changed=false`. |
| `latest` | The package is installed and updated to the latest version of the repository. | `changed=true`, if the package was not present or the version has changed after the operation. |

## installed — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Package name. |
| `version` | string | optional (default - no pin) | Exact version in distro-native form (`1.2.3`, `5:7.0.15-1~deb12u7`). Broadcasts to `name=version` (apt/apk) or `name-version` (dnf/yum). Empty/not specified → the default version from the repository without a pin is installed. |

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Package name. |

## latest — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `name` | string | required | Package name. (`version` for `latest` is ignored - the point of state is to pull up the latest version.) |

## Capabilities / side-effects

- **Requires root** (`run_as_root`): install/remove via package manager.
- **Executes subprocesses** (`exec_subprocess`): `apt-get` / `dnf` / `yum` / `apk`,
plus query commands (`dpkg-query` / `rpm -q` / `apk info`).
- **Changes the system:** installed set of packages.
- **Refresh repository index** before `installed`/`latest` on apt/apk
(`apt-get update` / `apk update`) is executed once during the life of process `soul`
(important for fresh VMs/containers where the index is empty). dnf/yum does not refresh -
they pull up the metadata by expiration themselves.

## Output / register

`installed`/`latest` give `{ name, installed: true, version }`; `absent` —
`{ name, installed: false }`. Version in output - best-effort: if pkg-mgr does not return
it is compact, the field can be empty (this does not affect the `installed` flag).

## Example

```yaml
# Package from the distribution repository. version is optional: caller may not transmit
# key - then core.pkg installs the default version from the repo without the =version pin.
- name: Install redis-server package
  module: core.pkg.installed
  retry: { count: 3, delay: 10s }
  params:
    name: redis-server
    version: "${ has(input.version) ? input.version : '' }"
```

(the working installation of `redis-server` is in [`examples/destiny/redis/tasks/install.yml`](../../../../examples/destiny/redis/tasks/install.yml); there `version` is made `required: true` and reads bare `input.version`, without a ternary - this is a choice of destiny, not a module limitation: empty `version` sets the default from the repo)

## Security

- **Package installation = execution of arbitrary postinst code with process rights
`soul` is the main risk of the module.** `installed`/`latest` are called
  `apt-get install` / `dnf install` / `yum install` / `apk add`
([`runInstall`/`runLatest`](../../../../soul/internal/coremod/pkg/pkg.go)), and
package manager executes package maintainer scripts (postinst/pre-remove
etc.) as root. The trust of a module is transitive: it is exactly equal to the trust in
the repository and package signature, not the Soul Stack itself. Package name (`name`) and
version (`version`) should come from the author of Destiny/scenario, not from
untrusted input - untrusted `name` = install a custom package from
configured repositories.
- **Signature verification is on the package manager side, not the module side.** `core.pkg`
**GPG/checksum itself does not check**: it does not add keys and does not switch
policy verification. Integrity guarantee is apt/dnf/yum/apk with their
repository keys on the host. Installing from an unverified/unsigned repo
(or with OS level check disabled) bypasses this protection, and the module is
does not insure in any way - the correct configuration of trusted repositories lies outside
module. Flags of the form `--allow-unauthenticated` / `--nogpgcheck` module **not**
exhibits (verified: `install` goes without them).
- **Privileges.** Manifest [`pkg.yaml`](../../../../shared/coremanifest/pkg.yaml)
announces `required_capabilities: [run_as_root, exec_subprocess]` —
install/remove via package manager always requires root and launch
subprocesses. This is a **declaration** for static reconciliation of `soul-lint` with
`allowed_capabilities` host (see [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
and **not** runtime elevation: the operation is executed with the privileges of the process
`soul` agent (under root), no elevation of rights inside the module; postinst scripts
run under the same root.
- **Backend and index refresh.** The manager is taken from the soulprint fact
`SoulprintFacts.os.pkg_mgr` (**primary**, [ADR-018(b)](../../../adr/0018-soulprint-typed.md));
**fallback** for an empty/unknown fact - runtime detection (`util.DetectPkgMgr` via
`command -v`). Before `installed`/`latest` on apt/apk
`apt-get update` / `apk update` is executed once during the life of the process
([`refreshIndex`](../../../../soul/internal/coremod/pkg/pkg.go)) - he pulls up
up-to-date metadata (including package revocations) from configured repositories;
trust in the contents of the index is again a property of the repo, not the module.

## See also

- [README.md](../../README.md) - directory of core modules.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
- [ADR-018(b)](../../../adr/0018-soulprint-typed.md) — `SoulprintFacts.os.pkg_mgr` as **primary** backend source; runtime detection - fallback.
