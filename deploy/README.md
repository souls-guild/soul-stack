# deploy/ — packaging and deployment of Soul Stack

Build/ops artifacts for running the three binaries (`keeper`, `soul`, `soul-lint`) in
a container and under systemd. Not domain entities — no business logic here, only
build and run wrappers.

## docker/

Three multi-stage Dockerfiles. Builder — `golang:1.26.3` (synchronized with
`go.work` / `go.mod`), runtime — `gcr.io/distroless/static:nonroot`: a static
binary without shell/libc/package manager, unprivileged user
(uid 65532). Static build (`CGO_ENABLED=0`), `-trimpath`, `-ldflags "-s -w"`.

Build context — **repo root** (that's where `go.work` + all modules live). Examples:

```sh
docker build -f deploy/docker/keeper.Dockerfile    -t soul-stack/keeper    --build-arg VERSION=$(git describe --tags --always --dirty) .
docker build -f deploy/docker/soul.Dockerfile      -t soul-stack/soul      --build-arg VERSION=$(git describe --tags --always --dirty) .
docker build -f deploy/docker/soul-lint.Dockerfile -t soul-stack/soul-lint .
```

`VERSION` is injected via ldflags in the form `-X main.<var>` (entrypoint — package
`main`; the full import-path form is silently ignored by the linker). Versioned:
`soul` (`main.soulVersion`, printed in Hello/BootstrapRequest for audit),
`keeper` (`main.version`, prints `keeper version`) and `soulctl`
(`main.soulctlVersion`, via `make build`). `soul-lint` doesn't yet have a
package-level version variable — `--build-arg VERSION` is reserved for it but not
passed to `-X`.

`.dockerignore` at the repo root excludes `.git`, local `bin/`, `dev/`, `docs/`,
`.pm/` from the context.

## Keeper in production — image + config

Production scenario: build the `keeper` image, push it to **your own** registry, roll it
with **your own** Helm chart (Soul Stack does not ship k8s manifests — you need an
image/binary).

### Build and publish the image

`make docker-keeper` builds the production image from `deploy/docker/keeper.Dockerfile`
(multi-stage, distroless-nonroot, version baked into the binary and OCI label) with tag
`$(KEEPER_IMAGE):$(VERSION)`. `VERSION` — `git describe` (or a release override),
`KEEPER_IMAGE` — image name (default `soul-stack/keeper`):

```sh
make docker-keeper                                   # soul-stack/keeper:<git-describe>
make docker-keeper VERSION=v0.2.0                    # fixed release tag
make docker-keeper KEEPER_IMAGE=registry.example.com/soul-stack/keeper VERSION=v0.2.0
```

The image is self-contained (toolchain pinned in the builder stage) — `make build-linux`
is not needed beforehand. Retag for your registry and push:

```sh
docker tag  soul-stack/keeper:v0.2.0 registry.example.com/soul-stack/keeper:v0.2.0
docker push registry.example.com/soul-stack/keeper:v0.2.0
```

ENTRYPOINT — `keeper`, CMD — `run` (daemon, **without** `--initialize`). A versioned
tag instead of `latest` — reproducible rollback and precise audit via `keeper version`.

### What keeper expects in production

The image carries only the binary. Everything else — the operator supplies via mount/env;
a complete config example — `examples/keeper/keeper.yml`.

- **Config** — `keeper.yml`, mounted at `/etc/keeper/keeper.yml`
  (default binary path; CMD does not pass `--config`). In k8s — ConfigMap →
  volumeMount. Non-secret settings: `kid`, `listen`, `pool`, `otel`, `logging`,
  `reaper`, `acolytes`.
- **Vault — hard-required** (keeper won't start without it): `vault.addr` + auth
  (approle `role_id` + `secret_id_file`, or token). Keeper resolves from Vault
  PKI (SoulSeed), the JWT signing key, Essence secrets, SSH CA, cloud credentials.
  Supply `secret_id` via a k8s Secret → file, path in `vault.auth.secret_id_file`.
- **Postgres** — `postgres.dsn_ref` (vault-ref, e.g. `vault:secret/keeper/postgres`,
  so the DSN with password doesn't sit in a ConfigMap). Cold storage of cluster state.
- **Redis** — `redis.addr` + `redis.password_ref` (vault-ref). Heartbeat/lease/
  pub-sub/Reaper leader; required for HA clusters (`acolytes > 0`).
- **JWT** — `auth.jwt.signing_key_ref` (vault-ref) + `issuer` + TTL. Without the
  `auth.jwt` block, both `keeper init` and `keeper run` fail.
- **Ports** (`listen`): gRPC bootstrap (`9442`, server-only TLS, separate
  listener), gRPC event_stream (`9443`, mTLS), OpenAPI (`8080`), MCP (`8081`),
  metrics (`9090`). TLS material (`server.crt/key`, `ca.crt`) — mounted into
  `/etc/keeper/tls/` from a Secret.

### First bootstrap (ONCE per cluster)

`keeper run` deliberately refuses to start when the `operators` registry is empty and
`--initialize` is **not** passed (ADR-013) — protection against silent auto-bootstrap. The
first Archon is created with a separate command using the same image and config (one-shot
Job / `kubectl run` / `docker run --rm`):

```sh
keeper init --archon=archon-ops-01 --config /etc/keeper/keeper.yml
```

The command, under a PG advisory lock, verifies the registry is empty, creates the first Archon
(`cluster-admin`, `permissions: ["*"]`), issues a bootstrap JWT (TTL from
`auth.jwt.ttl_bootstrap`) and writes it to a `mode 0400` file. Afterwards, the Deployment with
`keeper run` starts normally (the registry is no longer empty).

## systemd/

Units for `keeper` and `soul` (`soul-lint` has no unit — it's a CLI). Run under the
system user `soul-stack`, `Restart=on-failure`, `After=network-online`,
config path passed via `EnvironmentFile` (`keeper.env` / `soul.env`).

Hardening differs by role:

- **keeper** — strict profile (`ProtectSystem=strict`, `MemoryDenyWriteExecute`,
  `PrivateDevices` etc.): Keeper doesn't touch the host, so isolation doesn't get in the way.
- **soul** — relaxed profile: Soul applies Destiny (installs packages, edits files,
  manages services), so writing to the system is NOT forbidden. Don't tighten this without
  checking the apply cycle.

Installation instructions (creating the user, directories, permissions) — in the header of
each `.service` file.

## nfpm/

Native package configs for deb/rpm — `keeper.yaml`, `soul.yaml`, `soul-lint.yaml`.
Each packages the built binary (`*/bin/<name>`) and, for daemons, the systemd unit +
env file from `systemd/` + a sample config from `examples/`. `soul-lint` — a CLI, no
unit or config, just the binary.

Version and architecture are substituted from the environment (`${VERSION}` / `${ARCH}`),
which `make pkg` provides. The operator config (`/etc/keeper/keeper.env`,
`/etc/soul/soul.env`) is marked `config|noreplace` — upgrades won't overwrite it.
The main config sample is shipped under a separate name (`keeper.yml.example` /
`soul.yml.example`); the operator creates the working `*.yml` themselves.

## Packaging — how to build

All release artifacts are written to `dist/` (in `.gitignore`, not committed).
Targets are additive: not part of `make check`, and don't break it.

### SBOM (`make sbom`)

CycloneDX SBOM for the three release binaries via `cyclonedx-gomod` in `app` mode
(SBOM of what's actually linked into the binary, one file per binary in `dist/sbom/`:
`keeper.cdx.json` / `soul.cdx.json` / `soul-lint.cdx.json`). `app` mode (not
`mod`) — because the repo uses `go.work`: `mod` under the workspace for any module
produces the SBOM of the root module, and with `GOWORK=off` modules with local
cross-module dependencies don't resolve. The SBOM of the three binaries transitively
covers the library modules (`proto`/`sdk`/`shared`). If the tool isn't in PATH, the
target prints a hint and exits with an error (not silently):

```sh
go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest
make sbom
```

### deb/rpm (`make pkg`)

Native packages via `nfpm` (deb + rpm for each of the three binaries in
`dist/pkg/`). Binaries are rebuilt for `linux/$(PKG_ARCH)` (deb/rpm are always
Linux, the dev machine may be darwin); architecture overridden via
`make pkg PKG_ARCH=arm64`. If `nfpm` isn't in PATH — a hint and error exit:

```sh
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
make pkg
```

Installing the built package:

```sh
sudo dpkg -i dist/pkg/soul-stack-keeper_<version>_amd64.deb     # deb distros
sudo rpm  -i dist/pkg/soul-stack-keeper-<version>.x86_64.rpm    # rpm distros
# then: cp /etc/keeper/keeper.yml.example /etc/keeper/keeper.yml — edit the config
```

### Image signing (cosign) — post-publish, once a registry exists

Signing images and packages via cosign/sigstore is **deferred**: real signing
requires a registry to publish images to + keyless identity via OIDC (or a
private signing key). A local repo without CI/registry has neither.

`make sign` — a documented stub: prints the reason it's deferred and a link to this
section, and exits successfully (doesn't block the pipeline). Once CI + registry exist,
the plan is:

- keyless image signing in CI: `cosign sign <registry>/soul-stack/keeper:<tag>`
  under the workflow's OIDC identity (Fulcio issues an ephemeral certificate, Rekor
  logs transparency);
- verification at deploy time: `cosign verify --certificate-identity=<workflow>
  --certificate-oidc-issuer=<issuer> <image>`;
- optionally — attach the SBOM from `make sbom` to the image via `cosign attach sbom`.

## Deferred (next pass)

- **Signing images and packages** (cosign / sigstore) — see the section above, waiting
  on CI + registry.
- **Version variable** in `soul-lint`'s main package (then the ldflags `-X` and
  `--build-arg VERSION` will start injecting a version into it too — as already done for
  `soul`/`keeper`/`soulctl`).
