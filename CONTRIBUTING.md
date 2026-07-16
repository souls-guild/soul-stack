# Participation in Soul Stack

Project status is **closed small beta**. At this stage, contributions are accepted in one form: **bug reports**. Pull requests from external participants are not accepted; the source code for testers is read-only.

## What is accepted now

- **Bug reports** via GitHub Issues private repository. Describe: version (`keeper version` / `soul version` - printed at the start of the binary), environment (OS, Postgres/Redis/Vault versions), reproduction steps, expected and actual behavior, relevant logs. A minimal reproducible script (Destiny/scenario file) speeds up parsing.
- **Questions and suggestions** - also via Issues, with a separate tag.

## What is NOT accepted now

- **Pull requests with code.** External code changes are not accepted in closed beta. The code is read-only and local assembly (for reproducing bugs - see below).
- The PR template in `.github/PULL_REQUEST_TEMPLATE.md` serves internal process, not external contributions.

## CLA

Contributor License Agreement is not yet **established**. According to [ADR-016](docs/adr/0016-parity-license.md) the CLA starts when the first external contributor appears; Since external code contributions are not accepted in the closed beta, there is no need for a CLA yet. The CLA will appear simultaneously with the opening of indemnities - PRs with the code will still not be accepted until signing.

## Build from source to check the bug

Beta distribution - build-from-source: clone a private repository and build the binaries locally.

Environment requirements: Go (version - see `go.work` / `go.mod`), `make`, `protoc` with plugins `protoc-gen-go` / `protoc-gen-go-grpc` (needed only for `make gen`, not for bare assembly of already committed code).

```sh
git clone <private-repo-url> soul-stack
cd soul-stack
make build          # keeper / soul / soul-lint / soulctl → <module>/bin/
```

Binaries are placed in `keeper/bin/keeper`, `soul/bin/soul`, `soul-lint/bin/soul-lint`, `soulctl/bin/soulctl`. Check the version of the compiled binary:

```sh
./keeper/bin/keeper version
```

The version is injected from git during the build (`git describe`); overridden via `make build VERSION=...` ([ADR-011](docs/adr/0011-go-layout.md)).

To reproduce the bug on a live stack, the local dev circuit (Postgres + Redis + Vault via docker-compose) is raised with the commands `make dev-up` / `make dev-stand`; details - [docs/dev/local-setup.md](docs/dev/local-setup.md). Quick start of the operator - [docs/getting-started.md](docs/getting-started.md).

## Before starting an Issue

- Check the list of known limitations in beta - [docs/known-limitations.md](docs/known-limitations.md): some of the behavior there is a deliberate out-of-scope, not a bug.
- Check that the bug is reproduced on a freshly compiled binary from the current `main`.
