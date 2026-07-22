# Destiny - index

Documentation on destiny - the atomic declarative brick of the Soul Stack ("how to bring one host to the desired state").

## Where to start

| Document | What about |
|---|---|
| [concept.md](concept.md) | What is destiny, how does it relate to service / scenario / module, how does it differ from its neighbors. |
| [manifest.md](manifest.md) | Destiny-repo folder layout, format `destiny.yml` (manifest only), rules about `version:` and `tasks:`. |
| [tasks.md](tasks.md) | **Full specification** of the task format: all blocks (`module`, `include`, `parallel`, `loop`, `register`, `onchanges`, `onfail`, `require`, `retry`, `timeout`, …), agreements about naming, template context. Source of truth for DSL implementation. |
| [input.md](input.md) | `input:`-destiny contract: where it is validated, how it is used, the relationship with the general standard [`docs/input.md`](../input.md). |
| [output.md](output.md) | `output:`-destiny contract (top-level): symmetrical `input:` block declaration of what result destiny publishes to the caller; read from scenario via `register:` on the applier task. |
| [vars.md](vars.md) | `vars.yml` - destiny locals. Hard-coded values set by the destiny author; not overridden from outside; may refer to `input.*`. |
| [testing.md](testing.md) | Molecule-style testing of destiny on an ephemeral stand; layout `tests/<case>/`; open Q around the coverage tool. |
| [production-conventions.md](production-conventions.md) | "Production-grade destiny" checklist: passthrough flags, hybrid service account rule (DynamicUser vs manual uid), mandatory systemd-hardening, supply-chain, isolation. The standard is `node-exporter` (stateful account + supply-chain + privileged textfile collectors). |

## Related Documents

- [`docs/scenario/`](../scenario/README.md) - layer above destiny: orchestration, `on:`/`where:`-targeting, `apply: { destiny: … }`. After [ADR-009](../adr/0009-scenario-dsl.md), the scenario inherits the task DSL core from [tasks.md](tasks.md); destiny/scenario boundary - recommendation.
- [`docs/architecture.md`](../architecture.md) - layers above and below destiny:
  - [Addressing modules](../architecture.md), [Module manifest](../architecture.md) - layer under destiny.
  - [Service - structure and manifest](../architecture.md), [Targeting and host communication](../architecture.md) - layer above destiny.
  - [ADR-007](../adr/0007-versioning-git-ref.md) - why is there no `version:` field in `destiny.yml`.
- [`docs/input.md`](../input.md) - **general** format standard for `input:` (applies to destiny, scenario and module manifest).
- [`docs/templating.md`](../templating.md) - template engine spec (ADR-010): CEL for expressions in destiny tasks, Go text/template for `templates/*.tmpl`, marker `${ … }`, `core.file.rendered` as a render module.
- [`docs/soul-lint.md`](../soul-lint.md) - static checks of destiny at the CI/IDE stage.
- [`docs/module-collections.md`](../module-collections.md) — namespace prefix in module addressing.
- [`examples/destiny/redis/`](../../examples/destiny/redis/) is a working example of a full layout.
