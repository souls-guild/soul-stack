# `core.bootstrap`

Keeper-side onboarding of Soul agents. One public state, `core.bootstrap.issued`;
the task is routed by its module address and carries no `on:` key.

The second state, `core.bootstrap.delivered`, was **removed in NIM-834** —
[see below](#corebootstrapdelivered--removed-nim-834).

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

A converged host keeps its slot in `hosts[]`, in order: the list answers for
every requested host, and the flag carries WHY that one has no token. `count`
stays the number of requested SIDs; `skipped` counts the converged ones.

⚠ **Anything that re-maps `hosts` must branch on `onboarded` before reaching for
the token.** On a converged entry the `bootstrap_token` key is **absent, not
empty**, and reading an absent key in CEL is an error — so a mapping written for
the happy path fails the step on exactly the run that was repeated to repair it.
Writing an empty token in place of the flag satisfies the letter and breaks the
same thing: the consumer then treats a host with an identity as one waiting for a
capability.

The plaintext exists only in the current run's register so the next task can
deliver it. Postgres stores only its SHA-256 hash. `bootstrap_token` is a
sensitive key and is masked from audit, OTel, SSE and logs; it must not be copied
to `incarnation.state`. Audit event `bootstrap.issued` contains only
`{action,count,created,reissued,skipped,sids}`, converged hosts included in
`sids`.

Every successful repeat over an eligible SID returns fresh plaintext and
invalidates the preceding unused token. This is the recovery path after an
interrupted or expired delivery.

## `core.bootstrap.delivered` — REMOVED (NIM-834)

The state that put the token on the host, redeemed it and started the unit is
gone. It did two unrelated things — install the Soul binary and hand over the
token — and installation is different at every site, so the engine no longer has
one way to do it. Installing a host is the site's own job; minting stays here,
because it is an INSERT of a pending Soul plus the token hash in one Postgres
transaction.

Three requirements moved with the work, each paid for by a live run and **none of
them reproduced by writing a file with the token in it** — full text in the
[ADR-063 amendment 2026-09-09](../../../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-09--delivered-is-removed-installing-a-host-is-site-specific-nim-834):

1. **Redeem, do not merely place.** There is no soul-side pickup of a token file;
   `soul init` is the only thing that creates a seed, and it must sit behind the
   seed-cert guard because a bootstrap token is single-use.
2. **STDIN, never argv** — argv is visible in `ps`, `audit.log` and journald on
   the host itself.
3. **`daemon-reload && enable && start`**, or the unit does not survive a reboot.

The address is refused rather than ignored, offline by soul-lint and at runtime
by the module (`unknown state "delivered"`).

Ready-made VM chain today:

```yaml
- module: core.bootstrap.issued
  register: bootstrap
  params: {sids: "${ input.sids }"}

# Install the Soul agent on each host and redeem its token — off-engine and
# site-specific since NIM-834. Until it happens the hosts have no identity, and
# the barrier below waits for presence that cannot arrive.

- module: core.soul.registered
  require: [bootstrap]
  params:
    sid: "${ register.bootstrap.hosts.map(h, h.sid) }"
    await_online: true
    refresh_soulprint: true
```
