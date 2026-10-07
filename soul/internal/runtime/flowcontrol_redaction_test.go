package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// redactionSecret stands for a value the Keeper substituted into a predicate — a
// sealed register or a vault() var (predicate-rendering.md §5).
const redactionSecret = "S3CR3T-nim929-a1b2"

// TestFlowControl_SubstitutedSecretIsNeverEchoed: whichever way a predicate
// fails, the secret substituted into it reaches neither a TaskEvent, nor the
// RunResult, nor the Soul's log. The cases cover each echo site, and each failure
// class whose cel-go detail quotes the input: a lookup that misses quotes the key —
// the shape of a rendered `register.self.m[register.probe.k]` — and a compile error
// quotes the source.
func TestFlowControl_SubstitutedSecretIsNeverEchoed(t *testing.T) {
	conv := `{"a": 1}[dyn("` + redactionSecret + `")] == 1`
	for _, tc := range []struct {
		name     string
		task     *keeperv1.RenderedTask
		wantCode string
		wantLog  bool
	}{
		{"when eval", &keeperv1.RenderedTask{When: conv}, "flowcontrol.when_error", true},
		{"when compile", &keeperv1.RenderedTask{When: `"` + redactionSecret + `" ===`}, "flowcontrol.when_error", true},
		{"when unsupported", &keeperv1.RenderedTask{When: `now() == "` + redactionSecret + `"`}, "flowcontrol.when_error", true},
		{"when not bool", &keeperv1.RenderedTask{When: `"` + redactionSecret + `"`}, "flowcontrol.when_error", true},
		{"changed_when eval", &keeperv1.RenderedTask{ChangedWhen: conv}, "flowcontrol.changed_when_error", true},
		{"failed_when eval", &keeperv1.RenderedTask{FailedWhen: conv}, "flowcontrol.failed_when_error", true},
		{"failed_when true", &keeperv1.RenderedTask{FailedWhen: `"` + redactionSecret + `" != ""`}, "flowcontrol.failed_when", false},
		{"until eval", &keeperv1.RenderedTask{RetryCount: 2, RetryDelay: "1ms", Until: conv}, "flowcontrol.until_error", true},
		{"until exhausted", &keeperv1.RenderedTask{RetryCount: 2, RetryDelay: "1ms",
			Until: `register.self.stdout == "` + redactionSecret + `"`}, "flowcontrol.until_exhausted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			tc.task.Name = "verify"
			tc.task.Module = "core.exec.run"
			sink := &recordingSink{}
			reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"stdout": "x"})}
			if err := NewApplyRunner(reg, nil).Run(context.Background(), &keeperv1.ApplyRequest{
				ApplyId: "redaction",
				Tasks:   []*keeperv1.RenderedTask{tc.task},
			}, sink); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if len(sink.taskEvents) != 1 {
				t.Fatalf("taskEvents = %d, want 1", len(sink.taskEvents))
			}
			ev := sink.taskEvents[0]
			if got := ev.GetError().GetCode(); got != tc.wantCode {
				t.Fatalf("error.code = %q, want %q — the predicate did not fail the way this case means", got, tc.wantCode)
			}
			if !strings.Contains(ev.GetError().GetMessage(), `"verify"`) {
				t.Errorf("message %q does not name the task", ev.GetError().GetMessage())
			}
			if tc.wantLog && !strings.Contains(logs.String(), "flow-control predicate") {
				t.Fatalf("no flow-control log line, so the log was not checked:\n%s", logs.String())
			}

			for what, text := range map[string]string{
				"TaskEvent": protojson.Format(ev),
				"RunResult": protojson.Format(sink.runResult),
				"Soul log":  logs.String(),
			} {
				if strings.Contains(text, redactionSecret) {
					t.Errorf("the secret reached the %s:\n%s", what, text)
				}
			}
		})
	}
}

// The Soul's flow-control engine keeps no compiled predicate: a rendered one carries
// its substituted values and is unique per run, so a cache would grow with every run
// and hold each secret for the life of the process.
func TestApplyRunner_FlowEngineKeepsNoPredicate(t *testing.T) {
	sink := &recordingSink{}
	reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"stdout": "x"})}
	r := NewApplyRunner(reg, nil)
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "no-cache",
		Tasks: []*keeperv1.RenderedTask{{
			Name:       "verify",
			Module:     "core.exec.run",
			When:       `"x" != dyn("` + redactionSecret + `")`,
			FailedWhen: `register.self.stdout == dyn("` + redactionSecret + `")`,
		}},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Fatalf("status = %v, want OK — both predicates must have been evaluated", got)
	}
	if n := r.flowEngine.CachedPrograms(); n != 0 {
		t.Fatalf("the flow-control engine holds %d compiled predicates, want 0", n)
	}
}

// TestAsync_SubstitutedValueNamesNoBarrier — `register.<name>` inside a substituted
// value is data: the task does not wait for that flow, and the flow's failure is
// observed at the final barrier, not as a fail-stop at this task. A quote-pairing
// text reading took the escaped quote for the literal's end and saw `register.boom`.
func TestAsync_SubstitutedValueNamesNoBarrier(t *testing.T) {
	t.Parallel()
	m := newGateModule("boom", "consumer")
	m.mu.Lock()
	m.fail["boom"] = true
	m.mu.Unlock()
	sink := &safeSink{}

	consumer := gateTask("consumer", false)
	consumer.When = `"x" != dyn("a\" register.boom b")`

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-substituted",
		Tasks:   []*keeperv1.RenderedTask{gateTask("boom", true), consumer},
	}, sink)

	m.awaitStart(t, "consumer")
	m.open("consumer")
	m.open("boom")
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.byIdx(1).GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("consumer status = %v, want OK — it reads no async register", got)
	}
	if got := sink.result.GetStatus(); got != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("run status = %v, want FAILED — boom failed and the final barrier sees it", got)
	}
}
