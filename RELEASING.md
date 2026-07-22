# Soul Stack release

Release procedure. Valid for beta (`vX.Y.Z-beta.N`) onwards.

Versioning invariant: **one git tag per repository root = one logical version of all 7 modules** go.work ([ADR-011](docs/adr/0011-go-layout.md)). There are no separate versions for `keeper`/`soul`/`soul-lint`/`soulctl`/`shared`/`sdk`/`proto`. The version is injected into the binary at the linking stage via `-X main.<var>` (see `Makefile`: `KEEPER_LDFLAGS`/`SOUL_LDFLAGS`/`SOULCTL_LDFLAGS`), the binary prints it with the command `keeper version` / `soul version` / `soulctl version`. This does not contradict ADR-007 (version of Service/Destiny/Module artifacts - git ref): the product assembly itself is versioned here, not user artifacts.

## Procedure

### (a) Freeze HEAD

Commit the release commit to `main`. From this moment on, only what is already in the tree is released; new features - after the tag.

### (b) Green Gate

Run the full gate on Linux-CI:

```sh
make check              # fmt + vet + build + test + drift-checks + vuln + lint examples
make test-integration   # testcontainers (needs docker)
make e2e                # L3a fast-loop (docker required)
```

Here are docker-dependent levels up to L3a. Long-term L3b (`make e2e-live`) is a separate **blocking** pre-tag step (e), L3c (`make e2e-k8s`) is chased on-demand. The release is not issued until the gate is green.

### (c) Bump CHANGELOG

In [CHANGELOG.md](CHANGELOG.md) (Keep a Changelog format) transfer what has been accumulated from `[Unreleased]` to the new version section `[vX.Y.Z-beta.N]`, put the date (or the note "the date is fixed with the tag" - according to the file style), list the known-limitations of the release in a separate block. `[Unreleased]` remains empty after this (for post-release backlog). The CHANGELOG change is included in the release commit before the tag.

### (d) Verifying the relevance of documentation (docs-currency gate)

**Required step before tag creation.** `docs-writer` audits
relevance of documentation - drift code↔doc for all documented
surfaces (API/OpenAPI, CLI `soulctl`, behavior of core modules and per-module
README, config-schemes, behavior of the Keeper↔Soul proto-contract). Each
the discrepancy is either closed by editing the document, or explicitly fixed (known-limitation
in CHANGELOG / flag `adr_drift` PM if the code and ADR diverge). Release not
is tagged as long as the surfaces being documented remain uncovered or
unfixed drift.

### (e) e2e-live gate (real apply on a real host) - blocking

**Required step before creating a tag.** unit/integration drive stubs; the only one
test proving that `apply` works on a **real** host end-to-end
soul binary in a privileged Debian container, real `apt`-install + systemd), —
L3b `make e2e-live` (nginx / drift / redis-cluster cases). Without green e2e-live tag
**not cut**: apply on a real host could break, and only this one will catch it
level. It's the local equivalent of CI-gate - without the GitHub minutes.

1. Docker-free gate - green:

   ```sh
   make check    # build + vet + test + check-gen/openapi/template/doc-links + vuln + lint
   ```

2. L3b real apply - **all three cases** are green:

   ```sh
   make e2e-live    # nginx / drift / redis-cluster — real apt-install + systemd
   ```

On **WSL2 + Docker-Desktop**, forward the real WSL2 host-IP before running
(the soul container will not reach the keeper via `host.docker.internal` - that
points to the DD-VM gateway, not the WSL2 host):

   ```sh
   E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') make e2e-live
   ```

On native-Linux env-override is not needed (CI default `host.docker.internal`).
Environment details and recipe - [tests/e2e-live/README.md](tests/e2e-live/README.md).

The release is not tagged until `make check` and all `make e2e-live` cases are green.

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

`make pkg` rebuilds binaries under `linux/$(PKG_ARCH)` (default `amd64`; `make pkg PKG_ARCH=arm64` - for arm) with the same ldflags injection version. `make sbom` builds SBOM in `app` mode (graph of what is actually linked). Both targets require external tooling (`nfpm`, `cyclonedx-gomod`) - they are not included in `make check`, they are set via `go install` (the hint is printed if not found). For bare cross-assembly of binaries without packages - `make build-linux`.

### (h) Giveaway

Attach artifacts from `dist/pkg/` and `dist/sbom/` to the GitHub Release of the corresponding tag (or distribute beta testers directly - in public beta distribution is also build-from-source, see [CONTRIBUTING.md](CONTRIBUTING.md)).

## Automated pipeline (GoReleaser)

The steps above describe the manual `make`-based flow (still valid for local
builds). On a real release the same artifacts are produced automatically by
**GoReleaser** on a `v*` tag push — see [`.goreleaser.yaml`](.goreleaser.yaml)
and [`.github/workflows/release.yml`](.github/workflows/release.yml). One tag
run yields:

- binaries + `.tar.gz` archives (linux amd64/arm64), `.deb` + `.rpm` (nfpm),
  a Homebrew tap entry, and a `checksums.txt`;
- multi-arch GHCR images `ghcr.io/souls-guild/soul-stack/{keeper,soul}`;
- **cosign keyless** signatures (checksums blob + images) via the workflow's
  OIDC token — no long-lived key;
- **syft** CycloneDX SBOMs per archive.

Validate the config without releasing: `goreleaser check`. Dry-run the whole
pipeline offline: `goreleaser release --snapshot --clean`.

Two distribution channels run **out of band** from the tag workflow because
they need credentials we keep off GitHub Actions:

- **apt repo on Cloudflare R2** — mirror the `.deb` assets with
  [`deploy/apt-r2/publish-apt.sh`](deploy/apt-r2/README.md) after the release.
- **curl-installer** — `scripts/install.sh` pulls a released binary by tag and
  verifies its checksum (`curl -fsSL …/install.sh | sh`).
