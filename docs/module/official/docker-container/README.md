# official.docker-container

Idempotent docker container management via docker-CLI: create / start / stop / rm + drift-detect by image/env/ports/volumes/networks/restart_policy.

Full documentation - [`soul-mod-official-docker-container/README.md`](https://github.com/souls-guild/soul-stack-plugins/blob/main/soul-mod-official-docker-container/README.md) in the companion repo.

## Briefly

- **States:** `running` / `stopped` / `absent` (three-state is the natural semantics of the container, not `present`/`absent`).
- **Probe:** `docker inspect <name>` (JSON-parse `Config.Image`/`State.Running`/`HostConfig.Binds`/`HostConfig.PortBindings`/`NetworkSettings.Networks`/`HostConfig.RestartPolicy`).
- **Drift-detect:** image/command/env/ports/volumes/networks/restart_policy → re-creation of `stop → rm → create → start` (container ID changes, downtime ~seconds).
- **Plan + PlanReadSafe** ([ADR-031](../../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): `docker inspect` + computeDrift, no stop/rm/create.
- **Backend:** docker-CLI (post-MVP — docker-go-sdk).

## See also

- [Official plugins directory](../README.md).
- [ADR-016 amendment 2026-05-27 SDK-2](../../../adr/0016-parity-license.md).
