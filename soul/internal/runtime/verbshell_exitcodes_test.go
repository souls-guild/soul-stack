package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/cmd"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/exec"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// The `exit_codes` contract of the verb-shell modules, end to end (NIM-687).
//
// Guarded here rather than in coremod because every claim is about the TASK,
// not about an ApplyEvent: FAILED vs CHANGED is decided by the runtime,
// `failed_when:` is evaluated by the runtime, and `register.self.*` is built by
// the runtime out of the event's output. A module-level assertion would pin the
// event and leave the three joints between them free — which is exactly where
// this can break silently.
//
// The modules are the REAL ones, behind the production param-schema layer
// (coreLayer), running real `sh` processes. So a manifest that forgot to
// declare `exit_codes`, or declared it in a shape that rejects a range string,
// reddens here too.
//
// What made the parameter necessary: a demo-service-redis run whose
// cluster-slot step exited 1 with "Connection refused". Nothing failed —
// core.cmd.shell reported no failure, so the task was recorded CHANGED, the run
// went on and died a minute later on a neighbouring `until:` task. The
// incarnation ended up holding somebody else's reason for the failure.

// verbShellTask — how to make one verb-shell module run `sh` and exit with the
// given code. Keyed by the FULL address from [coremanifest.VerbShellModules],
// so a new verb module with no entry here fails the guard instead of quietly
// narrowing it to the two members someone remembered to list.
var verbShellTask = map[string]func(code string) (moduleAddr string, params map[string]any){
	// argv, no shell of its own: `sh -c "..."` as three argv tokens.
	"core.exec.run": func(code string) (string, map[string]any) {
		return "core.exec.run", map[string]any{
			"cmd":  "sh",
			"args": []any{"-c", "printf out; printf err >&2; exit " + code},
		}
	},
	// shell string, the module supplies `sh -c` itself.
	"core.cmd.shell": func(code string) (string, map[string]any) {
		return "core.cmd.shell", map[string]any{
			"cmd": "printf out; printf err >&2; exit " + code,
		}
	},
}

// verbShellRegistry is the production core layer with only the verb-shell
// modules in it — real implementations, real manifest checks.
func verbShellRegistry() Registry {
	return coreLayer(map[string]module.SoulModule{
		"core.exec": exec.New(),
		"core.cmd":  cmd.New(),
	})
}

// runVerbShell applies a single task built from the given params and returns
// its TaskEvent plus the run status.
func runVerbShell(t *testing.T, addr string, params map[string]any, mutate func(*keeperv1.RenderedTask)) (*keeperv1.TaskEvent, keeperv1.RunStatus) {
	t.Helper()

	task := &keeperv1.RenderedTask{Name: "probe", Module: addr, Params: mustStruct(t, params)}
	if mutate != nil {
		mutate(task)
	}
	sink := &recordingSink{}
	r := NewApplyRunner(verbShellRegistry(), nil)
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "exit-codes",
		Tasks:   []*keeperv1.RenderedTask{task},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d, want 1", len(sink.taskEvents))
	}
	return sink.taskEvents[0], sink.runResult.GetStatus()
}

// eachVerbShell runs body once per registered verb-shell module. The set comes
// from coremanifest, not from a literal here: a knob on only one of the two
// would split the contract, and the sameness is the point of that set existing.
func eachVerbShell(t *testing.T, body func(t *testing.T, task func(code string) (string, map[string]any))) {
	t.Helper()

	verbs := coremanifest.VerbShellModules()
	if len(verbs) == 0 {
		t.Fatal("coremanifest.VerbShellModules() is empty — the guard would assert nothing")
	}
	for _, full := range verbs {
		build, ok := verbShellTask[full]
		if !ok {
			t.Fatalf("verb-shell module %q has no entry in verbShellTask: a verb module was added "+
				"without extending this guard, so its exit_codes contract is unchecked. Add a builder "+
				"that runs sh and exits with the given code.", full)
		}
		t.Run(full, func(t *testing.T) { body(t, build) })
	}
}

// Guard 1 — a code outside the set fails the TASK. Before NIM-687 this was
// CHANGED, and the run carried on past a command that had not worked.
func TestVerbShell_UndeclaredNonZeroExit_FailsTheTask(t *testing.T) {
	eachVerbShell(t, func(t *testing.T, build func(string) (string, map[string]any)) {
		// Control: the same command exiting 0. Once this is clean, the params are
		// valid and sh runs on this box, so nothing but the exit code can explain
		// the failure below. Without it, a params error or a missing sh would look
		// identical to the contract working.
		addr, okParams := build("0")
		okEv, okRun := runVerbShell(t, addr, okParams, nil)
		if okEv.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED || okRun != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
			t.Fatalf("control probe (exit 0): status = %v, run = %v, message = %q — the probe cannot "+
				"run its command at all, so the non-zero case would prove nothing",
				okEv.GetStatus(), okRun, okEv.GetError().GetMessage())
		}

		_, failParams := build("3")
		ev, run := runVerbShell(t, addr, failParams, nil)
		if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
			t.Errorf("status = %v, want FAILED: exit 3 with no exit_codes must fail the task, "+
				"the default set is [0] alone", ev.GetStatus())
		}
		if run != keeperv1.RunStatus_RUN_STATUS_FAILED {
			t.Errorf("run status = %v, want FAILED — a failing task that does not fail the run leaves "+
				"the next tasks running past a command that did not work", run)
		}
		// The message must name both halves. "exit code 3" alone does not tell the
		// operator whether the command is wrong or the expectation is.
		if msg := ev.GetError().GetMessage(); !strings.Contains(msg, "3") || !strings.Contains(msg, "exit_codes: 0") {
			t.Errorf("message = %q, want it to name the code (3) and the accepted set (exit_codes: 0)", msg)
		}
	})
}

// Guard 2 — a code inside the declared set is success, written either way.
// Both forms are checked because the range form is the one with no precedent
// in coremanifest, and a manifest that only admits integers would still pass
// the list form.
func TestVerbShell_DeclaredExitCode_Succeeds(t *testing.T) {
	forms := map[string]any{
		"exact list": []any{0, 3},
		"range":      []any{0, "2-5"},
	}
	eachVerbShell(t, func(t *testing.T, build func(string) (string, map[string]any)) {
		for name, codes := range forms {
			t.Run(name, func(t *testing.T) {
				addr, params := build("3")
				params["exit_codes"] = codes
				ev, run := runVerbShell(t, addr, params, nil)
				if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_FAILED {
					t.Fatalf("status = FAILED for exit 3 declared as %v: %q", codes, ev.GetError().GetMessage())
				}
				if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
					t.Errorf("status = %v, want CHANGED — the command ran", ev.GetStatus())
				}
				if run != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
					t.Errorf("run status = %v, want SUCCESS", run)
				}
				if got := ev.GetRegisterData().GetFields()["exit_code"].GetNumberValue(); got != 3 {
					t.Errorf("register.exit_code = %v, want 3 — an accepted code must still be readable", got)
				}
			})
		}
	})
}

// Guard 3 — `failed_when: false` over a rejected code makes the task OK. This
// is the sanctioned way back to the pre-NIM-687 behaviour, so it has to work
// without an exit_codes list; if it did not, changing the default would leave
// scenario authors with no escape short of editing every call.
func TestVerbShell_FailedWhenFalse_WaivesTheRejectedCode(t *testing.T) {
	eachVerbShell(t, func(t *testing.T, build func(string) (string, map[string]any)) {
		addr, params := build("3")
		ev, run := runVerbShell(t, addr, params, func(task *keeperv1.RenderedTask) {
			task.FailedWhen = "false"
		})
		if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_FAILED {
			t.Fatalf("status = FAILED despite failed_when:false — ignore_errors no longer works, "+
				"and there is no way to keep a scenario that expects a non-zero code: %q", ev.GetError().GetMessage())
		}
		if run != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
			t.Errorf("run status = %v, want SUCCESS: a waived failure must not fail-stop the run", run)
		}
		// OK, not CHANGED — and that is a real difference from the pre-NIM-687
		// behaviour this waiver restores, not an oversight. runTask derives the
		// base outcome from the first matching case of failed/changed, so a
		// failed event's `changed` is never read, and waiving the failure leaves
		// OK. Consequence for scenario authors: an `onchanges:` dependent of a
		// waived verb task no longer fires.
		//
		// What this pins is the outcome, not the mechanism — worth stating,
		// because the mechanism has two independent halves and flipping either
		// one ALONE leaves this assertion green: the runtime would have to start
		// reading `changed` on a failed event, AND the module would have to set
		// it (util.SendFailedWithOutput deliberately takes no changed parameter).
		// It reddens when the pair moves together, which is also the only way
		// the documented consequence would actually change.
		if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_OK {
			t.Errorf("status = %v, want OK", ev.GetStatus())
		}
		// ADR-012(d): the waived error is preserved for audit rather than erased.
		if got := ev.GetRegisterData().GetFields()["ignored_error"].GetStringValue(); !strings.Contains(got, "3") {
			t.Errorf("register.ignored_error = %q, want the original exit-code error — a waived "+
				"failure that leaves no trace cannot be audited", got)
		}
	})
}

// Guard 4 — on a failure by exit code the output survives into register.self.
// It has to travel with the failing event: util.SendFailed carries no output,
// so the naive "code rejected → SendFailed" loses the stderr the failure was
// declared for AND makes `failed_when: register.self.exit_code == N`
// unevaluable, which is the same predicate scenario authors are told to use.
func TestVerbShell_FailureByExitCode_KeepsOutputInRegister(t *testing.T) {
	eachVerbShell(t, func(t *testing.T, build func(string) (string, map[string]any)) {
		addr, params := build("3")
		ev, _ := runVerbShell(t, addr, params, nil)
		if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
			t.Fatalf("status = %v, want FAILED — this guard is about what a code failure carries, "+
				"so it asserts nothing unless the code failed", ev.GetStatus())
		}

		reg := ev.GetRegisterData().GetFields()
		if reg == nil {
			t.Fatal("register_data is empty on a failure by exit code: stdout, stderr and exit_code " +
				"are all gone, and the operator has nothing to diagnose with")
		}
		if got := reg["exit_code"].GetNumberValue(); got != 3 {
			t.Errorf("register.self.exit_code = %v, want 3", got)
		}
		if got := reg["stdout"].GetStringValue(); got != "out" {
			t.Errorf("register.self.stdout = %q, want %q", got, "out")
		}
		if got := reg["stderr"].GetStringValue(); got != "err" {
			t.Errorf("register.self.stderr = %q, want %q — this is the stderr the failure exists to "+
				"surface; losing it is worse than the old silent CHANGED", got, "err")
		}
		if !reg["failed"].GetBoolValue() {
			t.Error("register.self.failed = false on a FAILED task")
		}

		// The predicate itself, not just the fields: a `failed_when:` reading
		// register.self.exit_code must EVALUATE. If the field were missing the
		// task would fail on a CEL error instead — a failure that looks the same
		// from a distance and means something else entirely.
		waived, _ := runVerbShell(t, addr, params, func(task *keeperv1.RenderedTask) {
			task.FailedWhen = "register.self.exit_code != 3"
		})
		if waived.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_FAILED {
			t.Errorf("failed_when %q left the task FAILED (%q) — the predicate could not be evaluated "+
				"over a failing task's own output", "register.self.exit_code != 3", waived.GetError().GetMessage())
		}
	})
}

// Guard 5 — the idempotence skip is decided before the exit code is judged, so
// a skipped task never fails however narrow exit_codes is. The command here
// would exit 3 if it ran; `creates:` points at a file that exists, so it must
// not run at all and the task must report exit_code 0.
//
// The failure mode this pins is cheap to introduce and expensive to notice:
// move the Allows check above the skip guards and every idempotent task with a
// non-empty creates/unless/onlyif starts failing on its second run, on hosts
// that are already in the desired state.
func TestVerbShell_SkippedTask_NeverFailsOnExitCodes(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "already-there")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed creates target: %v", err)
	}

	eachVerbShell(t, func(t *testing.T, build func(string) (string, map[string]any)) {
		addr, params := build("3")
		params["creates"] = existing
		// Narrower than the default on purpose: 0 is not accepted either, so a
		// skip that fell through to the verdict would fail on the code it
		// reports for itself.
		params["exit_codes"] = []any{7}

		ev, run := runVerbShell(t, addr, params, nil)
		if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_FAILED {
			t.Fatalf("status = FAILED for a skipped task: %q — creates/unless/onlyif must short-circuit "+
				"before the exit code is judged", ev.GetError().GetMessage())
		}
		if run != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
			t.Errorf("run status = %v, want SUCCESS", run)
		}
		reg := ev.GetRegisterData().GetFields()
		// `reason`, not `skipped`: the module does emit skipped=true, but
		// registerStruct overwrites that key unconditionally with false — in
		// register.self it names the `when:` skip, which is a different event and
		// cannot be true for a task that reached runTask. So the module's own
		// short-circuit shows up only as reason. Asserting on it here is what
		// keeps this guard from passing on a build where the command DID run:
		// exec/cmd set reason nowhere else.
		if got := reg["reason"].GetStringValue(); got != "creates" {
			t.Errorf("register.self.reason = %q, want %q — the command was not short-circuited, so "+
				"exit_code below is whatever it really returned and this guard proves nothing", got, "creates")
		}
		if got := reg["exit_code"].GetNumberValue(); got != 0 {
			t.Errorf("register.self.exit_code = %v, want 0 for a skip", got)
		}
	})
}

// Guard 6 — a malformed exit_codes is an author error and surfaces on every
// host, including the ones that happen to skip. The param is parsed before the
// skip guards for exactly this reason: a typo that only reddens on hosts where
// the file is absent is a typo that reaches production.
func TestVerbShell_MalformedExitCodes_FailsEvenWhenSkipped(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "already-there")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed creates target: %v", err)
	}

	eachVerbShell(t, func(t *testing.T, build func(string) (string, map[string]any)) {
		addr, params := build("0")
		params["creates"] = existing
		params["exit_codes"] = []any{"not-a-code"}

		ev, _ := runVerbShell(t, addr, params, nil)
		if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
			t.Fatalf("status = %v, want FAILED — a malformed exit_codes must not be excused by a skip",
				ev.GetStatus())
		}
		if msg := ev.GetError().GetMessage(); !strings.Contains(msg, "exit_codes") {
			t.Errorf("message = %q, want it to name the offending param", msg)
		}
	})
}
