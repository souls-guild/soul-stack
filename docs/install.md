# Installing Soul Stack

How to install the released binaries from a package channel. To build from source
instead — [getting-started.md](getting-started.md).

Two facts decide which channel you want:

- **`keeper` and `soul` are Linux-only.** They are daemons and build for
  `linux/amd64` and `linux/arm64` only.
- **The operator CLIs are cross-platform.** `soulctl`, `soul-lint` and `soul-trial`
  build for Linux, macOS and Windows, so an operator can drive Keeper from any
  desktop.

A server therefore takes a `.deb`/`.rpm`/`.apk` package or a container image; a
workstation takes Homebrew, the AUR package, or a release archive.

The current release is **`v0.1.0-beta.1`**, published as a GitHub **pre-release**.
What is out of scope for the beta — [known-limitations.md](known-limitations.md).

## apt (Debian / Ubuntu)

Packages are served from `https://apt.soul-stack.com` (suite `stable`, component
`main`, architectures `amd64` and `arm64`).

```sh
# Trust the repo key (keyring form; apt-key is deprecated). Only `tee` is elevated:
# piping straight into `sudo gpg` can leave an empty keyring, because a sudo password
# prompt may swallow the piped key — and apt then fails to verify the repo.
curl -fsSL https://apt.soul-stack.com/soul-stack.gpg.key \
  | gpg --dearmor | sudo tee /usr/share/keyrings/soul-stack.gpg >/dev/null

echo "deb [signed-by=/usr/share/keyrings/soul-stack.gpg] https://apt.soul-stack.com/ stable main" \
  | sudo tee /etc/apt/sources.list.d/soul-stack.list

sudo apt update
sudo apt install soul-stack-keeper   # a server runs one daemon
sudo apt install soul-stack-tools    # a workstation gets the whole CLI set
```

| Package | Contents |
|---|---|
| `soul-stack-keeper` | `keeper` daemon + systemd unit + `/etc/keeper/keeper.yml.example` |
| `soul-stack-soul` | `soul` agent daemon + systemd unit + `/etc/soul/soul.yml.example` |
| `soul-stack-soulctl` | `soulctl` operator CLI |
| `soul-stack-lint` | `soul-lint` offline artifact linter |
| `soul-stack-trial` | `soul-trial` offline Destiny / Scenario runner |
| `soul-stack-legion` | `soul-legion` load generator for sizing a Keeper cluster |
| `soul-stack-tools` | meta package — pulls in all four CLIs above, ships no files itself |

`soul-stack-tools` carries the same four binaries as the Homebrew cask and the
winget package, so every channel lands the same tool set. The daemons stay out of
it on purpose: a server should not drag in operator tooling, and each daemon is
installed on its own.

`soul-legion` drives load against a cluster — it needs cluster DB credentials and a
Vault PKI token, so point it at a bench cluster rather than production.

> **Package names changed after `v0.1.0-beta.1`.** The published beta still carries
> `soul-stack-soul-lint` / `soul-stack-soul-trial` and has no `soul-stack-tools`.
> The renamed packages declare `Provides`/`Replaces`/`Conflicts` on the old names,
> so an upgrade migrates itself — there is nothing to uninstall by hand.

The keeper and soul packages install a systemd unit and a config template, and
create their service user; they do not start a daemon that has no config yet.
Bringing a cluster up from these packages end to end — Vault provisioning, TLS,
`keeper init`, `soul init`, first connection — is
[operations/deb-onboarding.md](operations/deb-onboarding.md). How the repository
itself is built and published is [../deploy/apt-r2/README.md](../deploy/apt-r2/README.md).

## Homebrew (macOS)

```sh
brew install souls-guild/tap/soul-stack
```

The tap ships a **cask** that installs the cross-platform CLIs — `soulctl`,
`soul-lint`, `soul-trial` and `soul-legion`. The `keeper` and `soul` daemons are
Linux-only and are not in it; take them from the `.deb`/`.rpm` packages or the
container images.

Because it is a cask and not a formula, Homebrew tags the installed files with the
quarantine attribute — see [macOS: Gatekeeper and quarantine](#macos-gatekeeper-and-quarantine)
below if macOS refuses to run a binary.

## AUR (Arch Linux)

```sh
yay -S soul-stack-bin    # or: paru -S soul-stack-bin
```

`soul-stack-bin` repackages the released binaries rather than building from source.
It `provides`/`conflicts` `soul-stack`.

## winget (Windows)

**Not installable from winget yet.** The manifests for `SoulsGuild.SoulStack` are
generated on release and a submission is open against `microsoft/winget-pkgs`, but
it has not been merged, so `winget install` will not find the package. Until it
lands, take the Windows `.zip` from the release page (below).

The channel is also held back for pre-releases by design: a beta is not something
to push into a catalogue of shipping software. See
[../RELEASING.md](../RELEASING.md) for the release-side detail.

## Archives from GitHub Releases

Every release attaches, per platform:

- `soul-stack_<version>_linux_<amd64|arm64>.tar.gz` — all shipped binaries;
- `soul-stack_<version>_darwin_<amd64|arm64>.tar.gz` — the cross-OS CLIs;
- `soul-stack_<version>_windows_<amd64|arm64>.zip` — the cross-OS CLIs;
- `.deb`, `.rpm` and `.apk` packages per component;
- `checksums.txt` with its cosign signature (`.sig`) and certificate (`.pem`);
- a CycloneDX SBOM per archive (`<archive>.cdx.json`).

```sh
VERSION=0.1.0-beta.1
curl -fsSLO https://github.com/souls-guild/soul-stack/releases/download/v${VERSION}/soul-stack_${VERSION}_linux_amd64.tar.gz
curl -fsSLO https://github.com/souls-guild/soul-stack/releases/download/v${VERSION}/checksums.txt

sha256sum --check --ignore-missing checksums.txt
tar -xzf soul-stack_${VERSION}_linux_amd64.tar.gz
sudo install -m0755 soulctl /usr/local/bin/soulctl
```

On Linux, [`scripts/install.sh`](../scripts/install.sh) does the same thing in one
line and verifies the checksum for you:

```sh
curl -fsSL https://raw.githubusercontent.com/souls-guild/soul-stack/main/scripts/install.sh | sh
```

It installs `soulctl` by default; `SOULSTACK_BIN` selects `keeper`, `soul` or
`soul-lint` instead, `SOULSTACK_VERSION` pins a tag and `SOULSTACK_INSTALL_DIR`
picks the target directory. It is **Linux-only** — on any other OS it exits and
tells you to use a different channel.

### Verifying the signature

`checksums.txt` is signed with **cosign keyless**, so verifying it transitively
covers every archive and package listed in it:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature   checksums.txt.sig \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/souls-guild/soul-stack/\.github/workflows/release\.yml@refs/tags/' \
  checksums.txt
```

The identity is the release workflow itself — there is no long-lived signing key.

## Container images

Multi-arch (`linux/amd64`, `linux/arm64`) images on GHCR, signed with the same
keyless identity:

```sh
docker pull ghcr.io/souls-guild/soul-stack/keeper:0.1.0-beta.1
docker pull ghcr.io/souls-guild/soul-stack/soul:0.1.0-beta.1

cosign verify ghcr.io/souls-guild/soul-stack/keeper:0.1.0-beta.1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/souls-guild/soul-stack/\.github/workflows/release\.yml@refs/tags/'
```

The image tag carries no leading `v` (the git tag does: `v0.1.0-beta.1`).
`soul-lint` has no image — it is an offline CLI.

## From source

Build the binaries with `make build` and bring up a local stack — the walk-through
is [getting-started.md](getting-started.md). This is also the path for any platform
we do not publish binaries for.

## macOS: Gatekeeper and quarantine

Our macOS builds are **not notarized** and carry no Developer ID signature. The Go
linker ad-hoc signs `darwin/arm64` binaries even when cross-compiling from Linux, so
they do execute on Apple Silicon — an ad-hoc signature satisfies the code-signing
requirement, it just is not a signature Gatekeeper recognizes as a known developer.

What can block a binary is the **`com.apple.quarantine`** attribute. It is set by
whatever put the file on disk, not by the file itself:

- **Browsers set it.** A `.tar.gz` downloaded from the release page in Safari,
  Chrome or Firefox arrives quarantined.
- **`curl` and `wget` do not.** Nothing to clear if you fetched it from a shell.
- **Homebrew sets it for casks.** Our tap ships a cask, so `brew install` does
  apply it. (Formulae are never quarantined — casks are.)

Check before you fix anything:

```sh
xattr -p com.apple.quarantine ./soulctl
```

If it prints an attribute value, remove it:

```sh
xattr -d  com.apple.quarantine ./soulctl        # one file
xattr -dr com.apple.quarantine ./soul-stack/    # a whole extracted directory
```

If it prints `No such xattr`, the binary is not quarantined and whatever you are
debugging is something else.

For the Homebrew path you can also skip the attribute at install time:

```sh
brew install --cask --no-quarantine souls-guild/tap/soul-stack
```

## Windows: SmartScreen

The Windows binaries are unsigned, so SmartScreen may warn on first run
("Windows protected your PC" → *More info* → *Run anyway*). Verify the download
against `checksums.txt` first — see [Verifying the signature](#verifying-the-signature).
