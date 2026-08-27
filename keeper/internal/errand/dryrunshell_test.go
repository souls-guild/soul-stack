package errand

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// verbShellDryRunReq — the pair keeper refuses on its own: a verb-shell module under
// dry_run. Everything else about the request is valid, so a test using it exercises the
// module admission and nothing else.
func verbShellDryRunReq(module string) DispatchRequest {
	return DispatchRequest{
		SID:          gateSID,
		Module:       module,
		Input:        map[string]any{"command": "rm -rf /var/lib/soul-stack/state"},
		TimeoutSec:   5,
		DryRun:       true,
		StartedByAID: "archon-alice",
	}
}

// TestValidateDryRunModule — the rule itself, over the whole verb-shell set rather
// than a sample of it: adding a module to [coremanifest.VerbShellModules] extends this
// test automatically, which is the point of the set being the single source.
func TestValidateDryRunModule(t *testing.T) {
	verbShell := coremanifest.VerbShellModules()
	if len(verbShell) == 0 {
		t.Fatal("fixture bug: the verb-shell set is empty, so every case below would pass vacuously")
	}

	for _, module := range verbShell {
		refusal := ValidateDryRunModule(module, true)
		if refusal == nil {
			t.Errorf("ValidateDryRunModule(%q, dryRun=true) = nil, want a refusal: the module takes an arbitrary "+
				"command line and has no pure-read Plan, so the preview cannot be honored on any host", module)
			continue
		}
		if refusal.Module != module {
			t.Errorf("refusal.Module = %q, want %q (the operator cannot fix a request the error does not name)",
				refusal.Module, module)
		}
		if !errors.Is(refusal, ErrDryRunVerbShell) {
			t.Errorf("refusal does not unwrap to ErrDryRunVerbShell, so transport mappers fall through to 500")
		}

		// The Apply path admits verb-shell BY NAME (ADR-033 item 2) — refusing it here
		// would break plain `/exec` of a shell command, the most-used Errand there is.
		if refusal := ValidateDryRunModule(module, false); refusal != nil {
			t.Errorf("ValidateDryRunModule(%q, dryRun=false) = %v, want nil: without the flag this is an ordinary "+
				"Apply, which the runner allows by name", module, refusal)
		}
	}

	// A module dry_run genuinely reaches must pass. PlanReadSafe modules are what the
	// flag exists for since NIM-488; refusing them would leave dry_run reaching nothing.
	for _, module := range []string{"core.file.present", "core.pkg.installed", "core.service.running"} {
		if refusal := ValidateDryRunModule(module, true); refusal != nil {
			t.Errorf("ValidateDryRunModule(%q, dryRun=true) = %v, want nil: keeper does not know which modules a "+
				"given Soul carries or which declare a pure-read Plan (ADR-011) - that stays a per-target answer",
				module, refusal)
		}
	}
}

// TestDryRunVerbShellError_Detail — the wording is load-bearing: it is the whole
// content of the 400, on a path where nothing else will ever tell the operator why.
func TestDryRunVerbShellError_Detail(t *testing.T) {
	detail := (&DryRunVerbShellError{Module: "core.cmd.shell"}).Detail()

	for _, want := range []string{
		"core.cmd.shell",                     // which module
		"dry_run",                            // which field
		coremanifest.ReasonDryRunUnsupported, // ties the refusal to the per-target status the fleet reports
		"no pure-read",                       // why it cannot work
		"any host",                           // why waiting or retrying will not help
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, does not carry %q", detail, want)
		}
	}

	// Unlike the capability refusals, dropping the flag IS the fix here: the operator
	// asked to preview an imperative command line, and there is nothing to preview. The
	// message must say so, or they will retry the same call expecting a different answer.
	if !strings.Contains(detail, "Drop 'dry_run'") {
		t.Errorf("detail = %q, does not state the way forward - 'unsupported' alone reads as a transient failure", detail)
	}
}

// TestDispatch_DryRunVerbShell_RefusedBeforeDispatch — ★ THE GUARD for the keeper-side
// half of the ADR-033 contract row (NIM-489). The host is connected AND announces
// dry_run, so every other gate on this path is satisfied: the only thing that can refuse
// is the module admission. Without it the request went out, the Soul answered FAILED
// errand_dry_run_unsupported, and the operator got HTTP 200 carrying a per-target
// failure for a request that could not have succeeded anywhere.
func TestDispatch_DryRunVerbShell_RefusedBeforeDispatch(t *testing.T) {
	for _, module := range coremanifest.VerbShellModules() {
		t.Run(module, func(t *testing.T) {
			f := newGateFixture(&stubSoulCap{}) // announces everything, including dry_run

			res, err := f.d.Dispatch(context.Background(), verbShellDryRunReq(module))
			if err == nil {
				t.Fatal("a dry_run of a verb-shell module must be refused at the request: no host can honor it")
			}
			var refusal *DryRunVerbShellError
			if !errors.As(err, &refusal) {
				t.Fatalf("error = %v, want *DryRunVerbShellError (handlers map it to 400)", err)
			}
			if refusal.Module != module {
				t.Errorf("refusal.Module = %q, want %q", refusal.Module, module)
			}
			if res.ErrandID != "" {
				t.Errorf("ErrandID = %q, want empty", res.ErrandID)
			}
			f.assertNothingHappened(t)
		})
	}
}

// TestDispatch_DryRunVerbShell_PrecedesCapabilityGate — ordering, asserted rather than
// left to comments. The host lacks the dry_run announcement AND the module is
// impossible, so both refusals apply; keeper must report the one the operator can act
// on. Answering 409 "upgrade the agent" would send them to install a binary that
// refuses the request too — a fix that cannot work, which is worse than no advice.
func TestDispatch_DryRunVerbShell_PrecedesCapabilityGate(t *testing.T) {
	cap := &stubSoulCap{lacking: []string{gateSID}}
	f := newGateFixture(cap)

	_, err := f.d.Dispatch(context.Background(), verbShellDryRunReq("core.cmd.shell"))
	if err == nil {
		t.Fatal("both gates apply; the request must still be refused")
	}
	if !errors.Is(err, ErrDryRunVerbShell) {
		t.Fatalf("error = %v, want the verb-shell refusal: it is the one the operator can act on", err)
	}
	if errors.Is(err, ErrDryRunNotAnnounced) {
		t.Error("reported as a capability problem: the operator would upgrade an agent that refuses this too")
	}
	// And it did not pay for a presence round-trip to reach a verdict it did not use.
	if asked := cap.askedFor(config.CapabilityDryRun); len(asked) != 0 {
		t.Errorf("asked the presence store about %v, want nothing (the module admission needs no cluster state)", asked)
	}
	f.assertNothingHappened(t)
}

// TestDispatch_VerbShellWithoutDryRun_Dispatched — the other half, and the one that
// makes the guard above non-vacuous. An ordinary shell Errand is THE most common
// request on this endpoint; a refusal keyed on the module alone would break all of it
// while still passing every test above.
func TestDispatch_VerbShellWithoutDryRun_Dispatched(t *testing.T) {
	f := newGateFixture(&stubSoulCap{})

	req := verbShellDryRunReq("core.cmd.shell")
	req.DryRun = false

	if _, err := f.d.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("Dispatch: %v (a shell Errand without dry_run is a plain Apply and must go out)", err)
	}
	if n := f.ob.sentCount(); n != 1 {
		t.Fatalf("SendErrand calls = %d, want 1", n)
	}
	if f.ob.sentDryRun(0) {
		t.Error("ErrandRequest.dry_run = true on the wire for a request that did not ask for it")
	}
}
