package shellgate

import (
	"errors"
	"testing"
)

var errDenied = errors.New("permission denied")

func holds() error    { return nil }
func notHolds() error { return errDenied }

func TestParseMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeWarn, false},
		{"warn", ModeWarn, false},
		{"enforce", ModeEnforce, false},
		{"Enforce", "", true},
		{"off", "", true},
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("ParseMode(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRequired pins the gate's reach to the verb-shell set and nothing else — a
// read-safe module must never start demanding a console right.
func TestRequired(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"core.cmd.shell", "core.exec.run"} {
		if !Required(m) {
			t.Errorf("Required(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"core.http.probe", "core.pkg.installed", "core.cmd", "core.cmd.foo", ""} {
		if Required(m) {
			t.Errorf("Required(%q) = true, want false", m)
		}
	}
}

// TestAuthorize_Matrix is the core contract: the module decides whether the gate
// applies at all, the console check decides the outcome, and the mode decides
// whether a missing right blocks or is merely recorded.
func TestAuthorize_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mode    Mode
		module  string
		check   func() error
		wantErr bool
	}{
		{"warn/shell/no-console → allowed by the window", ModeWarn, "core.cmd.shell", notHolds, false},
		{"warn/shell/console → allowed", ModeWarn, "core.cmd.shell", holds, false},
		{"enforce/shell/no-console → denied", ModeEnforce, "core.cmd.shell", notHolds, true},
		{"enforce/shell/console → allowed", ModeEnforce, "core.cmd.shell", holds, false},
		{"enforce/exec/no-console → denied", ModeEnforce, "core.exec.run", notHolds, true},
		// Not over-narrowing: an ordinary Errand passes in either mode without
		// the console right, and the check is never even consulted.
		{"enforce/read-safe/no-console → allowed", ModeEnforce, "core.http.probe", notHolds, false},
		{"warn/read-safe/no-console → allowed", ModeWarn, "core.http.probe", notHolds, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := New(c.mode, nil, nil)
			err := g.Authorize(SurfaceREST, c.module, c.check)
			if c.wantErr {
				if !errors.Is(err, ErrConsoleRequired) {
					t.Fatalf("Authorize = %v, want ErrConsoleRequired", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize = %v, want nil", err)
			}
		})
	}
}

// TestAuthorize_NonShellNeverProbes proves the gate does not even ask the
// enforcer about a read-safe module — the ordinary Errand path stays untouched.
func TestAuthorize_NonShellNeverProbes(t *testing.T) {
	t.Parallel()
	probed := false
	g := New(ModeEnforce, nil, nil)
	if err := g.Authorize(SurfaceMCP, "core.http.probe", func() error { probed = true; return nil }); err != nil {
		t.Fatalf("Authorize = %v, want nil", err)
	}
	if probed {
		t.Error("the console check ran for a non-verb-shell module")
	}
}

// TestAuthorize_NilCheckDoesNotDeny: a choke-point that supplies no probe has
// verified nothing. During the window that must not become a denial — it is a
// wiring bug to surface, and the metric carries `unconfigured`.
func TestAuthorize_NilCheck(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{ModeWarn, ModeEnforce} {
		g := New(mode, nil, nil)
		if err := g.Authorize(SurfaceCadenceSpawn, "core.cmd.shell", nil); err != nil {
			t.Errorf("mode %q: Authorize(nil check) = %v, want nil", mode, err)
		}
	}
}

// TestNilGateIsTheWindow — a handler built without a gate behaves as the open
// deprecation window, never as a silent enforce and never as a silent bypass of
// the probe.
func TestNilGateIsTheWindow(t *testing.T) {
	t.Parallel()
	var g *Gate
	if got := g.Mode(); got != ModeWarn {
		t.Fatalf("nil gate Mode() = %q, want %q", got, ModeWarn)
	}
	if err := g.Authorize(SurfaceVoyage, "core.cmd.shell", notHolds); err != nil {
		t.Fatalf("nil gate Authorize = %v, want nil", err)
	}
}
