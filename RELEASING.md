# Soul Stack release

Release procedure. Valid for beta (`vX.Y.Z-beta.N`) onwards.

Versioning invariant: **one git tag per repository root = one logical version of all 7 modules** go.work ([ADR-011](docs/adr/0011-go-layout.md)). There are no separate versions for `keeper`/`soul`/`soul-lint`/`soulctl`/`shared`/`sdk`/`proto`. The version is injected into the binary at the linking stage via `-X main.<var>` (see `Makefile`: `KEEPER_LDFLAGS`/`SOUL_LDFLAGS`/`SOULCTL_LDFLAGS`), the binary prints it with the command `keeper version` / `soul version` / `soulctl version`. This does not contradict ADR-007 (version of Service/Destiny/Module artifacts - git ref): the product assembly itself is versioned here, not user artifacts.

## Procedure

### (a) Freeze HEAD

Commit the release commit to `main`. From this moment on, only what is already in the tree is released; new features - after the tag.

### (b) Green Gate

Run the full gate — one target covers the three tiers:

```sh
make check-all          # = check + test-race + test-integration (L1) + e2e (L3a); docker required
```

Where the race detector runs: `make test-race` over the untagged unit corpus (that
is where the concurrent code lives) and `make test-integration` over the tagged
packages. `make test` — and therefore `make check` — runs **without** it, so a
green `check` is silent about data races by design; the full table is in
[docs/testing/README.md](docs/testing/README.md).

`make check` alone is docker-free by design and therefore says nothing about L1
or L3a; it now prints the tiers it skipped. Do not measure release readiness with
it (see [docs/testing/README.md](docs/testing/README.md) for what a green `check`
was actually worth: nine tests were red behind one).

Here are docker-dependent levels up to L3a. Long-term L3b is step (e) and is the one step the pipeline enforces by itself — `make e2e-live-gate` runs on the tagged sha and goreleaser is behind it. L3c (`make e2e-k8s`) is chased on-demand. The release is not issued until the gate is green.

**Then confirm CI verified the exact commit you are about to ship — blocking:**

```sh
make check-ci           # verdict for HEAD; REF=release/R5 to ask about another ref
```

This is not the same question as `check-all`, and neither substitutes for the
other: `check-all` is this machine's verdict, `check-ci` is the remote's verdict
**about one sha**. Ask it by sha rather than by branch, because a branch listing
answers a different question than the one that matters (NIM-339):

- a green run on the branch may belong to an **older** sha — on release/R5 a green
  run sat on `7af81dc7` while the branch had moved two commits further, one of
  them a render change CI had never seen;
- a run for your sha may have been **evicted** by the next push and reported
  `cancelled`, which is neither pass nor fail — in the run list it sits next to
  `failure`, in memory next to `success`;
- an unpushed sha has no run at all, which looks exactly like a run nobody read.

`check-ci` exits non-zero for every one of those, and prints which one it was.
Eviction no longer happens on `main` or `release/*` (the concurrency exception in
`.github/workflows/ci.yml`), but a queued or unfinished run is still not a
verdict, so the check stays.

### (c) Bump CHANGELOG

In [CHANGELOG.md](CHANGELOG.md) (Keep a Changelog format) transfer what has been accumulated from `[Unreleased]` to the new version section `[vX.Y.Z-beta.N]`, put the date (or the note "the date is fixed with the tag" - according to the file style), list the known-limitations of the release in a separate block. `[Unreleased]` remains empty after this (for post-release backlog). The CHANGELOG change is included in the release commit before the tag.

### (c2) Stamp `introduced_in` with the version being cut

**Required step before tag creation.** Everything added since the previous tag has no `introduced_in` yet: an engine feature carries the release it first shipped in, and until the number exists there is nothing to write ([ADR-0076(i)](docs/adr/0076-engine-compat-window.md)). Now it exists — so every keeper-side feature added in this cycle gets stamped `X.Y.Z` (the version of THIS release):

- **DSL grammar** — the rows marked `Unreleased` in `keeperDSLFeatures` ([`shared/config/introduced.go`](shared/config/introduced.go)); add a row for any grammar element this cycle introduced and does not have one.
- **Core modules** — `introduced_in:` on the module, the state or the parameter in [`shared/coremanifest/*.yaml`](shared/coremanifest), for every module / state / parameter that did not exist in the previous release.

The value is what the tag will be, without the `v` prefix and without the pre-release suffix (`v0.2.0-beta.1` → `0.2.0`) — a window is declared at release granularity. Getting this wrong is not cosmetic: a missing stamp silently disables the cross-check that catches a `compat:` window promising more than the definition can deliver, and a stamp naming an unreleased version would reject definitions that render correctly today.

### (c3) Confirm the embedded UI is the one being released

**Required step before tag creation, and it must be run explicitly.** `keeper`
ships the operator UI inside the binary (`go:embed`, [ADR-055](docs/adr/0055-embed-ui-bundle.md)),
vendored from the companion `soul-stack-web`. If the companion moved and the
vendored copy was never re-synced, the release binary serves a UI nobody built
from current sources — silently, because a stale bundle carries its own correct
fingerprint. That has happened three times, and every time a human found it, not
a gate ([docs/web/README.md](docs/web/README.md#what-the-guards-do-and-do-not-cover)).

`make check-webui-freshness` is the check that sees it, and it is **binding on
`release/*` and `hotfix/*`, not on `main`** — deliberately, since third-party
clones sit on `main` and must not red because the companion moved. A tag is cut
from `main`, so the one place the check would matter most is the one place it
does not bind on its own. Hence: run it here, by hand, against the branch this
release came from, and treat its answer as blocking.

```sh
REL=<REL>                                    # e.g. R5, spelled as the branch spells it
export GITHUB_REF_NAME="release/${REL}"
test "$(scripts/check-webui-freshness.sh --context)" = required &&
  make check-webui-freshness
```

The first line is not ceremony. `GITHUB_REF_NAME` is what makes this run binding, and
anything that is not literally `release/…` or `hotfix/…` — `Release/R5`, `releases/R5`,
a shell that ate the variable — comes out **advisory and exits 0**, which is the same
answer a clean tree gives. A typo in the one variable this step turns on would therefore
read as a pass, on the step whose whole purpose is to be blocking. Asking `--context`
first makes the two outcomes different: with the name misspelled the `&&` short-circuits,
the check never runs, and nothing prints a green.

A red here means re-vendor before tagging (`make sync-webui` in core with the
companion checked out next to it, on `release/<REL>`, clean and with its
dependencies installed), then commit the updated `assets/` + `WEBUI_SOURCE`
along with the release commit. `WEBUI_FRESHNESS_SKIP=1` exists for working
offline; using it to get past this step is cutting a release without knowing
which UI is in it.

### (d) Verifying the relevance of documentation (docs-currency gate)

**Required step before tag creation.** `docs-writer` audits
relevance of documentation - drift code↔doc for all documented
surfaces (API/OpenAPI, CLI `soulctl`, behavior of core modules and per-module
README, config-schemes, behavior of the Keeper↔Soul proto-contract). Each
the discrepancy is either closed by editing the document, or explicitly fixed (known-limitation
in CHANGELOG / flag `adr_drift` PM if the code and ADR diverge). Release not
is tagged as long as the surfaces being documented remain uncovered or
unfixed drift.

### (e) e2e-live gate (real apply on a real host) - ENFORCED, not asked

**This step is the one the pipeline refuses on.** unit/integration drive stubs; the
only tests proving that `apply` works on a **real** host end to end (a real soul binary
in a privileged Debian container, real `apt`-install + systemd) are L3b, and
`make e2e-live-gate` is the five of them that block a tag.

**How it is enforced (NIM-879).** `.github/workflows/release.yml` runs
`make e2e-live-gate` against the tagged sha as the job `live-gate`, and the
`release` job declares `needs: live-gate`. So a tag whose live tier is red — or
never ran — produces **nothing from that workflow**: no GitHub Release, no GHCR
image, no cosign signature, no deb/rpm/brew/AUR/winget. There is no input that
skips it, no `continue-on-error` on the path and no `if:` calling a status function
— `always()`, `failure()`, `cancelled()` **or `success()`**, each of which drops a
job's implicit "all needs succeeded"; `success() || <anything>` releases over a red
gate while looking careful. `make check-release-gate` asserts those on every push,
along with the rule that every command named in this file is either gated on a tag or listed in
[`scripts/release-gate-allowlist.txt`](scripts/release-gate-allowlist.txt) with the
reason it stays a human's.

**Three things it does not prevent**, so that nobody reads the paragraph above as
wider than it is:

- Pushing the tag ref. Not blockable from inside the repository. It leaves a tag with
  no release, which is visible, and is re-run once the tier is green:

  ```sh
  gh workflow run release.yml --ref vX.Y.Z-beta.N
  ```

- Building `dist/pkg` by hand (steps (g)/(h) below) and attaching it to a Release made
  with `gh release create`. [`apt-publish.yml`](.github/workflows/apt-publish.yml) fires
  on `release: published` — any release — and would mirror those `.deb`s into the R2 apt
  pool. Closing that path needs a repository ruleset on who may publish a release, which
  is an organisation setting rather than a file in this tree.
- `workflow_dispatch` against an arbitrary ref: a branch sitting at the tag's commit with
  `needs:` deleted publishes a full release, and the only thing that objects is the
  `check` job on that push.

> ★ **Why this section used to read differently.** Until NIM-879 it said "required
> step before creating a tag" and nothing executed it. The L3b tier ran in no CI job
> that had ever fired, so `make e2e-live-gate` sat **red on the release tip from
> migration 118** (`i.name` → `i.id`) and it surfaced only because someone ran the
> tier by hand while working on NIM-876. A month of decisions cited a green that was
> never produced. If you are tempted to add a step here that only a human performs,
> read that sentence again and then add the allowlist line that says so out loud.

**Run it locally first anyway.** The tag run is where it is enforced, not where you
want to discover it — a red gate at that point costs the tag and 45 minutes. Before
step (f):

1. Docker-free gate - green:

   ```sh
   make check    # build + vet + test + check-gen/openapi/template/doc-links + vuln + lint
   ```

2. L3b, the five gate tests - **every case** is green:

   ```sh
   make e2e-live-gate    # the same target release.yml will run on the tag
   ```

   The broader tier (`make e2e-live`, every test behind the `e2e_live` tag) is
   dispatched from `.github/workflows/nightly.yml` and is not on a schedule — the
   repository is private, and a daily hour of L3b would consume most of the monthly
   Actions budget. Nothing therefore finds this tier's rot between releases; the tag
   gate is the backstop, and running it locally is how you avoid meeting it there.

   > ★ **What green here means, and what it does not (NIM-871 → NIM-876).** The gate went
   > from nine tests to three when `examples/service/redis` left the engine, and green then
   > meant a module was *delivered* rather than a service *operated*. NIM-876 brought the
   > second claim back with a subject that is not in this tree: the two
   > `TestL3bRedisServiceLive_*` cases run `github.com/soul-stack-services/redis` at the
   > commit pinned in `tests/e2e-live/harness/servicecatalog.go` — a create onto a roster,
   > then a day-2 scenario against the live instance.
   >
   > Still uncovered, and worth knowing before a tag: `update_config`, `restart`, `destroy`
   > and `rotate_tls` (scenarios the published service does not have yet), and the whole
   > **machine** half of a create — VMs, cloud-init, agent install over SSH — which a docker
   > tier whose souls are already onboarded cannot host. That half is proven by a hand-run
   > workstation stand and by nothing automated.
   >
   > **Bumping the pin is part of releasing a service change, not of releasing the engine.**
   > The gate proves the pinned commit; a fix in the service repository does not reach a tag
   > here until someone bumps that pin and re-runs this step.

On **WSL2 + Docker-Desktop**, forward the real WSL2 host-IP before running
(the soul container will not reach the keeper via `host.docker.internal` - that
points to the DD-VM gateway, not the WSL2 host):

   ```sh
   E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') make e2e-live-gate
   ```

On native Linux the override is not needed — the recipe falls back to the first
address `hostname -I` reports, which is what the Actions runner uses too.
Environment details and recipe - [tests/e2e-live/README.md](tests/e2e-live/README.md).

**If it goes red, read the label before deciding anything.** `make e2e-live-gate`
classifies each gate test as **STAND-SETUP** (the harness declared that bring-up
failed - no assertion ran, rerun that test alone), **TEST-FAILURE** (a finding) or
**NOT-RUN** (skipped or never started - never a pass). A tag is cut on a run where
every gate test says `--- PASS`, and on nothing else: STAND-SETUP is a reason to
rerun one test, not a reason to tag. Details - [docs/testing/README.md, "Reading a
red gate"](docs/testing/README.md#reading-a-red-gate-nim-406).

A red gate no longer depends on anyone acting on it: the tag run stops at `live-gate`
and `release` never starts. Reading the label is how you find out **why** in one pass
instead of rerunning until green.

### (f) Annotated git tag

One tag per repository root:

```sh
git tag -a vX.Y.Z-beta.N -m "Soul Stack vX.Y.Z-beta.N"
git push origin vX.Y.Z-beta.N
```

The first beta tag is `v0.1.0-beta.1`. **annotated** tag (not lightweight): `git describe` takes the nearest annotated tag, and that's what ends up in `VERSION` when built.

### (g) Assembling artifacts on a tag

With the checked-out tag (so that `git describe` gives a clean version without `-dirty`/hash) collect release artifacts:

```sh
make pkg    # native packages deb + rpm (nfpm) → dist/pkg/, binaries for linux/amd64
make sbom   # CycloneDX SBOM by keeper/soul/soul-lint → dist/sbom/
```

`make pkg` runs a packages-only `goreleaser --snapshot` build, so it yields exactly the release set - every package, `amd64` + `arm64`, deb + rpm + apk - from the same `nfpms:` definition the tag build uses. `make sbom` builds SBOM in `app` mode (graph of what is actually linked). Both targets require external tooling (`goreleaser`, `cyclonedx-gomod`) - they are not included in `make check`, they are set via `go install` (the hint is printed if not found). For bare cross-assembly of binaries without packages - `make build-linux`.

### (h) Giveaway

Attach artifacts from `dist/pkg/` and `dist/sbom/` to the GitHub Release of the corresponding tag (or distribute beta testers directly - in public beta distribution is also build-from-source, see [CONTRIBUTING.md](CONTRIBUTING.md)).

## Automated pipeline (GoReleaser)

The steps above describe the manual `make`-based flow (still valid for local
builds). On a real release the same artifacts are produced automatically by
**GoReleaser** on a `v*` tag push — see [`.goreleaser.yaml`](.goreleaser.yaml)
and [`.github/workflows/release.yml`](.github/workflows/release.yml). GoReleaser
does not start until the `live-gate` job it depends on is green (step (e)), so one
tag run is two jobs and the first is the L3b tier. One tag run yields:

- binaries + `.tar.gz` archives (linux amd64/arm64), `.deb` + `.rpm` (nfpm),
  a Homebrew tap entry, winget manifests, and a `checksums.txt`;
- the AUR package [`soul-stack-bin`](https://aur.archlinux.org/packages/soul-stack-bin)
  (`yay -S soul-stack-bin`), pushed over SSH with the `AUR_KEY` secret;
- multi-arch GHCR images `ghcr.io/souls-guild/soul-stack/{keeper,soul}`;
- **cosign keyless** signatures (checksums blob + images) via the workflow's
  OIDC token — no long-lived key;
- **syft** CycloneDX SBOMs per archive.

Validate the config without releasing: `goreleaser check`. Dry-run the whole
pipeline offline: `goreleaser release --snapshot --clean`.

Two distribution channels run **out of band** from the tag workflow:

- **apt repo on Cloudflare R2** — a separate workflow,
  [`apt-publish.yml`](.github/workflows/apt-publish.yml), fires on `release: published`
  and mirrors the `.deb` assets into the R2 pool by running
  [`deploy/apt-r2/publish-apt.sh`](deploy/apt-r2/README.md) with the R2 and GPG secrets
  release CI does not carry. It is out of band from the tag workflow, not out of band from
  Actions — the earlier wording here said this was a manual step after the release, which
  stopped being true when that workflow landed (NIM-174). Running the script by hand is
  still the fallback, and `workflow_dispatch` re-publishes an already-released tag.

  ★ It fires on **any** published release, including one created by hand, so it is the one
  publishing path the live gate does not cover — see step (e).
- **curl-installer** — `scripts/install.sh` pulls a released binary by tag and
  verifies its checksum (`curl -fsSL …/install.sh | sh`).

**winget publishes stable tags only.** On a stable tag GoReleaser renders the
manifests, pushes them to `souls-guild/winget-pkgs` on a per-version branch and
opens a pull request against `microsoft/winget-pkgs`; `skip_upload: auto` holds
pre-release tags back, because `winget upgrade` has no opt-in pre-release channel
and would offer a beta to everyone as the newest version. Betas still render
their manifests into `dist/`. The pipe needs a `WINGET_GITHUB_TOKEN` repository
secret — a **classic** PAT with `public_repo`, since fine-grained tokens cannot
open a pull request outside their owner's account. If the pull request fails to
open, GoReleaser logs it and the release still succeeds.

Landing is not instant: submissions go through the repository's automated
validation pipeline, and a moderator steps in when it flags something. Watch the
PR — the bot closes it after 7 days without a response.

A tag whose release is already published cannot be re-run to emit this PR:
`release.Pipe` fails on duplicate assets before the winget pipe is reached. The
0.1.0-beta.1 submission was therefore made once by hand from these same generated
manifests (upstream PR
[#407547](https://github.com/microsoft/winget-pkgs/pull/407547)).

**AUR publishes every tag, betas included** — Arch users track the beta on
purpose, and `pkgver` stays legal because GoReleaser rewrites `-` as `_`. Its
`skip_upload` is gated on `AUR_KEY` being present, so a missing secret skips the
push instead of failing the tag. The same already-published-tag limitation applies
(AUR runs after `release.Pipe` too), so `soul-stack-bin` 0.1.0_beta.1 was pushed
once by hand from the PKGBUILD this config generates; later tags publish on their
own. The package must install the binaries explicitly — GoReleaser's guessed
`package()` would ship only the first one.
