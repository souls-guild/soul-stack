# `core.ssh`

The Keeper's transport to a host that has no agent on it yet. One public state,
`core.ssh.run`; the task is routed by its module address and carries no `on:`
key ([ADR-063 amendment 2026-09-12](../../../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-12--the-transport-comes-back-without-the-policy-nim-849),
NIM-849).

It exists because `core.exec.run` and `core.file.present` are Soul-side and a
freshly created VM has no Soul: without it the engine can mint a bootstrap token
and then has no way to put anything on the machine that would redeem it.

**It carries no opinion about what the commands do.** That was
`core.bootstrap.delivered`'s mistake and the reason NIM-834 removed it: the
module also decided WHAT to install, and installation has no portable form, so
it accreted a second transport, a full-install mode, an init phase and a second
`soul.yml` port — one platform at a time. Here the commands are the service
author's, and the requirements binding that author are in the
[ADR-063 amendment 2026-09-09](../../../adr/0063-bootstrap-token-delivery.md#-requirements-on-whoever-installs-the-host).

## `core.ssh.run`

```yaml
- name: Install soul and redeem the token
  module: core.ssh.run
  register: install
  require: [tokens]
  params:
    hosts: "${ register.tokens.hosts }"
    ssh_provider: vault-ssh
    steps:
      - run: "curl -fsSL ${ vars.soul_binary_url } -o /usr/local/bin/soul && chmod 0755 /usr/local/bin/soul"
      - run: "install -d -m 0700 /etc/soul && umask 077 && cat > /etc/soul/token && chmod 0400 /etc/soul/token"
        stdin_from: bootstrap_token
      - run: 'test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN="$(cat /etc/soul/token)" soul init --config /etc/soul/soul.yml'
      - run: "systemctl daemon-reload && systemctl enable --now soul"
```

### Parameters

| Parameter | Type | Req. | Default | Semantics |
|---|---|---|---|---|
| `hosts` | list of object | required | — | The hosts to run on, in practice `${ register.<mint>.hosts }`. `sid` is required on every entry. An empty list is an error. |
| `steps` | list of object | required | — | Ordered steps; see below. An empty list is an error. |
| `ssh_provider` | string | required | — | The SshProvider plugin name (`keeper.yml::plugins.ssh_providers[].name`) used for `Authorize`/`Sign`. In `teleport` transport it does NOT determine the transport and goes only into the audit payload. |
| `ssh_user` | string | — | `root` | SSH user. |
| `ssh_port` | int (1..65535) | — | `22` | sshd TCP port. |
| `join_wait_timeout` | duration or int seconds | — | `15m` | Ceiling on the wait for a host to become reachable, on **both** transports. A fresh VM joins Teleport minutes after creation; on `direct` its sshd starts listening well after the DHCP lease. Past the deadline the step fails. The deadline is tested BETWEEN attempts, so one attempt that hangs (a dropped SYN rather than a refusal) can overrun it by up to the OS TCP timeout. See [What is waited for and what is not](#what-is-waited-for-and-what-is-not). |

A step is `run:` plus **at most one** stdin source:

| Key | Semantics |
|---|---|
| `run` | A shell command line, executed by the remote user's shell. Required, non-empty. |
| `stdin` | A literal value fed to the process's stdin — the SAME on every host. For a `${ vault(…) }` or `${ vars.* }` value. |
| `stdin_from` | The NAME of a field of the host entry; the module reads that host's value and feeds it to stdin. For a per-host secret. |

Declaring both `stdin` and `stdin_from` on one step is a refusal, not a
precedence rule: an author who wrote both meant one of them, and silently
picking would deliver the wrong value to a machine nobody can inspect.

### Why `stdin_from:` and not `${ host.bootstrap_token }`

There is no `host.*` CEL root, and there cannot be one here. A keeper-side task
is rendered **once for the whole run**, not once per host — `keeperVars` binds no
roster, and the render pass's seal is required to be host-invariant
(`TestRender_SealedSetDoesNotMoveWithTheRoster`) — so a per-host value has no
expression that could reach a params cell. `loop:` is not an escape either: it is
not supported on a keeper-side task at all.

Naming the field is the better shape regardless: the per-host secret never enters
the task's params, so keeping it out of the command line is constructive rather
than checked.

### The secret floor

**A secret in `run:` is REFUSED, before the connect.** Not masked afterwards: a
command argument is visible in `ps`, in `audit.log` and in journald **on the host
itself**, which is a machine the Keeper does not own yet, so redacting the
operator's copy would hide the leak rather than prevent it. Two independent
checks, because neither covers the other's case:

- **by path** — the render SEALED that `run:` cell, meaning its value came from a
  secret source (`${ vault(…) }`, a vault-backed `vars:` name, a secret
  `input:`, a `loop:` bind). The run's sealed set reaches the module on the
  module context;
- **by value** — the `run:` string quotes the value of a host field whose NAME
  the platform treats as sensitive (`bootstrap_token`, `password`, …): the token
  pulled out of the minting register and interpolated by hand. The seal never
  sees that one, because a register is a sealed source only when the producing
  module declared the output `secret: true`, and `core.bootstrap.issued` does
  not.

The second check compares only sensitive-NAMED fields, deliberately. A sid or a
hostname is a plausible substring of an ordinary command (`systemctl start
redis`), and under B1-strict one false positive costs the whole run.

**No token in the output, and no command output at all.** The register carries
`hosts[] = {sid, ran, skipped}` + `count` + `skipped`. stdout is not captured,
not registered and not audited — a step whose job is to write a secret can echo
it, and `accumulateKeeperRegister` writes the register row verbatim on purpose.
Capturing stdout is a separate decision with its own masking question.

**Fail-closed before the connect** (`direct` transport): the provider's
`Authorize` (a deny stops the run before a session is opened), an ephemeral
ed25519 keypair per host whose private half never leaves the Keeper, and
host-cert verification against the host CA from Vault. An empty CA set is an
error, never a blind connect — `InsecureIgnoreHostKey` is not reachable from
here.

### The `onboarded` branch

An entry flagged `onboarded: true` is a host of this run that was already up when
the minting step ran (NIM-189, NIM-780). It carries **no** `bootstrap_token` key
— absent, not empty — and no address either, since issuance knows no IPs. Such a
host is **skipped, not dialed**, reported as `{sid, ran: false, skipped: true}`
and counted in `skipped`; `count` keeps its meaning of every host in the roster.

The `primary_ip` requirement of the direct transport is settled **after** the
flag: demanding a dial address for the one host the step is about to skip is what
used to fail the step over it.

The skip lives inside the module because a scenario cannot express it: `when:` on
a keeper-side task is only half-evaluated — a static predicate works, one
reading `register.*` is silently ignored.

### Transport

`keeper.yml::push.transport`, **not** a scenario param: which way a Keeper
installation reaches its hosts is a property of the installation, and a task that
could pick one would be a task that has to know the site's topology.

- `direct` (default) — `push.Dial` by `primary_ip`, with Authorize/Sign, the
  ephemeral keypair and CA-signed host-cert verify. Requires the push dispatcher
  to be configured (`plugins.ssh_providers[]` + `push.host_ca_refs[]`); without
  it the step is refused by name rather than dialed.
- `teleport` — by name through the Teleport proxy (target = SID, never the IP).
  Authorize/Sign are not called and no Vault host CA is required: transport,
  user-auth and host-verify all come from the identity file
  (`push.teleport.{proxy_addr,identity_file,cluster}`, plus `use_system_trust` /
  `alpn_upgrade` behind an L7-TLS balancer — see
  [ADR-066](../../../adr/0066-teleport-onboarding-profile.md)).

On both, the connect is a bounded retry against `join_wait_timeout`.

### What is waited for and what is not

A machine that answers a readiness check is not a machine you can reach. The
readiness predicate is the cloud's, and the weakest form of it — a DHCP lease
carrying an address and a name, which `vmlocal` uses verbatim — is satisfied well
before sshd starts. Stronger predicates narrow the window without closing it
(the surviving cloud driver waits for the external IP to be *activated*,
[ADR-066 §(c)](../../../adr/0066-teleport-onboarding-profile.md)). So the connect
waits, and only the connect (NIM-872,
[ADR-063 amendment 2026-09-14](../../../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-14--the-bounded-wait-applies-to-direct-too-nim-872)).

| Failure | `direct` | `teleport` |
|---|---|---|
| TCP connect refused / unreachable / dial timeout | retried until `join_wait_timeout` | retried |
| Target refused by the bastion's `direct-tcpip` (`proxy_jump` set) | retried | retried |
| Bastion itself unreachable | retried | retried |
| Bastion declines on policy (`administratively prohibited`) | **fails at once** | retried |
| "node offline or does not exist" from the proxy | — | retried |
| SSH handshake rejected (host cert not from our CA, key refused) | **fails at once** | retried |
| `Authorize` deny, `Sign` failure | **fails at once** (both are upstream of the retry) | not called |
| A step exiting non-zero | **fails at once** | fails at once |

`direct` classifies by the error's shape and not by its text, in two shapes
because it has two sub-paths: our own socket, where `net.Dialer` reports every
connect failure as a `*net.OpError` with `Op == "dial"`; and — when the
SshProvider returns `proxy_jump` on its `SignReply` — the bastion's direct-tcpip
channel, where the same absent sshd is an `*ssh.OpenChannelError` with
`Reason == ConnectionFailed`. `teleport` retries everything, because the proxy
answers for an unenrolled node with an ordinary error string that carries nothing
to classify, and its transport, user-auth and host-verify all happen behind that
one call.

**Two consequences worth planning for.**

- **The signed credential must outlive the wait.** `Authorize` and `Sign` run
  once, before the retry; a provider issuing certificates shorter-lived than
  `join_wait_timeout` hands back one that expires mid-wait, and the expiry then
  surfaces as a handshake rejection — which is deliberately *not* retried, so the
  step fails immediately, minutes into the budget. Keep the provider's SSH role
  TTL above this setting.
- **A host that should already be up costs the full ceiling.** Against an
  existing machine with a decommissioned or mistyped `primary_ip`, `connection
  refused` is indistinguishable from a slow boot, so the step waits 15 minutes by
  default before saying so. Scenarios that address hosts which are supposed to be
  up already should set a short `join_wait_timeout` (seconds) rather than inherit
  the provisioning default.

A `retry:` on the task itself is a different instrument and remains legitimate:
it repeats the whole step, including commands that already ran. Use it for a
command that is flaky, not for a host that is late.

The wait is exercised against a real machine by the `libvirt`-tagged lane in
`keeper/internal/coremod/ssh/live_vmlocal_test.go` — it provisions a VM through
the `vmlocal` artifact, holds sshd down, and asserts both that the connect waits
and that the refusal really arrives as the error shape the classifier reads. It
needs a hypervisor and is outside every gate:

```
sg libvirt -c 'go test -tags libvirt -timeout 20m -v -run TestLive ./internal/coremod/ssh/'
```

### Semantics

B1-strict: a failure on any host — an Authorize deny, a connect failure, a
non-zero exit from any step — fails the whole step, so the run goes to
`error_locked` rather than committing state over a group that is only partly up.
Hosts are processed sequentially, sessions are closed on both paths.

`changed` is true when at least one host ran; a step whose every host was skipped
reports `changed: false`, because nothing happened.

Audit event `ssh.run` (`source: keeper_internal`): `{action, ssh_provider,
transport, count, skipped, steps, sids}` — counts and addressing. The command
text is **not** included: it is the site's own policy and the module cannot vouch
for what an author put in one.

### What this module cannot check for you

It refuses the argv mistake. It cannot refuse a `steps:` list that writes the
token and never redeems it, or one that does `systemctl start` without
`daemon-reload && enable` — both produce a host that never onboards, silently,
and both are in the
[ADR-063 requirements](../../../adr/0063-bootstrap-token-delivery.md#-requirements-on-whoever-installs-the-host).
The canonical description of what an installed host must end up with — paths,
permissions, `soul.yml`, the systemd unit — is
[`keeper/internal/soulinstall`](../../../../keeper/internal/soulinstall), which
describes an outcome and not a procedure.

## See also

- [docs/keeper/modules.md](../../../keeper/modules.md) — the keeper-side core catalog.
- [docs/module/core/bootstrap](../bootstrap/README.md) — the minting half of onboarding.
- [examples/service/example-cloud-bootstrap](../../../../examples/service/example-cloud-bootstrap) — the whole chain in one service.
