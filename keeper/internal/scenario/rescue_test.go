package scenario

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

func hostOwner() config.ScenarioSettings {
	owner := config.DefaultScenarioSettings()
	host := config.OnFailureHost
	owner.OnFailure = &host
	return owner
}

// ★ GUARD (rescues.md; ADR-0091 amendment NIM-993, rule 4). A dropped host judged
// again at a barrier — a source it still held when it was dropped, judged at its own
// unit's barrier — is out of the count: under `host` it neither counts against
// `tolerate` again nor empties the roster, and it is not dropped, nor audited, a
// second time. Once a failure has ended the run, a later failure under either radius
// is one more entry and the run walks on.
//
// Mutations: count an already dropped host as a fresh drop → tolerate: 1 ends the run
// at step 2; return every host-radius entry from drop → a is dropped twice; let an
// ended run end again → the scenario-radius failure stops the walk; join a Cancel
// after the end beside the list → the Cancel is named twice.
func TestRunFailures_RescueOfADroppedHost(t *testing.T) {
	f := newRunFailures()
	owner := hostOwner()
	owner.Tolerate = &config.Tolerate{N: 1}
	roster := []*topology.HostFacts{{SID: "a"}, {SID: "b"}}

	if _, err := f.settle(0, []failureEntry{{step: 0, sid: "a", radius: config.OnFailureHost, reason: "deploy"}}, owner, 2, roster); err != nil {
		t.Fatalf("first drop: %v", err)
	}
	dropped, err := f.settle(2, []failureEntry{{step: 2, sid: "a", task: "migrate", radius: config.OnFailureHost, reason: "migrate"}}, owner, 2, roster)
	if err != nil || len(dropped) != 0 || len(f.entries) != 2 {
		t.Fatalf("a dropped host judged again under host = %v, %v (entries %d) - want one more entry, no second drop, the run going on", dropped, err, len(f.entries))
	}
	if got := *f.hosts["a"].droppedAt; got != 1 {
		t.Errorf("host a is out from step %d, want 1: its first drop stands", got)
	}

	if _, err := f.settle(3, []failureEntry{{step: 3, sid: "b", radius: config.OnFailureScenario, reason: "boom"}}, owner, 2, roster); !errors.Is(err, errRunEnded) {
		t.Fatalf("scenario-radius failure = %v, want the run ended", err)
	}
	if _, err := f.settle(4, []failureEntry{{step: 4, sid: "b", task: "undo", radius: config.OnFailureScenario, reason: "undo failed"}}, owner, 2, roster); err != nil {
		t.Errorf("a failure judged after the end = %v, want one more entry", err)
	}
	_, cerr := f.settle(4, []failureEntry{{step: 4, sid: "b", radius: radiusCancelled, reason: "cancelled"}}, owner, 2, roster)
	if cerr == nil || errors.Is(cerr, errRunEnded) {
		t.Fatalf("a Cancel after the end = %v, want it to stop the run at once", cerr)
	}
	end := f.withEnd(cerr).Error()
	if !strings.Contains(end, "step 3 failed") || strings.Count(end, "was cancelled") != 1 || strings.Count(end, "undo failed") != 1 {
		t.Errorf("the ending error = %q, want the list once, the Cancel in it once", end)
	}
}

// ★ GUARD (settings.md §4, user 2026-10-08). A host that stopped reporting partway
// through a group: what it reported stays, the first task it was to run is FAILED
// with the row's reason code, every later one SKIPPED — not taken — and a child the
// Keeper decided to skip is SKIPPED wherever it sits, as is a task the Keeper skipped
// outside the message.
//
// Mutations: FAILED on every unreported task → `c` reads FAILED; FAILED on the first
// record whatever its status → the skipped child `b` reads FAILED; write the reported
// task again → `a` is in the records.
func TestUnreported_FirstTaskFailsTheRestAreNotTaken(t *testing.T) {
	a := &render.RenderedTask{Index: 10, Name: "a", Module: "core.exec.run"}
	b := &render.RenderedTask{Index: 11, Name: "b", FlowBySID: map[string]render.HostFlow{"h": {Skip: true}}}
	c := &render.RenderedTask{Index: 12, Name: "c", Module: "core.exec.run"}
	d := &render.RenderedTask{Index: 13, Name: "d", Module: "core.exec.run"}
	off := &render.RenderedTask{Index: 14, Name: "off"}
	j := &stepJudge{
		sent:    map[string][]*render.RenderedTask{"h": {a, b, c, d}},
		settled: map[string]*settledHost{"h": {records: []keeperRecord{{task: off, status: keeperv1.TaskStatus_TASK_STATUS_SKIPPED}}}},
	}
	v := failureEntry{sid: "h", reason: "soul_unavailable: agent stream lost for longer than soul_timeout (1m0s)"}
	reported := map[auditpg.ChangedTaskKey]struct{}{{SID: "h", PlanIndex: 10}: {}}

	got := map[string]string{}
	var failure *render.FlowFailure
	for _, rec := range unreported(j.message("h"), v, reported) {
		got[rec.task.Name] = rec.status.String()
		if rec.status == keeperv1.TaskStatus_TASK_STATUS_FAILED {
			failure = rec.failure
		}
	}
	want := map[string]string{
		"b":   keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String(),
		"c":   keeperv1.TaskStatus_TASK_STATUS_FAILED.String(),
		"d":   keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String(),
		"off": keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %v, want %v", got, want)
	}
	if failure == nil || failure.Code != soulUnavailable {
		t.Errorf("the FAILED record's error = %+v, want code %s", failure, soulUnavailable)
	}
}

func TestStopCode(t *testing.T) {
	for reason, want := range map[string]string{
		"send_apply_failed": "send_apply_failed",
		"orphaned: RunResult lost, Soul does not track apply_id": "orphaned",
		"soul_unavailable: no agent stream":                      "soul_unavailable",
		"task 3 core.pkg.installed: E: not found":                "",
		"scenario: RenderForHost: render svc/roll: boom":         "",
		"": "",
	} {
		if got := stopCode(reason); got != want {
			t.Errorf("stopCode(%q) = %q, want %q", reason, got, want)
		}
	}
}

// ★ GUARD (rescues.md, fork 1). After the wave whose failure ended the run, a wave
// keeps a host only where a rescue fires — the gate reads its earlier-step source
// failed there — and sends it its rescues alone, as a step past the end is rendered:
// an applier rescue's child with the terminal carrying its register, a `when: false`
// copy of a rescue that fires, but no ordinary task and no task whose `onfail:` names
// only its own step. A rescue whose source did not fail there, one whose `onfail:`
// names only its own step or a later one, and one kept only by a same-step source keep
// no host. A host the Keeper failed before sending stays when what failed is a rescue
// that fires there.
//
// Mutations: keep an applier rescue's child alone → the terminal is cut; read firing
// off the copy (no `when: false`, no source left) → k, whose rescue fires under a false
// `when:`, is dropped; keep a failed host by its cause alone → l stays; count an
// `onfail:` on its own step as a rescue → j is sent local; send a kept host its whole
// message → a is sent check.
func TestRescueWave_OnlyRescuesThatFire(t *testing.T) {
	src := &render.RenderedTask{Index: 2, Name: "deploy", Passage: 0}
	later := &render.RenderedTask{Index: 20, Name: "later", Passage: 7}
	ordinary := &render.RenderedTask{Index: 10, Name: "check", Passage: 5}
	rescue := &render.RenderedTask{Index: 11, Name: "revert", Passage: 5, OnFailIdx: []int{2}}
	local := &render.RenderedTask{Index: 12, Name: "local", Passage: 5, OnFailIdx: []int{10}}
	child := &render.RenderedTask{Index: 13, Name: "marker", Passage: 5, OnFailIdx: []int{2}}
	terminal := &render.RenderedTask{Index: 14, Name: "applier-register undo", Passage: 5, Register: "undo", AggregateOf: []int{13}}
	forward := &render.RenderedTask{Index: 15, Name: "forward", Passage: 5, OnFailIdx: []int{20}}
	mixed := &render.RenderedTask{Index: 16, Name: "mixed", Passage: 5, OnFailIdx: []int{2, 10}}
	notify := &render.RenderedTask{Index: 17, Name: "notify", Passage: 5, OnFailIdx: []int{2}}
	step := []*render.RenderedTask{ordinary, rescue, local, child, terminal, forward, mixed, notify}
	failed := map[auditpg.ChangedTaskKey]struct{}{}
	for _, sid := range []string{"a", "d", "f", "g", "j", "k"} {
		failed[auditpg.ChangedTaskKey{SID: sid, PlanIndex: 2}] = struct{}{}
	}
	gate := newCrossPassageGate(append([]*render.RenderedTask{src, later}, step...), nil, nil, failed)
	rescues, fires := rescueIndices(step, gate, 5), rescueFires(gate, 5, step)

	fired := func(t *render.RenderedTask) *render.RenderedTask {
		c := *t
		c.OnFailIdx = nil
		return &c
	}
	sameLeft := *mixed
	sameLeft.OnFailIdx = []int{10}
	perHost := map[string][]*render.RenderedTask{
		"a": {ordinary, fired(rescue)},
		"b": {ordinary, skippedOn(rescue, "b")},
		"e": {local},
		"f": {fired(child), terminal},
		"g": {fired(rescue), skippedOn(notify, "g")},
		"h": {&sameLeft},
		"i": {forward},
		"j": {fired(rescue), local},
		"k": {skippedOn(rescue, "k")},
	}
	rec := func(t *render.RenderedTask) keeperRecord {
		return keeperRecord{task: t, status: keeperv1.TaskStatus_TASK_STATUS_FAILED}
	}
	settled := map[string]*settledHost{
		"c": {failed: true, records: []keeperRecord{rec(ordinary), rec(rescue)}},
		"d": {failed: true, records: []keeperRecord{rec(rescue), rec(ordinary)}},
		"l": {failed: true, records: []keeperRecord{rec(rescue), rec(ordinary)}},
	}
	failedEarly := map[string]bool{"c": true, "d": true, "l": true}

	wave := rescueWave([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}, perHost, failedEarly, settled, rescues, fires)
	if want := []string{"a", "d", "f", "g", "j", "k"}; !reflect.DeepEqual(wave, want) {
		t.Errorf("wave = %v, want %v", wave, want)
	}
	names := func(sid string) []string {
		var out []string
		for _, rt := range perHost[sid] {
			out = append(out, rt.Name)
		}
		return out
	}
	for sid, want := range map[string][]string{
		"a": {"revert"},
		"f": {"marker", "applier-register undo"},
		"g": {"revert", "notify"},
		"j": {"revert"},
		"k": {"revert"},
	} {
		if got := names(sid); !reflect.DeepEqual(got, want) {
			t.Errorf("%s is sent %v, want %v", sid, got, want)
		}
	}
}
