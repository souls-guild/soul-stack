package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
)

// Guard tests for the console gate over the Errand path (ADR-0074 amendment,
// NIM-197) on the HTTP choke-points: POST /v1/souls/{sid}/exec, a kind=command
// Voyage and a kind=command Cadence recipe. The MCP tool and the Cadence spawn
// path are covered in their own packages.
//
// Every choke-point owes the same four answers, and each is asserted below:
//
//  1. `errand.run` alone + a verb-shell module → allowed while the window is
//     open, refused once it enforces;
//  2. plus `soul.console` → allowed in either mode;
//  3. a read-safe module → allowed WITHOUT `soul.console` (the gate must not
//     narrow an ordinary Errand);
//  4. `soul.console` without `errand.run` → still refused (the rights are
//     independent in both directions, ADR-0074(c)).

// perms builds a fake enforcer allowing exactly the listed `resource.action`.
func perms(list ...string) *fakeVoyageEnforcer {
	allow := make(map[string]bool, len(list))
	for _, p := range list {
		allow[p] = true
	}
	return &fakeVoyageEnforcer{allow: allow}
}

// isConsoleForbidden reports whether err is the gate's 403 (as opposed to any
// other failure further down the handler, which these tests deliberately
// tolerate — they assert the gate, not the dispatch).
func isConsoleForbidden(err error) bool {
	if err == nil {
		return false
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		return false
	}
	return d.Type == problem.TypeForbidden && strings.Contains(d.Detail, "soul.console")
}

// --- choke-point 1: POST /v1/souls/{sid}/exec ---

func TestErrandExec_ShellGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		mode       shellgate.Mode
		module     string
		granted    []string
		wantDenied bool
	}{
		{"window: errand.run alone reaches a shell", shellgate.ModeWarn, "core.cmd.shell", []string{"errand.run"}, false},
		{"enforce: errand.run alone is refused", shellgate.ModeEnforce, "core.cmd.shell", []string{"errand.run"}, true},
		{"enforce: errand.run + soul.console passes", shellgate.ModeEnforce, "core.cmd.shell", []string{"errand.run", "soul.console"}, false},
		{"enforce: core.exec.run is refused too", shellgate.ModeEnforce, "core.exec.run", []string{"errand.run"}, true},
		{"enforce: a read-safe module needs no console", shellgate.ModeEnforce, "core.http.probe", []string{"errand.run"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewErrandHandler(nil, nil, perms(c.granted...), shellgate.New(c.mode, nil, nil), nil)
			err := h.authorizeShell("archon-alice", "host.test", c.module)
			if got := isConsoleForbidden(err); got != c.wantDenied {
				t.Fatalf("console-forbidden = %v, want %v (err = %v)", got, c.wantDenied, err)
			}
		})
	}
}

// TestErrandExec_ShellGate_WiredIntoExec pins that ExecTyped actually consults the
// gate — the matrix above exercises the decision, this one proves the route runs
// it, and runs it BEFORE dispatching to the Soul.
func TestErrandExec_ShellGate_WiredIntoExec(t *testing.T) {
	t.Parallel()
	h := NewErrandHandler(buildCancelDispatcher(t, nil), nil, perms("errand.run"),
		shellgate.New(shellgate.ModeEnforce, nil, nil), nil)
	_, err := h.ExecTyped(context.Background(), claimsFor("archon-alice"), "host.test",
		ErrandRunInput{Module: "core.cmd.shell"})
	if !isConsoleForbidden(err) {
		t.Fatalf("ExecTyped err = %v, want the gate's console-forbidden 403", err)
	}
}

// TestErrandExec_ConsoleWithoutErrandRun — the route's `errand.run` middleware is
// the first gate and is unaffected: holding only `soul.console` does not open the
// Errand path. Asserted at the middleware selector level, since ExecTyped runs
// after that check has already passed.
func TestErrandExec_ConsoleWithoutErrandRun(t *testing.T) {
	t.Parallel()
	enf := perms("soul.console")
	if err := enf.Check("archon-alice", "errand", "run", map[string]string{"host": "host.test"}); err == nil {
		t.Fatal("soul.console alone satisfied errand.run; the rights must stay independent (ADR-0074(c))")
	}
}

// --- choke-point 2: POST /v1/voyages, kind=command ---

func TestVoyageCommand_ShellGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		mode       shellgate.Mode
		module     string
		granted    []string
		wantDenied bool
	}{
		{"window: errand.run alone reaches a shell batch", shellgate.ModeWarn, "core.cmd.shell", []string{"errand.run"}, false},
		{"enforce: errand.run alone is refused", shellgate.ModeEnforce, "core.cmd.shell", []string{"errand.run"}, true},
		{"enforce: errand.run + soul.console passes", shellgate.ModeEnforce, "core.cmd.shell", []string{"errand.run", "soul.console"}, false},
		{"enforce: a read-safe module needs no console", shellgate.ModeEnforce, "core.http.probe", []string{"errand.run"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &VoyageHandler{enforcer: perms(c.granted...), gate: shellgate.New(c.mode, nil, nil)}
			err := h.authorizeShellErr("archon-alice", c.module, []string{"host-a", "host-b"})
			if got := isConsoleForbidden(err); got != c.wantDenied {
				t.Fatalf("console-forbidden = %v, want %v (err = %v)", got, c.wantDenied, err)
			}
		})
	}
}

// TestVoyageCommand_ShellGate_AllOrNothing — the console right must cover EVERY
// resolved host. Trimming the batch instead would silently run a different
// command set than the operator submitted.
func TestVoyageCommand_ShellGate_AllOrNothing(t *testing.T) {
	t.Parallel()
	// Console on host-a only; the resolved scope also contains host-b.
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"errand.run": true}, consoleHosts: map[string]bool{"host-a": true}}
	h := &VoyageHandler{enforcer: enf, gate: shellgate.New(shellgate.ModeEnforce, nil, nil)}
	err := h.authorizeShellErr("archon-alice", "core.cmd.shell", []string{"host-a", "host-b"})
	if !isConsoleForbidden(err) {
		t.Fatalf("err = %v, want a console-forbidden 403 naming the uncovered host", err)
	}
	d, _ := AsProblemDetails(err)
	if !strings.Contains(d.Detail, "host-b") {
		t.Errorf("detail = %q, want it to name host-b", d.Detail)
	}
}

// --- choke-point 3: POST/PATCH /v1/cadences, kind=command ---

func TestCadenceRecipe_ShellGate(t *testing.T) {
	t.Parallel()
	shell := "core.cmd.shell"
	probe := "core.http.probe"
	cases := []struct {
		name       string
		mode       shellgate.Mode
		module     *string
		granted    []string
		wantDenied bool
	}{
		{"window: errand.run alone writes a shell schedule", shellgate.ModeWarn, &shell, []string{"errand.run"}, false},
		{"enforce: errand.run alone is refused", shellgate.ModeEnforce, &shell, []string{"errand.run"}, true},
		{"enforce: errand.run + soul.console passes", shellgate.ModeEnforce, &shell, []string{"errand.run", "soul.console"}, false},
		{"enforce: a read-safe recipe needs no console", shellgate.ModeEnforce, &probe, []string{"errand.run"}, false},
		{"enforce: a scenario recipe carries no module", shellgate.ModeEnforce, nil, []string{"incarnation.run"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &CadenceHandler{enforcer: perms(c.granted...), gate: shellgate.New(c.mode, nil, nil)}
			err := h.checkShellGateErr("archon-alice", c.module)
			if got := isConsoleForbidden(err); got != c.wantDenied {
				t.Fatalf("console-forbidden = %v, want %v (err = %v)", got, c.wantDenied, err)
			}
		})
	}
}

// TestShellGate_NilEnforcerDoesNotPanic — the handlers keep working when built
// without an enforcer (spec stubs, unit tests). A gate that panicked there would
// take down the OpenAPI dump; one that denied would fail closed on a wiring
// detail rather than on a permission.
func TestShellGate_NilEnforcerDoesNotPanic(t *testing.T) {
	t.Parallel()
	shell := "core.cmd.shell"
	gate := shellgate.New(shellgate.ModeEnforce, nil, nil)

	if err := (&CadenceHandler{gate: gate}).checkShellGateErr("archon-alice", &shell); err != nil {
		t.Errorf("cadence: %v, want nil", err)
	}
	if err := (&VoyageHandler{gate: gate}).authorizeShellErr("archon-alice", shell, []string{"host-a"}); err != nil {
		t.Errorf("voyage: %v, want nil", err)
	}
	if err := (&ErrandHandler{gate: gate}).authorizeShell("archon-alice", "host-a", shell); err != nil {
		t.Errorf("errand: %v, want nil", err)
	}
}
