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
2. accepts a not-yet-onboarded `pending` or `expired` agent Soul;
3. invalidates any previous unused token, including an already expired token;
4. issues one fresh token with the standard 24-hour TTL.

`revoked`, `destroyed`, and `transport=ssh` are refused fail-closed. A failure
names the SID and rolls back the whole batch, so no successful prefix remains
usable without an output.

### An already onboarded host (NIM-780)

`connected` and `disconnected` both mean the host already owns an identity, so no
token is issued for it either way. What differs is whose host it is:

- **This run's own** — a member of the run's incarnation, or of no incarnation
  yet — is **converged over**: passed through untouched, with no token and no
  write, reported as `{sid, onboarded: true}`. This is the repair path for a
  `create` that succeeded at onboarding and failed later (rollout, cluster
  assembly): the cloud plugin idempotently returns the same machines and the same
  SIDs, and issuance must not stand in the way of finishing the run.
- **Another incarnation's** is an identity takeover and is refused fail-closed,
  rolling the whole batch back. An unknown incarnation — a call made outside a
  scenario run — means *unknown*, never *no owner*, and can claim only unbound
  rows.

Ownership is the same predicate `core.cloud.created` used for its own
pass-through (`keepersoul.OwnedByRun`, [ADR-063 amendment
2026-09-04](../../../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-04--issuance-converges-over-a-host-this-run-already-onboarded-nim-780)).
Issuance still never rotates the identity of an onboarded Soul.

Output in `register.bootstrap`:

```yaml
action: issued
count: 2
created: 1
reissued: 0
skipped: 1
hosts:
  - sid: redis-1.example.com
    bootstrap_token: <one-time plaintext>
    expires_at: "2026-08-09T12:00:00Z"
    created: true
    reissued: false
  - sid: redis-2.example.com     # already up, converged over
    onboarded: true
```

A converged host keeps its slot in `hosts[]`: `core.bootstrap.delivered` skips
exactly that entry shape, and it refuses an *empty* list — so dropping the entry
would move the dead end to delivery instead of removing it. `count` stays the
number of requested SIDs; `skipped` counts the converged ones. The entry carries
no `primary_ip` and needs none on either transport — delivery settles the
`onboarded` flag before the `direct`-transport address requirement, because a
skipped host is never dialed.

⚠ **A scenario that re-maps `hosts` between the two steps must carry `onboarded`
through.** Rebuilt as `{sid, bootstrap_token, primary_ip}` alone, a converged
entry reaches delivery as a host with no token, which delivery is right to reject.

The plaintext exists only in the current run's register so the next task can
deliver it. Postgres stores only its SHA-256 hash. `bootstrap_token` is a
sensitive key and is masked from audit, OTel, SSE and logs; it must not be copied
to `incarnation.state`. Audit event `bootstrap.issued` contains only
`{action,count,created,reissued,skipped,sids}`, converged hosts included in
`sids`.

Every successful repeat over an eligible SID returns fresh plaintext and
invalidates the preceding unused token. This is the recovery path after an
interrupted or expired delivery.

## `core.bootstrap.delivered`

Consumes `hosts` from either `core.bootstrap.issued` or `core.cloud.created`,
puts each token on its host through stdin, runs guarded `soul init`, and
optionally starts `soul.service`. With `install: true` it first installs the full
Soul setup; full-install is available with Teleport transport.

Teleport addresses a host by `sid`, so `hosts[].primary_ip` is optional there.
Direct transport dials by IP and still requires a non-empty `primary_ip` — for
every host it actually dials. A host flagged `onboarded: true` is not one of
them and needs no address on either transport (see above). See [keeper module reference](../../../keeper/modules.md#corebootstrapdelivered)
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
