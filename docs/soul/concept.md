# Soul — concept

The `soul` binary is the agent daemon on a managed host. It receives from Keeper commands of the form "apply such-and-such Destiny with such-and-such Essence", executes module steps, and returns events and Soulprint back. In the SaltStack vocabulary it is a minion; in the Soul Stack vocabulary it is a Soul, a single soul in the `souls` registry.

## Where Soul sits in the overall picture

```
                        Keeper cluster
                              │
              ┌───────────────┴───────────────┐
              │                               │
   gRPC bidi (mTLS)                       SSH session
              │                               │
              ▼                               ▼
        ┌──────────┐                    ┌──────────┐
        │   soul   │                    │   soul   │
        │ (daemon, │                    │ (oneshot,│
        │  pull)   │                    │   push)  │
        └──────────┘                    └──────────┘
        transport: agent                transport: ssh
        record in souls                 record in souls
        + soul_seeds                    no soul_seeds
```

On the left is Keeper (the central server, see [architecture.md → Topology](../architecture.md)). To the right of Keeper is the Destiny / Service / Incarnation registry in Postgres. Soul sits "below Destiny": it receives an atomic brick and applies it on a single host through [modules](../architecture.md).

The detailed definition of the binaries is in [architecture.md → Binary roles](../architecture.md).

## Two transports

Soul is reachable through two modes of command delivery from Keeper — but it is the **same binary**, differing only in the launch conditions and the identity.

| | `transport: agent` (pull) | `transport: ssh` (push) |
|---|---|---|
| Launch | systemd daemon, holds the stream continuously | one-shot `soul apply`, comes up on each run via an SSH session |
| Identity | SoulSeed (mTLS pair), a `soul_seeds` record | SSH credentials from the provider (Vault SSH CA / static / Teleport); `soul_seeds` is **not used** for a push host |
| Connection initiator | Soul → Keeper | Keeper → Soul (SSH) |
| Run-plan delivery | `ApplyRequest` over the live gRPC stream, response — `TaskEvent`/`RunResult` back into the stream | `ApplyRequest` (protojson) into the stdin of `soul apply`, response — NDJSON `TaskEvent` + `RunResult` into stdout |
| DB record | `souls` with `transport: agent` | `souls` with `transport: ssh` — the **same table**, a different mode |
| Module cache on the host | in `/var/lib/soul-stack/{bin,modules}/` (see [modules.md](modules.md)) | in `/var/lib/soul-stack/{bin,modules}/` (see [modules.md](modules.md)) |
| Connection to Keeper | the `priority + failback` algorithm (see [connection.md](connection.md)) | the algorithm does not apply — Keeper reaches the host itself |

The push mode is detailed in [keeper/push.md](../keeper/push.md). Push is not used as a standalone "agent-less mode" with its own binary; it is a different launch mode of the same `soul`.

A push host can migrate to pull at any time (a daemon is installed, the operator changes `transport`) — the `souls` record remains, history is not lost.

## Principles

- **No outbound traffic beyond Keeper.** On its own initiative Soul reaches only its Keeper cluster (and the resources explicitly allowed within a Destiny). No telemetry calls, no automatic updates that bypass Keeper.
- **One binary — two modes.** The pull daemon and the push one-shot are `soul` with a different entry point (`soul` as a service vs `soul apply` as a command). The module set, step execution, and event format are identical.
- **One `souls` record per host, a different `transport`.** Push and pull are a field in `souls`, not separate entities. This yields a single registry, a single audit, a single RBAC, and the ability to switch modes without losing history.
- **Identity is decoupled from the transport.** SoulSeed is needed only by the pull mode (there Soul initiates the connection and must authorize itself). In push, the host identity is its SSH side; see [keeper/push.md → SSH authentication](../keeper/push.md).
- **The local admin endpoint is not defined yet.** Whether a separate socket/port on the host is needed for status, force-resync, or a Soulprint dump is an open question (see [architecture.md → Open questions](../architecture.md), the item "Local admin endpoint on Soul").

## See also

- [identity.md](identity.md) — the `souls` / `soul_seeds` / `bootstrap_tokens` registries, statuses.
- [onboarding.md](onboarding.md) — how a Soul becomes `connected`.
- [connection.md](connection.md) — `priority + failback` for the pull mode.
- [config.md](config.md) — the `soul.yml` format.
- [modules.md](modules.md) — the module cache on the host.
- [architecture.md → Binary roles](../architecture.md) — the definition of `soul` in the context of the three binaries.
- [architecture.md → Push mode](../architecture.md) — the push specification.
- [architecture.md → Module model](../architecture.md) — what Soul executes.
