package scenario

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

func TestSerialPresent(t *testing.T) {
	tests := []struct {
		name   string
		serial any
		want   bool
	}{
		{"nil → не задан", nil, false},
		{"пустая строка → не задан", "", false},
		{"int → задан", 2, true},
		{"процент-строка → задан", "50%", true},
		{"int 1 → задан", 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serialPresent(tt.serial); got != tt.want {
				t.Errorf("serialPresent(%v) = %v, want %v", tt.serial, got, tt.want)
			}
		})
	}
}

func TestHasSerialTask(t *testing.T) {
	tests := []struct {
		name string
		scn  *config.ScenarioManifest
		want bool
	}{
		{"nil scenario → нет serial", nil, false},
		{
			name: "ни одной serial-задачи",
			scn:  &config.ScenarioManifest{Tasks: []config.Task{{Name: "a"}, {Name: "b"}}},
			want: false,
		},
		{
			name: "одна задача с serial → есть",
			scn:  &config.ScenarioManifest{Tasks: []config.Task{{Name: "a"}, {Name: "b", Serial: 2}}},
			want: true,
		},
		{
			name: "serial процентом → есть",
			scn:  &config.ScenarioManifest{Tasks: []config.Task{{Name: "a", Serial: "25%"}}},
			want: true,
		},
		{
			name: "serial пустой строкой → нет (fail-closed в новый путь)",
			scn:  &config.ScenarioManifest{Tasks: []config.Task{{Name: "a", Serial: ""}}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasSerialTask(tt.scn); got != tt.want {
				t.Errorf("hasSerialTask = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGroupByHost(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "t0", Module: "core.exec.run"},
		{Index: 1, Name: "t1", Module: "core.file.present"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"host-a", "host-b"}},
		{TaskIndex: 1, TargetSIDs: []string{"host-b"}},
	}

	got := groupByHost(tasks, plans)
	if len(got) != 2 {
		t.Fatalf("hosts = %d, want 2", len(got))
	}
	if len(got["host-a"]) != 1 || got["host-a"][0].Name != "t0" {
		t.Errorf("host-a = %+v, want [t0]", got["host-a"])
	}
	if len(got["host-b"]) != 2 {
		t.Errorf("host-b tasks = %d, want 2", len(got["host-b"]))
	}
	// Task order within a host follows plans order (= scenario.tasks[]).
	if got["host-b"][0].Name != "t0" || got["host-b"][1].Name != "t1" {
		t.Errorf("host-b order = %v, want [t0 t1]", []string{got["host-b"][0].Name, got["host-b"][1].Name})
	}
}

func TestGroupByHost_EmptyTargets(t *testing.T) {
	tasks := []*render.RenderedTask{{Index: 0, Name: "t0"}}
	plans := []render.DispatchPlan{{TaskIndex: 0, TargetSIDs: nil}}
	got := groupByHost(tasks, plans)
	if len(got) != 0 {
		t.Errorf("hosts = %d, want 0 (where: отфильтровал всех)", len(got))
	}
}

func TestSortedSIDs(t *testing.T) {
	perHost := map[string][]*render.RenderedTask{
		"host-c": nil, "host-a": nil, "host-b": nil,
	}
	got := sortedSIDs(perHost)
	want := []string{"host-a", "host-b", "host-c"}
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Tests for the render→proto converter (TestToProtoTasks /
// TestToProtoTasks_OnChangesIdx / TestInt32Slice) moved to
// keeper/internal/render/prototask_test.go along with the converter itself
// (render.ToProtoTasks).

func TestEffectiveSerialWidth(t *testing.T) {
	tests := []struct {
		name  string
		plans []render.DispatchPlan
		want  int
	}{
		{"нет serial → 0", []render.DispatchPlan{{SerialWidth: 0}, {SerialWidth: 0}}, 0},
		{"один serial", []render.DispatchPlan{{SerialWidth: 2}, {SerialWidth: 0}}, 2},
		{"min среди нескольких", []render.DispatchPlan{{SerialWidth: 3}, {SerialWidth: 1}, {SerialWidth: 5}}, 1},
		{"пустой план", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveSerialWidth(tt.plans); got != tt.want {
				t.Errorf("effectiveSerialWidth = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestUnit_EffectiveSerialWidth_PerPassageSlice — ★ REVERSAL GUARD for
// per-passage width (ADR-056 §serial, min-width per Passage). dispatchPassage
// calls effectiveSerialWidth on the slice of ONE Passage (tasksForPassage-
// filtered plans), NOT on the whole run. Reversal (per-RUN min-width): the
// Passage 0 probe WITHOUT serial would go out in waves of 1 because of the
// Passage 1 serial:1 task (silent destructive throttle). The test proves width
// for each Passage = min among that Passage's own tasks.
func TestUnit_EffectiveSerialWidth_PerPassageSlice(t *testing.T) {
	// 3 tasks: Passage 0 — probe (#0, no serial); Passage 1 — two actions (#1
	// serial:1, #2 serial:3). A full-run (per-RUN) computation would give min=1
	// for the probe too.
	tasks := []*render.RenderedTask{
		{Index: 0, Passage: 0},
		{Index: 1, Passage: 1},
		{Index: 2, Passage: 1},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, SerialWidth: 0}, // probe Passage 0 — no serial
		{TaskIndex: 1, SerialWidth: 1}, // action Passage 1 — serial:1
		{TaskIndex: 2, SerialWidth: 3}, // action Passage 1 — serial:3
	}

	// Passage 0: only the probe (#0, serial 0) → width 0 (one wave for everyone).
	// Per-RUN reversal would take min(0,1,3 positive)=1 → the probe would go out
	// one host at a time.
	_, p0Plans := tasksForPassage(tasks, plans, 0)
	if w := effectiveSerialWidth(p0Plans); w != 0 {
		t.Fatalf("★ Passage 0 effectiveSerialWidth = %d, want 0 (probe БЕЗ serial — per-RUN min-width просочился бы из Passage 1 serial:1 → throttle)", w)
	}

	// Passage 1: actions #1(1) and #2(3) → min positive = 1.
	_, p1Plans := tasksForPassage(tasks, plans, 1)
	if w := effectiveSerialWidth(p1Plans); w != 1 {
		t.Fatalf("Passage 1 effectiveSerialWidth = %d, want 1 (min среди serial:1 и serial:3)", w)
	}
}

func TestSplitWaves(t *testing.T) {
	sids := []string{"a", "b", "c", "d", "e"}
	tests := []struct {
		name  string
		width int
		want  [][]string
	}{
		{"width 0 → одна волна", 0, [][]string{{"a", "b", "c", "d", "e"}}},
		{"width >= len → одна волна", 10, [][]string{{"a", "b", "c", "d", "e"}}},
		{"width 1 → по одному", 1, [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}}},
		{"width 2 → последний неполный", 2, [][]string{{"a", "b"}, {"c", "d"}, {"e"}}},
		{"width 3", 3, [][]string{{"a", "b", "c"}, {"d", "e"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitWaves(sids, tt.width)
			if len(got) != len(tt.want) {
				t.Fatalf("waves = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if len(got[i]) != len(tt.want[i]) {
					t.Fatalf("wave[%d] = %v, want %v", i, got[i], tt.want[i])
				}
				for j := range tt.want[i] {
					if got[i][j] != tt.want[i][j] {
						t.Errorf("wave[%d][%d] = %q, want %q", i, j, got[i][j], tt.want[i][j])
					}
				}
			}
		})
	}
}

func TestSplitWaves_Empty(t *testing.T) {
	got := splitWaves(nil, 2)
	if len(got) != 1 || len(got[0]) != 0 {
		t.Errorf("splitWaves(nil) = %v, want [[]]", got)
	}
}

func TestNoLogIndex(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, NoLog: false},
		{Index: 1, NoLog: true},
		{Index: 2, NoLog: false},
		{Index: 5, NoLog: true},
	}
	got := noLogIndex(tasks)
	if got[0] || got[2] {
		t.Errorf("non-no_log задачи попали в индекс: %v", got)
	}
	if !got[1] || !got[5] {
		t.Errorf("no_log задачи отсутствуют: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestFailureReason(t *testing.T) {
	strp := func(s string) *string { return &s }
	intp := func(i int) *int { return &i }

	tests := []struct {
		name  string
		hs    applyrun.HostStatus
		noLog map[int]bool
		want  string
	}{
		{
			name: "per-task summary доезжает дословно",
			hs:   applyrun.HostStatus{Status: applyrun.StatusFailed, TaskIdx: intp(0), ErrorSummary: strp("task 0 core.pkg.installed: E: Version '7.2.4' not found")},
			want: "task 0 core.pkg.installed: E: Version '7.2.4' not found",
		},
		{
			name: "нет summary → сам статус",
			hs:   applyrun.HostStatus{Status: applyrun.StatusFailed},
			want: "failed",
		},
		{
			name:  "no_log задача → stderr подавлен",
			hs:    applyrun.HostStatus{Status: applyrun.StatusFailed, TaskIdx: intp(2), ErrorSummary: strp("task 2 core.exec.run: secret-password-in-stderr")},
			noLog: map[int]bool{2: true},
			want:  "task 2: (no_log task failed)",
		},
		{
			name:  "не-no_log задача при наличии no_log-карты → message виден",
			hs:    applyrun.HostStatus{Status: applyrun.StatusFailed, TaskIdx: intp(0), ErrorSummary: strp("task 0 core.pkg.installed: boom")},
			noLog: map[int]bool{2: true},
			want:  "task 0 core.pkg.installed: boom",
		},
		{
			name: "cancelled без summary → статус",
			hs:   applyrun.HostStatus{Status: applyrun.StatusCancelled},
			want: "cancelled",
		},
		{
			// GUARD (ADR-056 §S1 fix Variant B): under staged/per-host-where the
			// local TaskIdx != the global FailedPlanIndex. The no_log map is
			// built from the global RenderedTask.Index. Resolution MUST go by
			// FailedPlanIndex: the task at global idx=5 is no_log, its stderr
			// carried a password; local idx=1 is absent from the no_log map.
			// REVERSAL (resolving by TaskIdx=1) → noLog[1]=false → the password
			// from ErrorSummary would leak into the operator-facing reason.
			// Security-relevant.
			name:  "staged: no_log резолвится по глобальному plan_index, не локальному task_idx",
			hs:    applyrun.HostStatus{Status: applyrun.StatusFailed, TaskIdx: intp(1), FailedPlanIndex: intp(5), ErrorSummary: strp("task 5 core.exec.run: secret-password-in-stderr")},
			noLog: map[int]bool{5: true},
			want:  "task 5: (no_log task failed)",
		},
		{
			// Symmetric case: global idx=5 is an ordinary task (not no_log),
			// even though local idx=2 happens to collide with the no_log index
			// of another task in another Passage. Resolving by
			// FailedPlanIndex(5) → noLog[5]=false → message is visible.
			// Reversal (by TaskIdx=2) would falsely suppress an ordinary
			// task's reason.
			name:  "staged: обычная задача не подавляется из-за коллизии локального task_idx",
			hs:    applyrun.HostStatus{Status: applyrun.StatusFailed, TaskIdx: intp(2), FailedPlanIndex: intp(5), ErrorSummary: strp("task 5 core.pkg.installed: boom")},
			noLog: map[int]bool{2: true},
			want:  "task 5 core.pkg.installed: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failureReason(tt.hs, tt.noLog); got != tt.want {
				t.Errorf("failureReason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	strp := func(s string) *string { return &s }

	tests := []struct {
		name      string
		statuses  []applyrun.HostStatus
		wantHosts int
		wantDone  bool
		wantFail  bool
	}{
		{
			name:      "all success",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusSuccess}, {SID: "b", Status: applyrun.StatusSuccess}},
			wantHosts: 2, wantDone: true,
		},
		{
			name:      "one running",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusSuccess}, {SID: "b", Status: applyrun.StatusRunning}},
			wantHosts: 2, wantDone: false,
		},
		{
			name:      "one failed → fail-closed",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusSuccess}, {SID: "b", Status: applyrun.StatusFailed, ErrorSummary: strp("boom")}},
			wantHosts: 2, wantFail: true,
		},
		{
			name:      "cancelled → fail-closed",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusCancelled}},
			wantHosts: 1, wantFail: true,
		},
		{
			// orphaned (Soul-reconcile, ADR-027(g)) — a terminal non-success:
			// the barrier counts it as a host failure (incarnation →
			// error_locked).
			name:      "orphaned → fail-closed",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusSuccess}, {SID: "b", Status: applyrun.StatusOrphaned}},
			wantHosts: 2, wantFail: true,
		},
		{
			// no_match (FINDING-01 variant (b)) — a benign terminal, like
			// success: targeted success + untargeted no_match → done, NOT fail
			// (incarnation goes to ready, not error_locked).
			name:      "success + no_match → done (benign)",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusSuccess}, {SID: "b", Status: applyrun.StatusNoMatch}},
			wantHosts: 2, wantDone: true,
		},
		{
			// All hosts untargeted (on: matched none): all no_match → done, the
			// run is benignly successful as a no-op, incarnation → ready.
			name:      "all no_match → done (benign)",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusNoMatch}, {SID: "b", Status: applyrun.StatusNoMatch}},
			wantHosts: 2, wantDone: true,
		},
		{
			// no_match does NOT mask a real target-host failure: failed breaks
			// the run even alongside a benign no_match.
			name:      "no_match + failed → fail-closed",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusNoMatch}, {SID: "b", Status: applyrun.StatusFailed, ErrorSummary: strp("boom")}},
			wantHosts: 2, wantFail: true,
		},
		{
			name:      "no_match не досчитан до wantHosts → не done",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusNoMatch}},
			wantHosts: 2, wantDone: false,
		},
		{
			name:      "fewer rows than wantHosts (poll опередил Insert)",
			statuses:  []applyrun.HostStatus{{SID: "a", Status: applyrun.StatusSuccess}},
			wantHosts: 2, wantDone: false,
		},
		{
			// keeper-target (on: keeper) is excluded from the host-barrier
			// count: its success row is written BEFORE the barrier, wantHosts =
			// real hosts only. One host still running → the barrier must NOT
			// declare done (previously the keeper row inflated the terminal
			// count and done would be true → silent success).
			name: "keeper success + один host running → не done",
			statuses: []applyrun.HostStatus{
				{SID: render.KeeperTargetSID, Status: applyrun.StatusSuccess},
				{SID: "a", Status: applyrun.StatusSuccess},
				{SID: "b", Status: applyrun.StatusRunning},
			},
			wantHosts: 2, wantDone: false,
		},
		{
			// All real hosts are terminal (+ keeper success, doesn't count) →
			// done.
			name: "keeper success + все host success → done",
			statuses: []applyrun.HostStatus{
				{SID: render.KeeperTargetSID, Status: applyrun.StatusSuccess},
				{SID: "a", Status: applyrun.StatusSuccess},
				{SID: "b", Status: applyrun.StatusSuccess},
			},
			wantHosts: 2, wantDone: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, failed := classify(tt.statuses, 0, tt.wantHosts, nil)
			if tt.wantFail {
				if failed == nil {
					t.Fatalf("failed = nil, want non-nil")
				}
				return
			}
			if failed != nil {
				t.Fatalf("failed = %v, want nil", failed)
			}
			if done != tt.wantDone {
				t.Errorf("done = %v, want %v", done, tt.wantDone)
			}
		})
	}
}

func TestCancelRequested(t *testing.T) {
	tests := []struct {
		name     string
		statuses []applyrun.HostStatus
		want     bool
	}{
		{
			name:     "ни одна строка не помечена",
			statuses: []applyrun.HostStatus{{SID: "a"}, {SID: "b"}},
			want:     false,
		},
		{
			// RequestCancel sets the flag on all running rows, but the barrier
			// only needs to see it on any one (cluster-wide Cancel, G1).
			name:     "одна строка помечена → отмена",
			statuses: []applyrun.HostStatus{{SID: "a"}, {SID: "b", CancelRequested: true}},
			want:     true,
		},
		{
			name:     "все строки помечены → отмена",
			statuses: []applyrun.HostStatus{{SID: "a", CancelRequested: true}, {SID: "b", CancelRequested: true}},
			want:     true,
		},
		{
			name:     "пустой срез",
			statuses: nil,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cancelRequested(tt.statuses); got != tt.want {
				t.Errorf("cancelRequested = %v, want %v", got, tt.want)
			}
		})
	}
}
