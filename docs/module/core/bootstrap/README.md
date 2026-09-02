# `core.bootstrap`

Keeper-side onboarding of Soul agents. The public states are
`core.bootstrap.issued` and `core.bootstrap.delivered`; both tasks require
`on: keeper`.

## `core.bootstrap.issued`

Issues one-time bootstrap tokens for ready-made VMs without invoking a
CloudDriver. Input:

```yaml
- name: Issue ready-made VM tokens
  module: core.bootstrap.issued
  register: bootstrap
  params:
    sids: [redis-1.example.com, redis-2.example.com]
```

`sids` must be a non-empty unique list of canonical FQDN/SIDs. The entire batch
is one Postgres transaction. For each SID Keeper:

1. creates a `pending`, `transport=agent` Soul if it does not exist;
2. accepts only a not-yet-onboarded `pending` or `expired` agent Soul;
3. invalidates any previous unused token, including an already expired token;
4. issues one fresh token with the standard 24-hour TTL.

`connected` and `disconnected` both mean that the host already owns an identity
and are refused fail-closed. `revoked`, `destroyed`, and `transport=ssh` are also
refused. A failure names the SID and rolls back the whole batch, so no successful
prefix remains usable without an output.

Output in `register.bootstrap`:

```yaml
action: issued
count: 2
created: 2
reissued: 0
hosts:
  - sid: redis-1.example.com
    bootstrap_token: <one-time plaintext>
    expires_at: "2026-08-09T12:00:00Z"
    created: true
    reissued: false
```

The plaintext exists only in the current run's register so the next task can
deliver it. Postgres stores only its SHA-256 hash. `bootstrap_token` is a
sensitive key and is masked from audit, OTel, SSE and logs; it must not be copied
to `incarnation.state`. Audit event `bootstrap.issued` contains only
`{action,count,created,reissued,sids}`.

Every successful repeat returns fresh plaintext and invalidates the preceding
unused token. This is the recovery path after an interrupted or expired delivery.
It never rotates the identity of an onboarded Soul.

## `core.bootstrap.delivered`

Consumes `hosts` from either `core.bootstrap.issued` or `core.cloud.created`,
puts each token on its host through stdin, runs guarded `soul init`, and
optionally starts `soul.service`. With `install: true` it first installs the full
Soul setup; full-install is available with Teleport transport.

Teleport addresses a host by `sid`, so `hosts[].primary_ip` is optional there.
Direct transport dials by IP and still requires a non-empty `primary_ip` for
every host. See [keeper module reference](../../../keeper/modules.md#corebootstrapdelivered)
and [ADR-063](../../../adr/0063-bootstrap-token-delivery.md) for the full delivery
parameters and secret-transfer invariants.

Typical ready-made VM chain:

```yaml
- on: keeper
  module: core.bootstrap.issued
  register: bootstrap
  params: {sids: "${ input.sids }"}

- on: keeper
  module: core.bootstrap.delivered
  require: [bootstrap]
  params:
    hosts: "${ register.bootstrap.hosts }"
    ssh_provider: teleport-ready-vm
    install: true

- on: keeper
  module: core.soul.registered
  params:
    sid: "${ register.bootstrap.hosts.map(h, h.sid) }"
    await_online: true
    refresh_soulprint: true
```
