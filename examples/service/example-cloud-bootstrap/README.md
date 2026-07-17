# example-cloud-bootstrap

Demo service under ADR-017(h) amendment 2026-05-27 (B-flat): cloud-init bootstrap of a new VM.

## What it demonstrates

A two-step `create` scenario:

1. **`provision`** — `core.cloud.provisioned` (`state: created`) with `generate_userdata: true`. Keeper renders cloud-config userdata from `keeper.yml::cloud_init` (PEM CA from Vault + soul binary URL); the CloudDriver plugin creates `count` VMs with this userdata. After Create — Keeper issues a per-VM bootstrap token and puts it into `register.provision.hosts[].bootstrap_token`.
2. **`push_token`** — `keeper.push.bootstrap_token` (via the SSH provider `soul-ssh-*`) delivers the per-VM token to each VM. Without this, Soul on the VM doesn't know which token to present to the Bootstrap RPC.

Cloud-init userdata **does NOT carry tokens** — the cloud provider API stores userdata in plaintext metadata (security floor).

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

In Postgres:
- Provider `aws-prod` created via OpenAPI/MCP (`POST /v1/providers`).
- Profile `example-tiny` created via `POST /v1/profiles`.

## See also

- [ADR-017(h) amendment 2026-05-27](../../../docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read) — normative decision.
- [docs/keeper/cloud.md → Cloud-init bootstrap (MVP)](../../../docs/keeper/cloud.md#cloud-init-bootstrap-mvp) — operator documentation.
- [keeper/internal/cloudinit/](../../../keeper/internal/cloudinit/) — render implementation.
