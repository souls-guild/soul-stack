package render

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// runMemoScenario is staged: only the probe (Passage 0) reads the secret, and the
// act (Passage 1, behind the probe's register) does not. The Passage-1 render still
// reads it, because it re-renders the probe that already ran.
const runMemoScenario = `
name: staged
description: one secret read by a task that finishes before the last pass
tasks:
  - name: probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: "detect-role ${ vault('secret/shared/db#password') }"
  - name: act on master only
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: promote
`

const runMemoSecret = "pw-only-the-probe-and-act-know"

func newRunMemoFixture(t *testing.T) (*countingKV, *Pipeline, *config.ScenarioManifest, []int) {
	t.Helper()
	kv := &countingKV{
		secrets: map[string]map[string]any{"secret/shared/db": {"password": runMemoSecret}},
		calls:   map[string]int{},
	}
	e, err := cel.New(cel.WithVault(kv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	m := loadStagedManifest(t, runMemoScenario)
	plan, err := Stratify(m.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if plan.Count != 2 {
		t.Fatalf("Passage.Count = %d, want 2", plan.Count)
	}
	return kv, NewPipeline(kv, e, nil, nil), m, plan.TaskPassage
}

// renderPass renders the run's pass `active` under ctx, as the stage loop does.
func renderPass(ctx context.Context, p *Pipeline, m *config.ScenarioManifest, taskPassage []int, active int) ([]*RenderedTask, error) {
	in := RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{ID: "redis-prod", Service: "redis"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"redis-prod"}, nil),
			host("b.example.com", []string{"redis-prod"}, nil),
		},
		TaskPassage:   taskPassage,
		ActivePassage: active,
	}
	if active > 0 {
		in.RegisterByHost = map[string]map[string]any{
			"a.example.com": {"role": map[string]any{"stdout": "master"}},
			"b.example.com": {"role": map[string]any{"stdout": "slave"}},
		}
	}
	tasks, _, err := p.Render(ctx, in)
	return tasks, err
}

// runContext binds what scenario.Runner.run binds before its first Render.
func runContext() context.Context {
	return WithVaultFence(context.Background(), "redis")
}

// ★ NIM-934: the passes of one run share one vault() memo. Two passes, two reads of
// the path (the probe at Passage 0, and again when Passage 1 re-renders it) → one
// Vault read. A Render that started its own memo on top of the run's turns this back
// into one read per pass.
func TestRender_RunMemo_PassesShareOneVaultRead(t *testing.T) {
	kv, p, m, tp := newRunMemoFixture(t)
	ctx := runContext()

	for active := 0; active < 2; active++ {
		if _, err := renderPass(ctx, p, m, tp, active); err != nil {
			t.Fatalf("pass %d: %v", active, err)
		}
	}
	if got := kv.calls["secret/shared/db"]; got != 1 {
		t.Fatalf("Vault reads across the run = %d, want 1 (the run's passes share one memo)", got)
	}
}

// ★ NIM-934: a secret that only an already-run task reads, gone before a later pass,
// does not fail that pass. The Passage-1 render re-renders the probe, and before the
// run-wide memo that re-read was `render_failed` over a task that had finished.
func TestRender_RunMemo_SecretGoneMidRunDoesNotFailALaterPass(t *testing.T) {
	kv, p, m, tp := newRunMemoFixture(t)
	ctx := runContext()

	if _, err := renderPass(ctx, p, m, tp, 0); err != nil {
		t.Fatalf("pass 0: %v", err)
	}
	delete(kv.secrets, "secret/shared/db") // rotated away, or Vault unreachable

	tasks, err := renderPass(ctx, p, m, tp, 1)
	if err != nil {
		t.Fatalf("pass 1 failed over a secret pass 0 already read: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("pass 1 rendered %d tasks, want 2", len(tasks))
	}
}

// Two runs never share: each binds its own memo, so the second run reads Vault again
// and sees what Vault holds then.
func TestRender_RunMemo_SeparateRunsDoNotShare(t *testing.T) {
	kv, p, m, tp := newRunMemoFixture(t)
	for run := 0; run < 2; run++ {
		if _, err := renderPass(runContext(), p, m, tp, 0); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if got := kv.calls["secret/shared/db"]; got != 2 {
		t.Fatalf("Vault reads over two runs = %d, want 2 (one per run)", got)
	}
}

// ★ The memo now rides the run's context for the whole run, and that context is
// handed to every keeper-side step and every log call that has it in reach. Printing
// it — the way a log line or an error wrap would — must not print a memoized secret.
// A memo type that grew a String/GoString showing its entries would leak every secret
// the run read into wherever the context was printed.
func TestRender_RunMemo_ContextDoesNotPrintMemoizedSecrets(t *testing.T) {
	_, p, m, tp := newRunMemoFixture(t)
	ctx := runContext()
	if _, err := renderPass(ctx, p, m, tp, 0); err != nil {
		t.Fatalf("pass 0: %v", err)
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Debug("run context", slog.Any("ctx", ctx))
	slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})).Debug("run context", "ctx", ctx)

	for _, out := range []string{
		fmt.Sprintf("%v", ctx),
		fmt.Sprintf("%+v", ctx),
		fmt.Sprintf("%#v", ctx),
		logs.String(),
	} {
		if strings.Contains(out, runMemoSecret) {
			t.Fatalf("a memoized secret is printed with the run context:\n%s", out)
		}
	}
}
