# `core.bootstrap`

Keeper-side onboarding of Soul agents. One public state, `core.bootstrap.issued`;
the task is routed by its module address and carries no `on:` key.

The second state, `core.bootstrap.delivered`, was **removed in NIM-834** —
[see below](#corebootstrapdelivered--removed-nim-834).

## `core.bootstrap.issued`

Issues one-time bootstrap tokens for ready-made VMs. It creates no machines —
that is a machine-provider plugin's job, and was a CloudDriver's before NIM-761
removed the contract. Input:

```yaml
- name: Issue ready-made VM tokens
  module: core.bootstrap.issued
  register: bootstrap
  params:
    sids: [redis-1.example.com, redis-2.example.com]
    reissue: false          # the default; see "Repeating the step" below
```

`sids` must be a non-empty unique list of canonical FQDN/SIDs. The entire batch
is one Postgres transaction. For each SID Keeper:

1. creates a `pending`, `transport=agent` Soul if it does not exist;
2. accepts a not-yet-onboarded `pending` or `expired` agent Soul;
3. with `reissue: true`, invalidates any previous unused token, including an
   already expired one; with `reissue: false` (the default) an **active** token
   stops the step for that host — see below;
4. issues one fresh token with the standard 24-hour TTL.

`revoked`, `destroyed`, and `transport=ssh` are refused fail-closed. A failure
names the SID and rolls back the whole batch, so no successful prefix remains
usable without an output.

### Repeating the step: `reissue` (NIM-900)

| host state | `reissue: false` (default) | `reissue: true` |
|---|---|---|
| no active token — never had one, expired, or already redeemed | issue, **changed** | issue, **changed** |
| holds an active, never-presented token | keep it, report `{sid, token_held: true}`, **unchanged** | invalidate it and issue a new one, **changed** |
| holds an identity (active seed), host of this run | pass through as `{sid, onboarded: true}`, **unchanged** | same — the flag does not reach this arm |
| holds an identity, another incarnation's | refused, `identity takeover` | same — the flag does not reach this arm |

**Active** means `used_at IS NULL AND expires_at > NOW()`. An expired unused token
is not active: nothing can redeem it ([`Burn`](../../../../keeper/internal/bootstraptoken/crud.go)
requires an unexpired row), so treating it as held would leave the host holding a
dead capability with no way to get a live one under the default.

With `reissue: false` and a held token, **nothing is written at all** — not the
token, not the Soul's `requested_at`, not the recovery window. The default is off
because the alternative is not recoverable: the plaintext of the token being
killed is unrecoverable the moment it is issued (Postgres keeps only the
SHA-256), so a step that reissues unasked destroys a capability that may already
be on the machine, and running again cannot hand the old one back.

`changed` is true exactly when the step issued or reissued at least one token. It
was the constant `true` before NIM-900, so a run over a fleet that was entirely up
reported itself as having changed something and every `onchanges:` behind it fired.

★ This is the same operation as `?force=true` on
[`POST /v1/souls/{sid}/issue-token`](../../../keeper/operator-api/souls.md#post-v1soulssidissue-token--reissue-bootstrap-token),
under a different name on purpose. The module's name follows its own output —
`reissued`, `IssuedHost.Reissued`, the `system-bootstrap-issued-reissue` marker —
and `force` would over-promise here: this module has a **second** guard the
endpoint does not, the identity arm that refuses `identity takeover`, and the flag
does not lift it. Re-registering a host that already holds an identity is
[`soul.forget`](../../../keeper/operator-api/souls.md), not a flag on issuance.

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

Ownership is the same predicate the removed `core.cloud.created` used for its own
pass-through (`keepersoul.OwnedByRun`, [ADR-063 amendment
2026-09-04](../../../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-04--issuance-converges-over-a-host-this-run-already-onboarded-nim-780)).
Issuance still never rotates the identity of an onboarded Soul.

Output in `register.bootstrap`:

```yaml
action: issued
count: 3
created: 1
reissued: 0
skipped: 1
held: 1
hosts:
  - sid: redis-1.example.com
    bootstrap_token: <one-time plaintext>
    expires_at: "2026-08-09T12:00:00Z"
    created: true
    reissued: false
  - sid: redis-2.example.com     # already up, converged over
    onboarded: true
  - sid: redis-3.example.com     # holds an active token, reissue: false
    token_held: true
```

Every requested host keeps its slot in `hosts[]`, in order: the list answers for
all of them, and a flag carries WHY one has no token. `count` stays the number of
requested SIDs; `skipped` counts the converged ones, `held` the ones whose
existing token was left alone.

⚠ **An entry in `hosts[]` has THREE possible shapes, and two of them carry no
`bootstrap_token` key at all** — it is **absent, not empty**, and reading an absent
key in CEL is an *error*:

| shape | means | keys |
|---|---|---|
| issued | a fresh token was minted for this host | `sid`, `bootstrap_token`, `expires_at`, `created`, `reissued` |
| `onboarded: true` | the host already holds an identity and needs no token (NIM-780) | `sid`, `onboarded` |
| `token_held: true` | the host holds an active token `reissue: false` left alone (NIM-900) | `sid`, `token_held` |

**Anything that re-maps `hosts` must branch on both flags before reaching for the
token.** A mapping written for the happy path fails the step on exactly the run
that was repeated to repair it. Writing an empty token in place of a flag
satisfies the letter and breaks the same thing: the consumer then treats a host
that has an identity — or a live capability — as one still waiting for one.

A `token_held` entry reaching [`core.ssh.run`](../ssh/README.md#token_held-is-the-other-tokenless-entry-and-it-is-refused)
is **refused by name, before the connect**, and that is the honest outcome rather
than a gap: there is no plaintext to hand over, and skipping the host silently
would report success over a machine that never onboards. A scenario whose repeat
path has to deliver a token wants `reissue: true`, not a branch that drops the
host.

The plaintext exists only in the current run's register so the next task can
deliver it. Postgres stores only its SHA-256 hash. `bootstrap_token` is a
sensitive key and is masked from audit, OTel, SSE and logs; it must not be copied
to `incarnation.state`. Audit event `bootstrap.issued` contains only
`{action,count,created,reissued,skipped,held,sids}`, converged and held hosts
included in `sids`.

A repeat over a SID whose previous unused token is **expired** returns fresh
plaintext under either flag value — that is the recovery path after an interrupted
or expired delivery. A repeat over a SID holding a **live** token needs
`reissue: true`, because the default is to keep what the host has.

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
  # reissue: true because the step below needs a plaintext token on every run,
  # including a repeat: a host holding one from an interrupted attempt would
  # otherwise come back as `token_held` with nothing to install with.
  params: {sids: "${ input.sids }", reissue: true}

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
