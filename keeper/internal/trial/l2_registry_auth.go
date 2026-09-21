//go:build integration

// Registry credentials and the L2 stand build.
//
// The systemd stand is built from tests/e2e-live/dockerfiles/debian-12.Dockerfile,
// whose only base image is the public `debian:12-slim` — the build needs no
// registry credentials at all. testcontainers resolves them anyway: before every
// Dockerfile build it reads the machine's docker config and asks the configured
// credential helper about EVERY registry listed there. And the two paths that ask
// disagree on what a failure means:
//
//   - the image-pull path logs it and continues anonymously ("No image auth found
//     for %s ... This is expected for public images", attemptToPullImage);
//   - the Dockerfile-build path returns it as a hard error ("auth configs from
//     Dockerfile", buildOptions).
//
// So a credential helper that cannot run takes down a build that never wanted
// credentials, while every pull-based suite on the same machine stays green.
//
// That is how NIM-307 presented. On WSL2 with Docker Desktop the helper is a
// Windows binary reached through WSL interop (/usr/bin/docker-credential-desktop.exe
// → /init). When interop is not registered in binfmt_misc — systemd-binfmt flushes
// the table and WSLInterop is not among the entries it restores from
// /usr/lib/binfmt.d — every exec of the helper fails with `exec format error`, and
// `make test-integration` goes red for everyone on the machine before a single
// assertion runs, with the cause four wrappings deep in the message.
//
// Two things here, both about that one failure:
//
//   - ensureRegistryAuthUsable probes the credential plumbing once per process and,
//     when it is broken, points testcontainers at an empty auth config so the build
//     proceeds without credentials. That costs nothing: a helper that cannot run
//     yields credentials for NO registry, so the pull path was already anonymous.
//     When the plumbing is healthy this is a no-op and real credentials stay.
//   - annotateRegistryAuthError turns a build that still died on credentials into a
//     sentence naming the helper and the machine-side fix, instead of a wrapped
//     fork/exec error that reads like a broken test.
package trial

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cpuguy83/dockercfg"
)

const (
	// dockerAuthConfigEnv is the testcontainers escape hatch: when it is set,
	// getDockerConfig() parses its value as the docker config instead of reading
	// ~/.docker/config.json, and no credential helper is executed at all.
	dockerAuthConfigEnv = "DOCKER_AUTH_CONFIG"

	// noRegistryAuth is a valid, empty docker config — no auths, no helpers.
	noRegistryAuth = `{}`
)

// registryAuthOnce guards the probe: one credential-helper exec per process is
// enough, and StartL2Stand runs once per L2 case.
var registryAuthOnce sync.Once

// ensureRegistryAuthUsable makes sure the machine's registry-credential plumbing
// cannot fail the stand build. Call it before a Dockerfile build; it is a no-op
// when the plumbing is healthy or when the caller has already pinned
// DOCKER_AUTH_CONFIG itself.
func ensureRegistryAuthUsable() {
	registryAuthOnce.Do(func() {
		preset := os.Getenv(dockerAuthConfigEnv)
		var probeErr error
		if preset == "" {
			// With the variable set there is nothing to learn: it overrides the very
			// config file the probe would read.
			probeErr = probeRegistryAuth()
		}
		value := decideRegistryAuth(preset, probeErr)
		if value == "" {
			return
		}
		if err := os.Setenv(dockerAuthConfigEnv, value); err != nil {
			fmt.Fprintf(os.Stderr, "trial L2: set %s: %v\n", dockerAuthConfigEnv, err)
			return
		}
		// Say it out loud. Dropping registry credentials is invisible until the day
		// a stand legitimately needs them, and a silent workaround is how the next
		// person loses the same day over again.
		fmt.Fprintf(os.Stderr, "trial L2: building the stand without registry credentials — %s\n\t%s\n",
			probeErr, registryAuthAdvice(probeErr))
	})
}

// probeRegistryAuth answers the one question the Dockerfile-build path asks: can
// the machine's registry-credential configuration be resolved at all. It must do
// so WITHOUT touching the docker daemon.
//
// The obvious implementation — testcontainers.DockerImageAuth, which is literally
// the code the build path runs — is the wrong tool here, and finding out why was
// the expensive part. It also resolves the docker host, and that resolution is
// cached process-wide TOGETHER WITH ITS ERROR (internal/core.ExtractDockerHost, a
// sync.Once). A pre-flight that bounded its own daemon call would therefore be able
// to cache a host-resolution failure for every container the package starts
// afterwards, turning a slow daemon into a dead one — a worse fault than the one
// being fixed, and observed while building this.
//
// So the credential half is replicated, mirroring getDockerAuthConfigs: every
// registry in `auths` whose entry carries no inline username/password, plus every
// registry with its own credHelper, is resolved through dockercfg. Any error means
// the helper could not be run — the only outcome the build path cannot survive. A
// registry with no stored credential is NOT an error: GetCredentialsFromHelper maps
// the helper's "credentials not found in native keychain" to empty strings and nil,
// which is why the stale credential-less `auths` entries a `docker login` leaves
// behind are harmless on their own.
//
// Mirroring upstream means it can drift from upstream. The degradation is graceful:
// a probe that wrongly says "healthy" leaves exactly the original failure, and
// annotateRegistryAuthError then explains it.
func probeRegistryAuth() error {
	cfg, err := dockercfg.LoadDefaultConfig()
	if err != nil {
		// No config file is the healthy end of the range — nothing to resolve, so
		// nothing that can fail. A config that cannot be parsed is worth reporting;
		// testcontainers fails the build on that too.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	for host, ac := range cfg.AuthConfigs {
		if ac.Username != "" || ac.Password != "" {
			continue // inline credentials — no helper is consulted for this registry
		}
		if err := resolveRegistryCredential(host); err != nil {
			return err
		}
	}
	for host := range cfg.CredentialHelpers {
		if err := resolveRegistryCredential(host); err != nil {
			return err
		}
	}
	return nil
}

// resolveRegistryCredential asks the configured helper about one registry, phrasing
// the failure the way testcontainers does so the diagnosis reads the same whether
// it came from the pre-flight or from the build.
func resolveRegistryCredential(host string) error {
	if _, _, err := dockercfg.GetRegistryCredentials(host); err != nil {
		return fmt.Errorf("getting credentials for %s: %w", host, err)
	}
	return nil
}

// decideRegistryAuth reports the value DOCKER_AUTH_CONFIG must take: empty string
// means "leave the environment alone".
//
// The policy, in one place so it can be pinned by a guard:
//
//   - a preset value wins. A caller that set the variable has already decided how
//     this process authenticates, and second-guessing it would silently override a
//     deliberate choice (CI passing credentials in, for instance);
//   - usable plumbing is left alone, so real credentials keep working — Docker Hub
//     rate limits are a good enough reason not to go anonymous for no gain;
//   - broken plumbing is replaced with an empty config, which is what the machine
//     effectively has anyway.
func decideRegistryAuth(preset string, probeErr error) string {
	if preset != "" {
		return ""
	}
	if registryAuthUsable(probeErr) {
		return ""
	}
	return noRegistryAuth
}

// registryAuthUsable classifies what the probe reports.
//
// The distinction that matters is not "were credentials found" but "could the
// question be asked at all". A registry with no stored credential is ordinary —
// the pull path treats it as ordinary too — while a helper that cannot be executed
// is the one outcome the Dockerfile-build path turns into a hard error.
//
// ErrCredentialsNotFound is tolerated rather than expected: dockercfg reports a
// missing credential as empty-strings-and-nil-error today, so the probe should
// never see the sentinel. Accepting it anyway costs nothing and keeps a future
// dockercfg that starts returning it from being read as broken plumbing.
func registryAuthUsable(err error) bool {
	return err == nil || errors.Is(err, dockercfg.ErrCredentialsNotFound)
}

// annotateRegistryAuthError attaches the credential diagnosis to a stand-build
// failure that named a credential helper. The pre-flight covers the plumbing
// being broken before the build; this covers it breaking during one, and the case
// where DOCKER_AUTH_CONFIG was preset to something that cannot resolve.
func annotateRegistryAuthError(err error) error {
	if err == nil || !mentionsCredentialHelper(err) {
		return err
	}
	return fmt.Errorf("%w\n\t%s", err, registryAuthAdvice(err))
}

// mentionsCredentialHelper reports whether err came from the credential-helper
// exec. Matching on text is fine for a diagnosis — testcontainers wraps the
// helper's name into the message and exports no sentinel for it — and a miss only
// costs a shorter error, never a wrong one.
func mentionsCredentialHelper(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "docker-credential") ||
		strings.Contains(msg, "get credentials from store") ||
		strings.Contains(msg, "getting credentials for")
}

// registryAuthAdvice explains the failure and the machine-side fix. It carries no
// copy of err.Error() — callers already print that — so the two paths can share it
// without saying the same thing twice.
func registryAuthAdvice(err error) string {
	advice := "the docker credential helper on this machine cannot be run, so testcontainers fails" +
		" every Dockerfile build before it starts (the image-pull path tolerates the same failure," +
		" which is why only the stand-building suites go red)"
	if isWindowsCredentialHelper(err) {
		advice += ".\n\tThat helper is a Windows binary executed through WSL interop:" +
			" `cat /proc/sys/fs/binfmt_misc/WSLInterop` must print `enabled`," +
			" and systemd-binfmt flushes binfmt_misc without restoring it." +
			" Restart WSL to re-register interop, or take `credsStore` out of ~/.docker/config.json"
	}
	return advice + "."
}

// isWindowsCredentialHelper recognises the WSL2 + Docker Desktop shape: a Windows
// helper that a Linux loader refuses to execute.
func isWindowsCredentialHelper(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, ".exe") && strings.Contains(msg, "exec format error")
}
