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
| `make check-webui` | Drift-guard in `make check`: error if the vendored copy diverges from the companion build ("forgot `sync-webui` after changing the UI"). Four branches, not two: companion present but not built → **fail**; drift → fail; no companion → skip off a release branch, but **fail** on one (a release worktree is where the companion is supposed to sit alongside), with `WEBUI_SKIP=1` as the declared escape. CI is the stated exception — see the note below. |
| `make check-webui-embed` | In `make check`: the vendored tree must match the `assets_sha256` recorded in `WEBUI_SOURCE` when it was mirrored. Needs no companion, no docker, no token. Catches a bundle edited outside `sync-webui.sh`; by construction it cannot catch a stale one, since a stale bundle matches its own recorded fingerprint. |
| `make check-webui-freshness` | In `make check`: has the companion branch moved past the `commit=` the bundle was vendored from? Asks the remote for one ref — no companion checkout, no build, no token. **Binding on `release/*` and `hotfix/*`** and on a checkout detached at a bare sha, advisory elsewhere (a tag and a bisect included); `WEBUI_FRESHNESS_SKIP=1` is the declared escape (offline work). One of the two tiers of `make check` that need the network — the other is `check-vuln`, which pulls the vulnerability database. |
| `make check-webui-freshness-guard` | In `make check`: guard tests for `check-webui-freshness`, for `check-webui`'s skip-versus-fail branch **and for `sync-webui.sh`**, against throwaway local repositories — no companion, and no network beyond loopback (three cases dial `127.0.0.1` and expect to be refused). `check-webui-embed` is the one web check not covered here. |
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

The case that neither of those two can see is a companion that moved on while core
was never re-synced at all. No commit in this repository touches `assets/` then, so
byte comparison has nothing to compare and the fingerprint check matches — a stale
bundle carries its own correct `assets_sha256`. That is not a hypothetical: it
happened on two consecutive web merges after NIM-273 and both times a human noticed,
never the gate. `check-webui-freshness` is the answer to exactly that question, and
it needs no companion checkout and no token — the companion is public, so one
anonymous `git ls-remote` resolves the branch tip. NIM-341 deferred this work on the
assumption that the companion was private; that blocker is gone.

Six things worth knowing before the check reds on you:

- **It compares pointers, not content.** A companion commit that could not have
  changed a byte of the bundle (a test, a README) still moves the tip and still
  demands a re-sync. That is the uncommon case — three of the last forty companion
  commits — and the cost of getting it wrong the other way is a stale UI on a demo
  stand. It is also deliberate: `vite.config.ts` holds both the build and the vitest
  config, so no path list can honestly separate "bundle-relevant" from "inert", and
  a list that must be extended whenever the companion grows a directory is the
  guard-emptied-by-a-rename failure of NIM-547. When the UI really did not change,
  the whole diff is two provenance lines.
- **Clearing it is one command but not one keystroke.** `make sync-webui` needs the
  companion checked out next to core, on the branch being assembled, with a clean
  tree and its dependencies installed — `sync-webui.sh` runs `npm run build`, never
  `npm ci`. That is worth knowing before a release evening; the messages say it.
- **It is binding on `release/*` and `hotfix/*`.** Both incidents landed on a
  release branch, and that is where the demo stand is built from; hotfix branches
  are there because they reach `main` without passing a release branch and the
  companion carries hotfix branches of its own. On feature branches, tags and
  `main` it prints the same finding and exits zero, so nobody's unrelated branch
  reds because someone merged web. A pull request follows its **base**: on GitHub
  `GITHUB_REF_NAME` is `<n>/merge` there, which is not a release branch and never
  will be, so judging by the ref name alone would switch the check off at the one
  review where an unpaired merge is still cheap to catch. A merge queue is read
  the same way and for the same reason: its ref is
  `gh-readonly-queue/<base>/pr-<n>-<sha>`, which is not a release branch either,
  and that run is the last gate before the commit lands on the base — so the base
  is read back out of the ref. That rule lives in one place —
  `check-webui-freshness.sh --context` — and `check-webui` asks it rather than
  keeping a second copy.
- **One shape is skipped outright, two more are advisory** — the difference
  matters, because only the first says nothing at all. Skipped: a tree with no
  `assets/`, which cannot carry a stale bundle. That exit is unconditional, and
  it is safe only because of something outside the check — `keeper` embeds the
  bundle with `//go:embed all:assets`, and Go refuses to compile an empty or
  missing directory, so "delete `assets/` and the gate goes quiet" reds the
  `build` tier instead. Advisory, meaning they still report and still exit zero:
  a checkout that is not a git repository (an unpacked source tarball), and one
  sitting exactly on a tag — a released version's bundle is supposed to be frozen
  at what that release shipped. A bundle *with* no `WEBUI_SOURCE` next to it is a
  failure, not an absence: `main` and `hotfix/R5-I-provision-hardening` are in
  that state today, so the hotfix branch reds until it carries a re-vendored
  bundle.
- **A checkout on no branch is binding too.** `--context` has three answers, not
  two: `required`, `advisory`, and `unknown` for a tree detached at a bare SHA.
  `unknown` binds, because the alternative is worse than a
  false red: one detached checkout would file a release worktree under "not a
  release branch" and quietly switch both this check and `check-webui` off — the
  exact silence the ticket exists to remove. Say which branch you are assembling
  (`GITHUB_REF_NAME=release/<REL> make check-webui-freshness`) or declare the skip
  (`make check WEBUI_FRESHNESS_SKIP=1`); the message says both. Three detachments
  that are not this case are recognised rather than counted as unanswerable: a
  conflicted rebase resolves to the branch being rebased and is then judged as
  that branch — so a rebase of `release/*` still reds — while a bisect and a tag
  both stay advisory, a bisect because it lands on old commits on purpose and
  every one of them carries the bundle that was current then, a tag because a
  released version's bundle is meant to be frozen at what that release shipped. A
  rebase started from an already-detached HEAD is *not* one of those three:
  git records the literal string `detached HEAD` as the branch being rebased, and
  since no branch is being rebased there, the answer stays `unknown`.
- **Binding is not the same as complete, and `unknown` is the gap.** Two questions
  are asked on `release/*`: is the bundle stale, and was it vendored from *this*
  branch. The second one needs to know the branch, so on a detached checkout only
  the first is asked — the recorded branch is what staleness is measured against.
  A bundle vendored from a foreign companion branch and sitting at that branch's
  tip therefore passes detached and fails on `release/*`. That cannot be closed
  from inside the check, only stated, and the `unknown` banner states it.

When the companion cannot be consulted, the two outcomes are kept apart, because
their remedies are opposite: *"the companion answers but has no branch `X`"* means
the branch has not been opened there yet (a release train that starts in core —
open it in the companion, or declare the wait), while *"could not reach the
companion"* means no network or no access, and is what `WEBUI_FRESHNESS_SKIP=1`
is for. Both are advisory off a release branch and red on one.

Which repository is consulted is derived, not chosen: every git remote of this
checkout with `-web` appended, origin first, each tried over its own transport and
then over https. On a fork-based checkout that means the fork's `-web` answers
first, and it may trail or lead upstream — so every mismatch names the repository
whose tip it is reporting, and `WEBUI_REMOTE_URL=<url>` pins a different one. A
red that re-vendoring does not clear is the sign to look at that line.

**A red has two shapes, and only one is cleared by re-vendoring.** The check
compares the recorded commit against the branch tip — equality, never ancestry —
so it cannot tell them apart and the message names both. The ordinary one is the
unpaired merge: the companion moved ahead, `make sync-webui` clears it. The other
is a bundle vendored from a companion commit **that was never pushed**, and there
`make sync-webui` is actively wrong: from the branch tip it rewinds the UI past
the work that was vendored, and from the local checkout it re-records the same
unpublished SHA. The fix there is to push the companion branch — until then the
release bundle cannot be rebuilt by anyone else, which is a defect worth a red in
its own right. `git -C <companion> fetch && git -C <companion> branch -r --contains <sha>`
separates them; `ls-remote | grep` does not, because an ancestor is published and
is the tip of nothing.

Four things a green from this check does **not** mean, listed because a gate
whose limits are unwritten gets read as covering more than it does:

- **Nobody edited `WEBUI_SOURCE` by hand.** Nothing binds the `commit=` line to
  the bytes beside it. Writing the companion's current tip into that line greens
  all three web checks at once — freshness compares the string it was given,
  `check-webui-embed` compares a fingerprint the bundle still matches because the
  bundle did not change, and `check-webui` does no byte comparison in CI. The
  script says "do not edit by hand"; that is a convention, not an enforcement.
- **The release train's companion branch exists.** When the companion has not
  opened the branch being assembled, the comparison falls back to the branch the
  bundle *was* vendored from. That keeps a new train from reding on day one, and
  it means the check obliges nothing about the new branch until the companion
  opens it — UI work landing elsewhere in the companion stays invisible.
- **The check ran.** `WEBUI_FRESHNESS_SKIP=1` exits zero, and `gate.sh` records
  the tier as PASS, so a declared skip and a performed check look identical in
  the summary table. `scripts/gate-test.sh` carries this as a known boundary for
  every tier, not just this one — the skip is visible in the log, not in the
  table.
- **Nothing in the environment answered for you.** Three variables change what
  these checks do, and each is deliberate for its own reason, which is exactly why
  a stale export of one is easy to miss. `GITHUB_REF_NAME` outranks the
  checked-out branch (a CI checkout is detached and has no branch to read) — so an
  export left over from an earlier command decides whether this tier binds, and
  `scripts/check-webui-freshness.sh --context` is how you ask instead of assume.
  A non-empty `CI` — **including `CI=false`** — routes `check-webui` to its quiet
  CI exception instead of the companion-absent failure; that costs the byte
  comparison only, since freshness still runs as its own tier. `WEBUI_REMOTE_URL`
  is the one to watch: it is the documented way to pin a companion, and pointing
  it at a local clone makes `ls-remote` answer from *that clone's* refs, so a
  clone nobody has fetched reports its own stale tip and the check goes **green**
  on a genuinely stale bundle. Of everything here it is the only bypass whose
  result is a green rather than a skip or a red.

The same rule is why `check-webui` now fails on a release branch when the companion
is missing: on `release/*` the companion is supposed to be checked out next to core,
so "no companion" there is an unconfigured tree, not a legitimate skip. `WEBUI_SKIP=1`
still passes, because a declared skip and an accidental one are different things.
CI is the exception, and knowingly so: making the byte comparison run there would
mean checking out the companion *and* running `npm run build` on every core push (a
checkout without a build is worse than none — it trips the "present but not built"
branch). In CI the binding check is `check-webui-freshness`, which asks the same
underlying question at the cost of one network round-trip.

## Why companion remains a separate repo

- **Different release cycles and dependencies.** The core (Go) and UI (TS+React, node_modules) are updated at different frequencies and with different toolchains; There is no need to mix assemblies.
- **Parallel front-end development** independent of core.
- **UI remains optional.** The operator can work via CLI (`soulctl`), MCP or directly OpenAPI; `web_ui_enabled: false` removes `/ui` without affecting `/v1/*` and `/docs`.

## Contact core

- **Contract:** [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) is the only public API contract.
- **TS client:** UI generates types via `openapi-typescript` (see `soul-stack-web/scripts/gen-api.sh`).
- **Authentication:** JWT ([ADR-013](../adr/0013-bootstrap-archon.md), [ADR-014](../adr/0014-operator-identity.md)) - the operator inserts the JWT into the UI manually (bootstrap token from `keeper init --archon` or token from `POST /v1/operators/{aid}/issue-token`); There is no separate `/v1/auth/login` endpoint yet.

See [ADR-035](../adr/0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui) (separation of distribution core ↔ UI) and [ADR-055](../adr/0055-embed-ui-bundle.md) (embed on `/ui`, amends ADR-035 p. 3).
