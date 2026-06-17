# Каталог official-плагинов

`official.*` — namespace для плагинов, которые поставляются и поддерживаются командой Soul Stack, в отличие от core-модулей (`core.*`, статически встроены в `soul`-бинарь) и community-плагинов (`community.*`, third-party).

Бинари — `soul-mod-official-<name>`. Живут в companion-репо [`soul-stack-plugins/`](https://github.com/co-cy/soul-stack-plugins) (Apache 2.0, [ADR-016](../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)).

## Pilot (SDK-2, 2026-05-27)

Первые 3 модуля — pattern-fixture для тиража SDK-3.

| Модуль | States | Назначение |
|---|---|---|
| [`official.postgres-user`](postgres-user/README.md) | `present` / `absent` | PostgreSQL ROLE (CREATE / ALTER / DROP) через `pgx/v5` + probe `pg_roles`. |
| [`official.nginx-vhost`](nginx-vhost/README.md) | `present` / `absent` | nginx vhost-config: render + `nginx -t` validate + write + symlink + reload. |
| [`official.docker-container`](docker-container/README.md) | `running` / `stopped` / `absent` | docker-контейнер через docker-CLI + drift-detect (image/env/ports/volumes/networks). |

Каждый pilot реализует Plan + `PlanReadSafe` ([ADR-031 Scry](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) — drift-detect на `dry_run: true` без мутации хоста.

## Полный список (планируется тиражом SDK-3)

См. [ADR-016 amendment SDK-2 (2026-05-27)](../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack).

## Тестовое покрытие

Pattern-fixture (зафиксирован SDK-2 для тиража SDK-3):

- **L0** — in-memory fake-runner + fake stream через `grpc.ServerStreamingServer[ApplyEvent]`. Hermetic, без сети/диска/docker.
- **L1** (build-tag `integration`) — testcontainers или real-daemon. Full lifecycle.
- **L3b** (build-tag `live`) — privileged Debian-12 + real Soul-host + плагин. На SDK-2 — skeleton `t.Skip`, ждёт Vigil-extension L3b-harness в core-repo.

## Soul-стороннее использование

```yaml
# destiny/destiny.yml
- module: official.postgres-user
  state: present
  params:
    dsn: "${ vault('secret/keeper/pg-admin#dsn') }"
    username: appuser
    password: "${ vault('secret/keeper/app-credentials#pg_password') }"
    createdb: true
```

Keeper-side vault-resolve ([ADR-010](../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) подставляет реальные значения ДО `ApplyRequest` — плагин видит резолвлённое.

## См. также

- [ADR-016](../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — стратегия parity + open core / freemium.
- [ADR-020](../../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle) — plugin-инфраструктура (manifest/handshake/lifecycle).
- [ADR-026](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) — Sigil (целостность official-бинарей под подписью Архонта).
- [ADR-031](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile) — Scry (read-safe Plan marker, default-deny на `dry_run`).
- [Plugin author guide](https://github.com/co-cy/soul-stack-plugins/blob/main/docs/module-author-guide.md).
