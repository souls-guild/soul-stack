# apt repository on Cloudflare R2

Soul Stack publishes `.deb` packages (`soul-stack-keeper`, `soul-stack-soul`,
`soul-stack-soul-lint`) through a plain, flat apt repository hosted on a
**Cloudflare R2** bucket fronted by a public custom domain. The GitHub release
workflow (`.github/workflows/release.yml`) produces the `.deb` assets; mirroring
them into the apt pool is a **separate, out-of-band step** — it needs R2
credentials and the repository signing key, which we deliberately keep off
GitHub Actions.

`publish-apt.sh` does the mirroring; this file is the one-time setup and the
per-release runbook.

## Why out of band

- **Credentials scope.** GitHub OIDC signs release artifacts (cosign keyless),
  but the apt pool lives in R2 — an S3-compatible bucket with its own access
  keys. Putting long-lived R2 keys into Actions widens the blast radius; instead
  an operator runs `publish-apt.sh` from a trusted machine (or a self-hosted
  runner with the secrets scoped to it).
- **Signing key custody.** The apt `Release` file is signed with a dedicated GPG
  key. That private key never touches the public repo or hosted CI.

## One-time setup

### 1. R2 bucket + public domain

1. Create an R2 bucket, e.g. `soul-stack-apt`.
2. Attach a public custom domain (R2 → Settings → Public access → custom domain),
   e.g. `https://apt.soul-stack.com`. Objects then serve at
   `https://apt.soul-stack.com/<key>`.
3. Create an R2 API token (Account → R2 → Manage API Tokens) with
   Object Read & Write on that bucket. Note the access key id / secret and the
   account-scoped S3 endpoint `https://<accountid>.r2.cloudflarestorage.com`.

### 2. rclone remote

`publish-apt.sh` syncs with [rclone](https://rclone.org). Configure a remote
(here named `r2`) against the R2 S3 endpoint:

```ini
# ~/.config/rclone/rclone.conf
[r2]
type = s3
provider = Cloudflare
access_key_id = <R2_ACCESS_KEY_ID>
secret_access_key = <R2_SECRET_ACCESS_KEY>
endpoint = https://<accountid>.r2.cloudflarestorage.com
acl = private
```

The bucket path becomes the remote target, e.g. `RCLONE_REMOTE=r2:soul-stack-apt`.

### 3. Signing GPG key

Generate a dedicated repo-signing key (once), back up the private key securely,
and publish the public key so clients can trust the repo:

```sh
gpg --quick-generate-key "Soul Stack apt <apt@soul-stack.com>" ed25519 sign never
gpg --armor --export apt@soul-stack.com > soul-stack.gpg.key   # ships in the bucket root
```

Use its key id / email as `APT_GPG_KEY_ID`.

## Per-release runbook

After a `v*` release finishes (deb assets attached to the GitHub Release):

```sh
# Option A: reuse the .deb built locally by goreleaser (dist/pkg or dist/).
#   goreleaser release --clean   # leaves the .deb under dist/
# Option B: download the release's *.deb assets into ./dist/pkg first.

export APT_GPG_KEY_ID="apt@soul-stack.com"
export RCLONE_REMOTE="r2:soul-stack-apt"
export DEB_DIR="./dist"          # where the .deb files are (default ./dist/pkg)

deploy/apt-r2/publish-apt.sh
```

The script builds `pool/main/` + `dists/stable/…` indexes, signs `Release`
(detached `Release.gpg` + inline `InRelease`), exports the public key as
`soul-stack.gpg.key`, and `rclone sync`s the whole tree to the bucket. It is
idempotent — re-running re-indexes and re-syncs.

Tunables (env): `APT_SUITE` (default `stable`), `APT_COMPONENT` (`main`),
`APT_ARCHS` (`amd64 arm64`), `DEB_DIR`, `WORK_DIR`.

## Client install

```sh
# Trust the repo key (keyring form; apt-key is deprecated).
curl -fsSL https://apt.soul-stack.com/soul-stack.gpg.key \
  | sudo gpg --dearmor -o /usr/share/keyrings/soul-stack.gpg

echo "deb [signed-by=/usr/share/keyrings/soul-stack.gpg] https://apt.soul-stack.com/ stable main" \
  | sudo tee /etc/apt/sources.list.d/soul-stack.list

sudo apt update
sudo apt install soul-stack-keeper   # or soul-stack-soul / soul-stack-soul-lint
```

## Layout in the bucket

```
soul-stack.gpg.key                       # ASCII-armored public signing key
pool/main/*.deb                          # package blobs
dists/stable/Release                     # suite Release + signatures
dists/stable/Release.gpg
dists/stable/InRelease
dists/stable/main/binary-amd64/Packages(.gz)
dists/stable/main/binary-arm64/Packages(.gz)
```
