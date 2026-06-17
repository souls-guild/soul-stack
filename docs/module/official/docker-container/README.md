# official.docker-container

Idempotent-управление docker-контейнером через docker-CLI: create / start / stop / rm + drift-detect по image/env/ports/volumes/networks/restart_policy.

Полная документация — [`soul-mod-official-docker-container/README.md`](https://github.com/co-cy/soul-stack-plugins/blob/main/soul-mod-official-docker-container/README.md) в companion-репо.

## Кратко

- **States:** `running` / `stopped` / `absent` (трёхсостоянка — естественная семантика контейнера, не `present`/`absent`).
- **Probe:** `docker inspect <name>` (JSON-parse `Config.Image`/`State.Running`/`HostConfig.Binds`/`HostConfig.PortBindings`/`NetworkSettings.Networks`/`HostConfig.RestartPolicy`).
- **Drift-detect:** image/command/env/ports/volumes/networks/restart_policy → пересоздание `stop → rm → create → start` (container ID меняется, downtime ~секунды).
- **Plan + PlanReadSafe** ([ADR-031](../../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): `docker inspect` + computeDrift, без stop/rm/create.
- **Backend:** docker-CLI (post-MVP — docker-go-sdk).

## См. также

- [Каталог official-плагинов](../README.md).
- [ADR-016 amendment 2026-05-27 SDK-2](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack).
