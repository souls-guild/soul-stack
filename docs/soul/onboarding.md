# Soul onboarding: the bootstrap token and delivery

Applies to the pull mode (`transport: agent`). For the push mode (`transport: ssh`) onboarding reduces to configuring SSH access and does not use a bootstrap token — see [architecture.md → Push mode](../architecture.md).

## The bootstrap-token lifecycle

"Burning" is a two-sided operation, both sides atomic.

### On the Keeper side

On presentation of the token and the CSR — in a single Postgres transaction:

```sql
UPDATE bootstrap_tokens
   SET used_at     = now(),
       used_by_kid = $self_kid
 WHERE token_hash  = $hash_of_presented
   AND sid         = $claimed_sid
   AND used_at     IS NULL
   AND expires_at  > now()
RETURNING token_id;
```

- `UPDATE … WHERE used_at IS NULL` + a row-level lock makes the operation race-safe: two simultaneous presentations of the same token yield one success and one rejection.
- An empty `RETURNING` — the token is already burned, expired, or does not exist → `403`, `souls.status` stays `pending`.
- In the same transaction an `active` record with the signed certificate is created in `soul_seeds`. The half-state "the token is burned but the seed is not created" is impossible.
- **`souls.status` is NOT written here.** It stays `pending` — "token issued, Soul not yet connected", which is what is true: a signed certificate is one network hop short of a host holding one. The row reaches `connected` from the EventStream handshake. See [identity.md → Soul statuses and transitions](identity.md#soul-statuses-and-transitions) and [ADR-0090](../adr/0090-bootstrap-reply-loss-recovery.md) (NIM-865).
- Keeper does not store the plain token — after being issued to the operator it leaves memory. In the DB — only `token_hash` (SHA-256, hex, no salt — the token is already high-entropy by itself).

#### Recovery: a burn commits before the reply is known to have arrived

The burn above records that a certificate was **produced**. The Soul wraps dial, handshake and RPC in one deadline and writes nothing to disk until the reply lands, so a slow Vault PKI inside that deadline leaves the token spent and the host with no credential. A re-presentation is therefore admitted under **all** of:

1. the token has not expired;
2. the burn was a Soul's — not a marker written by `issue-token?force=true` or by a cascade (`used_by_kid` is a real KID);
3. `souls.last_seen_at IS NULL` — the host has never held a stream;
4. the presented CSR carries the **same public key** as the active seed already issued for that SID.

It re-signs and re-stamps that seed **in place**: the fingerprint is the SubjectPublicKeyInfo hash, so re-signing one key does not move the identity, and `soul_seeds_fingerprint_idx` is globally unique anyway.

**What stays one-shot is the binding `SID → public key`, not the presentation.** An attacker replaying a captured token wants a certificate for their own key, and condition (4) refuses that exactly as a second burn refuses it. Condition (3) is what keeps the window from becoming a hole in the other direction: once the host has appeared the credential provably arrived, so a token plaintext still sitting in `/etc/soul/token` cannot be turned against a host that is up and working.

Every refusal is the same `PermissionDenied`, undistinguished, as on the first-presentation path.

### On the Soul side

- `soul init` receives the token as a **string** — from the `--token` flag or the env `SOUL_BOOTSTRAP_TOKEN` (see [§ Onboarding flow](#onboarding-flow-agent-mode)); the binary itself **does not read or delete** any token files. One `init` process = one presentation; after a successful bootstrap the token is already burned on the Keeper side (the SQL transaction above) and is not reusable.
- **A failed `init` may be re-run, and that is the supported recovery.** The private key is written to `paths.seed/pending-key.pem` (`0400`) before the RPC and reused by the next attempt, so the retry presents the same public key and the Keeper recognizes it as the same onboarding (conditions above). The file sits beside the version directories, not in one, so `seed.Load` — which reads only through `current/` — never mistakes it for a half-written seed; it is removed once the seed is on disk. A key with no certificate authenticates nothing, which is why it may be left there at all.
- If the delivery channel put the token into a file (for example, `/etc/soul/token`, the path the removed `core.bootstrap.delivered` used, [ADR-063](../adr/0063-bootstrap-token-delivery.md)) — the file remains an artifact of the **delivery channel**, and its hygiene (`0400` permissions, cleanup) is on the channel/operator, not on the `soul` binary. The main protections are the one-time use and the short TTL of the token, not overwriting the disk.
- The token contents are **never** logged — neither by `soul` nor by Keeper (on Keeper's outputs the `bootstrap_token` key is masked by `audit.MaskSecrets`).

### Recommendations for the operator

Outside the Soul Stack area of responsibility, but it affects onboarding security:

- The token file — `mode 0400`, owner `soul:soul`, the directory `mode 0700`.
- On systemd ≥ 250 — use `LoadCredential=` in the unit file. The token lives in tmpfs, is passed into the process through an ephemeral fd, and is **never written to disk**. The best solution; everything else is a compromise.

**The Reaper** later picks up the used tokens: the rule `purge_used_tokens` with `max_age: 90d` from `used_at` — this is no longer about security, but GC. See [architecture.md → Reaper](../architecture.md).

## Onboarding flow (agent mode)

1. The operator (or a script), via Keeper's OpenAPI/MCP, registers the host and obtains a bootstrap token for a specific SID:
   - **Primary registration** — `POST /v1/souls` (permission `soul.create`, MCP tool `keeper.soul.create`, [operator-api.md](../keeper/operator-api/souls.md)). A record appears in `souls` with status `pending`, `requested_at = now()`, `created_by_aid` = the calling Archon (FK to `operators(aid)`). For `transport: agent` a bootstrap token is issued in the same operation — one-time, TTL 24h by default; the plain token is in the response once.
   - **Re-issue** — `POST /v1/souls/{sid}/issue-token` (permission `soul.issue-token`, MCP tool `keeper.soul.issue-token`). Used when a token is lost or for a planned re-issue for an already-existing Soul. Details — [§ Recovery: a lost token](#recovery-a-lost-token).
   - **Scenario batch for ready-made VMs** — `core.bootstrap.issued` takes a unique non-empty list of FQDN/SIDs, atomically creates/checks `pending`, `transport=agent` Souls and returns one fresh token per host in the current run register. It creates no machines: that is a machine-provider plugin's job, and has been since NIM-761 removed `core.cloud` with the CloudDriver contract. **A repeat is the recovery operation after an interrupted or EXPIRED delivery, and that is now the boundary** (NIM-900): a host whose previous unused token has **expired** is issued a fresh one under either value of the new `reissue` parameter — an expired token cannot be redeemed by anyone, so keeping it would strand the host. A host still holding an **active** (`used_at IS NULL AND expires_at > NOW()`) never-presented token is a different case — under the default `reissue: false` the step leaves that token alone and returns the host as `{sid, token_held: true}`, **with no `bootstrap_token`**, because the plaintext of what it holds is unrecoverable (only the SHA-256 is stored). Recovering a lost token on such a host therefore needs `reissue: true` on the step, or [`POST /v1/souls/{sid}/issue-token?force=true`](../keeper/operator-api/souls.md#post-v1soulssidissue-token--reissue-bootstrap-token) for a single host — see [§ Recovery: a lost token](#recovery-a-lost-token). A token that was already **redeemed** is a third thing again and not a case for either flag: redeeming it wrote a seed, so the host now holds an identity and takes the onboarded arm below with no token at all. (Issuance does mint over a spent token whose host no longer has an active seed — a host that was forgotten, or whose seed was revoked — but that is a host being onboarded afresh, not a delivery being recovered.) An already onboarded identity gets no token under either value, and whose host it is decides the rest (NIM-780): one of **this run** — member of the run's incarnation, or of none yet — is converged over as `{sid, onboarded: true}` and counted in `skipped`, so a `create` that failed after onboarding can be repeated to completion; one held by **another** incarnation is refused fail-closed. ★ So an entry in `hosts[]` has three shapes and two of them carry no token — a consumer that reads `bootstrap_token` unconditionally fails on exactly the repeat run. See [keeper/modules.md](../keeper/modules.md#corebootstrapissued).
2. Delivering the `soul` binary, the ready config, and the SoulSeed token to the host is the operator's task. Allowed mechanisms are in the "Ways to deliver the token" section below.
3. The operator runs `soul init [--config /etc/soul/soul.yml]` once. The command:
   - takes the token from the flag `--token=<token>` **or** from the env `SOUL_BOOTSTRAP_TOKEN` (the flag beats the env; both empty → error). **stdin is not read.** The env form is preferable: the `--token` value shows up in `ps` and shell history, the env does not;
   - takes endpoints, retry, tls from the config (`keeper.endpoints`, etc.); from the command-line arguments — only `--token`, `--config`, and `--sid` (SID override: `--sid` > `sid:` of the config > `os.Hostname` lowercased);
   - locally generates a private key (it never leaves the host) and a CSR with SID = FQDN;
   - connects to the Keeper Bootstrap listener on `endpoints[].bootstrap_port` (server-only TLS), traversing endpoints by `priority` from smaller to larger without in-group shuffle (one-shot, the order is deterministic; spray/shuffle exist only in the EventStream phase — see [connection.md → Two phases, two ports](connection.md)), and presents the token + CSR;
   - having received the signed certificate, atomically lays out the SoulSeed into `paths.seed` and terminates.

   If a SoulSeed already lies on disk — `init` fails with an error, so as not to accidentally re-issue (for a re-issue there is a separate procedure, see [identity.md → SoulSeed rotation](identity.md)).
4. Keeper, on presentation of the token and the CSR:
   - checks that the token is valid, not expired, not used, and that the SID matches;
   - issues a SoulSeed (an mTLS certificate and key via Vault PKI / the built-in CA — the concrete implementation is fixed in ADR-006 / the Vault section);
   - returns it to Soul;
   - marks the token used (the SQL transaction above);
   - moves the `souls` record to `connected`, fills in the seed fields.
5. Then — the ordinary launch of the `soul` daemon (via a systemd unit, etc.); it holds the stream by the algorithm from [connection.md](connection.md).

## Recovery: a lost token

Keeper stores the plain bootstrap token only until it is issued to the operator — only `token_hash` remains in the DB ([§ On the Keeper side](#on-the-keeper-side)). A lost token cannot be recovered, only a new one issued.

The recovery flow for an existing Soul (`transport: agent`) that has not yet passed onboarding (`status: pending`):

1. An operator with permission `soul.issue-token` calls `POST /v1/souls/{sid}/issue-token` (the CLI/wrapper — `--force` when there is an active token; MCP — `force: true`).
2. The invariant `UNIQUE (sid) WHERE used_at IS NULL` holds — at most one active token per Soul:
   - **without `force`** with an already-active token → `409 bootstrap-token-active` (protection against proliferating valid tokens);
   - **with `force=true`** → the old active token is marked used (`used_at = now()` — frees the partial-unique slot `WHERE used_at IS NULL`), a new one is issued.
3. The new plain token is delivered to the host in the ordinary way ([§ Ways to deliver the token](#ways-to-deliver-the-token)), `soul init` is repeated.

For a Soul with `transport: ssh`, `issue-token` returns `422 validation-failed` — ssh onboarding does not use a bootstrap token ([architecture.md → Push mode](../architecture.md)).

## Ways to deliver the token

"The operator generated a bootstrap token → the token ended up in a file on the VM where `soul` will launch" — different paths are allowed for this. Some of them are **inside Soul Stack** (via `keeper.push`), some are **outside the area of responsibility** (the operator's external tools). Soul Stack accepts all variants, because issuing a token and accepting it are API/MCP operations; the method of physical delivery is the operator's choice.

- **Via the scenario step `core.bootstrap.issued` plus a site-specific installer (inside Soul Stack, [ADR-063](../adr/0063-bootstrap-token-delivery.md)).** `issued` mints per-host tokens for ready-made VMs, in one Postgres transaction, into the run register — and on a repeat it mints only for the hosts that need it, so a step that must deliver a token on **every** run declares `reissue: true` (NIM-900). Getting each token onto its host and redeeming it is **no longer an engine step** — `core.bootstrap.delivered` was removed in NIM-834 because installation differs at every site. Whatever does it must satisfy three requirements, none of which is met by writing a file with the token in it: redeem with guarded `soul init` (there is no soul-side pickup of a token file, and the token is single-use), pass the token through STDIN and never argv, and activate the unit with `daemon-reload && enable && start`. Specifications: [issued](../keeper/modules.md#corebootstrapissued), [the requirements on the installer](../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-09--delivered-is-removed-installing-a-host-is-site-specific-nim-834).
- **A configuration-management role.** The recommended official role will live in a separate repository; it accepts the token and the Keeper address as variables. Good for those who standardize on a configuration-management tool.
- **Ordinary SSH/SCP** — the operator puts the token and the `soul` binary in place manually or with their own script.
- **CI/CD pipelines** — the token is taken from the CI secret store, delivered via a terraform provisioner or a bootstrap script.
- **Cloud-init / image baking** — for ephemeral VMs, where the token is injected at the instance-creation stage.

## Protections on the Soul Stack side

- **Token TTL** — short by default (24h), configurable by the operator.
- **One-time binding** — the first successful CSR burns the token and fixes the keypair it may ever issue for. A later presentation is admitted only as a retry of that same binding by the same key, on a host that has never connected ([§ Recovery](#recovery-a-burn-commits-before-the-reply-is-known-to-have-arrived)); everything else, an attacker's own CSR included, is refused as it always was.
- **Binding to a specific SID** — the token is valid only for the FQDN it was issued for.
- **Audit** — every issue and use of a token is logged in Keeper.

Additional protections (binding to an IP/CIDR, requiring cloud-metadata proof, manual approval) — see the open question "SoulSeed token leakage" in [architecture.md → Open questions](../architecture.md).

## See also

- [identity.md](identity.md) — the `bootstrap_tokens` registry, the `soul_seeds` registry, Soul statuses.
- [connection.md](connection.md) — the algorithm by which `soul init` connects to Keeper.
- [config.md](config.md) — where `paths.seed` and `tls.ca` are on the host.
- [architecture.md → Soul lifecycle and the soul registry](../architecture.md) — the architectural overview of onboarding and the registries.
- [architecture.md → Delivering the SoulSeed token to the host](../architecture.md) — a short enumeration of delivery methods.
