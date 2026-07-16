# example-cloud-bootstrap

Demo-сервис под ADR-017(h) amendment 2026-05-27 (B-flat): cloud-init bootstrap новой VM.

## Что показывает

Двухшаговый scenario `create`:

1. **`provision`** — `core.cloud.provisioned` (`state: created`) с `generate_userdata: true`. Keeper рендерит cloud-config userdata из `keeper.yml::cloud_init` (PEM CA из Vault + URL soul-бинаря), CloudDriver-плагин создаёт `count` VM с этой userdata. После Create — Keeper выписывает per-VM bootstrap-токен и кладёт в `register.provision.hosts[].bootstrap_token`.
2. **`push_token`** — `keeper.push.bootstrap_token` (через SSH-провайдера `soul-ssh-*`) доставляет per-VM-токен на каждую VM. Без этого Soul на VM не знает, какой токен предъявить на Bootstrap-RPC.

Cloud-init userdata **НЕ несёт токены** — cloud-provider API хранит userdata в plaintext metadata (security floor).

## Prerequisites

В `keeper.yml`:

```yaml
cloud_init:
  bootstrap_endpoint: lb.keeper.example:9442
  tls_ca_ref:         vault:secret/keeper/ca       # KV: {ca: <PEM>}
  soul_binary_url:    https://artifacts.example/soul/v1.0.0/soul
  soul_version:       v1.0.0

push:
  # ... настройки SSH-доставки токенов (см. docs/keeper/push.md)
```

В Postgres:
- Provider `aws-prod` создан через OpenAPI/MCP (`POST /v1/providers`).
- Profile `example-tiny` создан через `POST /v1/profiles`.

## См. также

- [ADR-017(h) amendment 2026-05-27](../../../docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read) — нормативное решение.
- [docs/keeper/cloud.md → Cloud-init bootstrap (MVP)](../../../docs/keeper/cloud.md#cloud-init-bootstrap-mvp) — оператор-документация.
- [keeper/internal/cloudinit/](../../../keeper/internal/cloudinit/) — реализация рендера.
