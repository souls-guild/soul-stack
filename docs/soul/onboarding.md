# Soul onboarding: the bootstrap token and delivery

Applies to the pull mode (`transport: agent`). For the push mode (`transport: ssh`) onboarding reduces to configuring SSH access and does not use a bootstrap token — see [architecture.md → Push mode](../architecture.md#push-режим-keeperpush).

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
- In the same transaction: `souls.status: pending → connected`, and an `active` record with the signed certificate is created in `soul_seeds`. The half-state "the token is burned but the seed is not created" is impossible.
- Keeper does not store the plain token — after being issued to the operator it leaves memory. In the DB — only `token_hash` (SHA-256, hex, no salt — the token is already high-entropy by itself).

### On the Soul side

- `soul init` receives the token as a **string** — from the `--token` flag or the env `SOUL_BOOTSTRAP_TOKEN` (see [§ Onboarding flow](#onboarding-flow-agent-mode)); the binary itself **does not read or delete** any token files. One `init` process = one presentation; after a successful bootstrap the token is already burned on the Keeper side (the SQL transaction above) and is not reusable.
- If the delivery channel put the token into a file (for example, `/etc/soul/token` from `core.bootstrap.delivered`, [ADR-063](../adr/0063-bootstrap-token-delivery.md)) — the file remains an artifact of the **delivery channel**, and its hygiene (`0400` permissions, cleanup) is on the channel/operator, not on the `soul` binary. The main protections are the one-time use and the short TTL of the token, not overwriting the disk.
- The token contents are **never** logged — neither by `soul` nor by Keeper (on Keeper's outputs the `bootstrap_token` key is masked by `audit.MaskSecrets`).

### Recommendations for the operator

Outside the Soul Stack area of responsibility, but it affects onboarding security:

- The token file — `mode 0400`, owner `soul:soul`, the directory `mode 0700`.
- On systemd ≥ 250 — use `LoadCredential=` in the unit file. The token lives in tmpfs, is passed into the process through an ephemeral fd, and is **never written to disk**. The best solution; everything else is a compromise.

**The Reaper** later picks up the used tokens: the rule `purge_used_tokens` with `max_age: 90d` from `used_at` — this is no longer about security, but GC. See [architecture.md → Reaper](../architecture.md#reaper--жнец).

## Onboarding flow (agent mode)

1. The operator (or a script), via Keeper's OpenAPI/MCP, registers the host and obtains a bootstrap token for a specific SID:
   - **Primary registration** — `POST /v1/souls` (permission `soul.create`, MCP tool `keeper.soul.create`, [operator-api.md](../keeper/operator-api/souls.md#post-v1souls--зарегистрировать-soul)). A record appears in `souls` with status `pending`, `requested_at = now()`, `created_by_aid` = the calling Archon (FK to `operators(aid)`). For `transport: agent` a bootstrap token is issued in the same operation — one-time, TTL 24h by default; the plain token is in the response once.
   - **Re-issue** — `POST /v1/souls/{sid}/issue-token` (permission `soul.issue-token`, MCP tool `keeper.soul.issue-token`). Used when a token is lost or for a planned re-issue for an already-existing Soul. Details — [§ Recovery: a lost token](#recovery-a-lost-token).
2. Delivering the `soul` binary, the ready config, and the SoulSeed token to the host is the operator's task. Allowed mechanisms are in the "Ways to deliver the token" section below.
3. The operator runs `soul init [--config /etc/soul/soul.yml]` once. The command:
   - takes the token from the flag `--token=<token>` **or** from the env `SOUL_BOOTSTRAP_TOKEN` (the flag beats the env; both empty → error). **stdin is not read.** The env form is preferable: the `--token` value shows up in `ps` and shell history, the env does not;
   - takes endpoints, retry, tls from the config (`keeper.endpoints`, etc.); from the command-line arguments — only `--token`, `--config`, and `--sid` (SID override: `--sid` > `sid:` of the config > `os.Hostname` lowercased);
   - locally generates a private key (it never leaves the host) and a CSR with SID = FQDN;
   - connects to the Keeper Bootstrap listener on `endpoints[].bootstrap_port` (server-only TLS), traversing endpoints by `priority` from smaller to larger without in-group shuffle (one-shot, the order is deterministic; spray/shuffle exist only in the EventStream phase — see [connection.md → Two phases, two ports](connection.md#две-фазы-два-порта)), and presents the token + CSR;
   - having received the signed certificate, atomically lays out the SoulSeed into `paths.seed` and terminates.

   If a SoulSeed already lies on disk — `init` fails with an error, so as not to accidentally re-issue (for a re-issue there is a separate procedure, see [identity.md → SoulSeed rotation](identity.md#ротация-soulseed)).
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

For a Soul with `transport: ssh`, `issue-token` returns `422 validation-failed` — ssh onboarding does not use a bootstrap token ([architecture.md → Push mode](../architecture.md#push-режим-keeperpush)).

## Ways to deliver the token

"The operator generated a bootstrap token → the token ended up in a file on the VM where `soul` will launch" — different paths are allowed for this. Some of them are **inside Soul Stack** (via `keeper.push`), some are **outside the area of responsibility** (the operator's external tools). Soul Stack accepts all variants, because issuing a token and accepting it are API/MCP operations; the method of physical delivery is the operator's choice.

- **Via the scenario step `core.bootstrap.delivered` (the target variant, inside Soul Stack, [ADR-063](../adr/0063-bootstrap-token-delivery.md)).** A Keeper-side core delivery module: over SSH it puts the per-VM token into a file on the host (`token_path`, default `/etc/soul/token`; the token goes through STDIN, not argv), does the redeem right there — `test -e <seed-cert> || SOUL_BOOTSTRAP_TOKEN="$(cat <token_path>)" soul init` — and optionally activates the unit (`daemon-reload && enable && start`). Two modes: **token-only** (the setup already put cloud-init in place) and **full-install** (the whole setup — CA/soul.yml/unit/binary — over the same SSH channel, for platforms without cloud-init userdata). A typical application is after `core.cloud.created` in a provision scenario ([keeper/cloud.md](../keeper/cloud.md)); the module specification is [keeper/modules.md → core.bootstrap.delivered](../keeper/modules.md#corebootstrapdelivered). The advantage: a single audit, RBAC, and logs in Keeper, no third-party tools.
- **An Ansible role.** The recommended official role will live in a separate repository; it accepts the token and the Keeper address as variables. Good for those for whom Ansible is a corporate standard.
- **Ordinary SSH/SCP** — the operator puts the token and the `soul` binary in place manually or with their own script.
- **CI/CD pipelines** — the token is taken from the CI secret store, delivered via a terraform provisioner or a bootstrap script.
- **Cloud-init / image baking** — for ephemeral VMs, where the token is injected at the instance-creation stage.

## Protections on the Soul Stack side

- **Token TTL** — short by default (24h), configurable by the operator.
- **One-time use** — the token is burned on the first successful CSR (the SQL transaction above).
- **Binding to a specific SID** — the token is valid only for the FQDN it was issued for.
- **Audit** — every issue and use of a token is logged in Keeper.

Additional protections (binding to an IP/CIDR, requiring cloud-metadata proof, manual approval) — see the open question "Утечка SoulSeed-токенов" in [architecture.md → Open questions](../architecture.md#открытые-вопросы).

## See also

- [identity.md](identity.md) — the `bootstrap_tokens` registry, the `soul_seeds` registry, Soul statuses.
- [connection.md](connection.md) — the algorithm by which `soul init` connects to Keeper.
- [config.md](config.md) — where `paths.seed` and `tls.ca` are on the host.
- [architecture.md → Soul lifecycle and the soul registry](../architecture.md#жизненный-цикл-soul-и-реестр-душ) — the architectural overview of onboarding and the registries.
- [architecture.md → Delivering the SoulSeed token to the host](../architecture.md#доставка-soulseed-токена-на-хост) — a short enumeration of delivery methods.
