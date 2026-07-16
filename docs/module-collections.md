# Module collections (feature backlog)

This document collects ideas and open Q around **collection** - the top level of module addressing (`namespace` in `<namespace>.<module>.<state>`, see ["Module Addressing"](architecture.md#module-addressing)). The addressing scheme itself is fixed. The collection as a full-fledged **entity** of the Soul Stack is a feature backlog: below is a list of what it can provide, and a list of forks before implementation begins.

Entity name at the Soul Stack dictionary level **not selected yet** - in documents we use the neutral "collection / namespace". See open Q below for candidates.

## Why is the collection (what does it give us)

Currently, in `core.pkg.installed` addressing, the `core` prefix simply works as a namespace separator. If you expand the collection into a full-fledged entity, a set of practically useful capabilities appears.

### 1. Distribution unit

Collection = bundle of modules with one manifest, one git ref and source. We don't download 30 separate binaries - we download the collection using a specific tag and get all its modules in a consistent set. Similar to Ansible Galaxy collections and Salt Packs.

```yaml
# hypothetical keeper-config or service.yml.
# Collection version - git ref (tag or branch), without semver-range - see ADR-007.
collections:
  - { name: core,      builtin: true }
  - { name: wb,        ref: v1.5.0,  source: "git://gitlab.wildberries.ru/grimoires/wb" }
  - { name: community, ref: v0.12.3, source: "https://collections.soul-stack.io" }
```

### 2. Trust boundary

Signing and verification occurs at the collection level, not each module. One publisher → one key. Keeper has a policy of "trust `core` + `wb`, do not trust `community`".

> **Current model - Sigil (Option A).** Integrity of plugins in MVP is closed **Sigil** - Keeper-signed digest index ([ADR-026](adr/0026-sigil.md)): permission of specific hashes `(namespace, name, ref) → sha256`, signature with Keeper's key, explicit permission by Archon. Author-signed collections/publishers (collection level trust boundary by `(namespace, ref)`, one publisher → one key - **Option B**) - **post-MVP** together with this entity, extends Sigil additively without breaking changes.

### 3. RBAC and allow-list

"The role `app-team` can only call `core.*` and `wb.*`", "production-destinies cannot use `community.*`." In the reality of a large fleet of operators, this is almost always necessary.

### 4. Consistent versioning

Modules within the same collection move together. Not "`pkg@1.5` is compatible with `service@1.3`, but not with `service@1.4`," but "the entire `wb` under the git tag `v1.5.0` is a consistent set." Destiny/service.yml declares a single `ref:` collection instead of a module-by-module version matrix.

### 5. Discovery, UI, MCP

"Show all modules from `wb`", "what states does `wb.haproxy` have". UI Keeper groups the catalog into collections, and the MCP server gives it to operators and LLM agents in a structured form.

### 6. Delivery cache in push mode

The architecture already describes the `/var/lib/soul-stack/modules/` SHA-256 cache for push. With collections, the cache key becomes not a "separate module", but "collection@ref" - there are fewer pieces to cache, it's easier to check consistency, the "update the collection across the entire fleet" pipeline is easier.

### 7. Visual clue about origin

From the line `core.pkg.installed` you can immediately see: built-in, without network dependencies. From `community.kubernetes.deployed` - third-party collection, installation required. This solves the "it's not clear where core / where custom" is where the conversation started.

## What needs to be decided before implementation (open Q)

All points are propose-and-wait and are not fixed silently.

1. **Entity name in the Soul Stack dictionary.** Candidates:
   - **Grimoire** (grimoire) - modules = spells, grimoire = their book. It fits into the "spiritual" metaphor.
   - **Codex** (code) - more neutral, same idea of ​​a "book".
   - **Order** (order) - a social metaphor, a guild of publishers. Weaker than the book version.
   - **Collection** / **Pack** / **Bundle** - neutral, without metaphor. Understandable, but not original.
   - **Namespace** is a technical name, not an entity.

2. **Declaration in destiny/service.yml.** Options:
   - Explicit block `required_collections:` with versions + `required_modules:` references short-name in the spirit of `core.pkg`;
   - Versions are pulled from the module name automatically (`core` → built-in, `wb` → last installed);
   - Hybrid: collections are declared globally (Keeper config), destiny refers only to names.

3. **Versioning model.** The basic rule is enshrined in [ADR-007](adr/0007-versioning-git-ref.md): the version of a collection is the git ref (tag or branch), no semver-range. Open: tag naming convention (mandatory `vMAJOR.MINOR.PATCH` or free form), breaking change policy for `protocol_version` modules inside, whether the collection needs to have its own manifest with `min_keeper_version`-like compat flags.

4. **Where the registry of trusted collections lives.** Postgres (part of Keeper-state) vs static Keeper config vs both. Affects whether collections can be managed via API/MCP at runtime, or whether this is a deployment-time artifact.

5. **Collection source.** Git repo / OCI registry / own artifact store / smesh. Convergence with the approach to delivering the `soul` binary and custom modules (see "Delivery of the soul binary and modules to the host" in architecture.md).

6. **Push cache for a collection.** Tar-bundle of the entire collection with one artifact vs cache for individual modules. Tar-bundle is easier for integrity and signature, piece by piece saves traffic for partial updates.

7. ~~**Composition of the core collection.**~~ **Closed** ([ADR-015](adr/0015-core-modules-mvp.md) / [ADR-017](adr/0017-keeper-side-core.md)). Core MVP composition: **16 Soul-side** - `core.pkg`, `core.file` (including `core.file.rendered` - template rendering), `core.service`, `core.user`, `core.group`, `core.exec`, `core.cmd`, `core.cron`, `core.mount`, `core.git`, `core.archive`, `core.sysctl`, `core.url` (`fetched` - download-by-URL, https-only), `core.line` (`present`/`absent` - in-place line-by-line editing pilot, lineinfile-equivalent), `core.repo` (`present`/`absent` - apt/dnf/yum/apk package repository), `core.firewall` (`present`/`absent` - one ufw/firewalld firewall rule, without enable/default-policy); **3 Keeper-side** (dispatcher `on: keeper`) - `core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`; **infrastructure** - `core.module.installed` (delivery/cache of plugins to the host). `core.template` is deliberately NOT highlighted - the rendering is done by `core.file.rendered`. `core.copy` is deliberately NOT highlighted - covered by `core.file.present` with inline-content. `cloud-provision` as a destiny construct is rejected - this is the keeper-side step `core.cloud.provisioned` ([ADR-017](adr/0017-keeper-side-core.md)). `state.migrate` as escape module rejected - state_schema migrations are covered by DSL ([ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

8. **Compatibility and breaking change when updating a collection.** What is considered a compatible update; how Keeper marks destiny, which is no longer valid with the new version of the collection; whether it is necessary to "fix the collection version in incarnation".

## Dependencies

- Related to [ADR-004](adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) (binary layout) and section ["Module Model"](architecture.md#module-model) - collection = add-on to the current module model.
- Related to the section ["Delivery of `soul` binary and modules to the host"](keeper/push.md) in `docs/keeper/push.md` and push cache.
- When UI Keeper appears ([open Q "UI Keeper"](architecture.md#open-questions)) - the collections directory becomes one of the main pages.
