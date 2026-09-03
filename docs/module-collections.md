# Module collections (feature backlog)

This document collects ideas and open Q around **collection** — the top level of module addressing (see ["Module Addressing"](architecture.md#module-addressing)). The addressing scheme itself is fixed. The collection as a full-fledged **entity** of the Soul Stack is a feature backlog: below is a list of what it can provide, and a list of forks before implementation begins.

> **Updated 2026-08-06 (NIM-377).** Level 1 is now the **registration alias** — the name the *operator* gives an artifact in `keeper.yml::plugins.*[].name` — not a `namespace:` field read out of the artifact. The artifact carries **no self-identity at all** ([ADR-020(p)](adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)), so the same bytes registered twice answer at two addresses. That closes **open Q 1, 2 and 4** below and changes what the remaining ones are asking. The full addressing model is **NIM-376**, a separate open ticket.

Entity name at the Soul Stack dictionary level **still not selected** — in documents we use the neutral "collection / alias". See open Q 1 below.

## Why is the collection (what does it give us)

Currently, in `core.pkg.installed` addressing, the `core` prefix simply works as a namespace separator. If you expand the collection into a full-fledged entity, a set of practically useful capabilities appears.

### 1. Distribution unit

Collection = bundle of modules with one manifest, one git ref and source. We don't download 30 separate binaries - we download the collection using a specific tag and get all its modules in a consistent set.
```yaml
# hypothetical keeper-config or service.yml.
# Collection version - git ref (tag or branch), without semver-range - see ADR-007.
collections:
  - { name: core,      builtin: true }
  - { name: acme,        ref: v1.5.0,  source: "git://gitlab.example.com/grimoires/acme" }
  - { name: community, ref: v0.12.3, source: "https://collections.soul-stack.com" }
```

### 2. Trust boundary

Signing and verification occurs at the collection level, not each module. One publisher → one key. Keeper has a policy of "trust `core` + `acme`, do not trust `community`".

> **This benefit does not survive the alias, and that is worth being explicit about.** A policy phrased as "trust `acme`" reads level 1 as a statement about a publisher; since NIM-377 level 1 is a label the local operator chose, so such a policy would say only "trust what I already decided to trust". A real publisher-level trust boundary needs a publisher-level identity, which is exactly what Variant B below would introduce — and it would key on the artifact **source**, not on the alias.

> **Current model - Sigil (Option A).** Integrity of plugins in MVP is closed **Sigil** - Keeper-signed digest index ([ADR-026](adr/0026-sigil.md)): permission of specific hashes, keyed on the artifact source `(source, ref) → sha256` since NIM-377/438, signature with Keeper's key, explicit permission by Archon. Author-signed collections/publishers (collection level trust boundary by `(namespace, ref)`, one publisher → one key - **Option B**) - **post-MVP** together with this entity, extends Sigil additively without breaking changes.

### 3. RBAC and allow-list

"The role `app-team` can only call `core.*` and `acme.*`", "a production destiny may use only the artifacts this fleet approved." In the reality of a large fleet of operators, some such narrowing is almost always necessary.

> **No origin-keyed RBAC narrowing exists in the code.** Nothing anywhere keys on `community.*` or `official.*`, and no policy narrows a role by where a plugin came from. The module gates that do exist are the [SDK marker interfaces](naming-rules.md#sdk-marker-interfaces) (`PlanReadSafe` / `ErrandReadSafe`, default-deny per module) and, for the console/Errand shell gate, a set of **full core addresses**: the closed verb-shell set the console gate reads, plus — on the Errand path only — the exact admit `core.http.probe` and a `core.http.` prefix reject (`soul/internal/runtime/errandrunner/whitelist.go:73,84,87`). Those last two are prefix/exact matches on an address, so the flat "no glob anywhere" is not the claim; what *is* the claim is that all of them sit inside `core.`, a [reserved](naming-rules.md#reserved-namespace-names) level 1 no plugin can claim, and none of them reads a plugin's level 1 at all. A rule phrased "production cannot use `community.*`" would also have nothing stable to bind to: level 1 is the local operator's [registration alias](naming-rules.md#plugin-manifest-and-handshake), so the same bytes registered under another name walk straight past it. What such a policy has to key on is the same pair the rest of this document keeps arriving at — the **alias** an operator declared, and the artifact **`source`** behind it — which is also what the Sigil allow-list already keys on ([ADR-026(a)](adr/0026-sigil.md#amendment-2026-08-06-nim-377-the-registry-keys-on-the-artifact-source-the-signature-is-not-a-control-on-declarations)). The origin-grouping level itself is removed ([ADR-020 amendment 2026-09-02](adr/0020-plugin-infrastructure.md#amendment-2026-09-02-nim-764--nim-765-a-plugin-address-is-pluginobjectaction-and-the-origin-grouping-level-is-removed), NIM-765), so `community.*` is not a prefix a future rule could be written against either.

### 4. Consistent versioning

Modules within the same collection move together. Not "`pkg@1.5` is compatible with `service@1.3`, but not with `service@1.4`," but "the entire `acme` under the git tag `v1.5.0` is a consistent set." Destiny/service.yml declares a single `ref:` collection instead of a module-by-module version matrix.

### 5. Discovery, UI, MCP

"Show all modules from `acme`", "what states does `acme.haproxy` have". UI Keeper groups the catalog into collections, and the MCP server gives it to operators and LLM agents in a structured form.

### 6. Delivery cache in push mode

The architecture already describes the `/var/lib/soul-stack/modules/` SHA-256 cache for push. With collections, the cache key becomes not a "separate module", but "collection@ref" - there are fewer pieces to cache, it's easier to check consistency, the "update the collection across the entire fleet" pipeline is easier.

> **Largely delivered by NIM-377, without the entity.** One artifact already serves several modules, and the host slot is one directory per alias holding one executable ([soul/modules.md](soul/modules.md)) — so the cache unit is already "artifact@ref" rather than "module". What a collection entity would still add is grouping across *several* artifacts.

### 7. Visual clue about origin

From the line `core.pkg.installed` you can immediately see: built-in, without network dependencies. From a non-`core` level 1 — third-party artifact, installation required. This solves the "it's not clear where core / where custom" is where the conversation started, **and that is the whole of what it solves.**

> **Weaker since NIM-377, in one direction only.** `core` is still a reliable signal — it is [reserved](naming-rules.md#reserved-namespace-names) and cannot be claimed by a plugin, so "not `core`" still means "delivered, installation required". What a non-`core` level 1 no longer tells you is *whose* it is: it means whatever the local operator meant by it. Origin is answered by the catalog entry's `source`, which is also what the allow-list keys on.
>
> **And since 2026-09-02 the address does not group by origin at all.** Level 1 is the plugin's name and level 2 the object it manages, so an example of the `community.kubernetes.deployed` form — origin at level 1, subject at level 2 — is not a clue that got weaker, it is a spelling that is removed ([ADR-020 amendment 2026-09-02](adr/0020-plugin-infrastructure.md#amendment-2026-09-02-nim-764--nim-765-a-plugin-address-is-pluginobjectaction-and-the-origin-grouping-level-is-removed), NIM-765). The `core` / not-`core` signal above is untouched by that: it rests on `core` being reserved, not on level 1 naming an origin.

## What needs to be decided before implementation (open Q)

All points are propose-and-wait and are not fixed silently.

1. ~~**Entity name in the Soul Stack dictionary.**~~ **Closed as "no entity" (NIM-377).** The candidates were Grimoire / Codex / Order / Collection / Pack / Bundle — all of them names for *a thing a publisher ships*. Level 1 is not that thing: it is a **local label an operator chose**, and it identifies nothing about origin. Naming it would invent an entity the system does not have.

   The name in the dictionary is therefore **registration alias** ([naming-rules.md](naming-rules.md#plugin-manifest-and-handshake)) — a DevOps term, not a Soul Stack entity, per the "small = DevOps" rule. **Bundle** was taken for something real and different: the set of modules one artifact serves (`module.Bundle`), which is a publisher-side fact.

   What remains open is whether a *publisher-side* collection entity is ever needed — that is question 2 of the deferred Variant B below, not a naming question.

2. ~~**Declaration in destiny/service.yml.**~~ **Closed by the alias (NIM-377):** the third option, "collections are declared globally in the Keeper config, destiny refers only to names". The operator declares `{name, source, ref}` once in `keeper.yml::plugins.*`, and `required_modules:` / `service.yml::modules[]` carry only names. No `required_collections:` block, and no version in a destiny.

   This is not a preference — it is forced. Any option where a destiny declares a *version* would need the artifact to have a name a destiny could bind a version to, and it does not. The version lives with the `ref` in the catalog entry, where the operator put it.

3. **Versioning model.** The basic rule is enshrined in [ADR-007](adr/0007-versioning-git-ref.md): the version of a collection is the git ref (tag or branch), no semver-range. Open: tag naming convention (mandatory `vMAJOR.MINOR.PATCH` or free form) and the breaking-change policy for `protocol_version` of the modules inside. **Partly answered by NIM-377:** the artifact declares its engine window as `compat: {keeper: ">=0.9 <2.0"}` in the [schema document](keeper/plugins.md#schema-document) — declared once per artifact, since a bundle's modules ship together on one version line. So the "own manifest with `min_keeper_version`-like compat flags" half of this question is closed; the tag-convention half is not.

4. ~~**Where the registry of trusted collections lives.**~~ **Closed: both, on two different axes (NIM-377).** The **catalog** (`{alias, source, ref}`) is static Keeper config — `keeper.yml::plugins.*`. The **allow-list** is Postgres — `plugin_sigils`, keyed on the artifact source since [ADR-026(a)](adr/0026-sigil.md#amendment-2026-08-06-nim-377-the-registry-keys-on-the-artifact-source-the-signature-is-not-a-control-on-declarations) as amended, mutated at runtime via API/MCP by an Archon's `plugin.allow`.

   The split is deliberate and answers the "can they be managed at runtime" part: *which artifacts exist* is deployment-time, *which digests may run* is runtime and audited. Note the two use **different keys** — the alias names the address, the source keys the trust record — so registering one artifact under two aliases is one allow decision.

5. **Collection source.** Git repo / OCI registry / own artifact store / smesh. Convergence with the approach to delivering the `soul` binary and custom modules (see "Delivery of the soul binary and modules to the host" in architecture.md).

6. **Push cache for a collection.** Tar-bundle of the entire collection with one artifact vs cache for individual modules. Tar-bundle is easier for integrity and signature, piece by piece saves traffic for partial updates.

7. ~~**Composition of the core collection.**~~ **Closed** ([ADR-015](adr/0015-core-modules-mvp.md) / [ADR-017](adr/0017-keeper-side-core.md)). Core MVP composition: **16 Soul-side** - `core.pkg`, `core.file` (including `core.file.rendered` - template rendering), `core.service`, `core.user`, `core.group`, `core.exec`, `core.cmd`, `core.cron`, `core.mount`, `core.git`, `core.archive`, `core.sysctl`, `core.url` (`fetched` - download-by-URL, https-only), `core.line` (`present`/`absent` - in-place line-by-line editing pilot, lineinfile-equivalent), `core.repo` (`present`/`absent` - apt/dnf/yum/apk package repository), `core.firewall` (`present`/`absent` - one ufw/firewalld firewall rule, without enable/default-policy); **3 Keeper-side** (dispatcher `on: keeper`) - `core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`; **infrastructure** - `core.module.installed` (delivery/cache of plugins to the host). `core.template` is deliberately NOT highlighted - the rendering is done by `core.file.rendered`. `core.copy` is deliberately NOT highlighted - covered by `core.file.present` with inline-content. `cloud-provision` as a destiny construct is rejected - this is the keeper-side step `core.cloud.provisioned` ([ADR-017](adr/0017-keeper-side-core.md)). `state.migrate` as escape module rejected - state_schema migrations are covered by DSL ([ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

8. **Compatibility and breaking change when updating a collection.** What is considered a compatible update; how Keeper marks destiny, which is no longer valid with the new version of the collection; whether it is necessary to "fix the collection version in incarnation".

## Dependencies

- Related to [ADR-004](adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) (binary layout) and section ["Module Model"](architecture.md#module-model) - collection = add-on to the current module model.
- Related to the section ["Delivery of `soul` binary and modules to the host"](keeper/push.md) in `docs/keeper/push.md` and push cache.
- When UI Keeper appears ([open Q "UI Keeper"](architecture.md#open-questions)) - the collections directory becomes one of the main pages.
