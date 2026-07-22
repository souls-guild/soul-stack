# official.nginx-vhost

Idempotent control nginx vhost: render config + `nginx -t` validate BEFORE write + symlink `sites-enabled/` + `nginx -s reload`.

Full documentation - [`soul-mod-official-nginx-vhost/README.md`](https://github.com/souls-guild/soul-stack-plugins/blob/main/soul-mod-official-nginx-vhost/README.md) in the companion repo.

## Briefly

- **States:** `present` (vhost-config written + symlink + reload), `absent` (config file and symlink deleted + reload).
- **Render:** Go `text/template`, minimal set of directives (`listen`, `server_name`, `root`, `location`-blocks with `proxy_pass`/`root`/`try_files`).
- **Validate before write:** pending file `<config>.soulstack-pending` → `nginx -t` → atomic move. With syntax-error, final-write does not occur, reload is not executed, failure event occurs on nginx stderr.
- **Plan + PlanReadSafe** ([ADR-031](../../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): comparison content + symlink target, no side-effects.

## See also

- [Official plugins directory](../README.md).
- [ADR-016 amendment 2026-05-27 SDK-2](../../../adr/0016-parity-license.md).
