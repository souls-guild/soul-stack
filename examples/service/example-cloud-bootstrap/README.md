# example-cloud-bootstrap

Demo service for onboarding **ready-made** VMs under [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md): the keeper issues a bootstrap token per host, installs Soul with it, and waits for the Souls to come online.

## What it demonstrates

### `scenario/existing-vm` — the CloudDriver-independent chain

1. **`bootstrap`** — `core.bootstrap.issued`: one token per FQDN/SID, in a single transaction, into `register.bootstrap.hosts[].bootstrap_token`.
2. **`delivered`** — `core.bootstrap.delivered` with `install: true`: installs the Soul binary and hands each host its own token over the configured Teleport transport. Without it, Soul on the VM does not know which token to present to the Bootstrap RPC.
3. **`onboarded`** — `core.soul.registered` with `await_online: true`, the blocking onboarding barrier, plus a Soulprint refresh.

## What it no longer demonstrates

The `create` and `create-inline` scenarios created the VMs themselves through a `core.cloud.created` step and the CloudDriver plugin contract. Both went away with that contract in NIM-761: a cloud plugin is now an ordinary `side: keeper` SoulModule with its own module address, its own params and its own credentials, so there is no engine-side cloud step for an example to show. What is left here is the half that never depended on the contract — and the half a fleet actually reuses, because a VM created by any means at all is onboarded exactly this way.

## Prerequisites

In `keeper.yml`:

```yaml
cloud_init:
  bootstrap_endpoint: lb.keeper.example:9442
  tls_ca_ref:         vault:secret/keeper/ca       # KV: {ca: <PEM>}
  soul_binary_url:    https://artifacts.example/soul/v1.0.0/soul
  soul_version:       v1.0.0

push:
  # ... SSH token-delivery settings (see docs/keeper/push.md)
```

Nothing needs to exist in Postgres: the scenario takes the SIDs as input.

## See also

- [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md) — bootstrap-token delivery, the normative decision behind steps 1–2.
- [ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md) — the `await_online` barrier of step 3.
- [keeper/internal/cloudinit/](../../../keeper/internal/cloudinit/) — the cloud-config render the `install: true` path still uses.
