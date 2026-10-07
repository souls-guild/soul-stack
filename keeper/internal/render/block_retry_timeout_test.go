package render

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// NIM-910: a block's `retry:` and `timeout:` are defaults each child inherits and
// may override — the child's whole `retry:` map, the nearest block's on nesting.

const blockDefaultsBody = `
    retry: { count: 5, delay: 30s }
    timeout: 10m
    block:
      - name: plain
        module: core.exec.run
        params: { cmd: a }
      - name: own
        module: core.exec.run
        retry: { count: 2 }
        timeout: 1m
        params: { cmd: b }
      - name: explicit
        module: core.exec.run
        settings: { task_timeout: 1h }
        params: { cmd: c }
      - name: inner
        retry: { count: 3, delay: 1s }
        timeout: 2m
        block:
          - name: nested
            module: core.exec.run
            params: { cmd: d }
`

// retryTimeout is what reaches the wire for one task.
type retryTimeout struct {
	Count   int
	Delay   string
	Timeout string
}

func retryTimeoutByName(t *testing.T, tasks []*RenderedTask) map[string]retryTimeout {
	t.Helper()
	out := map[string]retryTimeout{}
	for _, rt := range tasks {
		out[rt.Name] = retryTimeout{rt.RetryCount, rt.RetryDelay, rt.Timeout}
	}
	return out
}

// ★ GUARD. Every child gets the block's retry:/timeout: unless it writes its own;
// a child's retry: replaces the block's whole map (own: no delay inherited); a
// child whose settings: writes task_timeout keeps no wire timeout — the settings
// merge decides it (explicit); a nested block's own keys beat the outer block's.
// The same in staged dispatch and in both linear modes — the block hands its
// children the same values whether they are steps of their own or one group.
//
// Mutations: drop the retry/timeout rows from mergeBlockInheritance → plain
// carries nothing; let the block win → own carries 5/10m; merge retry key by key →
// own carries delay 30s; drop InheritTimeout's task_timeout stop → explicit
// carries 10m.
func TestRenderBlock_RetryTimeoutDefaultsByMode(t *testing.T) {
	want := map[string]retryTimeout{
		"plain":    {5, "30s", "10m"},
		"own":      {2, "", "1m"},
		"explicit": {5, "30s", ""},
		"nested":   {3, "1s", "2m"},
	}
	hosts := []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)}
	render := func(t *testing.T, src string, linear bool) map[string]retryTimeout {
		t.Helper()
		m := loadStagedManifest(t, src)
		in := RenderInput{Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: hosts}
		if linear {
			_, in.Steps = config.LinearPassage(m.Tasks, m.Settings)
		}
		tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		return retryTimeoutByName(t, tasks)
	}
	overlay := "name: s\ntasks:\n  - name: grp" + blockDefaultsBody
	group := "name: s\ntasks:\n  - name: grp\n    serial: 1\n    settings: { serial_scope: block }" + blockDefaultsBody

	for name, tc := range map[string]struct {
		src    string
		linear bool
	}{
		"staged":         {overlay, false},
		"linear overlay": {overlay, true},
		"linear group":   {group, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := render(t, tc.src, tc.linear); !reflect.DeepEqual(got, want) {
				t.Errorf("retry/timeout = %+v, want %+v", got, want)
			}
		})
	}
}

// ★ GUARD. The settings half: the block's timeout: is the children's task_timeout,
// and a child's explicit task_timeout beats it while its wire timeout stays empty.
//
// Mutation: pass blockTask.Settings instead of the block to InheritSettings (the
// pre-NIM-910 merge) → plain's task_timeout is nil.
func TestMergeBlockInheritance_BlockTimeoutIsTheChildrensTaskTimeout(t *testing.T) {
	block := config.Task{Name: "grp", Timeout: "10m"}
	explicit := moduleTask("explicit", "core.exec.run")
	explicit.Settings = &config.ScenarioSettings{TaskTimeout: new(time.Hour)}

	plain := mergeBlockInheritance(block, moduleTask("plain", "core.exec.run"))
	if plain.Settings == nil || plain.Settings.TaskTimeout == nil || *plain.Settings.TaskTimeout != 10*time.Minute {
		t.Errorf("plain settings = %+v, want task_timeout 10m", plain.Settings)
	}
	got := mergeBlockInheritance(block, explicit)
	if got.Settings == nil || got.Settings.TaskTimeout == nil || *got.Settings.TaskTimeout != time.Hour || got.Timeout != "" {
		t.Errorf("explicit = {settings:%+v timeout:%q}, want task_timeout 1h and no wire timeout", got.Settings, got.Timeout)
	}
}

// ★ GUARD. A block's timeout: crosses an applier child into the destiny's tasks,
// whose own timeout: wins (settings.md §3 rule 6), and a destiny block hands its
// own retry:/timeout: to its children. The scenario block's retry: never reaches
// the destiny — it is refused (next test).
//
// Mutation: drop the Timeout row from mergeApplierInheritance → d-plain carries none.
func TestRenderBlock_TimeoutCrossesAnApplierIntoTheDestiny(t *testing.T) {
	own := moduleTask("d-own", "core.exec.run")
	own.Timeout = "1m"
	res := &ResolvedDestiny{
		Name: "d",
		Tasks: []config.Task{
			moduleTask("d-plain", "core.exec.run"),
			own,
			{
				Name:    "d-block",
				Timeout: "3m",
				Retry:   &config.RetrySpec{Count: 4},
				Block:   &config.BlockTask{Block: []config.Task{moduleTask("d-inner", "core.exec.run")}},
			},
		},
	}
	block := config.Task{
		Name:    "grp",
		Timeout: "10m",
		Block:   &config.BlockTask{Block: []config.Task{{Name: "apply-step", Apply: &config.ApplyTask{Destiny: "d"}}}},
	}
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{block}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     staticResolver{res},
	}
	tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := retryTimeoutByName(t, tasks)
	for name, w := range map[string]retryTimeout{
		"d-plain": {0, "", "10m"},
		"d-own":   {0, "", "1m"},
		"d-inner": {4, "", "3m"},
	} {
		if got[name] != w {
			t.Errorf("%s = %+v, want %+v", name, got[name], w)
		}
	}
}

// ★ GUARD. A block's retry: reaching an applier fails the render on every path,
// not only the run path's config.RetryOverApply — renderApplyDestiny would drop it.
//
// Mutation: drop the Retry case from guardPilotBlockChild → Render succeeds and
// the destiny task carries no retry.
func TestRenderBlock_RetryOverAnApplierFailsTheRender(t *testing.T) {
	block := config.Task{
		Name:  "grp",
		Retry: &config.RetrySpec{Count: 3},
		Block: &config.BlockTask{Block: []config.Task{{Name: "apply-step", Apply: &config.ApplyTask{Destiny: "d"}}}},
	}
	_, err := applierEnv(t, block)
	if !errors.Is(err, ErrUnsupportedDSL) || !strings.Contains(err.Error(), "retry:") {
		t.Fatalf("Render error = %v, want ErrUnsupportedDSL naming retry:", err)
	}
}
