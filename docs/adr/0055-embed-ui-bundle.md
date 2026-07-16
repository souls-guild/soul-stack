# ADR-055. Embed UI bundle — optional single-binary keeper with UI at `/ui`

> **Status: active.** User's decision (names + default-ON toggle) + architect's design. **Amends [ADR-035](0035-distribution-split.md) §3** — activates the previously deferred embed-compat-shim (ADR-035 "What is deferred" → embed for air-gapped/enterprise) already for the beta as **default-ON**, NOT a reversal of the distribution split. The canon is fixed docs-first BEFORE code; implementation is a separate slice (see §Slice map).

**Context.** [ADR-035](0035-distribution-split.md) split the distribution: core = Go artifacts only, web — companion repo `soul-stack-web`, the core↔web contract = OpenAPI, the UI is deployed separately. For the beta this two-component onboarding is an extra barrier: an operator who has brought up a single `keeper` binary wants to see the UI immediately, without a separate build/deploy/static-serving of soul-stack-web. ADR-035 already anticipated this case in "What is deferred" ("Embed compat-shim for air-gapped enterprise … single-binary with UI") — this ADR activates it earlier and makes it the **default** (not a separate `keeper-bundled` build), for the beta bootstrap.

There is already a precedent for embedding static assets in keeper: the OpenAPI viewer `GET /docs` carries a go:embed RapiDoc bundle (~840 KB) in `keeper/internal/api/docsassets/` ([ADR-054](0054-openapi-code-first.md), Amendment 2026-06-15, mechanism A). Embedding the built web UI is the same mechanically, larger in volume.

**Decision.** Keeper optionally (**default-ON**) embeds a vendored build snapshot of the UI (`soul-stack-web`) via `go:embed` and serves it at the `/ui` route.

- **(a) Package `keeper/internal/webui/`, assets in `keeper/internal/webui/assets/`.** The built static UI artifact lives in the `assets/` subfolder (NOT `dist/`). The `dist/` folder is silently swallowed by the `dist/` gitignore rule (release artifacts) — embed would get an empty tree, the bundle would not make it into the binary and would not be caught in review. `assets/` is neutral to gitignore.

- **(b) Route `/ui` + `/ui/*` with SPA fallback.** `GET /ui` and `GET /ui/*path` serve static assets from the embed tree; an unknown sub-path (a deep-link SPA router such as `/ui/incarnations/42`) → fallback to `index.html` (the standard single-page-application pattern), NOT a 404. Existing real assets (`/ui/assets/*.js`, `*.css`) are served as files. The mount is next to `/docs`/`/healthz`/`/openapi.yaml`, **outside `/v1`**.

- **(c) Static assets are PUBLIC — parity with `/docs`.** The `/ui` tree is served without auth (JS/CSS/HTML is not a secret; the same model as the public `/docs` shell). What is protected is the **API**, not the static assets: `/v1/*` stays behind `RequireJWT` + RBAC + default-deny ([ADR-014](0014-operator-identity.md), [ADR-028](0028-rbac-storage.md), [ADR-047](0047-purview.md)). After loading, the UI fetches `/v1/*` with a Bearer JWT that the operator enters in the UI itself (a JWT paste form, [ADR-035](0035-distribution-split.md) "What is deferred" → OIDC SSO remains a separate slice). Serving static assets does not expose the API surface — data comes only from `/v1` behind a JWT.

- **(d) Config toggle `web_ui_enabled` (scalar `*bool`).** A root keeper config key, `*bool`: `nil` (omitted) → **default-ON** (the beta wants the UI out of the box); explicit `false` → opt-out (static assets are not mounted). Symmetric to the footgun guard of neighboring subsystems (`tempo.enabled`/Conductor/Toll — default-ON when a backend is present), but without a dependency on infrastructure: the UI is baked into the binary and requires no external backend. **Restart-required** (not hot-reload): the effective value is read once at startup and baked into the mounted router; SIGHUP / API reload does not toggle it — the `/ui` mount does not appear or disappear on the fly. Symmetric to the restart-required toggles `toll.enabled` / `tempo.enabled` ([ADR-021](0021-hot-reload-config.md): infrastructure is brought up/torn down only at startup). An atomic router re-mount for a routing toggle is rejected as over-engineering: the static assets are baked into the binary, carry no state, toggling is rare (beta onboarding), and disposing/swapping the chi router under traffic is unnecessary risk for the sake of a binary flag.

- **(e) Shares the `:8080` listener — NO new ports/systemd units.** `/ui` is mounted into the same chi router and the same Operator API HTTP listener as `/v1`/`/docs`/`/healthz`. No separate web server, port, systemd service or reverse proxy. Single-binary in the literal sense: one process, one port.

- **(f) Binary growth ~+2–5 MB.** The built SPA bundle (minified JS/CSS/HTML) is on the order of a few megabytes; acceptable for single-binary onboarding (the same order as the `/docs` RapiDoc bundle × a few). It does not pull in node_modules/vite/sources — only the finished artifact.

**Cross-repo: source-of-truth and sync.**

- **Source-of-truth for the UI = companion repo `soul-stack-web`** ([ADR-035](0035-distribution-split.md) §2). The sources (React/TS, vite config, tests) live THERE and are NOT copied into the core repo. Only the **vendored build snapshot** (the output of `vite build`) makes it into core, in `keeper/internal/webui/assets/`.

- **Sync — `scripts/sync-webui.sh` + drift guard `make check-webui`** — a carbon copy of the plugin-template mechanism ([sync-template.sh](../../scripts/sync-template.sh) / `check-template` in [Makefile](../../Makefile)). `sync-webui.sh` mirrors the build output from the companion (`../soul-stack-web/<build-output>` → `keeper/internal/webui/assets/`, `rsync --delete` / rm+cp fallback). `check-webui` is a CI guard against the drift "forgot to sync after rebuilding the UI in the companion"; **skipped without the companion** (on a foreign machine/CI the companion repo may not be next to it — the gate does not fail but is skipped with a warning, like `check-template`). The exact name of the companion's build-output folder and the dev story (how to build/update the snapshot locally) belong to the sync-mechanism slice.

- **vite `base: '/ui/'`.** So that the SPA's relative asset paths resolve under the `/ui` prefix (rather than from the root), the companion build is configured with `base: '/ui/'`. This is a change in `soul-stack-web` (noted for the frontend slice), not in core.

**Clarification of the [ADR-035](0035-distribution-split.md) invariant "no HTML/CSS/TS in the repo".** The invariant is preserved for **sources**: React/TS sources, vite config, npm dependencies in the core repo are still prohibited (the ADR-035 toolchain split is not reversed — `go.work`/`Makefile` do not pull in node, core CI does not depend on the web build). Exactly the **built static artifact** (a minified JS/CSS/HTML bundle) in `keeper/internal/webui/assets/` is allowed — exactly as the `/docs` RapiDoc bundle is already allowed ([ADR-054](0054-openapi-code-first.md)). The boundary: **UI source — prohibited, built artifact — allowed.**

**Security.**
- The API boundary is not weakened: `/v1/*` — JWT + RBAC + default-deny unchanged. Only the static assets are public (parity with `/docs`).
- Air-gapped/offline: the UI is baked in, no CDN is pulled (the same motivation as go:embed RapiDoc) — the single binary carries everything.
- The attack-surface increase is a static file server on a public path; the SPA fallback serves only the embed tree (no path traversal into the external FS — serving from `embed.FS`, not disk FS).

**Slice map** (per the architect's design, per the bulk-operations rule — a pilot before replicating the mechanism):
1. **ADR** (this document) — the docs-first canon.
2. **Backend pilot** — package `keeper/internal/webui/` with a **stub embed** (a minimal placeholder `index.html`), route `/ui` + `/ui/*` SPA fallback, toggle `web_ui_enabled`, guard tests (mount/SPA fallback/toggle-off → not mounted/static publicness). Proves the mechanics before the real bundle.
3. **Frontend** — `vite base: '/ui/'` in the companion `soul-stack-web` (frontend agent's zone).
4. **Sync mechanism** — `scripts/sync-webui.sh` + `make check-webui` (+ an entry in the `check` chain), dev story.
5. **Real snapshot** — the first vendored build snapshot of the UI in `assets/` + an end-to-end check (the UI loads at `/ui`, fetches `/v1` with a JWT).
6. **Docs** — user documentation for beta onboarding (docs-writer's zone).

**Relation to other ADRs.**
- **[ADR-035](0035-distribution-split.md)** — **amends** (0035 status → amended): activates the deferred embed-compat-shim as default-ON for the beta; the ADR-035 toolchain split and companion source-of-truth are preserved, only the §3 invariant "no embedded UI assets" is reversed (embed is now allowed for the built artifact).
- **[ADR-054](0054-openapi-code-first.md)** — the precedent for go:embed static assets in keeper (the `/docs` RapiDoc bundle, a public mount outside `/v1`); `/ui` follows the same model.
- **[ADR-021](0021-hot-reload-config.md)** — the config key `web_ui_enabled` is present in the config contract but is **restart-required** (not hot-reloadable): the mount is read once at startup, like `toll.enabled` / `tempo.enabled`.
- **[ADR-014](0014-operator-identity.md)** — the API is behind a JWT; the UI fetches `/v1` with a Bearer, static assets are public.

**Rejected alternatives.**
- **(a) Build-from-companion in core CI** (core CI clones soul-stack-web and builds the UI). Rejected: a cross-repo build dependency + node toolchain in core CI is a direct violation of the toolchain split ([ADR-035](0035-distribution-split.md): "core CI/CD does not depend on the web build", "no npm dependencies in Makefile/go.work"). A vendored build snapshot + drift guard keeps core CI go-only.
- **(b) A git submodule for soul-stack-web.** Rejected secondarily — already rejected in [ADR-035](0035-distribution-split.md) (the rejected alternative "(c) UI as a monorepo with a git submodule": submodules are inconvenient in practice). A sync snapshot is simpler.
- **(c) Downloading dist from a GitHub Release at `go build`/startup.** Rejected: network access in `go build` (breaks offline/air-gapped builds), access tokens for a private release, a non-deterministic build. A direct contradiction of the single-binary-offline motivation.
