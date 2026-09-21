package shellgate

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// The point of NIM-197 is that the gate and the Soul-side Errand allow-list read
// ONE list. `shared/` cannot import `keeper/internal/`, so the couple of places
// that must agree with it are pinned here instead.

// TestVerbShellSet pins the closed set itself. It is a security boundary: adding
// a module here widens what `soul.console` is required for, and dropping one
// re-opens the hole NIM-197 closed. Both deserve a failing test, not a silent
// diff.
func TestVerbShellSet(t *testing.T) {
	t.Parallel()
	got := coremanifest.VerbShellModules()
	want := []string{"core.cmd.shell", "core.exec.run"}
	if len(got) != len(want) {
		t.Fatalf("VerbShellModules = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("VerbShellModules = %v, want %v", got, want)
		}
	}
}

// TestModesMatchConfigEnum — `console.errand_shell_gate` is validated in the
// config phase against a literal list in `shared/config`, and parsed at runtime
// by [ParseMode]. A value accepted by one and rejected by the other would either
// fail a valid config at boot or let a typo through into a security gate.
func TestModesMatchConfigEnum(t *testing.T) {
	t.Parallel()
	if len(config.ErrandShellGateModes) != len(Modes) {
		t.Fatalf("config.ErrandShellGateModes = %v, shellgate.Modes = %v", config.ErrandShellGateModes, Modes)
	}
	for i, m := range config.ErrandShellGateModes {
		if m != Modes[i] {
			t.Fatalf("config.ErrandShellGateModes = %v, shellgate.Modes = %v", config.ErrandShellGateModes, Modes)
		}
		if _, err := ParseMode(m); err != nil {
			t.Errorf("ParseMode(%q) rejected a value the config phase accepts: %v", m, err)
		}
	}
}
