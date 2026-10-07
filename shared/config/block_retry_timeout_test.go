package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// NIM-910: `retry:` and `timeout:` on a block are defaults every child inherits and
// may override. These pin the parse half and the settings/wire agreement; the
// render half is keeper/internal/render/block_retry_timeout_test.go.

// ★ GUARD. Both keys parse on a block, in a scenario and in a destiny, with no
// diagnostic of any level.
//
// Mutation: put "retry" or "timeout" back into blockForbiddenKeys → this test
// reports <key>_on_block_invalid.
func TestBlockRetryTimeout_ParseOnABlock(t *testing.T) {
	const block = `
  - name: Run the group
    retry: { count: 3, delay: 5s }
    timeout: 30s
    block:
      - name: Run the inner step
        module: core.exec.run
        params: { cmd: "true" }
        retry: { count: 2, until: register.self.changed }
`
	_, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte("name: x\ntasks:"+block), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	if len(diags) != 0 {
		dump(t, diags)
		t.Fatalf("a block's retry:/timeout: must lint clean in a scenario")
	}
	_, ddiags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(strings.ReplaceAll(block, "\n  ", "\n")), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadDestinyTasksFromBytes: %v", err)
	}
	if len(ddiags) != 0 {
		dump(t, ddiags)
		t.Fatalf("a block's retry:/timeout: must lint clean in a destiny")
	}
}

// ★ GUARD. `until:` in a block's `retry:` is refused in both entities, and only
// there — a child's own `until:` stays legal (the parse test above).
//
// Mutation: drop validateRetryUntilOnBlock from the block branch → no code.
func TestBlockRetryTimeout_UntilOnABlockRefused(t *testing.T) {
	const block = `
  - name: Run the group
    retry: { count: 3, until: register.self.changed }
    block:
      - name: Run the inner step
        module: core.exec.run
        params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte("name: x\ntasks:"+block), ValidateOptions{})
	if !hasCode(diags, "retry_until_on_block_invalid") {
		dump(t, diags)
		t.Fatalf("scenario: expected retry_until_on_block_invalid")
	}
	_, ddiags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(strings.ReplaceAll(block, "\n  ", "\n")), ValidateOptions{})
	if !hasCode(ddiags, "retry_until_on_block_invalid") {
		dump(t, ddiags)
		t.Fatalf("destiny: expected retry_until_on_block_invalid")
	}
}

// ★ GUARD. A block's `timeout:` is its `task_timeout` (settings.md §2.3), below an
// explicit `task_timeout` on the same block and above any key the child writes.
//
// Mutations: InheritSettings merging block.Settings without the fold → the first
// case resolves to nil; folding the block's timeout over the merged result → the
// last case resolves to 10m.
func TestInheritSettings_BlockTimeoutIsItsTaskTimeout(t *testing.T) {
	cases := []struct {
		name         string
		block, child Task
		want         time.Duration
	}{
		{"block timeout reaches the child", Task{Timeout: "10m"}, Task{}, 10 * time.Minute},
		{"explicit task_timeout beats timeout on the block", Task{Timeout: "10m", Settings: &ScenarioSettings{TaskTimeout: new(30 * time.Minute)}}, Task{}, 30 * time.Minute},
		{"child timeout beats it", Task{Timeout: "10m"}, Task{Timeout: "1m"}, time.Minute},
		{"child task_timeout beats it", Task{Timeout: "10m"}, Task{Settings: &ScenarioSettings{TaskTimeout: new(time.Hour)}}, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InheritSettings(tc.block, tc.child)
			if got == nil || got.TaskTimeout == nil || *got.TaskTimeout != tc.want {
				t.Fatalf("task_timeout = %s, want %s", settingsString(deref(got)), tc.want)
			}
		})
	}
}

// timeoutLevel is one level's way of writing a task timeout.
type timeoutLevel int

const (
	levelNone timeoutLevel = iota
	levelTimeout
	levelTaskTimeout
	levelBoth
	// levelBothDiffer writes `timeout: d` and `task_timeout: 2d` — the one
	// disagreement left, which timeout_shadowed_by_task_timeout warns about.
	levelBothDiffer
)

func (l timeoutLevel) task(d time.Duration) Task {
	var t Task
	if l == levelTimeout || l == levelBoth {
		t.Timeout = d.String()
	}
	if l == levelTaskTimeout || l == levelBoth {
		t.Settings = &ScenarioSettings{TaskTimeout: &d}
	}
	if l == levelBothDiffer {
		t.Timeout = d.String()
		t.Settings = &ScenarioSettings{TaskTimeout: new(2 * d)}
	}
	return t
}

func (l timeoutLevel) String() string {
	return [...]string{"none", "timeout", "task_timeout", "both", "both-differ"}[l]
}

// ★ GUARD — the two spellings agree. Through outer block → inner block → leaf,
// each level writing nothing, `timeout:`, `settings.task_timeout`, both equal or
// both different, the lowest level that writes one decides both the wire timeout
// and the settings merge, read as a reader will — the leaf's [Task.OwnSettings].
// The wire then carries that level's `timeout:` or none: a level writing only
// `task_timeout` leaves it empty (its value reaches the Soul with NIM-949), and a
// level writing both different values is the warned disagreement — its
// `timeout:` on the wire, its `task_timeout` in the merge.
//
// Mutation: drop the task_timeout stop in InheritTimeout → an outer `timeout:`
// rides past an inner `task_timeout` and the wire carries the overridden value.
func TestInheritTimeout_AgreesWithTheSettingsMerge(t *testing.T) {
	durations := [3]time.Duration{time.Hour, 10 * time.Minute, time.Minute}
	levels := []timeoutLevel{levelNone, levelTimeout, levelTaskTimeout, levelBoth, levelBothDiffer}
	for _, a := range levels {
		for _, b := range levels {
			for _, c := range levels {
				name := fmt.Sprintf("outer=%s/inner=%s/leaf=%s", a, b, c)
				t.Run(name, func(t *testing.T) {
					outer, inner, leaf := a.task(durations[0]), b.task(durations[1]), c.task(durations[2])
					inherit := func(block, child Task) Task {
						out := child
						out.Settings = InheritSettings(block, child)
						out.Timeout = InheritTimeout(block, child)
						return out
					}
					got := inherit(inherit(outer, inner), leaf)

					var want *time.Duration
					wire := ""
					for i, l := range []timeoutLevel{a, b, c} {
						if l == levelNone {
							continue
						}
						d := durations[i]
						want, wire = &d, d.String()
						switch l {
						case levelTaskTimeout:
							wire = ""
						case levelBothDiffer:
							want = new(2 * d)
						}
					}
					merged := got.OwnSettings().TaskTimeout
					if (want == nil) != (merged == nil) || (want != nil && *want != *merged) {
						t.Fatalf("merged task_timeout = %v, want %v", merged, want)
					}
					if got.Timeout != wire {
						t.Fatalf("wire timeout = %q, want %q", got.Timeout, wire)
					}
				})
			}
		}
	}
}

func retryOverApplyNames(ds []diag.Diagnostic) []string {
	var out []string
	for _, d := range ds {
		if d.Code != "retry_on_apply_invalid" || d.Level != diag.LevelError {
			continue
		}
		out = append(out, d.Message)
	}
	return out
}

// ★ GUARD. A block's retry: reaching an applier is refused against the nearest
// enclosing block that writes retry:, at any depth; a block without retry: above an
// applier, and a top-level applier, are not.
//
// Mutations: stop the walk at the first level → the nested applier passes; keep the
// outer block as the source → the message names "outer" for the nested one.
func TestRetryOverApply(t *testing.T) {
	applier := func(name string) Task { return Task{Name: name, Apply: &ApplyTask{Destiny: "d"}} }
	block := func(name string, retry *RetrySpec, children ...Task) Task {
		return Task{Name: name, Retry: retry, Block: &BlockTask{Block: children}}
	}
	tasks := []Task{
		applier("top"),
		block("outer", &RetrySpec{Count: 3},
			applier("direct"),
			block("inner", &RetrySpec{Count: 2}, applier("deep")),
			block("plain", nil, applier("under-plain")),
		),
		block("no-retry", nil, applier("free")),
	}
	got := retryOverApplyNames(RetryOverApply(tasks))
	want := []string{
		`retry: on block "outer" reaches the applier "direct"`,
		`retry: on block "inner" reaches the applier "deep"`,
		`retry: on block "outer" reaches the applier "under-plain"`,
	}
	if len(got) != len(want) {
		t.Fatalf("diagnostics = %q, want %d", got, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("diagnostic[%d] = %q, want prefix %q", i, got[i], want[i])
		}
	}
}

// ★ GUARD. An applier a within-block include splices in is refused too — the case
// the per-file validator cannot see.
func TestRetryOverApply_AfterIncludeExpansion(t *testing.T) {
	root := []Task{{
		Name:  "grp",
		Retry: &RetrySpec{Count: 3},
		Block: &BlockTask{Block: []Task{{Include: &IncludeTask{Include: "apply.yml"}}}},
	}}
	files := map[string]string{"apply.yml": `
- name: spliced
  apply: { destiny: d }
`}
	expanded, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("ExpandIncludes: %v", diags)
	}
	if got := retryOverApplyNames(RetryOverApply(expanded)); len(got) != 1 || !strings.Contains(got[0], `"spliced"`) {
		t.Fatalf("diagnostics = %q, want one naming the spliced applier", got)
	}
}
