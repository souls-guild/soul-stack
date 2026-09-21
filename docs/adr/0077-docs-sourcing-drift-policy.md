# ADR-0077. Docs sourcing and drift policy

- **Status.** Active.

- **Context.** Documentation lives on two surfaces: the in-repo `docs/` tree (ADRs plus
  design specs — architecture, the Keeper↔Soul protocol, templating, destiny, scenario,
  migrations, soulprint, modules) and the public documentation site. Treating them as
  two copies of the same thing invites wasteful duplication and, worse, drift between
  prose and the real code. Drift is already observable: this repository's own
  `CLAUDE.md` still describes the project as "only stubs / 4 commits / no real logic"
  and the license as "Apache 2.0 for everything" — both long out of date. Hand-written
  prose rots; generated artifacts (the huma-derived `docs/keeper/openapi.yaml`, guarded
  by `check-openapi`) do not.

- **Decision.**
  1. **One source of truth per fact; generate the rest.** Machine-checkable facts — the
     API surface, module inputs and schemas, the permission catalog, config keys,
     statuses, metrics — are derived from code, never hand-written. Reference
     documentation is generated from the same manifests and schemas the code uses (as
     `gen-openapi` already does for the API; the module catalog is generated the same
     way from the core manifests). Generated docs cannot drift.
  2. **Prose lives next to the code it describes and changes in the same PR.** Design
     rationale is recorded in ADRs; a design change edits the relevant ADR in the same
     commit. A pull request that changes a documented surface must also update the doc,
     or the generator that produces it.
  3. **Drift is a build failure, not a hope.** CI enforces it: idempotent regeneration
     (`check-gen`, `check-openapi`, extended with a documentation-generation guard),
     link integrity (`check-doc-links`), linted examples (`soul-lint` over `examples/`,
     so a stale or invalid example fails the build), and a documentation review gate
     before each release.
  4. **Split the two surfaces by audience — one home each.** The in-repo `docs/` tree is
     design and decision material (ADRs and specs) for contributors: English, versioned
     with the code, the source of truth for "why it is built this way". The
     documentation site is user-facing material for operators: the single home for "how
     to use it", generated from code wherever possible. Content that currently overlaps
     (getting-started, module reference, and the like) collapses to one home; the other
     surface links to it instead of restating it.
  5. **Translations go through the site's i18n only.** Reader translations are an overlay
     over the English base on the documentation site, with automatic fallback for
     untranslated pages. Hand-forked translation copies inside the repository are not
     allowed — two full copies always drift. The single exception is the top-level
     `README` (the repository's front door): small and stable, it is translated in-repo
     under `docs/i18n/` with a language switcher.

- **Consequences.**
  - Each fact has a single home; there is no second hand-written copy to keep in sync.
  - The in-repo `docs/` tree stays English-only design material, consistent with the
    repository's English-only source rule; reader translations live on the site.
  - Follow-up work: a documentation-generation idempotency gate in `make check`, linted
    and current `examples/`, an inventory of repo↔site overlap to collapse into a single
    home, and a refresh of the stale `CLAUDE.md`. These are tracked separately.
