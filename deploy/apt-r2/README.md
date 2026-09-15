# apt repository on Cloudflare R2

Soul Stack publishes its `.deb` packages — the `soul-stack-keeper` and
`soul-stack-soul` daemons, the `soul-stack-soulctl`, `soul-stack-lint`,
`soul-stack-trial` and `soul-stack-legion` CLIs, and the `soul-stack-tools` meta
package that pulls all four in at once — through a plain, flat apt repository
hosted on a **Cloudflare R2** bucket fronted by the public domain
`https://apt.soul-stack.com`.
The GitHub release workflow (`.github/workflows/release.yml`) produces the `.deb`
assets; a **separate** workflow ([`apt-publish.yml`](../../.github/workflows/apt-publish.yml))
mirrors them into the apt pool after each release.

`publish-apt.sh` does the mirroring — the CI workflow just runs it, and an operator can
run the same script by hand as a fallback. This file is the one-time setup and the
per-release reference.

## Credentials & security

Mirroring needs two credentials that release CI (cosign keyless / GitHub OIDC) does not
provide:

- an **R2 access key** to write the bucket, and
- the **GPG key** that signs the apt `Release`.

Both live as **repo secrets** and are injected into the publish workflow. This is a
deliberate, bounded trade-off:

- GitHub-hosted runners mean no self-hosted machine to own. The minutes are metered — the
  repository is **private** (`gh api repos/souls-guild/soul-stack -q .private` → true),
  and this text claimed the opposite until NIM-879 — but the job is short and fires only
  on a release.
- A private repository has no untrusted fork PRs to leak secrets to, and both triggers
  (`release: published`, `workflow_dispatch`) require write access anyway. The earlier
  version of this bullet rested the same conclusion on the repo being public; the
  conclusion holds, its stated reason did not.
- The R2 token is **scoped to Object Read & Write on the single apt bucket** (not the
  whole account), and the signing key is a **dedicated apt key**, not a personal one.
  Blast radius is one bucket + one rotatable signing key.

## One-time setup

### 1. R2 bucket + public domain

1. Create an R2 bucket, e.g. `soul-stack-apt`.
2. Attach a public custom domain (R2 → Settings → Public access → custom domain), e.g.
   `apt.soul-stack.com`. Objects then serve at `https://apt.soul-stack.com/<key>`.
3. Create an R2 API token (Account → R2 → Manage API Tokens) with **Object Read &
   Write** scoped to that bucket. Note the access key id / secret and the account id (the
   S3 endpoint is `https://<accountid>.r2.cloudflarestorage.com`).

### 2. Signing GPG key

Generate a dedicated repo-signing key (once) and back up the private key securely:

```sh
gpg --quick-generate-key "Soul Stack apt signing <noreply@soul-stack.com>" rsa4096 sign never
```

Use its long key id (or uid) as `APT_GPG_KEY_ID`. `publish-apt.sh` exports the public
half to `soul-stack.gpg.key` in the bucket root on every run, so clients can trust it.

### 3. Wire up the workflow secrets/variables

The [`apt-publish.yml`](../../.github/workflows/apt-publish.yml) workflow reads:

| Kind | Name | Value |
|------|------|-------|
| secret | `R2_ACCESS_KEY_ID` | R2 token access key id |
| secret | `R2_SECRET_ACCESS_KEY` | R2 token secret |
| secret | `APT_GPG_PRIVATE_KEY` | ASCII-armored **private** signing key (`gpg --armor --export-secret-keys <id>`) |
| variable | `R2_ACCOUNT_ID` | Cloudflare account id (for the S3 endpoint) |
| variable | `APT_GPG_KEY_ID` | signing key id, e.g. `noreply@soul-stack.com` |
| variable | `RCLONE_REMOTE` | `r2_soul_stack_apt:soul-stack-apt` |

```sh
gh secret   set R2_ACCESS_KEY_ID     -R souls-guild/soul-stack
gh secret   set R2_SECRET_ACCESS_KEY -R souls-guild/soul-stack
gh secret   set APT_GPG_PRIVATE_KEY  -R souls-guild/soul-stack < apt-signing-private.asc
gh variable set R2_ACCOUNT_ID  -R souls-guild/soul-stack -b <accountid>
gh variable set APT_GPG_KEY_ID -R souls-guild/soul-stack -b noreply@soul-stack.com
gh variable set RCLONE_REMOTE  -R souls-guild/soul-stack -b r2_soul_stack_apt:soul-stack-apt
```

The workflow creates its rclone remote named `r2_soul_stack_apt`, so `RCLONE_REMOTE`
is identical in CI and on any operator machine.

## Automation

[`apt-publish.yml`](../../.github/workflows/apt-publish.yml) runs on `ubuntu-latest` when
a GitHub Release is **published**, and can be re-run by hand for a given tag:

```sh
gh workflow run apt-publish.yml -R souls-guild/soul-stack -f tag=v0.1.0-beta.1
```

It installs `apt-utils`/`rclone`, configures the R2 remote from the secrets above,
imports the signing key, downloads the release's `*.deb` assets, and runs
`publish-apt.sh`.

## Manual publish (fallback)

From a machine with `rclone` (remote `r2_soul_stack_apt` configured), `gpg` (signing key
imported) and `apt-ftparchive` (apt-utils):

```sh
gh release download vX.Y.Z -R souls-guild/soul-stack -p '*.deb' -D dist/pkg

export APT_GPG_KEY_ID="noreply@soul-stack.com"
export RCLONE_REMOTE="r2_soul_stack_apt:soul-stack-apt"
deploy/apt-r2/publish-apt.sh
```

The script builds `pool/main/` + `dists/stable/…` indexes (including per-digest
`by-hash/` copies — see below), signs `Release` (detached `Release.gpg` + inline
`InRelease`), exports the public key as `soul-stack.gpg.key`, and `rclone sync`s the
whole tree to the bucket. It is idempotent — re-running re-indexes and re-syncs, and
`--delete-before` prunes packages dropped from a release.

It passes `--s3-no-check-bucket` because a bucket-scoped R2 token cannot
`HeadBucket`/`CreateBucket` at the account level (the default probe would `403`). rclone
against R2 may also log a one-off `501 NotImplemented` on the first `PutObject` of a run
and then succeed on retry — harmless.

Tunables (env): `APT_SUITE` (default `stable`), `APT_COMPONENT` (`main`), `APT_ARCHS`
(`amd64 arm64`), `DEB_DIR` (`./dist/pkg`), `WORK_DIR` (`./dist/apt-repo`).

### What the script refuses to publish

`DEB_DIR` alone decides the live repository: the remote prune deletes whatever the
staging tree no longer carries, so a directory that is missing packages does not
publish less — it **unpublishes** the ones it lacks. Two traps make that easy to hit
by accident: `make pkg` writes an incomplete set (3 of the 7 shipped packages) into
that exact directory, and a tree left over from an earlier build survives there
indefinitely. So the script vets its input before touching the bucket, printing the
resolved `DEB_DIR` and every package with its version, and then fails closed on:

- **a dev build** — any version carrying `-dirty`, `-g<sha>` or `SNAPSHOT`. Override
  with `APT_ALLOW_UNRELEASED=1`.
- **an incomplete set** — the expected package names are read from `.goreleaser.yaml`
  (the same source of truth that defines a release, so renames and new packages are
  picked up automatically). The error lists what is missing. Override with
  `APT_ALLOW_PARTIAL=1` — legitimate when re-publishing an older tag that predates a
  package.

Before the real sync it also runs `rclone sync --dry-run` and prints the objects that
would be **removed** from the bucket, so deletions are visible in advance rather than
discovered afterwards.

## CDN caching & `by-hash`

Cloudflare edge-caches R2 objects by file extension and **overrides the origin
`Cache-Control`** (default browser TTL 4 h). That is fatal for a plain apt layout: after
a publish the signed `InRelease` is served fresh (no cacheable extension) but the
`Packages.gz` it checksums is served from a stale edge cache, so apt fails with
`Hash Sum mismatch`.

The fix, baked into `publish-apt.sh`, is Debian's standard **`Acquire-By-Hash`**:
`Release` carries `Acquire-By-Hash: yes` and every index is also copied to
`…/binary-<arch>/by-hash/<MD5Sum|SHA1|SHA256|SHA512>/<hash>`. apt then fetches indexes by
the hash it read from the fresh `InRelease` (it prefers **SHA512**, so all digests must
exist or it 404s and falls back to the stale plain path). Those hash-named URLs have no
cacheable extension and are immutable, so the edge always serves content consistent with
the signature. No Cloudflare-side cache rule or purge is required.

Conversely, do **not** enable a Cloudflare *Cache Everything* rule for this host: the
scheme relies on the extension-less objects (`InRelease`, the `by-hash/` copies) being
served fresh from origin.

## Client install

```sh
# Trust the repo key (keyring form; apt-key is deprecated). Only `tee` is elevated:
# piping straight into `sudo gpg` can leave an empty keyring, because a sudo password
# prompt may swallow the piped key — and apt then fails to verify the repo.
curl -fsSL https://apt.soul-stack.com/soul-stack.gpg.key \
  | gpg --dearmor | sudo tee /usr/share/keyrings/soul-stack.gpg >/dev/null

echo "deb [signed-by=/usr/share/keyrings/soul-stack.gpg] https://apt.soul-stack.com/ stable main" \
  | sudo tee /etc/apt/sources.list.d/soul-stack.list

sudo apt update

# A workstation that drives a cluster — the whole CLI set in one step:
sudo apt install soul-stack-tools     # soulctl + soul-lint + soul-trial + soul-legion

# A server — install just the daemon it runs:
sudo apt install soul-stack-keeper    # or soul-stack-soul
```

`soul-stack-tools` carries the same four binaries as the Homebrew cask and the winget
package, so every channel lands the same tool set — see [docs/install.md](../../docs/install.md)
for the per-package table. Any of the four is also installable on its own; note that
`soul-legion` drives load against a cluster and needs that cluster's DB credentials plus
a Vault PKI token, so point it at a bench cluster rather than production.

> The package names above land with the **next** release. The published
> `v0.1.0-beta.1` predates the rename: it carries `soul-stack-soul-lint` /
> `soul-stack-soul-trial` and has no `soul-stack-tools`. Upgrades migrate
> themselves — the renamed packages replace the old ones.

## Layout in the bucket

```
soul-stack.gpg.key                                     # ASCII-armored public signing key
pool/main/*.deb                                        # package blobs
dists/stable/{Release,Release.gpg,InRelease}           # suite Release + signatures
dists/stable/main/binary-<arch>/Packages{,.gz}         # per-arch index
dists/stable/main/binary-<arch>/by-hash/<ALGO>/<hash>  # content-addressed index copies
```
