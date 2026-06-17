# official.nginx-vhost

Idempotent-управление nginx vhost: render config + `nginx -t` validate ДО write + symlink `sites-enabled/` + `nginx -s reload`.

Полная документация — [`soul-mod-official-nginx-vhost/README.md`](https://github.com/co-cy/soul-stack-plugins/blob/main/soul-mod-official-nginx-vhost/README.md) в companion-репо.

## Кратко

- **States:** `present` (vhost-config записан + symlink + reload), `absent` (config-файл и symlink удалены + reload).
- **Render:** Go `text/template`, минимальный набор директив (`listen`, `server_name`, `root`, `location`-блоки с `proxy_pass`/`root`/`try_files`).
- **Validate перед write:** pending-файл `<config>.soulstack-pending` → `nginx -t` → атомарный move. При syntax-error — final-write не происходит, reload не выполняется, failure-событие со stderr-у nginx.
- **Plan + PlanReadSafe** ([ADR-031](../../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): сравнение content + symlink target, side-effects нет.

## См. также

- [Каталог official-плагинов](../README.md).
- [ADR-016 amendment 2026-05-27 SDK-2](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack).
