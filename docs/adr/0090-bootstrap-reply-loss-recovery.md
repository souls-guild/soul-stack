# ADR-0090. A lost Bootstrap reply: what anti-replay actually protects, and when a host counts as onboarded.

**Status:** accepted. Option 2 chosen by the owner 2026-09-19 (NIM-865) over Option 1. Amends [ADR-012(b)](0012-keeper-soul-grpc.md) (the Bootstrap RPC's one-shot semantics) and the `souls.status` enum documented in [`../soul/identity.md`](../soul/identity.md); interacts with [ADR-0063](0063-bootstrap-token-delivery.md) (the idempotent install command) and [ADR-006(a)](0006-cache-redis.md) (presence is a lease, not a column).

## Context

`soul init` wraps dial, TLS handshake and the whole unary RPC in **one** deadline — `soul/internal/bootstrap/bootstrap.go:125-128`, default 10s, applied at `:234` — and writes its private key to disk only **after** a successful reply (`:116-119`, "If bootstrap fails, we leave nothing behind on disk"; the write is at `:194`).

The Keeper commits the burn **before** it knows the reply was deliverable. `keeper/internal/grpc/bootstrap.go:509-541` is one transaction: `bootstraptoken.Burn` (`:510`) → `SupersedeBySID` → `soulseed.Insert` → `soul.UpdateStatus(connected)` (`:534`) → COMMIT. `Burn` is `UPDATE … WHERE used_at IS NULL AND expires_at > NOW()` (`keeper/internal/bootstraptoken/crud.go:76-85`) — the authoritative anti-replay check.

A slow Vault PKI is enough. Sign returns at t≈9.9s, COMMIT lands, the client's deadline fires at t=10.0s and gRPC discards the reply. The token is burned, `soul_seeds` holds an active row, `souls.status='connected'` — and the host threw its private key away and never received a certificate. NIM-854's pre-auth budget narrowed the window (`bootstrapPreauthWaitShare`) but could not remove it, and says so in its own godoc at `bootstrap.go:260-267`.

**Both halves of the state are wrong at once**, and the second half is not cosmetic:

- `souls.status` is documented as `connected` = "stream alive, Keeper holds lease in Redis" (`keeper/internal/soul/soul.go:31`). `bootstrap.go:534` is the **only** writer of that value anywhere in the keeper — the EventStream never writes it. So the column asserts a stream that has never existed, for *every* bootstrap, and the assertion is merely short-lived in the happy path.
- Nothing ever corrects it. `select_disconnect_candidates` (`keeper/migrations/043_mark_disconnected_lease_aware.up.sql:47-57`) matches on `last_seen_at < NOW() - stale_after`, and `NULL < x` is false — a host that never connects sits at `connected` **forever**.
- Decisions are taken on it. `keeper/internal/coremod/bootstrap/issuer_pg.go:149-166` reads `connected`/`disconnected` as "already onboarded", writes nothing, and comments "no token to issue … this host has a seed to authenticate with". The host has no seed on disk. Re-running the very scenario that onboards it is therefore a silent no-op, and the documented recovery does not run.
- [ADR-0063](0063-bootstrap-token-delivery.md)'s install command is `test -e /var/lib/soul-stack/seed/current/cert.pem || … soul init` — deliberately idempotent, and the client side of this already distinguishes "I onboarded" from "I did not". On the next boot the guard correctly decides to retry, and the server refuses it forever.

## What must be unrepeatable

The token is a bearer capability: *whoever presents this may obtain one machine identity for SID X*. `Burn` implements that by making **presentation** unrepeatable. That is a proxy, and a lossy one: it equates "the reply was produced" with "the reply arrived", which is exactly the equality the network does not supply. No amount of retry logic fixes a proxy that measures the wrong event.

What the anti-replay rule is actually defending is narrower and states itself cleanly:

> **The binding `SID → public key` is one-shot.** The first presentation fixes it permanently. A later presentation carrying **the same** public key mints no new binding — it re-delivers a credential that binding already entitles that key to. A later presentation carrying **a different** public key is a second binding and is refused, exactly as today.

Against an attacker this is not weaker. An attacker's whole purpose in replaying a token is a certificate for **their own** key, and that is refused by the same rule that refuses it today. An attacker who replays the *original* CSR byte-for-byte obtains a certificate for a public key whose private half they do not hold. The one hole that remains — whoever presents **first** wins — is unchanged, and is the pre-existing threat model of a one-time token.

The repository already agrees with this framing, in a place that predates the question. `soulseed.FingerprintFromCert` (`keeper/internal/soulseed/soulseed.go:63-72`) hashes the **SubjectPublicKeyInfo**, documented as "bound to key, not to the full DER certificate (the latter changes when the same key is reissued)", and `SeedAuthenticator` (`keeper/internal/grpc/auth.go:49-68`) authenticates every stream by that fingerprint. Identity here **is** the public key. Re-signing the same key does not move it.

## Decision — two options, and they are not alternatives at the same level

Option 1 is a prerequisite of Option 2, not a competitor: both need one honest predicate for "this host has never appeared". The fork is whether we stop there.

### Option 1 — stop lying about the state; recover by re-issuing a token

1. **`Bootstrap` leaves `souls.status` at `pending`.** No new name is minted: `pending` is already documented as "operator issued bootstrap token, Soul not yet connected" (`soul.go:30`), which is precisely true after a bootstrap and stays true until a stream exists.
2. **The EventStream writes `connected` on first contact**, next to the existing `last_seen_at` flush (`keeper/internal/grpc/eventstream.go:1374-1390`). That is the event the enum already claims the value means.
3. Recovery is then by re-issuing a token — `issue-token` on the Operator API or MCP. (At the time this option was written it also looked as though `IssuerPG.issueOne` would re-arm such a host automatically, since it would now read `pending`. Implementation showed that must NOT happen: a host with an active seed being handed a fresh token is a takeover primitive — see "What implementation and review added", item 2. The scenario path therefore converges over it, and re-issuing is a deliberate operator act.)

**Anti-replay contract: untouched.** No proto change, no `Burn` change, no new column.

**Cost.** An unattended host — a baked image carrying a token file, no keeper-side actor to re-run — cannot recover itself, and neither can one whose disk was wiped while its seed stayed active: both need an operator to issue a new token. ADR-0063's `test -e … || soul init` guard keeps retrying and keeps getting `PermissionDenied`, because the burn is still keyed on presentation. We would be fixing the state that lies and leaving the mechanism that strands the host.

**Blast radius to check before shipping:** everything gated on `status='connected'` now sees `pending` for the seconds between bootstrap and first stream — `keeper/internal/incarnation/membership_gate.go:86` is the one call site that refuses on it.

### Option 2 — Option 1, plus a key-bound re-presentation window

Everything in Option 1, and additionally: a **used** token may be presented again when **all three** hold —

1. `expires_at > NOW()` — the token's own TTL still bounds it (24h default; the Reaper keeps used rows 90 days, `migrations/011`, so the row is still there to check);
2. the presented CSR's SubjectPublicKeyInfo fingerprint **equals** the fingerprint of the active seed for that SID — the one-shot binding, checked directly, needing **no schema change**: `x509.CertificateRequest.RawSubjectPublicKeyInfo` hashes to the same value `FingerprintFromCert` produced from the issued certificate, and `soul_seeds` holds exactly one active row per SID (`migrations/009:41-44`);
3. `souls.last_seen_at IS NULL` — the host has never held a stream. This column is written **only** by the EventStream flush (`keeper/internal/soul/crud.go:195-207`) and reset to NULL on re-provision (`:108-118`); `Bootstrap` never touches it. It is a free, already-existing "never appeared" predicate.

A re-presentation re-signs and **updates the existing seed row in place** — new serial, same fingerprint. It must not `Supersede` + `Insert`: `soul_seeds_fingerprint_idx` is **globally unique** (`migrations/009:46-50`), so a second row for the same key is a constraint violation, and superseding the row would revoke the identity the retry is trying to complete.

**Condition (3) is what keeps this from becoming a hole.** Without it, anyone holding a spent token plaintext — and the plaintext may still be sitting in `/etc/soul/token` on the box — could force a re-sign against a live, working host at any time inside the TTL. With it, the token is finished the instant the host is seen, and the two halves of this defect turn out to be **one mechanism**: the honest status is not a courtesy to the operator, it is the predicate the anti-replay carve-out is gated on.

**A system-marked burn is never re-presentable.** `used_by_kid` values `system-force-reissue` / `system-cloud-destroy` / `system-cloud-reprovision` / `system-bootstrap-issued-reissue` / `system-soul-forget` (`bootstraptoken/crud.go:241-316`) record an invalidation where no Soul presented anything. The re-presentation path requires a real KID. An operator who force-reissued a token has deliberately killed it, and this must not resurrect it.

**Cost — and it is a real one.** Condition (2) requires the Soul to present the **same** public key on retry, and today `soul init` generates a fresh RSA key per invocation (`bootstrap.go:120`). The client must persist its key between attempts, which softens "If bootstrap fails, we leave nothing behind on disk" (`:116-119`).

That property is worth less than it reads. A private key with no certificate is not a credential — it confers nothing until the CA signs it, and the successful path writes the same bytes to the same directory at mode `0400` moments later. The property that actually matters is preserved: the pending key lives **outside** any `vN/` version directory, so `seed.Load` — which reads only through the `current` symlink (`soul/internal/seed/seed.go:20-33`) — cannot mistake it for a half-written seed, and `ErrIncomplete` still means what it means.

## Recommendation

**Option 2.** Option 1 alone leaves the documented onboarding command permanently broken on exactly the failure this ADR is about, and that command is how hosts onboard. Option 2's extra surface is small and contained — a second SQL statement beside `burnSQL`, a branch in the handler, an in-place seed update, a pending-key file — and it buys the property the client already believes it has.

## What implementation and review added to the decision

Four things the design above did not anticipate, each found by building it or by an adversarial read of the diff. They are part of the decision, not footnotes.

1. **The seed write must decide from the registry, not from which token path authorized it.** `soul_seeds_fingerprint_idx` is globally unique, so a key the registry has already recorded cannot be inserted a second time — and the ordinary operator recovery (issue a fresh token; the host still holds its key) is exactly that case. Deciding by path left it violating the index and burning the new token for nothing.

2. **`core.bootstrap.issued` had to stop deciding "onboarded" by status.** With the status left honest, a host that had onboarded but not yet streamed fell into the re-arm arm, which mints a fresh token and ships the plaintext through the provisioning channel. Anyone reading it presents it with a key of their own — a valid *first* presentation, which supersedes the real host's seed. The arm is now chosen by whether an **active seed exists**. Restoring the old lie would have hidden this; naming the right fact fixes it.

3. **The CSR's self-signature is verified.** Without it the SubjectPublicKeyInfo — the value the whole one-shot-binding argument rests on — was an unproven claim: anyone could present anyone's public key, which is public. Verifying makes possession of the private key a precondition, which is what the fingerprint was always taken to mean, and it is what keeps a *keyless* impersonation attempt from reaching Vault at all. It does **not** close the case where the caller genuinely holds the other host's key — that is still refused, but at the seed write, after a certificate has been signed against a transaction that then rolls back. See the residual below.

4. **`force` must close a recovery window that is already open.** Every existing "kill this token" statement matches `used_at IS NULL`, because a burned token used to be inert. It is not any more, so `issue-token --force` gained [`CloseRecoveryBySID`](../../keeper/internal/bootstraptoken/crud.go); without it an operator reacting to a leak would be told the old token was killed while it still worked.

On the Soul side the pending key is bound to its SID in a PEM header. A key that outlives its host — a golden image snapshotted after a failed `soul init`, a hostname that settles after the first attempt — would otherwise be reused by a different SID, bound by whoever got there first, and refuse every other host permanently with nothing on the wire to say why.

## Accepted residual risks

Named rather than silently carried:

- **A replay with the original CSR costs a Vault signature each time.** Re-presentation is not counted, so a party holding both the token plaintext *and* the original CSR can drive signing in a loop until the host connects or the TTL expires. It yields them no identity (the certificate is for a key they do not hold), and the pre-auth budget bounds concurrency, but it is unbounded in total. The precondition is narrow — the CSR travels only inside TLS to the Keeper — so this is accepted rather than fixed with a counter column.
- **A refused cross-SID presentation still spends a Vault signature.** The seed decision is made inside the transaction, after signing, so a caller holding another host's private key (and a valid token of its own) makes Vault issue a certificate that is then rolled back — real in Vault's issued set, present in no `soul_seeds` row, and therefore not revocable through the registry. Moving the check above Vault would mean reading `soul_seeds` by fingerprint outside the transaction and trusting it, which is the read-then-write window the whole design avoids. The precondition — possessing another host's private key — already implies that host is compromised.
- **A recovery overwrites the previous certificate's serial** in the seed row (`RefreshCertificate`), so the superseded certificate is revocable through no registry. It is the same key and SID, and by construction it was never delivered; the cost is forensic, not a privilege.
- **Hosts in the bootstrap→first-stream window no longer count as `connected`**, which slightly shrinks the denominator of the write-API circuit breaker (`toll.BaselineConnected`) and makes `ScreenBindCandidates` refuse a bind for those seconds. Both are the honest answer to "is this host connected", and the window is now bounded by the handshake rather than open-ended.

## Acceptance

Guard tests, mutated in the form of real code:

1. A reply lost after COMMIT leaves the token usable: the same SID and the same key, presented again, onboard.
2. **A successful token stays refused.** Re-presentation after the host has been seen on a stream (`last_seen_at` non-NULL) is `PermissionDenied`. This one outranks (1) — fixing (1) while breaking (2) trades an inconvenience for a hole.
3. A re-presentation carrying a **different** public key is refused, whatever the host's state.
4. A token invalidated by a system marker (`system-force-reissue` and its siblings) is never re-presentable.
5. `Bootstrap` leaves the row at `pending`; a row reaches `connected` from the stream.
6. A fresh token redeemed with a kept key re-stamps the seed in place instead of violating the fingerprint index.
7. A CSR carrying another host's public key is refused. (It is refused at the seed write, not before Vault — see the residual below; `CheckSignature` stops only the form of this attack where the caller does not hold the private key.)
8. `core.bootstrap.issued` mints no token for a `pending` host that already holds an active seed — and still mints one for a `pending` host that does not.
9. `issue-token --force` disarms already-burned tokens, not only the unredeemed one.
10. A pending key generated for one SID is not reused by another.

## Open

- Whether `souls.status='expired'` should ever be reached by a stalled `pending` host. The enum documents it as "Reaper moved `pending` after bootstrap token TTL" (`soul.go:34`) but **no such rule exists** — `expire_pending_seeds` deletes `bootstrap_tokens` rows, not souls. Out of scope here; recorded because Option 1 makes `pending` a state hosts can now linger in.
