package errandrunner

import (
	"context"
	"strings"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sdkmod "github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/cmd"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/exec"
)

// The Errand half of the `exit_codes` contract (NIM-687).
//
// Errand reaches the same two verb-shell modules a scenario does, but assembles
// its answer through a different seam: buildResult calls extractFinal, which
// reads the final ApplyEvent's Output on its own terms. So the scenario guards
// in soul/internal/runtime say nothing about what an operator gets back from an
// ad-hoc command — that has to be pinned here.
//
// What is being pinned is the same trap: routing a rejected exit code through
// util.SendFailed instead of util.SendFailedWithOutput leaves this path
// answering "failed" with exit_code 0 and two empty streams. The operator would
// then see that something broke and nothing whatsoever about what — strictly
// worse than the silent CHANGED this change replaced.
// docs/keeper/operator-api/errands.md and the keeper.soul.run-command output
// schema both state this behaviour; without this file they state it on trust.
//
// Real modules and real `sh` processes, in the manner of errand_corefile_test.go:
// a fake that replays a prepared event proves only that the fake was written to
// match the assertion.

// verbShellErrandInput builds the Errand input that makes one verb-shell module
// write to both streams and exit with the given code. Keyed by the FULL address
// from [coremanifest.VerbShellModules], so a verb module added to that set with
// no entry here fails the guard instead of quietly narrowing it.
var verbShellErrandInput = map[string]func(code string) map[string]any{
	// argv, no shell of its own: `sh -c "..."` as three argv tokens.
	"core.exec.run": func(code string) map[string]any {
		return map[string]any{
			"cmd":  "sh",
			"args": []any{"-c", "printf out; printf err >&2; exit " + code},
		}
	},
	// shell string, the module supplies `sh -c` itself.
	"core.cmd.shell": func(code string) map[string]any {
		return map[string]any{"cmd": "printf out; printf err >&2; exit " + code}
	},
}

func realVerbShellRunner() *Runner {
	return New(coremod.NewRegistry(map[string]sdkmod.SoulModule{
		exec.Name: exec.New(),
		cmd.Name:  cmd.New(),
	}), nil, nil)
}

func eachVerbShellErrand(t *testing.T, fn func(t *testing.T, addr string, input func(code string) map[string]any)) {
	t.Helper()
	for _, addr := range coremanifest.VerbShellModules() {
		build, ok := verbShellErrandInput[addr]
		if !ok {
			t.Fatalf("verb-shell module %q has no entry in verbShellErrandInput: the set grew "+
				"and this guard would have skipped the new member in silence", addr)
		}
		t.Run(addr, func(t *testing.T) { fn(t, addr, build) })
	}
}

// A code outside the default set fails the Errand — and the failure still
// carries the code and both streams.
func TestErrand_VerbShell_RejectedExitCode_FailsWithOutputIntact(t *testing.T) {
	t.Parallel()
	eachVerbShellErrand(t, func(t *testing.T, addr string, input func(string) map[string]any) {
		// Control first: the same command at exit 0 must come back SUCCESS.
		// Without it a FAILED below could equally mean a wrong module address,
		// an input the schema rejects, or no `sh` on the box.
		ctrl := realVerbShellRunner().Run(context.Background(), &keeperv1.ErrandRequest{
			ErrandId:       "e-687-control",
			Module:         addr,
			Input:          mustStruct(t, input("0")),
			TimeoutSeconds: 10,
		})
		if ctrl.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
			t.Fatalf("control at exit 0: status = %v; want SUCCESS (err=%q)", ctrl.GetStatus(), ctrl.GetErrorMessage())
		}

		res := realVerbShellRunner().Run(context.Background(), &keeperv1.ErrandRequest{
			ErrandId:       "e-687-rejected",
			Module:         addr,
			Input:          mustStruct(t, input("3")),
			TimeoutSeconds: 10,
		})
		if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_FAILED {
			t.Fatalf("status = %v; want FAILED — exit 3 is outside the default exit_codes [0] (err=%q)",
				res.GetStatus(), res.GetErrorMessage())
		}
		if res.GetExitCode() != 3 {
			t.Errorf("exit_code = %d; want 3 — the failure dropped the code", res.GetExitCode())
		}
		if res.GetStdout() != "out" {
			t.Errorf("stdout = %q; want \"out\" — the failure dropped stdout", res.GetStdout())
		}
		if res.GetStderr() != "err" {
			t.Errorf("stderr = %q; want \"err\" — the failure dropped the stderr this change exists to surface",
				res.GetStderr())
		}
		if msg := res.GetErrorMessage(); !strings.Contains(msg, "3") || !strings.Contains(msg, "exit_codes: 0") {
			t.Errorf("error_message = %q; want both the code received and the accepted set", msg)
		}
	})
}

// The documented way out for an ad-hoc command that legitimately exits
// non-zero: keeper.soul.run-command has no exit_codes argument, and
// docs/keeper/mcp-tools/souls.md sends the caller to POST /v1/souls/{sid}/exec,
// whose free-form input takes the module's own params. That answer is only true
// if the param survives the Errand input path — including in its range form,
// which no other param in the core manifest has.
func TestErrand_VerbShell_DeclaredExitCode_Succeeds(t *testing.T) {
	t.Parallel()
	forms := []struct {
		name  string
		codes []any
	}{
		{"exact", []any{0, 3}},
		{"range", []any{0, "2-5"}},
	}
	eachVerbShellErrand(t, func(t *testing.T, addr string, input func(string) map[string]any) {
		for _, form := range forms {
			t.Run(form.name, func(t *testing.T) {
				in := input("3")
				in["exit_codes"] = form.codes

				res := realVerbShellRunner().Run(context.Background(), &keeperv1.ErrandRequest{
					ErrandId:       "e-687-declared",
					Module:         addr,
					Input:          mustStruct(t, in),
					TimeoutSeconds: 10,
				})
				if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
					t.Fatalf("status = %v; want SUCCESS — 3 is in %v (err=%q)",
						res.GetStatus(), form.codes, res.GetErrorMessage())
				}
				if res.GetExitCode() != 3 {
					t.Errorf("exit_code = %d; want 3 — an accepted code is still reported, not normalised away",
						res.GetExitCode())
				}
			})
		}
	})
}
