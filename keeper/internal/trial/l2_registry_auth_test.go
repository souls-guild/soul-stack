//go:build integration

package trial

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cpuguy83/dockercfg"
)

// helperExecErr reproduces the error shape that actually reaches us: dockercfg
// wraps the fork/exec failure, testcontainers wraps that per registry, and the
// per-registry results come back joined. Reconstructed rather than asserted
// against a literal string so the guards exercise errors.Is/errors.Join the way
// the real chain does.
func helperExecErr(registry string) error {
	execErr := fmt.Errorf("fork/exec /usr/bin/docker-credential-desktop.exe: exec format error")
	fromHelper := fmt.Errorf("execute %q stdout: %q stderr: %q: %w",
		"docker-credential-desktop.exe", "", "", execErr)
	fromStore := fmt.Errorf("get credentials from store: %w", fromHelper)
	return fmt.Errorf("getting credentials for %s: %w", registry, fromStore)
}

// writeDockerConfig lays out a docker config dir plus a credential helper that
// exists, sits on PATH, is executable — and is not a program this kernel can load,
// so exec'ing it fails with `exec format error`. That is precisely what a Windows
// .exe is to a Linux loader when WSL interop is not registered, and the `.exe`
// suffix also exercises the WSL half of the advice.
// Returns the config dir; the caller decides what config.json says about it.
//
// The content must NOT begin with the `MZ` magic, tempting as that is for a file
// pretending to be a Windows binary: on a WSL2 machine where interop IS registered,
// binfmt_misc claims magic `4d5a` and hands the file to /init, which then spends
// tens of seconds failing to start it as a Windows process. Bytes no binfmt handler
// claims fail instantly, and with the error this fixture is meant to reproduce.
func writeDockerConfig(t *testing.T, config string) string {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "docker-credential-unloadable.exe")
	if err := os.WriteFile(bin, []byte("not a loadable image\n"), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	cfgDir := filepath.Join(dir, "docker")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("write docker config: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", cfgDir)
	// DOCKER_AUTH_CONFIG would override the file the fixture just wrote.
	t.Setenv(dockerAuthConfigEnv, "")
	return cfgDir
}

// TestProbe_AgainstRealDockerConfigs closes the gap the pure guards cannot: they
// classify an error this file built by hand, so they would keep passing if
// dockercfg changed what it actually returns. These cases drive the real probe
// against real config files — the NIM-307 shape reproduced without waiting for the
// machine to break — and pin which of them count as broken plumbing.
//
// Deliberately no docker daemon anywhere: the probe resolves credentials from the
// filesystem, and it has to stay that way. Reaching the daemon would cache its host
// resolution, errors included, for every container the package starts afterwards.
func TestProbe_AgainstRealDockerConfigs(t *testing.T) {
	// The credential-less `auths` entries are the shape a stale `docker login`
	// leaves behind, and the reason the scan runs at all for a Dockerfile whose only
	// base image is public.
	const staleAuths = `"auths":{"127.0.0.1:8123":{},"gitlab.co-cy.online:5050":{}}`

	t.Run("helper that cannot be executed is broken plumbing", func(t *testing.T) {
		writeDockerConfig(t, `{`+staleAuths+`,"credsStore":"unloadable.exe"}`)

		probeErr := probeRegistryAuth()
		if probeErr == nil {
			t.Fatal("probe reported healthy credentials against a helper that cannot be executed — " +
				"the fixture no longer reproduces the failure, so this guard proves nothing")
		}
		if registryAuthUsable(probeErr) {
			t.Errorf("registryAuthUsable() = true for an unexecutable helper: %v", probeErr)
		}
		if got := decideRegistryAuth("", probeErr); got != noRegistryAuth {
			t.Errorf("decideRegistryAuth() = %q, want %q — the stand build would still die on credentials it does not need: %v",
				got, noRegistryAuth, probeErr)
		}
		if !mentionsCredentialHelper(probeErr) {
			t.Errorf("mentionsCredentialHelper() = false, so a build failure would carry no diagnosis: %v", probeErr)
		}
		// The fixture reproduces the NIM-307 error shape exactly — an `.exe` that the
		// loader refuses — so the WSL-specific half of the advice has to recognise it
		// here, on a real error rather than a hand-built one.
		if advice := registryAuthAdvice(probeErr); !strings.Contains(advice, "WSLInterop") {
			t.Errorf("advice for a real unloadable .exe omits the interop check:\n%s\n(error: %v)", advice, probeErr)
		}
	})

	// A per-host credHelper is the other way a helper gets invoked, and it is the
	// arrangement someone reaches for to keep desktop.exe only where it is needed —
	// so it must be probed as well, or the fix would silently stop covering them.
	t.Run("per-host credHelper is probed too", func(t *testing.T) {
		writeDockerConfig(t, `{"credHelpers":{"gitlab.co-cy.online:5050":"unloadable.exe"}}`)

		if probeErr := probeRegistryAuth(); registryAuthUsable(probeErr) {
			t.Errorf("registryAuthUsable() = true for a broken credHelper: %v", probeErr)
		}
	})

	// Inline credentials never reach a helper, so a broken one alongside them is
	// irrelevant — and treating it as broken would throw away working credentials.
	t.Run("inline credentials do not consult the helper", func(t *testing.T) {
		writeDockerConfig(t, `{"auths":{"registry.example.com":{"username":"u","password":"p"}},"credsStore":"unloadable.exe"}`)

		if probeErr := probeRegistryAuth(); !registryAuthUsable(probeErr) {
			t.Errorf("registryAuthUsable() = false although no helper had to run: %v", probeErr)
		}
	})

	// An empty config has nothing to resolve, so nothing that can fail.
	t.Run("empty config is healthy", func(t *testing.T) {
		writeDockerConfig(t, `{}`)

		if probeErr := probeRegistryAuth(); !registryAuthUsable(probeErr) {
			t.Errorf("registryAuthUsable() = false for an empty config: %v", probeErr)
		}
	})

	// A machine that never ran `docker login` has no config file at all. Reading
	// that as broken plumbing would drop credentials on every clean CI runner.
	t.Run("missing config file is healthy", func(t *testing.T) {
		t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "absent"))
		t.Setenv(dockerAuthConfigEnv, "")

		if probeErr := probeRegistryAuth(); !registryAuthUsable(probeErr) {
			t.Errorf("registryAuthUsable() = false with no config file: %v", probeErr)
		}
	})
}

// TestRegistryAuthUsable_OnlyPlumbingFailureCounts pins the classification the
// whole fix rests on: "no credentials for this registry" is an ordinary answer,
// "the credential config could not be read" is the failure. Collapsing the two
// would either neutralize auth on every machine that never logged into a private
// registry (throwing away Docker Hub credentials for nothing) or neutralize it on
// none, which is the bug (NIM-307).
func TestRegistryAuthUsable_OnlyPlumbingFailureCounts(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"credentials resolved", nil, true},
		{"no entry for this registry", dockercfg.ErrCredentialsNotFound, true},
		{"no entry, wrapped", fmt.Errorf("probe: %w", dockercfg.ErrCredentialsNotFound), true},
		{"helper cannot be executed", helperExecErr("127.0.0.1:8123"), false},
		{"helpers joined, as the config scan returns them", errors.Join(
			helperExecErr("127.0.0.1:8123"),
			helperExecErr("gitlab.co-cy.online:5050"),
		), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := registryAuthUsable(tt.err); got != tt.want {
				t.Errorf("registryAuthUsable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestDecideRegistryAuth pins the policy: step in only for broken plumbing, and
// never over a caller who pinned the variable themselves.
func TestDecideRegistryAuth(t *testing.T) {
	broken := helperExecErr("127.0.0.1:8123")

	tests := []struct {
		name     string
		preset   string
		probeErr error
		want     string
	}{
		{"healthy plumbing is left alone", "", nil, ""},
		{"registry without credentials is left alone", "", dockercfg.ErrCredentialsNotFound, ""},
		{"broken plumbing is replaced", "", broken, noRegistryAuth},
		{"a preset value wins over a broken probe", `{"auths":{}}`, broken, ""},
		{"a preset value wins over a healthy probe", `{"auths":{}}`, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideRegistryAuth(tt.preset, tt.probeErr); got != tt.want {
				t.Errorf("decideRegistryAuth(%q, %v) = %q, want %q", tt.preset, tt.probeErr, got, tt.want)
			}
		})
	}
}

// TestAnnotateRegistryAuthError_ExplainsWithoutSwallowing — the diagnosis is added
// ON TOP of the original error, never instead of it, and it stays out of the way
// of failures that have nothing to do with credentials (a stand that cannot get
// --privileged must keep reading like a privileged-denied error, because
// isDockerSetupErr classifies on that text).
func TestAnnotateRegistryAuthError_ExplainsWithoutSwallowing(t *testing.T) {
	t.Run("credential failure gains the advice and keeps the cause", func(t *testing.T) {
		cause := helperExecErr("127.0.0.1:8123")
		got := annotateRegistryAuthError(fmt.Errorf("create container: build options: auth configs from Dockerfile: %w", cause))
		if !errors.Is(got, cause) {
			t.Errorf("annotated error no longer wraps its cause: %v", got)
		}
		for _, want := range []string{"Dockerfile build", "image-pull path", "WSLInterop"} {
			if !strings.Contains(got.Error(), want) {
				t.Errorf("annotated error does not mention %q:\n%v", want, got)
			}
		}
	})

	t.Run("unrelated failure is returned untouched", func(t *testing.T) {
		cause := errors.New("container start: privileged mode is not allowed")
		if got := annotateRegistryAuthError(cause); got != cause {
			t.Errorf("annotateRegistryAuthError rewrote an unrelated error: %v", got)
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		if got := annotateRegistryAuthError(nil); got != nil {
			t.Errorf("annotateRegistryAuthError(nil) = %v, want nil", got)
		}
	})
}

// TestRegistryAuthAdvice_NamesTheAsymmetryAndTheFix — the point of the message is
// that it answers the two questions the raw fork/exec error provokes: why is only
// this suite red, and what do I do about it. A message that merely restates the
// exec failure would leave the next person exactly where NIM-307 found us.
func TestRegistryAuthAdvice_NamesTheAsymmetryAndTheFix(t *testing.T) {
	t.Run("WSL shape gets the interop check", func(t *testing.T) {
		advice := registryAuthAdvice(helperExecErr("127.0.0.1:8123"))
		for _, want := range []string{"image-pull path", "WSLInterop", "credsStore"} {
			if !strings.Contains(advice, want) {
				t.Errorf("advice does not mention %q:\n%s", want, advice)
			}
		}
	})

	t.Run("other shapes still explain the asymmetry", func(t *testing.T) {
		advice := registryAuthAdvice(errors.New("get credentials from store: exit status 1"))
		if !strings.Contains(advice, "Dockerfile build") {
			t.Errorf("advice does not explain which path fails:\n%s", advice)
		}
		if strings.Contains(advice, "WSLInterop") {
			t.Errorf("advice offers WSL-specific instructions for a non-WSL failure:\n%s", advice)
		}
	})
}
