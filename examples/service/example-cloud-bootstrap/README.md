# example-cloud-bootstrap

Demo service under ADR-017(h) amendment 2026-05-27 (B-flat): cloud-init bootstrap of a new VM.

## What it demonstrates

Two scenarios that create the same VMs from **two different sources of the driver tuple** (see [docs/keeper/cloud.md → Two sources for the driver](../../../docs/keeper/cloud.md#two-sources-for-the-driver-registry-or-inline)):

### `scenario/create` — registry source

The provider and the profile are rows in the `providers`/`profiles` registries; the step names them:

1. **`provision`** — `core.cloud.created` with `provider:`/`profile:` (row names, not plugin names). In B-flat (`self_onboard: false`, the default) it renders cloud-config userdata from `keeper.yml::cloud_init` (PEM CA from Vault + soul binary URL); the CloudDriver plugin creates `count` VMs with it. After Create, Keeper issues a per-VM bootstrap token into `register.provision.hosts[].bootstrap_token`.
2. **`deliver`** — `core.bootstrap.delivered` (via the SSH provider `soul-ssh-*`) delivers the per-VM token to each VM. Without it, Soul on the VM doesn't know which token to present to the Bootstrap RPC. Skipped when `self_onboard: true` — then the VM picks its own token out of userdata in its first cloud-init cycle (Variant T).
3. **`onboarded`** — `core.soul.registered` with `await_online: true`, the blocking onboarding barrier.

In B-flat the cloud-init userdata **does NOT carry tokens** — the cloud provider API stores userdata in plaintext metadata (security floor). `self_onboard: true` deliberately trades that away for the test stand.

### `scenario/create-inline` — inline source, zero registry rows

The same create, with the whole tuple in the step instead: `driver` (plugin alias), `credentials` (a `vault:` ref), `region`, `fqdn_suffix`, and `profile` as the VM **spec itself** rather than the name of a row. Everything but the credentials ref comes from `vars/00-base.yaml`, so the three topologies (`standalone` / `sentinel` / `cluster`) are ordinary service data in git — a matrix the profiles registry cannot express, because each combination would need a row created by hand and a service repo cannot ship rows.

`credentials` must be written **literally**, not through `${ … }`: the vault-resolve phase walks the raw params and runs before the CEL phase (ADR-010), so a ref produced by an expression would never be resolved.

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

In Postgres — for `scenario/create` **only**:
- Provider `aws-prod` created via OpenAPI/MCP (`POST /v1/providers`).
- Profile `example-tiny` created via `POST /v1/profiles`.

`scenario/create-inline` needs neither: it wants only the KV path its `credentials:` ref names (`secret/cloud/wb-dev` here) to hold the driver's credential map.

## See also

- [ADR-017(h) amendment 2026-05-27](../../../docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read) — normative decision.
- [docs/keeper/cloud.md → Cloud-init bootstrap (MVP)](../../../docs/keeper/cloud.md#cloud-init-bootstrap-mvp) — operator documentation.
- [keeper/internal/cloudinit/](../../../keeper/internal/cloudinit/) — render implementation.
