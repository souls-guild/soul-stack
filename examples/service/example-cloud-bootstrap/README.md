# example-cloud-bootstrap

Demo service for onboarding **ready-made** VMs under [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md): the keeper mints a bootstrap token per host and waits for the Souls to come online.

## What it demonstrates

### `scenario/existing-vm` — the onboarding chain, and the hole in the middle of it

1. **`bootstrap`** — `core.bootstrap.issued`: one token per FQDN/SID, in a single Postgres transaction, into `register.bootstrap.hosts[].bootstrap_token`. Minting lives in the Keeper because it is an INSERT of a pending Soul plus the hash of the token, atomically; nothing else can do it.
2. **install the host — NOT a step here.** The Soul binary has to be on the host and the token has to be redeemed. `core.bootstrap.delivered` used to do that and was [removed in NIM-834](../../../docs/adr/0063-bootstrap-token-delivery.md#amendment-2026-09-09--delivered-is-removed-installing-a-host-is-site-specific-nim-834): installation is different at every site — a prepared image, a cloud-init cycle, somebody's own shell, a foreign fleet manager — and there is no portable way to do it from the engine.
3. **`onboarded`** — `core.soul.registered` with `await_online: true`, the blocking onboarding barrier, plus a Soulprint refresh.

**Run it as it stands and it will not finish.** Step 1 mints tokens, nobody redeems them, and step 3 blocks until the run timeout. That is not an oversight in the example — it is the shape of onboarding today, and the example shows it rather than hiding it behind a step that no longer exists. To make it complete, put your own installation between steps 1 and 2, driven by `register.bootstrap.hosts`.

## What your installer must do

Three requirements, each one paid for by a live run, and **none of them satisfied by writing a file with the token in it**. An installer that misses one produces a host that never onboards, silently — and the failure you then debug is the barrier in step 3, minutes and one wrong subject away from the cause.

1. **Redeem the token, do not merely place it.** There is no soul-side pickup of a token file; the seed is created ONLY by `soul init`:

   ```sh
   test -e /var/lib/soul-stack/seed/current/cert.pem || \
     SOUL_BOOTSTRAP_TOKEN="$(cat /etc/soul/token)" /usr/local/bin/soul init --config /etc/soul/soul.yml
   ```

   The seed-cert guard is mandatory: a bootstrap token is single-use, so an unguarded retry after a successful redeem fails the host on the installer's second run. Take the guard path from `soulinstall.SeedCertPath` rather than hardcoding it — it is pinned against soul's own seed layout by a test.

2. **The token travels in STDIN, never in argv.** A command argument is visible in `ps`, in `audit.log` and in journald **on the host itself**. Write it as `cat > /etc/soul/token` fed from stdin, and note that the redeem above carries the literal unexpanded `$(cat …)`, which the host's own subshell expands — so the token is not in the caller's argv either. The same floor applies to the CA PEM and `soul.yml`.

3. **Activate with `daemon-reload && enable && start`,** not a bare `start`. Also a live finding: without `enable` the unit did not come up after a VM reboot.

What the removed module also gave for free, and an installer now owns: authorization before the connect (fail-closed), an ephemeral keypair whose private half never leaves the Keeper, CA-signed host-cert verification instead of trust-on-first-use, keeping the token out of the step's own output, failing the whole step on any host rather than committing state over a half-built group, and a bounded, named wait for a freshly created host to become reachable at all.

## What an installed host must look like

The canonical description — paths, permissions, the `soul.yml` body (note that `event_stream_port` is a **separate port** from `bootstrap_endpoint`; deriving both from one address was a live blocker) and the systemd unit — is [`keeper/internal/soulinstall`](../../../keeper/internal/soulinstall). Neither of its renderers has a caller any more; they are kept precisely so an installer has a result to match rather than a shape to guess.

## Prerequisites

Nothing needs to exist in Postgres: the scenario takes the SIDs as input.

## See also

- [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md) — bootstrap tokens; the amendment 2026-09-09 is the normative version of the requirements above.
- [ADR-066](../../../docs/adr/0066-teleport-onboarding-profile.md) — the live-proven environment constraints for reaching a fresh VM through Teleport (a bot identity without `pin_source_ip`, `alpn_upgrade` behind an L7-TLS balancer, an active external IP before enroll).
- [ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md) — the `await_online` barrier of step 3.
