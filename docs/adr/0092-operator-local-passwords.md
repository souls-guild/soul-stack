# ADR-0092. Local operator passwords — `auth_method: local`

**Status:** accepted 2026-10-24; number 0092 stamped at its squash-merge into `release/NIM-1049`
**Amends:** [ADR-014](0014-operator-identity.md) (the `auth_method` enum gains `local`; a `local` operator's credential is a password, not a token in the create reply), [ADR-058](0058-operator-auth-ldap-oidc.md) (a third login endpoint beside LDAP/OIDC under the same cookie, the same `/auth/token` exchange and the same `auth.rate_limit`; `GET /auth/methods` gains `local`; the `provisioning_allowed_methods` domain gains `local`), [ADR-013](0013-bootstrap-archon.md) (the first Archon may be a `local` operator)
**Leaves unchanged:** [ADR-014](0014-operator-identity.md) amendment NIM-1045 — every token is still minted through `operator.MintJWT`, a password login mints a `session` row like LDAP/OIDC; [ADR-028](0028-rbac-storage.md) — roles are read from the snapshot on every request, the token's roles stay informational
**Implemented by:** NIM-1051 (release NIM-1049)

### Problem

An operator with `auth_method: jwt` has no way to log in: a human pastes a token. The two
login methods that exist — LDAP and OIDC — need an identity provider, and a cluster that is
being set up, a test stand, or an installation without a directory has none. The user's own
words: "at the very start, without LDAP, we want to test somehow".

### Decision

A password Keeper checks itself, as a separate login method:

- **A new `auth_method` value `local`** (migration 130 extends the CHECK `auth_method_valid`,
  the way migration 083 added `ldap`/`oidc`). Only an operator enrolled with `local` logs in by
  password. The anti-takeover rule of ADR-058(d) holds unchanged: a `jwt` operator —
  `archon-system`, a service account, a bootstrap Archon created without `--password-stdin` — is
  never given a password and is never logged in by one, and neither is an `ldap`/`oidc` operator,
  whose password lives at the IdP. `auth_method` is fixed at creation, so the only way to an
  operator with a password is to create it `local` (the API, or `keeper init --password-stdin`).
  The name `local` and not `password`: `GET /auth/methods` already says `password: true` for
  "paste an operator JWT", and renaming that would break the contract.
- **The hash lives in its own table**, `operator_passwords` (one row per operator, argon2id in
  PHC form), never in `operators`: no read of an operator carries it. Among the non-test Go
  files of `keeper/` only `keeper/internal/operator/password.go` names the table; a guard test
  fails when a string literal in any other one does (tests and migrations are outside it). A **composite foreign key** `(aid, auth_method) → operators(aid,
  auth_method)` with `auth_method = 'local'` pinned by a CHECK makes the anti-takeover rule a
  property of the schema too: a hash row cannot point at a `jwt` operator whatever the code does.
- **argon2id, RFC 9106 §4 second recommended option** (64 MiB, 3 passes, 4 lanes, 16-byte salt,
  32-byte key). Verification reads the parameters back from the stored string (bounded: at most
  1 GiB and 16 passes, so a planted row cannot turn a login into an unbounded allocation), so
  raising them later needs no migration — but nothing re-hashes a stored password on login, so
  until each is set again its verification costs the old parameters' time, which the decoy below
  does not imitate. At most four hashes run at once per Keeper, one queue shared by login,
  create, reset, change and the decoy, bounded only by the request's context — the login rate
  limit is per IP and per AID, and a spread of both would otherwise allocate without bound. The
  trade-off is chosen: a flood of such a spread makes every password operation on that Keeper
  wait in the queue, rather than take the Keeper's memory.
- **Opt-in config block `auth.local`** (`local: {}` turns it on). Without it this Keeper mounts
  no password login and sets no password: create with `auth_method: local`, the admin reset and
  the change-my-password route answer `422` (a create the provisioning policy refuses first
  answers its `403`, see below). `min_password_length` (default **12**, allowed
  **8..128**) is the only key.
- **Password policy:** at least `min_password_length` characters and at most **128**, valid
  UTF-8, no control characters, not equal to the AID (case-insensitive). No composition rules
  (NIST SP 800-63B).

### The surface

- **`POST /auth/local/login`** `{aid, password}` → `204` and a `soul_session` cookie built by
  the same `newSessionCookie` as LDAP/OIDC (HttpOnly, `SameSite=Strict`, `Path=/auth`, and Secure unless
  `auth.insecure_cookies`, which OIDC ignores — ADR-058(l); lifetime
  `auth.local.session_ttl` since NIM-1050), exchanged at `POST /auth/token` like theirs. The session is an
  `operator_jwts` row of kind `session` minted through `MintJWT`, **bound to the password that
  verified**: the mint runs in one transaction that re-reads the hash `FOR SHARE` and refuses
  (the same `401`) when it is no longer the one checked. The check is slow on purpose, so a reset
  can land between it and the mint; without the binding a login begun with the old password
  would keep a fresh session after the reset revoked all the others. A reset still running when
  the mint reads waits for it on the row lock; a reset that starts after the mint revokes the
  session it minted. Mounted only with `auth.local`.
  The AID is trimmed and lowercased before the lookup. **Anti-oracle:** an unknown AID, an
  operator of another method and a wrong password are one `401` (`unauthenticated`), and the
  first two still spend one argon2 verification against a decoy hash so the latency does not
  say which AIDs have a password. The decoy is made once per process, with the current
  parameters, on the first such refusal; the refusals of that moment wait for it whatever their
  own context says, and that first one costs a hash more.
  An AID outside the AID form, an empty password or one over 512 bytes is refused at once — no
  such AID can exist, so there is nothing to hide. `401 operator-revoked-token` is told only after the password
  matched. `operator.login` is written with `method: local`.
- **Brute force:** the existing `auth.rate_limit` (`AuthLoginLimit` over `LoginGuard`): per IP
  and per AID, the AID normalized the same way the handler normalizes it — otherwise every
  spelling of one AID would get its own lockout counter. The per-AID counter is shared with the
  LDAP form's per-username one, so the same name is one counter whichever form it was typed in.
  While the limiter runs (Redis present, `auth.rate_limit.enabled` not `false`), a login body
  over **4 KiB** — the most the key is read from — is refused with `413` before either layer
  and before any password is checked (credentials are a few hundred bytes, and a body padded
  past the key would otherwise be judged by the per-IP layer alone). The bound applies on every
  route the limiter keys by a name: the LDAP login too, and `PUT /v1/me/password`, whose key
  comes from the token but whose body is two passwords and fits easily.
- **Creating a local operator:** `POST /v1/operators` takes `auth_method` (`jwt` — default, the
  reply carries the token as before; `local` — the request carries `password`, the reply carries
  no token: `jwt`/`jwt_id`/`expires_at` are omitted and `auth_method` says why). A `local`
  create with `ttl` or `label`, or a `jwt` create with a password, is `422`. The insert, the hash
  and any role grants are one transaction. MCP `keeper.operator.create` stays `jwt`-only: a
  password does not travel through an agent's channel. **Contract change:** in the OpenAPI
  schema `OperatorCreateReply.jwt`/`jwt_id`/`expires_at` stop being required for every create
  reply (a `jwt` reply still carries all three); a client generated from the spec has to treat
  them as optional, and the reply gains `auth_method`. `operator.created` carries `jwt_id` only
  when a token was minted.
- **Admin set/reset:** `PUT /v1/operators/{aid}/password` `{password}` → `204`, permission
  **`operator.set-password`**, audit **`operator.password-set`** `{aid, tokens_revoked}`. It
  revokes **all** of the target's web sessions and personal tokens (`kind` `session` and, since
  NIM-1052 added it, `personal`) that may still verify — those
  unexpired and those expired within the RBAC snapshot's five-minute clock margin — so whoever
  held the old password is logged out everywhere, a login in flight included (see above), and a
  personal token it issued itself does not outlive the reset by up to `auth.jwt.ttl_max`. Tokens of other kinds are untouched — they are not the
  password's. **Not restricted by target**, like `operator.issue-token`: whoever may set a
  password may log in as that operator, so the permission is granted as `cluster-admin` would be.
  `404` for an unknown AID, `409 operator-revoked` for a revoked one, `422` for a target that is
  not `local` or a password outside the policy. **The caller's own AID is refused (`422`):** the
  route asks for no current password, so a token holding the permission — a personal one
  included — would be a way to its owner's password and from there to new personal tokens;
  `PUT /v1/me/password` is the way, and it asks. This closes the direct path only: the holder
  can still reset another local Archon that holds the permission, log in as it and reset the
  owner from there — which is why the permission is granted like `cluster-admin`. A first
  Archon that forgot its password has no current one to give: another administrator, a token
  one as well as a local one, resets it. With none yet, the bootstrap token (valid
  `auth.jwt.ttl_bootstrap`, 30 days by default) creates one, and can also issue the first
  Archon a personal token of up to `auth.jwt.ttl_max` for later; with none of these left,
  nothing in the API recovers a sole local Archon. A personal token cannot be
  minted past the reset either: the reset and a personal token issue both take a transaction
  advisory lock on the AID first, so an issue running alongside a reset either commits first
  and is revoked with the rest, or waits. Not the operator row: the reset writes rows whose
  foreign keys share-lock the acting administrator's row, and an operator revoke holds the
  administrators' rows `FOR UPDATE` while it waits for the target's, so a lock on the target's
  row would deadlock with it. A three-way race is not closed: a reset, an administrator
  revoking one of the target's tokens and an RBAC change that runs the lockout probe can still
  meet in a cycle of foreign-key share-locks, and Postgres then aborts one of them (`40P01`, a
  `500`, nothing written). And the issue reads the revocation of the token it is asked with
  from that token's row, not from the RBAC snapshot, so a session the reset ended a moment ago
  mints nothing on a node that has not heard of it yet (`401 jwt-revoked`). One gap is left as
  it is: the 20-a-day limit of NIM-1052 counts issued rows, revoked ones included, so an intruder
  who issued 20 that day leaves the owner unable to issue a personal token until the oldest of
  them is a day old.
- **Change my password:** `PUT /v1/me/password` `{current_password, new_password}` → `204`, no
  permission (the subject is the caller), audit **`operator.password-changed`**. The caller's
  other web sessions are revoked; the one the request came with (its `jti`) stays, and so do its
  personal tokens — the operator's own scripts; one it suspects is revoked from its token list. The
  write is
  bound to the password that verified: it re-reads the hash `FOR UPDATE` in its transaction, and
  a reset committed since the check refuses the change (`403`) instead of being overwritten by
  it. An operator revoked before its current password is checked answers
  `401 operator-revoked-token`; one revoked after the check still has the password changed —
  harmless, every token of it is refused anyway. A wrong
  current password is **`403`**, not `401` — the caller's token is fine and a `401` would log the
  UI out — and it counts towards the same per-AID lockout as the login form, so a stolen Bearer
  cannot brute-force the password behind it faster than the form could. The route runs the whole
  `AuthLoginLimit`, so a wrong current password also counts towards the per-IP lockout (behind a
  shared address it adds to that address's count, as a wrong password on the form does), and a
  Redis error answers `429` (fail-closed, as on the form). A caller that is not `local` gets `422`.
- **`GET /auth/methods`** gains **`local: bool`**.
- **Provisioning policy:** the `provisioning_allowed_methods` domain becomes
  `{user, ldap, oidc, local}`. A token operator created through `POST /v1/operators` is gated by
  `user`, a password one by `local` (MCP `keeper.operator.create` applies no policy — a gap older
  than this ADR, left as it is); `created_via` of both is `user`. A policy set before `local` existed
  does not name it and therefore refuses local creation until it does; an absent key allows
  everything, as before. As for the other methods, the policy gates creation and never login.
- **Bootstrap:** `keeper init --archon=<aid> --password-stdin` reads one password from stdin
  (the whole input minus one trailing line ending, as `docker login --password-stdin` does),
  checks it against the whole password policy, with the minimum of `auth.local` (required for
  the flag), before touching anything,
  and creates the first Archon with `auth_method: local` and the password in the bootstrap
  transaction (`set_by_aid` NULL). The bootstrap token is minted and written as without the flag.

### Alternatives considered

- **A password as an extra credential of any `jwt` operator, no new method.** No CHECK change,
  and the bootstrap Archon could get a password without a flag. Rejected: every `jwt` operator,
  service accounts included, becomes a password target; `auth_method` stops saying how an
  operator logs in; and production could not switch the method off for creation. Rejected by the
  user on 2026-10-24 in favour of the method.
- **Reusing `operator.issue-token` for the reset.** The same power — minting a token for an
  operator is becoming it — but then password resets cannot be granted without token issuance.
- **Restricting the reset to targets whose rights the caller holds.** Stricter than
  `issue-token`; rejected so the two equally strong permissions behave alike.

### What is not here

- **Session lifetime.** A password session lived `auth.jwt.ttl_default`, like LDAP/OIDC; since
  NIM-1050 it lives `auth.local.session_ttl`, 7 days by default ([ADR-058(k)](0058-operator-auth-ldap-oidc.md)).
- **Logging out a session server-side.** Done in NIM-1050: the web UI's log-out revokes its
  session, and `POST /v1/me/sessions/revoke` ends all of them ([ADR-058(k)](0058-operator-auth-ldap-oidc.md)).
- **Changing an existing operator's method** (`jwt` → `local`). `auth_method` stays immutable.
- **Password expiry, history, composition rules, breached-password lists.**
