# core.firewall

Manage **one** firewall rule (port/protocol/source/action).
**Soul-side**, statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/firewall/firewall.go`](../../../../soul/internal/coremod/firewall/firewall.go)
(dispatcher + params parsing),
[`soul/internal/coremod/firewall/ufw.go`](../../../../soul/internal/coremod/firewall/ufw.go)
(backend ufw) and
[`soul/internal/coremod/firewall/firewalld.go`](../../../../soul/internal/coremod/firewall/firewalld.go)
(backend firewalld).

Backend is detected automatically (`util.DetectFirewall`) based on the installed
to the control binary, **not** by Soulprint: **ufw** (checked first - more often on
debian-park), then **firewalld** (`firewall-cmd`, redhat-park). If none
found - step drops (`no supported firewall detected (ufw/firewalld)`).
**iptables deliberately deferred** (requires chain semantics and `ip(6)tables-save`,
not covered by the add/delete pair of one rule).

## Security

The module works **only** with a specific rule (add/delete). He **never**
touches the default policy and **never** turns on the entire firewall: no
`ufw enable`, neither `systemctl start firewalld`, nor `ufw default` edits / target
zones ([ADR-016](../../../adr/0016-parity-license.md) "safety first").
Enabling a firewall with the default deny policy on a remote host instantly
would cut off SSH and lose control. This is covered by the unit test: `Apply` is not
should generate no enable/default commands.

For firewalld, mutations go through `--permanent` + explicit `firewall-cmd --reload`
(the rule survives a restart; `--reload` applies the permanent config at runtime,
**doesn't** restart the service and **doesn't** change the default policy).

- **Privileges.** Manifest
[`firewall.yaml`](../../../../shared/coremanifest/firewall.yaml) announces
`required_capabilities: [run_as_root, exec_subprocess]` - editing rules
firewall requires UID 0 and goes through subprocesses `ufw` / `firewall-cmd`
(status/list + add/delete + `--reload`). This is a **declaration** for static
checking `soul-lint` with `allowed_capabilities` host (see [docs/keeper/plugins.md →
required_capabilities](../../../keeper/plugins.md)),
but **not** runtime escalation: backend calls come with process privileges
`soul` agent (under root), there is no elevation of rights inside the module.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `present` | There is a rule. | `changed=true` if there was no rule and it is added. If already present - `changed=false` (reconciliation by parsing `ufw status` / `firewall-cmd --list-ports`/`--list-rich-rules`). |
| `absent` | The rule has been deleted. | `changed=true` if the rule was deleted. If it is not there - `changed=false`. |

## params

Same for `present` and `absent`.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `port` | int/string | required | Port `1..65535`. Accepted as a number or string (in case of `${…}` interpolation yielding a string). |
| `proto` | string | optional (default `tcp`) | Protocol: `tcp` \| `udp`. |
| `action` | string | optional (default `allow`) | Action: `allow` \| `deny`. |
| `source` | string | optional (default - any source) | IPv4-CIDR (`192.168.0.0/24`) or single IPv4 (`10.0.0.1`). Normalized to the canonical form: single IP → `/32`, CIDR with host bits collapses to the network address. **IPv6 is not supported in MVP** - rejected on `Validate` (both backends only work with IPv4, silent reception of IPv6 would result in a loopy add/drift). |
| `zone` | string | optional (default - default-zone) | Firewalld zone. Ignored for ufw. |

## Semantics of backends

- **ufw.** `present`/`absent` are translated to `ufw allow|deny …` and
`ufw delete allow|deny …`. Without `source` the short form is used
(`80/tcp`), with `source` - expanded (`proto tcp from <src> to any port <n>`),
is symmetrical to how it would normally be printed in `ufw status`. Idempotency -
parsing table `ufw status` (direction tokens `IN`/`OUT` are taken into account;
IPv6 mirrors `(v6)` are ignored).
- **firewalld.** Simple `allow` rule without `source` - via `--add-port` /
`--remove-port`, availability is checked in `--list-ports`. Rule with `source`
**or** `action: deny` requires rich-rule (simple port - always accept),
availability is checked in `--list-rich-rules`. `deny` → rich-rule with `reject`.

CLI output parsing is fragile between tool versions - covered by strict
unit tests on fixed samples.

## Capabilities / side-effects

- **Requires root** (`run_as_root`): Editing firewall rules.
- **Executes subprocesses** (`exec_subprocess`): `ufw` / `firewall-cmd`
(status/list + add/delete + `--reload` for firewalld).
- **Changes the system:** set of firewall rules (side-effect `port`). Default policy and
"on/off" state - **does not touch** (see security invariant).

## Output / register

`{ changed, backend, port, proto, action }`, where `backend` - `ufw` \|
`firewalld`. The fields `source` and `zone` are present in output only if they were
are specified (and `source` is already in normalized form).

## Example

```yaml
- name: Allow PostgreSQL from internal subnet
  module: core.firewall.present
  params:
    port: 5432
    proto: tcp
    action: allow
    source: 10.0.0.0/8
```

(minimum valid example - there are no tasks for `core.firewall` in `examples/` yet)

## See also

- [README.md](../../README.md) - directory of core modules.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
- [ADR-016](../../../adr/0016-parity-license.md) - "safety comes first" (default policy invariant).
