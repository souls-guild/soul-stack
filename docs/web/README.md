# Soul Stack Web - operator UI

Source-of-truth UI lives in a **separate companion repository** [`soul-stack-web`](https://github.com/souls-guild/soul-stack-web) (TS + React): development, release cycle and node dependencies are there.

**With [ADR-055](../adr/0055-embed-ui-bundle.md), the assembled UI is by default BUILT IN into the `keeper`-binary** (`go:embed`) and is given to it on the `/ui` route - no separate process, port or deployment is required. This is a **not** reversal of the distribution split: the companion repo remains the canon of the UI code, only the vendored snapshot of the collected statics (`dist/`) gets into the core. Toggle - [`web_ui_enabled`](../keeper/config.md#web_ui_enabled-top-level) (default-ON, explicit `false` - opt-out).

## Where does it live?

| | Where | What |
|---|---|---|
| **Source UI** | companion `soul-stack-web` | TS+React sources, npm dependencies, vite build, front release cycle |
| **Vendored snapshot** | `keeper/internal/webui/assets/` (this repo) | compiled by `dist/` companion, compiled into `keeper` via `go:embed`; given to `/ui` |
| **API contract** | [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) | the only public contract; UI generates TS types from it (`openapi-typescript`) |

## Snapshot synchronization (core ← companion)

Vendored statics are updated from the companion repo with two Make targets:

| Target | What does |
|---|---|
| `make sync-webui` | Vendoring: rsync `--delete` mirrors the assembled `dist/` companion `soul-stack-web` → `keeper/internal/webui/assets/`. Script - `scripts/sync-webui.sh`. |
| `make check-webui` | Drift-guard in `make check`: error if the vendored copy diverges from the companion build ("forgot `sync-webui` after changing the UI"). Three outcomes, not two: no companion → skip (legitimate: CI, third-party clone); companion present but not built → **fail**; drift → fail. |
| `make check-webui-embed` | Also in `make check`, and the only one of these that CI can actually run: the vendored tree must match the `assets_sha256` recorded in `WEBUI_SOURCE` when it was mirrored. Needs no companion, no docker, no token. |
| `make check-webui-provenance` | On-demand: compares the recorded companion commit against the companion's local HEAD. A note, not a gate — the companion may legitimately be on another branch. |

Typical cycle when changing the UI: edit in companion → build `dist/` there → `make sync-webui` in core → commit the updated snapshot.

### What the guards do and do not cover

`keeper/internal/webui/assets/` is a build artefact of another repository committed
into this one, so the failure that matters is an **unpaired merge**: the UI moves in
the companion and the vendored copy is not re-synced, after which keeper serves a
bundle nobody built from current sources. That happened once already (NIM-273) and
reached a release.

`WEBUI_SOURCE` (next to `assets/`, deliberately outside it so `go:embed` does not
ship it) answers "which companion commit produced these bytes", which is what makes
an unpaired merge visible in review: the `commit=` line does not move while the UI
changed. `check-webui-embed` is what makes that line trustworthy — if the bundle
could change without the provenance changing, "the SHA did not move" would no longer
imply "the bundle did not change".

Still not covered: a companion that moved on while core was never re-synced at all.
No commit in this repository touches `assets/` in that case, so nothing here can see
it; detecting it needs read access to the private companion from CI, which is the
open half of NIM-341. Until then that case rests on a reviewer of the release merge
checking whether the UI moved without `commit=` moving.

## Why companion remains a separate repo

- **Different release cycles and dependencies.** The core (Go) and UI (TS+React, node_modules) are updated at different frequencies and with different toolchains; There is no need to mix assemblies.
- **Parallel front-end development** independent of core.
- **UI remains optional.** The operator can work via CLI (`soulctl`), MCP or directly OpenAPI; `web_ui_enabled: false` removes `/ui` without affecting `/v1/*` and `/docs`.

## Contact core

- **Contract:** [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) is the only public API contract.
- **TS client:** UI generates types via `openapi-typescript` (see `soul-stack-web/scripts/gen-api.sh`).
- **Authentication:** JWT ([ADR-013](../adr/0013-bootstrap-archon.md), [ADR-014](../adr/0014-operator-identity.md)) - the operator inserts the JWT into the UI manually (bootstrap token from `keeper init --archon` or token from `POST /v1/operators/{aid}/issue-token`); There is no separate `/v1/auth/login` endpoint yet.

See [ADR-035](../adr/0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui) (separation of distribution core ↔ UI) and [ADR-055](../adr/0055-embed-ui-bundle.md) (embed on `/ui`, amends ADR-035 p. 3).
