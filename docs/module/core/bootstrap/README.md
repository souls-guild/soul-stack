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
| holds an identity (active seed), host of this run, **its agent is connected now** | pass through as `{sid, onboarded: true}`, **unchanged** | same — the flag does not reach this arm |
| holds an identity, host of this run, **no agent connected** | refused, `no agent is connected with it` (NIM-886) | same — the flag does not reach this arm |
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

### An already onboarded host (NIM-780, NIM-886)

A host is taken to hold an identity when its status is `connected` or
`disconnected`, or when it is `pending` or `expired` with an **active seed** — the
state between Bootstrap and its first stream (NIM-865). `revoked` and `destroyed`
are refused before any of this, whatever seed is on file. No token is ever issued
for a host holding an identity. What happens instead depends on two questions,
asked in this order:

1. **Whose host is it?** Another incarnation's is an identity takeover and is
   refused fail-closed, rolling the whole batch back. An unknown incarnation — a
   call made outside a scenario run — means *unknown*, never *no owner*, and can
   claim only unbound rows. This run's own — a member of the run's incarnation,
   or of no incarnation yet — goes on to the second question.
2. **Is an agent connected with that identity right now?** Yes: the host is
   **converged over** — passed through untouched, with no token and no write,
   reported as `{sid, onboarded: true}`. No: the step is **refused** (below).

Converging is the repair path for a `create` that succeeded at onboarding and
failed later (rollout, cluster assembly): the machine provider idempotently returns
the same machines and the same SIDs, their agents are up, and issuance must not
stand in the way of finishing the run.

Ownership is the same predicate the removed `core.cloud.created` used for its own
pass-through (`keepersoul.OwnedByRun`, [ADR-063 amendment
2026-09-04](../../../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-04--issuance-converges-over-a-host-this-run-already-onboarded-nim-780)).
Issuance still never rotates the identity of an onboarded Soul.

### An identity with no agent behind it is refused (NIM-886)

The registry cannot tell these machines apart: one whose agent is stopped or
cannot reach the Keeper, and one that was **re-created under the same SID** — a
ready-made host reinstalled under its old FQDN, a provider that reuses names. All
are a row holding an identity with no stream. Passing the re-created one through
as `onboarded` used to make the install step skip it and report success, and the
run then failed at the onboarding barrier after the whole `await_timeout`, blaming
the last presence poll. Only a live stream proves the identity is still held, so
without one the step stops at once:

```
bootstrap issued: 2 host(s) hold an identity in the registry but no agent is connected
with it: "redis-a-s1-k3f9q.example.com" (status disconnected, last stream on record
2026-09-12T10:01:22Z), "redis-a-s1-z7c1d.example.com" (status pending, no stream on
record). If a machine was re-created under the same SID it cannot use that identity:
forget the record (DELETE /v1/souls/{sid}) and repeat the run. If it is the same
machine, bring its agent up and repeat — forgetting a host whose disk still holds its
seed leaves it unable to onboard
```

**Which way out is the operator's call**, because only the operator knows which
machine is standing there, and the wrong one costs:

| the machine behind the SID | what to do |
|---|---|
| re-created — its disk holds no seed | [`DELETE /v1/souls/{sid}`](../../../keeper/operator-api/souls.md) (`soul.forget`), then repeat: the SID is minted for like a new one |
| the same machine, agent stopped or unable to reach the Keeper | bring the agent up (start the unit, fix its reach to the EventStream port), then repeat |
| the same machine, `no stream on record` because its Bootstrap reply was lost (it holds no certificate) | run `soul init` on it again with the token it was given — the reply-loss re-presentation of [ADR-0090](../../../adr/0090-bootstrap-reply-loss-recovery.md), within that token's TTL; the plaintext is on the host only if the install kept it (the example installs write `/etc/soul/token`). Past the TTL or without it, [`POST /v1/souls/{sid}/issue-token`](../../../keeper/operator-api/souls.md#post-v1soulssidissue-token--reissue-bootstrap-token) mints a fresh one that the key `soul init` kept redeems in place, with no membership lost. Then bring the agent up — the install stopped at the failed `soul init`, before its unit was started — and repeat |

⚠ **Never forget a host whose disk still holds its seed.** The seed guard every
install carries ([ADR-063 amendment 2026-09-09](../../../adr/0063-bootstrap-token-delivery.md#-requirements-on-whoever-installs-the-host),
`test -e …/seed/current/cert.pem || soul init`) then skips `soul init`,
the agent presents a seed the Keeper no longer has, and the host cannot onboard
until its seed directory is cleared, or `soul init` is run on it by hand. Forgetting also takes the host's
incarnation memberships and Choir Voices with it: a repeated `create` binds the
membership again through `core.soul.registered`, but a declared role returns only
if that scenario runs `core.choir.present`.

Issuance never retires an identity itself. Today that is an operator act
(`soul.forget`), or the Reaper's opt-in `purge_souls` once a `disconnected` row is
older than its `max_age`.

- **Every such host that reaches the check is named at once**, in batch order, so a
  group re-created under its old names is repaired in one pass. A host refused
  for another reason first — an identity takeover, `revoked`, `destroyed`,
  `transport=ssh` — stops the batch before the check, and is what the step names.
- **The whole batch rolls back**, like every other refusal: no host of it — not
  even a fresh one — got a token.
- **"Connected" is the presence lease** in Redis, the source the onboarding barrier
  polls, not `souls.status`. It is read once per batch and only when the batch holds
  an identity at all. ⚠ The lease can outlive a machine killed without closing its
  connection by a few minutes — the Keeper notices the dead stream only when TCP
  does (NIM-962) — so a machine re-created under the
  same SID inside that window is still passed through. A lease that cannot be read within 5 seconds refuses the step
  (`check agent presence: …`) rather than guess, and so does a Keeper with no
  presence source.
- **The cost:** a repeat run that lands while a host's agent is restarting is
  refused, even though the agent would be back in seconds; before NIM-886 that host
  was waited for at the barrier until its agent returned. So is one that lands
  between a host's Bootstrap and its first stream. Repeat the run.

### How a repeated run behaves

One incarnation's life, three machines, and what the steps of the ready-made
chain — issue → install → [`core.soul.registered`](../soul/README.md) barrier — do
each time:

| situation | what happens |
|---|---|
| first run | no records yet: three tokens, three installs, three agents online |
| the run failed after onboarding (rollout, cluster assembly) and is repeated | the same three SIDs, agents connected: passed through as `onboarded`, the install skips them, the barrier passes at the first poll, the run goes on |
| the group grows from three to five | three pass through as above; the two new SIDs get tokens and installs |
| a machine was lost and the provider replaces it under a **new** name (`wbcloud` draws a fresh one since NIM-894) | a new SID with no record: token, install, online. The lost machine's record stays behind; targeting skips it for want of a lease, and it goes only by `soul.forget` or, where enabled, the Reaper's `purge_souls` |
| a host's agent is stopped and the run is repeated | **refused at issuance**, naming the host — bring the agent up, repeat |
| a machine is re-created under its **old** SID (ready-made host reinstalled, a provider that reuses names) | **refused at issuance**, naming the host — `DELETE /v1/souls/{sid}`, repeat: token, install, online |
| tokens were issued but an install never brought its agent up | the barrier fails after `await_timeout` — or when the run ends first — and says per host what the registry holds: [no stream on record, a last stream before the wait, or one during it](../soul/README.md#onboarding-barrier-await_online). An install step that itself fails (`soul init` refused) fails the run there instead, naming the host |
| …and that run is repeated | a host whose Bootstrap never completed holds no identity and is minted for again (`reissue: true`); one whose Bootstrap completed — certificate delivered, or the reply lost — is **refused at issuance**, `no stream on record`, and the table above says which way out |

The first four rows are unchanged by NIM-886. Every red outcome after them says
what it is.

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
| `onboarded: true` | the host already holds an identity, its agent is connected, and it needs no token (NIM-780, NIM-886) | `sid`, `onboarded` |
| `token_held: true` | the host holds an active token `reissue: false` left alone (NIM-900) | `sid`, `token_held` |

**Anything that re-maps `hosts` must branch on both flags before reaching for the
token.** A mapping written for the happy path fails the step on exactly the run
that was repeated to repair it. Writing an empty token in place of a flag
satisfies the letter and breaks the same thing: the consumer then treats a host
that has an identity — or a live capability — as one still waiting for one.

A `token_held` entry reaching [`core.ssh.run`](../ssh/README.md#token_held-is-the-other-tokenless-entry-and-it-is-refused) or [`core.ssh.apply`](../ssh/README.md#coresshapply)
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
