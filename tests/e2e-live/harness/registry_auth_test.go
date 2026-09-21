//go:build e2e_live

package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cpuguy83/dockercfg"
)

// These guards are the reason registry_auth.go may exist as a second copy of
// keeper/internal/trial/l2_registry_auth.go: each copy is verified where it lives,
// so a divergence shows up as a failing test rather than as one harness quietly
// losing the fix. Keep them in step with the canon's guards.

// helperExecErr reproduces the error shape that actually reaches us: dockercfg
// wraps the fork/exec failure and testcontainers wraps that per registry, joining
// the per-registry results. Reconstructed rather than compared against a literal
// so the guards exercise errors.Is/errors.Join the way the real chain does.
func helperExecErr(registry string) error {
	execErr := fmt.Errorf("fork/exec /usr/bin/docker-credential-desktop.exe: exec format error")
	fromHelper := fmt.Errorf("execute %q stdout: %q stderr: %q: %w",
		"docker-credential-desktop.exe", "", "", execErr)
	fromStore := fmt.Errorf("get credentials from store: %w", fromHelper)
	return fmt.Errorf("getting credentials for %s: %w", registry, fromStore)
}

// writeDockerConfig lays out a docker config dir plus a credential helper that
// exists, sits on PATH, is executable — and is not a program this kernel can load,
// so exec'ing it fails with `exec format error`, exactly as a Windows .exe does to
// a Linux loader when WSL interop is not registered.
//
// The content must NOT begin with the `MZ` magic: on a WSL2 machine where interop
// IS registered, binfmt_misc claims magic `4d5a` and hands the file to /init, which
// then spends tens of seconds failing to start it as a Windows process.
func writeDockerConfig(t *testing.T, config string) {
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
}

// TestProbe_AgainstRealDockerConfigs drives the real probe against real config
// files. Deliberately no docker daemon anywhere: the probe resolves credentials
// from the filesystem, and it has to stay that way — reaching the daemon would
// cache its host resolution, errors included, for every container the suite starts
// afterwards.
func TestProbe_AgainstRealDockerConfigs(t *testing.T) {
	// The credential-less `auths` entries are the shape a stale `docker login` leaves
	// behind, and the reason the scan runs at all for a public base image.
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
			t.Errorf("decideRegistryAuth() = %q, want %q — the build would still die on credentials it does not need: %v",
				got, noRegistryAuth, probeErr)
		}
		if advice := registryAuthAdvice(probeErr); !strings.Contains(advice, "WSLInterop") {
			t.Errorf("advice for a real unloadable .exe omits the interop check:\n%s", advice)
		}
	})

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

	t.Run("empty config is healthy", func(t *testing.T) {
		writeDockerConfig(t, `{}`)

		if probeErr := probeRegistryAuth(); !registryAuthUsable(probeErr) {
			t.Errorf("registryAuthUsable() = false for an empty config: %v", probeErr)
		}
	})

	// A machine that never ran `docker login` has no config file at all. Reading that
	// as broken plumbing would drop credentials on every clean CI runner.
	t.Run("missing config file is healthy", func(t *testing.T) {
		t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "absent"))
		t.Setenv(dockerAuthConfigEnv, "")

		if probeErr := probeRegistryAuth(); !registryAuthUsable(probeErr) {
			t.Errorf("registryAuthUsable() = false with no config file: %v", probeErr)
		}
	})
}

// TestRegistryAuthUsable_OnlyPlumbingFailureCounts pins the classification the fix
// rests on: "no credentials for this registry" is ordinary, "the credential config
// could not be read" is the failure. Collapsing the two would either neutralize
// auth on every machine that never logged into a private registry, or on none —
// which is the bug.
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
// ON TOP of the original error, never instead of it, and it stays out of the way of
// failures that have nothing to do with credentials.
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
