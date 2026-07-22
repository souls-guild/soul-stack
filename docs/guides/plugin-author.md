# How to write your own SoulModule plugin - index

**SoulModule plugin** is a separate executable binary `soul-mod-<namespace>-<name>`, which the `soul` daemon (or Keeper in push mode) launches as a sub-process and communicates with it via gRPC over a Unix socket. The plugin implements **the same interface** `SoulModule` (Validate / Plan / Apply) from [`sdk/module`](../../sdk/module/module.go) as the built-in core modules - the only difference is in the packaging: core modules are statically compiled into `soul`, the plugin is delivered to the host separately and cached.

**When to write a plugin, not a core / scenario:** the resource is not among the core modules ([ADR-015](../adr/0015-core-modules-mvp.md)), the logic is **reused** and **typed** (you need an input schema, validation, drift-detection), and you don't want to edit the core - the plugin lives in its own repository under its own license (open-core, [ADR-016](../adr/0016-parity-license.md)). The one-time command on the host is `core.exec.run` / `core.cmd.shell` in the script; template file - `core.file.rendered`. Inline vs Outline Boundary - Recommendation [ADR-009](../adr/0009-scenario-dsl.md).

## Author's authoritative guide - in the companion repo

Plugins for [ADR-016](../adr/0016-parity-license.md) live in the companion repository `soul-stack-plugins` - there is their home and **the author's standard step-by-step guide**:

→ **[`soul-stack-plugins/docs/module-author-guide.md`](https://github.com/souls-guild/soul-stack-plugins/blob/main/docs/module-author-guide.md)**

It covers: architectural overview and handshake, `soul-lint plugin-init <namespace>/<name>` scaffold, Validate / Plan (Scry-marker `PlanReadSafe`) / Apply contracts with idempotency invariant, manifest format (`spec.states.<state>.input`, `required_capabilities`, `side_effects`), secret parameters (`pattern: "^vault:.*"`), `ErrandReadSafe`, test levels L0 / L1 / L3b, Sigil-trust before production, official vs community publication. Skeleton scaffold - `soul-lint plugin-init`.

Do not duplicate this tutorial here: code examples, scaffold tree, step-by-step walk-through are there, and only there they are updated.

## What is relevant to the author in this (core) repository

The plugin pulls core as a Go dependency; The core repo contains artifacts on which the guide relies:

- **SDK interface** - [`sdk/module/module.go`](../../sdk/module/module.go): interface `SoulModule` (Validate / Plan / Apply), embeddable `module.BaseModule` (no-op defaults + safe default-deny by markers), `module.Serve(impl)` (encapsulates handshake).
- **Proto-contract** - [`proto/plugin/v1/`](../../proto/plugin/v1/): `soulmodule.proto` (RPC Validate / Plan / Apply), `manifest.proto`, `handshake.proto`. This is a separate go.mod submodule - the author of the plugin uses only this, without `keeper`/`soul`.
- **[ADR-011](../adr/0011-go-layout.md)** - Go code layout: why `proto/plugin` is a separate go.mod and what exactly the author of the plugin is pulling.
- **[ADR-016](../adr/0016-parity-license.md)** — parity strategy and license: namespace scheme (`core` / `official` / third parties), open-core, prohibition of foreign-runtime wrappers.
- **[ADR-026](../adr/0026-sigil.md)** - Sigil: Keeper-signed digest plugin integrity index, verify before exec.

Regulatory plugin infrastructure spec (manifest, handshake, lifecycle, integrity, all three kinds) - [docs/keeper/plugins.md](../keeper/plugins.md) and [ADR-020](../adr/0020-plugin-infrastructure.md). The skeleton of a custom module to illustrate the format is [`examples/module/`](../../examples/module/).
