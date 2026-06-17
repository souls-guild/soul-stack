# Как написать свой SoulModule-плагин — указатель

**SoulModule-плагин** — это отдельный исполняемый бинарь `soul-mod-<namespace>-<name>`, который `soul`-демон (или Keeper в push-режиме) запускает как sub-process и общается с ним по gRPC поверх Unix-socket. Плагин реализует **тот же интерфейс** `SoulModule` (Validate / Plan / Apply) из [`sdk/module`](../../sdk/module/module.go), что и встроенные core-модули, — разница только в упаковке: core-модули статически вкомпилированы в `soul`, плагин доставляется на хост отдельно и кешируется.

**Когда писать плагин, а не core / scenario:** ресурса нет среди core-модулей ([ADR-015](../adr/0015-core-modules-mvp.md)), логика **переиспользуемая** и **типизированная** (нужна input-схема, валидация, drift-detection), и ты не хочешь править ядро — плагин живёт в своём репозитории под своей лицензией (open-core, [ADR-016](../adr/0016-parity-license.md)). Разовая команда на хосте — это `core.exec.run` / `core.cmd.shell` в сценарии; файл с шаблоном — `core.file.rendered`. Граница «инлайн vs вынести» — рекомендация [ADR-009](../adr/0009-scenario-dsl.md).

## Авторитетный гайд автора — в companion-репо

Плагины по [ADR-016](../adr/0016-parity-license.md) живут в companion-репозитории `soul-stack-plugins` — там их дом и **нормативный пошаговый гайд автора**:

→ **[`soul-stack-plugins/docs/module-author-guide.md`](https://github.com/co-cy/soul-stack-plugins/blob/main/docs/module-author-guide.md)**

Он покрывает: архитектурный обзор и handshake, скаффолд `soul-lint plugin-init <namespace>/<name>`, контракты Validate / Plan (Scry-marker `PlanReadSafe`) / Apply с инвариантом идемпотентности, формат manifest (`spec.states.<state>.input`, `required_capabilities`, `side_effects`), secret-параметры (`pattern: "^vault:.*"`), `ErrandReadSafe`, уровни тестов L0 / L1 / L3b, Sigil-trust перед production, публикацию official vs community. Скаффолд скелета — `soul-lint plugin-init`.

Не дублируй этот туториал здесь: примеры кода, дерево скаффолда, пошаговый walk-through — там, и только там обновляются.

## Что релевантно автору в этом (core) репозитории

Плагин тянет core как Go-зависимость; в core-репо живут артефакты, на которые гайд опирается:

- **SDK-интерфейс** — [`sdk/module/module.go`](../../sdk/module/module.go): интерфейс `SoulModule` (Validate / Plan / Apply), embeddable `module.BaseModule` (no-op-дефолты + безопасный default-deny по marker-ам), `module.Serve(impl)` (инкапсулирует handshake).
- **Proto-контракт** — [`proto/plugin/v1/`](../../proto/plugin/v1/): `soulmodule.proto` (RPC Validate / Plan / Apply), `manifest.proto`, `handshake.proto`. Это отдельный go.mod-подмодуль — автор плагина тянет только его, без `keeper`/`soul`.
- **[ADR-011](../adr/0011-go-layout.md)** — раскладка Go-кода: почему `proto/plugin` вынесен отдельным go.mod и что именно тянет автор плагина.
- **[ADR-016](../adr/0016-parity-license.md)** — parity-стратегия и лицензия: namespace-схема (`core` / `official` / третьи стороны), open-core, запрет wrapper-ов над Ansible.
- **[ADR-026](../adr/0026-sigil.md)** — Sigil: Keeper-signed digest-индекс целостности плагина, verify до exec.

Нормативная спека плагин-инфраструктуры (manifest, handshake, lifecycle, integrity, все три kind-а) — [docs/keeper/plugins.md](../keeper/plugins.md) и [ADR-020](../adr/0020-plugin-infrastructure.md). Скелет custom-модуля для иллюстрации формата — [`examples/module/`](../../examples/module/).
