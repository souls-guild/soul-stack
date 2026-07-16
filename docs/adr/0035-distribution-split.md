# ADR-035. Distribution split — core (API+CLI) vs web (UI).

> **Status: amended.** [ADR-055](0055-embed-ui-bundle.md) activates the deferred embed-compat-shim (see §"What is deferred" → "Embed compat-shim for air-gapped enterprise") as an optional **default-ON** embed UI on the `/ui` route for the beta (single-binary onboarding). This is **not a reversal** of the distribution split: the companion source-of-truth (point 2) and the toolchain split (invariants: core CI doesn't depend on the web build, no npm in Makefile/go.work) are preserved; only the invariant of point 3/"Rejected (b)" — "no embed UI-assets in keeper" — is reversed: a vendored **built artifact** (not sources) is now allowed. Details — [ADR-055](0055-embed-ui-bundle.md).

**Context.** At the MVP stage, `ui/` appeared in the repo — a React+TS+Vite scaffold UI (5 pages + 7 tests). As it grew, it became clear: TS tooling (node_modules, vite, vitest) lives by different rules than the Go core; the UI is an optional component (the operator works via CLI/MCP/OpenAPI), but it drags ~400+ npm packages into the main repo.

**Decision.**
1. **Core distribution = Go artifacts only.** This repo holds: `keeper`, `soul`, `soul-lint`, `soulctl`, `proto/`, `sdk/`, `shared/`, `docs/`, `examples/`. `ui/` is removed.
2. **Web — a separate repo `soul-stack-web`.** A companion repository on the model of **SaltStack ↔ salt-manager**, **OpenStack ↔ Horizon**, **Kubernetes ↔ Dashboard**.
3. **The contract between core and web — OpenAPI.** [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) — the sole public API contract. Web consumes it via `openapi-typescript` type generation; no other integration points (no embed UI-assets in the keeper binary, no shared SDK).
4. **Release cycles are independent.** Core and web are versioned separately (web may ship minor releases under one major core-API version). The UI compat matrix is fixed in `soul-stack-web/README.md`.
5. **The operator MAY work without the UI.** `soulctl` + MCP + direct OpenAPI — a fully functional operator interface. The UI is a usability layer, not a mandatory dependency.
6. **`docs/web/README.md` in the core repo** — a stub pointer to the external repo.

**Invariants.**
- No HTML/CSS/TS files in this repo (except `docs/` fixtures).
- No npm dependencies in `Makefile`/`go.work`.
- Core CI/CD doesn't depend on the web build.
- Web consumes OpenAPI as a **read-only** contract (only generated types, no custom extensions).

**Rejected alternatives.**
- (a) UI in a `ui/` subdirectory of the main repo — rejected (a bloated repo, a mixed toolchain, different release cycles).
- (b) UI as embedded assets in the keeper binary (like pgAdmin/Grafana) — rejected (the keeper binary should stay small, without UI dependencies; the deployment model — keeper serves the API, the UI is deployed separately).
- (c) UI as a monorepo with a git submodule — rejected (submodules are inconvenient in practice; an OpenAPI vendor via a plain `cp` or artifact publishing is simpler).

**What is deferred.**
- **OpenAPI as a published artifact.** Right now the web team pulls openapi.yaml via `cp`/cloning the core repo. In the future — a published artifact (e.g., in Harbor or GitHub Releases) with a semver tag. Doesn't block the MVP.
- **OIDC SSO in web.** Right now a JWT paste-form (the UI scaffold found this gap). OIDC — a separate slice in the web repo after an ADR-amendment to ADR-014.
- **An embed compat-shim for air-gapped enterprise.** If an enterprise variant wants a single-binary with a UI — a separate `keeper-bundled` build (a separate build target), not the default. Not part of the open-core MVP.
