# example-cloud-bootstrap

Demo service for onboarding **ready-made** VMs under [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md): the keeper mints a bootstrap token per host and waits for the Souls to come online.

## What it demonstrates

### `scenario/existing-vm` — the whole onboarding chain

1. **`bootstrap`** — `core.bootstrap.issued`: one token per FQDN/SID, in a single Postgres transaction, into `register.bootstrap.hosts[].bootstrap_token`. Minting lives in the Keeper because it is an INSERT of a pending Soul plus the hash of the token, atomically; nothing else can do it.
2. **`install`** — [`core.ssh.run`](../../../docs/module/core/ssh/README.md): the keeper-side transport to a host that has no agent on it yet. The engine opens the session and enforces the secret floor; **the commands are this service's**, because installation is different at every site — a prepared image, a cloud-init cycle, somebody's own shell, a foreign fleet manager — and that opinion is what [NIM-834](../../../docs/adr/0063-bootstrap-token-delivery.md#amendment-2026-09-09--delivered-is-removed-installing-a-host-is-site-specific-nim-834) took out of `core.bootstrap.delivered`. [NIM-849](../../../docs/adr/0063-bootstrap-token-delivery.md#amendment-2026-09-12--the-transport-comes-back-without-the-policy-nim-849) brought the transport back without it.
3. **`onboarded`** — `core.soul.registered` with `await_online: true`, the blocking onboarding barrier, plus a Soulprint refresh.

**Step 2 is the example's real subject.** Fork this service and rewrite it for your own platform: change the binary URL, the package manager, the paths. What you must NOT drop is below — the engine refuses one of those mistakes and cannot see the other two.

## What the install step must do

Three requirements, each one paid for by a live run, and **none of them satisfied by writing a file with the token in it**. Miss one and the host never onboards, silently — and the failure you then debug is the barrier in step 3, minutes and one wrong subject away from the cause.

1. **Redeem the token, do not merely place it.** There is no soul-side pickup of a token file; the seed is created ONLY by `soul init`:

   ```sh
   test -e /var/lib/soul-stack/seed/current/cert.pem || \
     SOUL_BOOTSTRAP_TOKEN="$(cat /etc/soul/token)" /usr/local/bin/soul init --config /etc/soul/soul.yml
   ```

   The seed-cert guard is mandatory: a bootstrap token is single-use, so an unguarded retry after a successful redeem fails the host on the second run. Take the guard path from `soulinstall.SeedCertPath` rather than hardcoding it — it is pinned against soul's own seed layout by a test. **The engine cannot check this one for you.**

2. **The token travels in STDIN, never in argv.** A command argument is visible in `ps`, in `audit.log` and in journald **on the host itself**. This is the one requirement `core.ssh.run` **enforces**: `stdin_from: bootstrap_token` names the host field and the module feeds it to the process, and a `run:` holding a declared secret — or quoting the value of a secret-named host field — is refused before the connect rather than masked afterwards. Note that the redeem above carries the literal unexpanded `$(cat …)`, which the host's own subshell expands, so the token is not in the caller's argv either.

3. **Activate with `daemon-reload && enable && start`,** not a bare `start`. Also a live finding: without `enable` the unit did not come up after a VM reboot. **The engine cannot check this one either.**

What the module does give you for free: authorization before the connect (fail-closed), an ephemeral keypair whose private half never leaves the Keeper, CA-signed host-cert verification instead of trust-on-first-use, no command output in the step's register, failing the whole step on any host rather than committing state over a half-built group, a bounded named wait for a freshly created host to become reachable, and skipping a host that was already onboarded before this run.

## What an installed host must look like

The canonical description — paths, permissions, the `soul.yml` body (note that `event_stream_port` is a **separate port** from `bootstrap_endpoint`; deriving both from one address was a live blocker) and the systemd unit — is [`keeper/internal/soulinstall`](../../../keeper/internal/soulinstall). Neither of its renderers has a caller: they describe an outcome to match, not a procedure to run, and the procedure is yours.

## Prerequisites

Nothing needs to exist in Postgres: the scenario takes the SIDs as input.

## See also

- [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md) — bootstrap tokens; the amendment 2026-09-09 is the normative version of the requirements above, and 2026-09-12 is the transport that runs them.
- [docs/module/core/ssh](../../../docs/module/core/ssh/README.md) — the `core.ssh.run` reference: params, the secret floor, the transports.
- [ADR-066](../../../docs/adr/0066-teleport-onboarding-profile.md) — the live-proven environment constraints for reaching a fresh VM through Teleport (a bot identity without `pin_source_ip`, `alpn_upgrade` behind an L7-TLS balancer, an active external IP before enroll).
- [ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md) — the `await_online` barrier of step 3.
