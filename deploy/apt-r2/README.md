# apt repository on Cloudflare R2

Soul Stack publishes its `.deb` packages — `soul-stack-keeper`, `soul-stack-soul`,
`soul-stack-soul-lint`, `soul-stack-soulctl`, `soul-stack-soul-trial`,
`soul-stack-soul-legion` — through a plain, flat apt repository hosted on a
**Cloudflare R2** bucket fronted by the public domain `https://apt.soul-stack.com`.
The GitHub release workflow (`.github/workflows/release.yml`) produces the `.deb`
assets; mirroring them into the apt pool is a **separate, out-of-band step** — it needs
R2 credentials and the repository signing key, which we deliberately keep off GitHub
Actions.

`publish-apt.sh` does the mirroring; this file is the one-time setup and the per-release
runbook.

## Why out of band

- **Credentials scope.** GitHub OIDC signs release artifacts (cosign keyless), but the
  apt pool lives in R2 — an S3-compatible bucket with its own access keys. Putting
  long-lived R2 keys into Actions widens the blast radius; instead an operator runs
  `publish-apt.sh` from a trusted machine.
- **Signing key custody.** The apt `Release` file is signed with a dedicated GPG key.
  That private key never touches the public repo or hosted CI.

Publishing is automated on a **self-hosted runner** — see
[Automation](#automation-self-hosted-runner) below. That runner is a trusted machine
already holding the R2 remote and the signing key, so nothing sensitive lives in
GitHub-hosted CI. Running `publish-apt.sh` by hand from any such machine is the
equivalent bootstrap / fallback (the first publish, or before the runner is registered).

## One-time setup

### 1. R2 bucket + public domain

1. Create an R2 bucket, e.g. `soul-stack-apt`.
2. Attach a public custom domain (R2 → Settings → Public access → custom domain), e.g.
   `apt.soul-stack.com`. Objects then serve at `https://apt.soul-stack.com/<key>`.
3. Create an R2 API token (Account → R2 → Manage API Tokens) with **Object Read &
   Write** scoped to that bucket. Note the access key id / secret and the account-scoped
   S3 endpoint `https://<accountid>.r2.cloudflarestorage.com`.

### 2. rclone remote

`publish-apt.sh` syncs with [rclone](https://rclone.org). Configure a remote against the
R2 S3 endpoint:

```ini
# ~/.config/rclone/rclone.conf
[r2]
type = s3
provider = Cloudflare
access_key_id = <R2_ACCESS_KEY_ID>
secret_access_key = <R2_SECRET_ACCESS_KEY>
endpoint = https://<accountid>.r2.cloudflarestorage.com
```

Point `RCLONE_REMOTE` at the bucket path, e.g. `RCLONE_REMOTE=r2:soul-stack-apt`.

The script passes `--s3-no-check-bucket` because a bucket-scoped R2 token cannot
`HeadBucket`/`CreateBucket` at the account level, so the default bucket probe would
`403`. (rclone against R2 may also log a one-off `501 NotImplemented` on the first
`PutObject` of a run and then succeed on retry — harmless.)

### 3. Signing GPG key

Generate a dedicated repo-signing key (once), back up the private key securely, and let
the script publish the public half so clients can trust the repo:

```sh
gpg --quick-generate-key "Soul Stack apt signing <noreply@soul-stack.com>" rsa4096 sign never
```

Use its long key id (or uid) as `APT_GPG_KEY_ID`. `publish-apt.sh` exports the public
key to `soul-stack.gpg.key` in the bucket root on every run.

## Per-release runbook

After a `v*` release finishes (deb assets attached to the GitHub Release):

```sh
# 1. Collect the release .deb assets into ./dist/pkg (default DEB_DIR).
gh release download vX.Y.Z -R souls-guild/soul-stack -p '*.deb' -D dist/pkg
#    (or reuse the .deb goreleaser leaves under dist/ and set DEB_DIR accordingly)

# 2. Mirror + sign + sync.
export APT_GPG_KEY_ID="<key-id>"
export RCLONE_REMOTE="r2:soul-stack-apt"
deploy/apt-r2/publish-apt.sh
```

The script builds `pool/main/` + `dists/stable/…` indexes (including per-digest
`by-hash/` copies — see below), signs `Release` (detached `Release.gpg` + inline
`InRelease`), exports the public key as `soul-stack.gpg.key`, and `rclone sync`s the
whole tree to the bucket. It is idempotent — re-running re-indexes and re-syncs, and
`--delete-before` prunes packages dropped from a release.

Tunables (env): `APT_SUITE` (default `stable`), `APT_COMPONENT` (`main`), `APT_ARCHS`
(`amd64 arm64`), `DEB_DIR` (`./dist/pkg`), `WORK_DIR` (`./dist/apt-repo`).

## Automation (self-hosted runner)

[`.github/workflows/apt-publish.yml`](../../.github/workflows/apt-publish.yml) runs the
runbook above automatically when a GitHub Release is published (and on manual
`workflow_dispatch` for a given tag). It runs on a **self-hosted** runner on purpose: the
R2 write key and the GPG signing key stay on that trusted machine and never enter
GitHub-hosted CI.

One-time activation:

1. Register a self-hosted runner (repo → Settings → Actions → Runners) labelled
   `apt-publisher`, on a machine that has `rclone` (R2 remote configured), `gpg` (signing
   key imported), `apt-ftparchive` (apt-utils) and `gh`.
2. Set two repo **variables** (Settings → Secrets and variables → Actions → Variables —
   not secrets; these are not sensitive): `APT_GPG_KEY_ID` and `RCLONE_REMOTE`.
3. Publish a release (or dispatch the workflow against a tag). Until a matching runner is
   online the job simply queues; it never blocks the release workflow.

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
# Trust the repo key (keyring form; apt-key is deprecated).
curl -fsSL https://apt.soul-stack.com/soul-stack.gpg.key \
  | sudo gpg --dearmor -o /usr/share/keyrings/soul-stack.gpg

echo "deb [signed-by=/usr/share/keyrings/soul-stack.gpg] https://apt.soul-stack.com/ stable main" \
  | sudo tee /etc/apt/sources.list.d/soul-stack.list

sudo apt update
sudo apt install soul-stack-keeper   # or soul / soul-lint / soulctl / soul-trial / soul-legion
```

## Layout in the bucket

```
soul-stack.gpg.key                                     # ASCII-armored public signing key
pool/main/*.deb                                        # package blobs
dists/stable/{Release,Release.gpg,InRelease}           # suite Release + signatures
dists/stable/main/binary-<arch>/Packages{,.gz}         # per-arch index
dists/stable/main/binary-<arch>/by-hash/<ALGO>/<hash>  # content-addressed index copies
```
