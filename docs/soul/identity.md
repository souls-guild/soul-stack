# Soul identity and registries

An architectural overview and the relation to other sections are in [architecture.md → Soul lifecycle and the soul registry](../architecture.md). Here is the assembled picture from the Soul's point of view: "who am I in Keeper's DB and by which fields am I described there".

## Identity

- **SID — Soul ID = the host FQDN.** Not a UUID. This automatically gives dedup on reinstalling the agent onto the same host, at the cost of a FQDN rename = a migration (see "Host rename" below).
- **KID — Keeper ID.** The stable identifier of a Keeper instance. It appears in the fields `last_seen_by_kid`, `used_by_kid`, `issued_by_kid`.
- **SoulSeed — the single Soul identity artifact.** A pair (mTLS certificate + private key) with which Soul authenticates when connecting to Keeper. On first launch Soul requests "give me a seed" from Keeper, receives a SoulSeed, and lays it out on the host. Afterwards — regular rotations (once a week): over the live stream Soul asks for a new SoulSeed, Keeper issues it, the old one is marked `superseded`.
- **Coven — a Soul group label.** An arbitrary label/tag for logically grouping Souls (by DC, by role, by environment). Used in RBAC, Destiny targeting, and potentially in load-balancer routing. Real routing by Coven is an open question (`LB-1`).

The SoulSeed private key **never leaves the host**. It is generated locally during [onboarding](onboarding.md), used to sign the CSR, and afterwards lives in `paths.seed` next to the signed certificate. Keeper's DB stores only the fingerprint, not the PEM and not the key.

## The souls registry

A table in Postgres with the following fields (simplified schema, without operational indexes):

| field | type | meaning |
|---|---|---|
| `sid` | text PK | host FQDN |
| `transport` | enum | `agent` \| `ssh` — how Keeper delivers commands |
| `status` | enum | `pending` \| `connected` \| `disconnected` \| `revoked` \| `expired` \| `destroyed` (see below) |
| `coven` | text\[\] | group labels |
| `registered_at` | timestamptz | when the soul was first accepted |
| `last_seen_at` | timestamptz | time of the last successful check (the actual value is in Redis, in PG — a flush) |
| `last_seen_by_kid` | text | which Keeper last held the stream |
| `created_by_aid` | text FK → `operators(aid)` | who created the soul |
| `requested_at` | timestamptz | when the operator issued the SoulSeed token |
| `note` | text | a free operator field |

## The bootstrap_tokens registry

A separate table: a Soul can have **one active (unused) token** at a time. After use the record remains in the table for audit, cleaned up by the Reaper (see [architecture.md → Reaper](../architecture.md)).

For push hosts (`transport: ssh`) records in `bootstrap_tokens` are **not created** — they have no bootstrap phase, symmetrically with `soul_seeds` (see below).

| field | type | meaning |
|---|---|---|
| `token_id` | UUID PK | the record's primary key |
| `sid` | text FK → `souls.sid` | which Soul it was issued for |
| `token_hash` | text | SHA-256 of the plain token in hex; **the plain token itself is not stored in the DB** |
| `created_at` | timestamptz | when issued |
| `expires_at` | timestamptz | TTL, by default `created_at + 24h` |
| `used_at` | timestamptz NULL | when burned (NULL = still active) |
| `used_by_kid` | text NULL | which Keeper accepted the presentation |
| `created_by_aid` | text FK → `operators(aid)` | who issued it |

**Invariant:** `UNIQUE (sid) WHERE used_at IS NULL` — a Soul can have only one unused token at a time.

**Cascade on cloud-destroy ([ADR-017](../adr/0017-keeper-side-core.md)):** if a SID is deleted via `core.cloud.provisioned destroyed`, the not-yet-used tokens of this SID are marked `used_at = NOW()`, `used_by_kid = 'system-cloud-destroy'` (a special marker, **not** a real KID and **not** an AID; it protects the anti-replay invariant).

The lifecycle (issue → delivery → CSR → burn) and the presentation SQL transaction are in [onboarding.md](onboarding.md).

## The soul_seeds registry

A separate table: one SID — many seeds (the rotation history). One active at a time.

| field | type | meaning |
|---|---|---|
| `seed_id` | UUID PK | primary key |
| `sid` | text FK → `souls.sid` | owner |
| `fingerprint` | text | SHA-256 of the certificate's public key, hex (no HMAC, no salt) |
| `serial_number` | text | the certificate serial |
| `issued_at` | timestamptz | when issued |
| `expires_at` | timestamptz | when it expires |
| `issued_by_kid` | text | which Keeper issued it |
| `status` | enum | `active` \| `superseded` \| `expired` \| `revoked` \| `orphaned` (see below) |
| `revocation_reason` | text NULL | if revoked — why |

**Invariants:**
- `UNIQUE (sid) WHERE status='active'` — exactly one active seed per SID.
- The DB **does not store** PEMs, private keys, or a separate public key — only the fingerprint. The main protection is the CA private key in Vault.

**Statuses:**
- `active` — the current issued certificate, exactly one per SID.
- `superseded` — replaced by a rotation, a new seed is already active.
- `expired` — moved by the Reaper / Vault PKI after `not_after`.
- `revoked` — the operator revoked it (security incident, compromise). Audit semantics: "the operator made a decision".
- `orphaned` — the host was cascade-deleted from `core.cloud.provisioned destroyed` ([ADR-017](../adr/0017-keeper-side-core.md)). Audit semantics: "the VM lifecycle ended". The cascade applies only to `active` seeds; `revoked` is NOT overwritten (precedence revoked > orphaned).

For push hosts (`transport: ssh`) `soul_seeds` is **not used** — they have no mTLS identity, see [concept.md → Two transports](concept.md).

## Soul statuses and transitions

- **`pending`** — the operator issued a SoulSeed token for this SID, Soul has not arrived yet.
- **`connected`** — a legacy lifecycle snapshot "last known: the stream was alive". **NOT the source of presence** (see below): online/offline is decided by the Redis SID lease, not this status.
- **`disconnected`** — a legacy lifecycle snapshot "last known: the stream was closed/lost". Soul may return; presence (online) will then be restored via lease capture, regardless of whether the Reaper managed to reconcile the snapshot back to `connected`.
- **`revoked`** — the operator revoked it. The certificate in `soul_seeds` is marked `revoked`, new connections from this SID are rejected at the TLS level.
- **`expired`** — the Reaper moved `pending` after the bootstrap-token TTL (Soul never arrived).
- **`destroyed`** — a terminal state ([ADR-017](../adr/0017-keeper-side-core.md) cascade): the host was physically deleted via `core.cloud.provisioned destroyed`. There are no outgoing transitions — the record remains as a forensic object and **is not part of** the default set `purge_souls.statuses` ([keeper/reaper.md](../keeper/reaper.md)). The operator may delete it manually.

### Presence (online/offline) = the Redis SID lease, not souls.status

**The authority for "Soul online" is a live Redis SID lease** `soul:<sid>:lock` (the value = the `kid` of the owning Keeper instance, [ADR-006(a)/(b)](../adr/0006-cache-redis.md)). The lease is captured on EventStream session-open (after handshake) and renewed by a renewal goroutine while the stream is alive; it goes out on a normal Release (teardown) or by TTL after an instance crash. **There is NO synchronous write of presence into `souls.status` on connect/disconnect** — at the 100k VM target this would be a hot path of PG writes; presence is held in the hot Redis layer.

**The run's target resolver derives online from the lease**, not from `souls.status`: in two phases — (1) SQL candidates by Coven membership + status NOT terminal/onboarding (`pending`/`revoked`/`expired`/`destroyed` excluded), (2) filtering out candidates without a live SID lease (batch EXISTS). So a Soul that reconnected after a Keeper restart is visible to the resolver immediately upon lease capture — even if the `souls.status` snapshot is still `disconnected`. An idle Soul (sends only soulprint once per `refresh_interval`, no app messages in the window) remains online as long as the renewal holds the lease. Without a configured Redis (single-instance dev / unit) the resolver degrades to the SQL snapshot (`status='connected'`).

**What writes `souls.status`** (a lifecycle snapshot for the Operator API's "last known", NOT presence):

- **Bootstrap-RPC** (onboarding): `pending` → `connected`, records `last_seen_by_kid`. One write at onboarding, not a hot path. A reconnect of an already-onboarded Soul does **not** touch Bootstrap-RPC (onboarding is already done) — the snapshot back to `connected` is moved by the Reaper reconcile (below).
- **Reaper `mark_disconnected`** ([keeper/reaper.md](../keeper/reaper.md)) — a **lazy reconciliation of the snapshot IN BOTH DIRECTIONS**: it marks `connected` → `disconnected` on a stale `last_seen_at` with a dead SID lease (lease-aware, it does not touch an idle Soul on a live lease) **and** `disconnected` → `connected` on a live SID lease (Soul is really online — a reconnect captured the lease, but the PG snapshot stayed `disconnected`). This is a background bringing of the PG snapshot to the fact, not a source of presence — the run does not depend on the snapshot. An ordinary reconnect does **not move the snapshot directly** (eventstream presence is not written to PG on the hot path); it is moved precisely by the Reaper reconcile based on the fact of a live lease. Without the reverse direction the snapshot would latch into `disconnected` forever after the first "drop+sweep".

`last_seen_at` is a separate snapshot of "when it was last seen" (a throttled flush from the stream into PG, the real-time value in the Redis heartbeat), needed by the Operator API and the Reaper; it is also **not** a presence predicate.

The corresponding cascade in the same PG transaction:

- `souls.status` → `destroyed`;
- active `soul_seeds` of the SID → `orphaned` (`active` → `orphaned`; `revoked` is NOT overwritten — precedence revoked > orphaned);
- not-yet-used `bootstrap_tokens` of the SID → `used_at = NOW()`, `used_by_kid = 'system-cloud-destroy'` (anti-replay invariant: the token is "settled at the moment of cloud-destroy", even if it was never presented).

The diagram shows the transitions of the **lifecycle snapshot `souls.status`** (for the Operator API),
NOT presence: online/offline is decided by the Redis SID lease separately (see above).

```
   operator request               first successful connection
   for a SoulSeed token           (handshake + match fingerprint)
       │                                  │
       ▼                                  ▼
   ┌─────────┐  TTL 24h        ┌─────────────────┐  Reaper: stale   ┌──────────────┐
   │ pending │ ── Reaper ──►   │   connected     │  + no lease      │ disconnected │
   └─────────┘   expired       └─────────────────┘ ──────────────►  └──────┬───────┘
        │                              ▲          (lazy reconcile →)        │
        │            Reaper reconcile (live lease) / Bootstrap-RPC          │
        │                              └────────────────────────────────────┘
        │                                  (← lazy reconcile)
        │  TTL 24h with                                   Reaper, max_age 30d
        │  no use                                         (if it never
        ▼                                                 came back)
   record deleted                                         record deleted
   by Reaper                                              by Reaper
                                                                │
                        operator revoked ──────► ┌─────────┐
                                                 │ revoked │
                                                 └─────────┘
                                                                │
             cloud-destroy (ADR-017) ──────────► ┌───────────┐
                                                 │ destroyed │ (terminal,
                                                 └───────────┘  not in default purge_souls)
```

## On-disk format of paths.seed (normative)

`paths.seed` is a **directory** (mode `0700`) with a versioned layout. The active version is selected via the `current` symlink; a rotation switches it atomically, so that the disk never holds a desynchronized `cert↔key` pair.

```
paths.seed/
  current -> v3          # RELATIVE symlink to the active version
  v2/                    # the previous version (kept as a rollback safety net)
    cert.pem  key.pem  ca.pem
  v3/                    # the active version
    cert.pem  (0644)
    key.pem   (0400)
    ca.pem    (0644)
```

- **Version directories** — `vN/` (mode `0700`), monotonically increasing numbering (`v1`, `v2`, …); a new version = `max(existing) + 1`.
- **Version files** — fixed names `cert.pem` (`0644`) / `key.pem` (`0400`) / `ca.pem` (`0644`).
- **`current`** — a relative symlink (`current -> v3`, not an absolute path), transparent to open(2): the tls config reads material through `current/{cert,key,ca}.pem`, so a version swap changes the source without reinitializing the paths.
- **Hard-cut (M1):** the old flat format (`cert.pem`/`key.pem`/`ca.pem` directly in `paths.seed` without `current`) is **not supported**. There is no auto-migration: if the directory holds the flat format without `current` — Soul considers the bootstrap not performed (the operator runs `soul init` again).

### Writing a new version and the atomic swap

`soul init` (the primary bootstrap) and rotation use one write path:

1. **Validate the `cert↔key` pair** via X509 — fail-fast **before** any write to disk. An inconsistent pair is not written to disk; the error text does not contain the private key.
2. **Write the whole version** into `vN+1/`: three files atomically (temp + chmod + fsync + rename), then **fsync the version directory** — without it the file renames may not reach the disk before a crash, and the version would turn out incomplete.
3. **Atomic swap**: a temp symlink `-> vN+1` next to it, `rename(2)` over `current` (atomic on POSIX), then **fsync the `paths.seed` directory**.
4. **Cleanup** (best-effort, after a successful swap): the active version and one previous are kept; versions older than that are deleted. A cleanup error does not fail the write.

Until step 3 `current` points to the previous version — **a failure at steps 1–2 leaves a valid previous active version** (crash-safety): the new version is written next to it and becomes active by a single atomic symlink switch.

### Reading

Reading goes through `current/{cert,key,ca}.pem`. After reading, the `cert↔key` pair is checked for consistency via X509:

- no `current` (or one of the three files is missing in the active version) → `ErrIncomplete` ("bootstrap not performed", hint `run soul init`);
- the `cert↔key` pair is desynchronized (for example, the version was partially overwritten bypassing the atomic swap) → `ErrMismatched` (differs from `ErrIncomplete`: "material is present, but does not form a valid pair").

## SoulSeed rotation

- The default period is **once a week**.
- Over the live stream Soul asks Keeper for a new SoulSeed in advance (at `expires_at - 24h`).
- Keeper issues a new one and returns it to Soul. Soul deploys it on the host by the scheme [On-disk format → writing and swap](#writing-a-new-version-and-the-atomic-swap) (a new version `vN+1/` next to it, then an atomic swap of the `current` symlink), and switches to the new certificate.
- A new `active` row is created in `soul_seeds`; the previous one is marked `superseded`.
- The Reaper later picks up the old `superseded` / `expired` records.

Rotation happens exclusively over the live stream; no separate re-bootstrap flow via a token is required. If the stream was dropped and Soul did not arrive for a long time — after returning it first connects on the old seed, then initiates a rotation.

## Revocation (revoke)

- An operator operation via API/MCP. It changes `souls.status = 'revoked'` and `soul_seeds.status = 'revoked'` for all active/live seeds of the SID.
- At the TLS level Keeper refuses the connection on the next handshake (via CRL or a direct fingerprint match in the DB with a status filter).

## Host rename

In-place SID rename is not supported. If the host's FQDN changed — this is a **new Soul**: the operator creates a new SoulSeed token for the new SID and installs Soul onto the host anew; the old record is `revoked` or picked up by the Reaper. The decision was made deliberately: SID = FQDN, a SID migration = a new installation.

## See also

- [onboarding.md](onboarding.md) — the bootstrap-token lifecycle, the presentation SQL transaction, delivery.
- [connection.md](connection.md) — how a Soul with an already-issued SoulSeed connects to Keeper.
- [config.md](config.md) — where `paths.seed` and `tls.ca` live on the host.
- [architecture.md → Soul lifecycle and the soul registry](../architecture.md) — the architectural overview.
- [architecture.md → Reaper](../architecture.md) — the registry GC rules.
- [naming-rules.md](../naming-rules.md) — the vocabulary of names (SID, KID, SoulSeed, Coven).
