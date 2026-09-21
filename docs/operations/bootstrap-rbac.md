# First Archon Bootstrap and RBAC

The procedure for the first cluster initialization, releasing additional Archons via API/MCP, basic RBAC operations. Design Details - [ADR-013](../adr/0013-bootstrap-archon.md), [ADR-014](../adr/0014-operator-identity.md), [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres); the permissions directory, string format and built-in roles are [`docs/keeper/rbac.md`](../keeper/rbac.md).

## `keeper init` - first Archon

On first initialization, the `operators` registry in Postgres is empty; without a special mechanism, all APIs/MCPs will return 403 (`default_policy: deny`, [ADR-013](../adr/0013-bootstrap-archon.md)). Bootstrap - administrative subcommand of the `keeper` binary itself:

```sh
keeper init \
  --archon=archon-alice \
  --config=/etc/keeper/keeper.yml \
  --credential-out=/etc/keeper/archon-alice.jwt
```

What's happening:

1. Take **PG advisory lock** on bootstrap-lock-id ([ADR-013(e)](../adr/0013-bootstrap-archon.md)). Race between several `keeper init` is resolved at the database level.
2. Checks that `operators` is empty. If not empty, it will fail with the message `cluster already initialized; archon <aid> exists since <ts>`, exit != 0.
3. Record `operators(aid=archon-alice, created_by_aid=NULL, bootstrap_initial=true)` ([ADR-014(a)](../adr/0014-operator-identity.md)) is created. The invariant is exactly one record with `created_by_aid IS NULL` (partial unique index).
4. Role `cluster-admin` (seed role from migration, `permissions: ["*"]`) is bound - line `rbac_role_operators(cluster-admin, archon-alice)` ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).
5. Issued JWT with TTL = `auth.jwt.ttl_bootstrap` (default 30 days; configured in `keeper.yml`).
6. JWT is written to `--credential-out` with **`mode 0400`**, owner is the user running `keeper init`. Two values are not that: `-` is a sentinel meaning stdout and opens no path at all, and a path leading to a fifo or a character device (`/dev/stdout`) is written **in place** as a stream. Everything else a path can lead to — a block device, a socket, a directory — is refused. See § Token on stdout below.
7. Audit: `operator.created` (`source=keeper_internal`, `archon_aid=NULL`, `payload={bootstrap_initial: true, ...}`).

Nothing `keeper init` prints goes to stdout: the completion line and the warnings go to **stderr**, and the structured log line goes wherever `logging` in `keeper.yml` sends it — stderr by default, a file if `logging.file` is set. Stdout carries the token and nothing else, and only when asked for.

### Token on stdout

`--credential-out=-` writes the JWT to stdout instead of opening any path:

```sh
keeper init \
  --archon=archon-alice \
  --config=/etc/keeper/keeper.yml \
  --credential-out=- > archon-alice.jwt
```

This exists for containers. A distroless image has no shell and no `cat`, so a token written to a file *inside* the container cannot be read back out — `kubectl exec … cat` has nothing to run, and `kubectl cp` needs `tar` in the container. The fd `kubectl exec` is already attached to is the only way out:

```sh
kubectl exec -i deploy/keeper -- \
  keeper init --archon=archon-alice --config=/etc/keeper/keeper.yml --credential-out=- \
  > archon-alice.jwt
```

`-i` without `-t`, and it matters: `-t` allocates a TTY, which merges the container's stdout and stderr into one stream. The warning and the completion line would land in `archon-alice.jwt` alongside the JWT.

`--credential-out=/dev/stdout` is the same destination by a different route — the path is opened and written **in place**, never unlinked, never truncated, never `chmod`ed — but only while stdout really is a stream: a terminal, a pipe, the fd `kubectl exec` attached. Redirected into a file, `--credential-out=/dev/stdout > archon-alice.jwt` is **refused**. Not because the write would go wrong — under `>` the shell truncates the file before `keeper` starts, and under the forms that do not truncate (`>>`, `1<>`) a leftover tail is not what makes this unsafe. Because at that point the fd behind `/dev/stdout` is an ordinary file that already exists with a mode of its own, and `--credential-out=<path>` promises `mode 0400` — a promise `keeper init` can only keep on a file it created itself. It refuses rather than dropping the guarantee silently. Use `--credential-out=-` for a redirect, which promises no mode and says so, or name the file directly. A file genuinely named `-` is still reachable as `./-`.

Any other fifo or character device works the same way, and a fifo has one extra condition: **a reader must already be attached**. `keeper init` will not wait for one — a blocked open here would hang after the Archon is committed, with the token reaching nobody and `keeper init` refusing to run a second time — so a readerless fifo fails immediately, saying so. Start the reader first. The error names that remedy only when the fifo was named by its own path; reached through a symlink the name looks like a symlink and nothing else, so the message is the bare `ENXIO`.

**The path form trusts the directory it writes into, and the target it lands on. Neither is checked, and neither can be.** Whoever can create a name in that directory can put a fifo — or a symlink to one — where the credential is about to go and take delivery of the token; `keeper init` cannot tell that apart from an operator pointing at a stream of their own, and refusing to follow symlinks would not help, because a fifo planted at the path directly is the same attack. On the target side, every character device is accepted: `--credential-out=/dev/kmsg` puts the JWT in the kernel ring buffer and from there in the journal, and merely opening `/dev/watchdog` arms a reboot before any check can run. Block devices, sockets and directories are refused. Point `--credential-out` at a directory only trusted uids can write to and at a target you named on purpose — or use `--credential-out=-`, which resolves no path at all and so can be diverted by nothing on disk.

A token on a stream carries **no permission guarantee** — `mode 0400` is a property of a file `keeper init` created itself, and it created nothing here. Every stream write says so on stderr, and the structured log line carries `credential_is_stream: true`. Redirecting into a file you own re-establishes the guarantee only as far as that file's own mode does; `umask 077` before the redirect. If the other end was a container's own stdout, the token is now in the cluster's log pipeline: create a second Archon with it, then revoke the first (§ Archon Revocation) — the last `*`-Archon cannot be revoked, so the order is not optional.

**A failed stream write puts the token on stderr on purpose.** If whatever was reading stdout goes away before the write lands, `keeper init` does not die with it: the Archon is already committed and the audit already written, so the token is the only thing left to salvage and it is printed to stderr between explicit markers. In a container that is the same log pipeline stdout was going to, so the JWT reaches the cluster's logs — deliberately, because the alternative is a bootstrapped cluster nobody holds the credential for. Treat it exactly as above: create a second Archon with it, then revoke the first.

### Restart semantics

After the catastrophic wipe Postgres (truncate `operators`) Keeper **does not re-bootstrap automatically** - this is protection against accidental release of the admin token in the logs ([ADR-013(d)](../adr/0013-bootstrap-archon.md)):

| Condition | Behavior |
|---|---|
| `operators` empty + **no** `--initialize` (or env `KEEPER_INITIALIZE=true`) | Keeper refuses to start: `operators registry is empty; run 'keeper init --archon=<aid>' before starting the cluster`. Exit != 0. |
| `operators` empty + `--initialize` | Starts in read-only mode: listeners are raised, but all API/MCP calls return `503 cluster awaiting first archon` until `keeper init` finishes. |
| `operators` is not empty | Starts normally (read-only check). |

In an HA cluster, `keeper init` runs **once** on one instance. The remaining simultaneous `keeper init` wait for the advisory lock, see a non-empty registry and refuse, indicating the already created Archon.

### Bootstrap JWT storage

File `--credential-out` - **source material for first setup**, not "long-term token storage":

- `mode 0400`, owner is a human operator (not `soul-stack`). Hides in the password manager / Vault operator immediately after bootstrap.
- TTL 30 days - a window to have time to set up further administration (issue tokens for CI, machine-identity, additional people). After using the first Archon to create a second, the original JWT can be revoked (see § Revocation) or allowed to expire.
- In git, in /etc/keeper/, in systemd-credential-store - **do not put** for a long time. This is an admin-credential with `*` rights.
- With `--credential-out=-` there is no file and no mode: the token lands wherever the shell pointed stdout. That is fine when you are holding the other end (a redirect into a file you own, a pipe, `kubectl exec`) and not fine when the other end is a log collector.

## Second+ Archon via Operator API

After bootstrap, the only way to create Archons is the Operator API (`POST /v1/operators`) or MCP-tool `keeper.operator.create` with permission `operator.create` ([`docs/keeper/rbac.md` → permissions directory](../keeper/rbac.md)).

### Creation of the Archon

```sh
curl -X POST https://keeper.internal:8080/v1/operators \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" \
  -H "Content-Type: application/json" \
  -d '{"aid": "archon-bob", "display_name": "Bob"}'
```

The answer is JSON with the fields of the created Archon. The JWT itself for the new Archon returns **separate endpoint** `operator.issue-token` (permission `operator.issue-token`):

```sh
curl -X POST https://keeper.internal:8080/v1/operators/archon-bob/issue-token \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)"
```

The request has **no body** - the TTL of the issued token is not a per-request parameter (the full endpoint contract - [`operator-api/operator.md`](../keeper/operator-api/operator.md)). Answer:

```json
{"aid": "archon-bob", "jwt": "eyJ…", "expires_at": "2026-05-26T15:30:00Z"}
```

JWT is the only time the operator sees it; Keeper does not store issued tokens (only signing-key and the Archon registry).

### Token lifetimes

There is no per-request TTL: every lifetime is a `keeper.yml` knob read **once at Keeper startup** ([config.md → `auth.jwt`](../keeper/config.md)), so changing any of them requires editing the file and **restarting** Keeper - a hot-reload will not affect newly issued tokens.

| Knob | Default | Applies to |
|---|---|---|
| `auth.jwt.ttl_default` | `24h` | Every JWT issued through `POST /v1/operators` and `operator.issue-token`, plus the internal JWT behind the `soul_session` cookie of an LDAP/OIDC login. |
| `auth.jwt.ttl_bootstrap` | `720h` (30 days) | Only the first token written by `keeper init`. Read by the subcommand itself at each invocation, so it does not depend on the running daemon. |
| `auth.jwt.exchange_ttl` | `10m` (floor `1m`) | The short Bearer that `POST /auth/token` gives the web UI in exchange for a `soul_session` cookie ([ADR-058](../adr/0058-operator-auth-ldap-oidc.md)); additionally capped by the remaining lifetime of the cookie. |

Issuing a fresh token does **not** invalidate the previous one: both live until their own `exp`. The only levers that cut a live token short are § Archon Revocation and § Emergency recall of all JWTs below - with the reservations stated there.

### Role assignment

After creation, the Archon itself **does not have a single permission** (default-deny). Binding to a role is a separate operation (permission `role.grant-operator`):

```sh
curl -X POST https://keeper.internal:8080/v1/roles/db-operator/operators \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" \
  -H "Content-Type: application/json" \
  -d '{"aid": "archon-bob"}'
```

If the role `db-operator` does not exist yet, it is created by `role.create`:

```sh
curl -X POST https://keeper.internal:8080/v1/roles \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "db-operator",
    "permissions": [
      "incarnation.* on service=redis,vault-cluster",
      "soul.list"
    ]
  }'
```

The permission string grammar is [`docs/keeper/rbac.md` → permissions format](../keeper/rbac.md). The `on <key>=<values>` selector supports the `service` / `coven` / `incarnation` / `host` keys.

## RBAC: scope by coven / service / incarnation

Permission string with a selector is the only mechanism for a narrow scope ([rbac.md → Permissions format](../keeper/rbac.md)). Examples from a real installation:

| Problem | Permission |
|---|---|
| Full cluster admin | `*` |
| Read only all Souls | `soul.list` |
| Apply to a specific coven | `incarnation.run on coven=prod-eu-west` |
| Apply to a specific service in any coven | `incarnation.* on service=redis` |
| Creation/review of Archons | `operator.create`, `operator.revoke`, `operator.issue-token` |
| Role management | `role.create`, `role.delete`, `role.update`, `role.grant-operator`, `role.revoke-operator` |
| Reading audit-log (when `GET /v1/audit` appears) | `audit.read` |
| Push to hosts | `push.apply on coven=<coven>` |
| Service-registry CRUD | `service.create`, `service.list`, `service.update`, `service.delete` |

`cluster-admin` is a built-in role with `*`, it cannot be deleted via `role.delete` (`builtin=true` in `rbac_roles`).

### Self-lockout protection

Invariant ([ADR-013(c)](../adr/0013-bootstrap-archon.md)): **You cannot remove the last operator with `*`-permission** active (via `cluster-admin` or via an explicit role with `permissions: ["*"]`). Trying via API - `409 Conflict` with `would lock out the cluster`.

Same for `revoked_at`: attempt to revoke the last `*`-Archon - 409. Real "recall all admins" = "first create a new one, then recall the old ones."

## Archon Revocation

```sh
curl -X POST https://keeper.internal:8080/v1/operators/archon-old/revoke \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)"
```

What's happening:

- `operators.revoked_at = NOW()` is installed. The Archon remains on the registry for auditing, and new JWTs for it are no longer released.
- **Active JWTs of a recalled Archon continue to operate until their `exp`** ([ADR-014(d)](../adr/0014-operator-identity.md)). Short TTL (`ttl_default: 24h`) is a natural protection.
- Forced revocation of "all live JWTs" is a separate post-MVP task (requires JWT-blocklist/session-store, not part of MVP).

### Emergency recall of all JWTs - signing-key rotation

If the live JWT is compromised and you can't wait for `exp`, the only reliable way in MVP is: **JWT signing-key rotation** ([`docs/keeper/prod-setup.md` → Signing-key rotation](../keeper/prod-setup.md)). Immediately invalidates **all** living JWTs (the signature will not match) - you will need to re-issue tokens to all Archons.

```sh
vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"
systemctl restart keeper  # RESTART, not reload (see below)
```

**A `reload` is not enough.** The signing-key is resolved from Vault once during startup, and the JWT verifier/issuer are built from that copy for the whole process lifetime (`bootstrap.LoadSigningKey` in `keeper/cmd/keeper/daemon.go`); the SIGHUP hot-reload path re-reads `keeper.yml` but does not rebuild them ([config.md → reload-policy](../keeper/config.md), row `auth.jwt.signing_key_ref`). After a `reload` Keeper keeps signing **and accepting** tokens made with the compromised key - that is, the rotation silently does not happen. In an HA cluster restart **every** instance: an instance still holding the old key will keep validating the old Bearers.

After this, re-issue JWT via `operator.issue-token` for all active Archons (old Bearers → 401). The bootstrap token from `keeper init` is signed with the same key and dies together with the rest.

## Audit RBAC operations

All RBAC operations are written to `audit_log` ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) with a specific `event_type`:

| Operation | `event_type` |
|---|---|
| Creation of the Archon | `operator.created` |
| Revocation of the Archon | `operator.revoked` |
| Archon JWT Release | `operator.token_issued` |
| Create a role | `role.created` |
| Delete a role | `role.deleted` |
| Linking the Archon to the role | `role.operator_granted` |
| Decoupling | `role.operator_revoked` |

`source` - `api` or `mcp` (depending on the call transport), `archon_aid` - who initiated (bootstrap `operator.created` - `NULL`, `payload={bootstrap_initial: true}`). Retention - `purge_audit_old` (default 365 days). Viewing audit-log via `GET /v1/audit` is a separate task (see [ADR-022(j)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); for now - a direct SQL query in PG.

## Operational Administration Scenarios

### Transfer of access from the retiring Archon

1. The current Archon (or another `cluster-admin`) creates a new one: `POST /v1/operators` + `role.grant-operator`.
2. Issues JWT to new: `POST /v1/operators/archon-new/issue-token`.
3. **Across the TTL** intersection the old Archon roars: `POST /v1/operators/archon-old/revoke`.
4. The old Archon's live JWTs stop working **at once** (2026-08-07, NIM-421): every authenticated route answers `401 operator-revoked-token` from the next request, so the handover window is the one you plan in step 3, not `ttl_default`. Signing-key rotation (see above) is for a different case - a leaked *key* or a stolen *token* whose Archon you are not revoking.

### Machine-identity (CI / scripts)

MVP - JWT with a long TTL (separate Archon `archon-ci-deployer`, narrow role with the required set of permissions). Stored in CI secret-store as a Bearer token. Scheduled re-release (in short `ttl_default` - for example, 7 days) - through `operator.issue-token` from the admin-Archon or by a script using the previous JWT before its `exp`.

mTLS-cert-form of Archon for machine-identity - post-MVP ([ADR-014(b)](../adr/0014-operator-identity.md)), extension via `auth_method` enum without breaking changes.

### Reset to "single admin" (catastrophic recovery)

Case: loss of all living JWTs, credentials forgotten, human operator left. **Without access to the Keeper host** - no way (`keeper init` requires physical access).

With physical access:

1. **DO NOT truncate `operators`** - this will break FK chains (`created_by_aid`, `state_history.changed_by_aid`, audit).
2. Create a **temporary admin** via SQL directly in PG:
   ```sql
   INSERT INTO operators (aid, display_name, auth_method, created_at, created_by_aid)
   VALUES ('archon-recovery', 'recovery', 'jwt', NOW(), NULL);
   INSERT INTO rbac_role_operators (role_name, aid, granted_at, granted_by_aid)
   VALUES ('cluster-admin', 'archon-recovery', NOW(), NULL);
   ```
3. Issue JWT for `archon-recovery` by the admin utility (see [open question in `disaster-recovery.md`](disaster-recovery.md#open-questions-runbook) - is a separate subcommand `keeper issue-token` needed; at the time of writing - a post-MVP task, done through signing-key rotation + repeated bootstrap-like process if necessary).

This is an **emergency procedure**, requires audit-trail and access to PG. In the standard operating model, have at least 2 `cluster-admin`-Archons and store their JWTs in separate password managers.

## See also

- [`docs/keeper/rbac.md`](../keeper/rbac.md) - full permissions directory, grammar, built-in roles.
- [`docs/keeper/operator-api.md`](../keeper/operator-api.md) - REST endpoints, JWT claims, RFC 7807 errors.
- [`docs/keeper/mcp-tools.md`](../keeper/mcp-tools.md) - MCP-tools (1:1 with REST endpoints).
- [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md) - JWT signing-key, rotation.
- [`docs/architecture.md` → ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) — audit-pipeline.
