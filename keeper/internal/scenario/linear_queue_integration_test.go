//go:build integration

package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// Guards for NIM-961: a linear run's step goes through the work-queue. The run
// goroutine decides which hosts get the step; an Acolyte that shares nothing with
// it but PG renders the step for its host and sends it.

type stepQueueAnswer struct {
	ok  bool
	err error
}

func (a stepQueueAnswer) AllLiveTakeStepRows(context.Context) (bool, error) { return a.ok, a.err }

// newLinearQueueRunner is the instance that owns the run: Acolytes on, linear,
// and every live instance answers that it takes step rows.
func newLinearQueueRunner(t *testing.T, disp ApplyDispatcher) *Runner {
	t.Helper()
	r := newRunnerAcolyte(t, disp, "")
	r.deps.ExecutionModel = ExecutionLinear
	r.deps.StepQueue = stepQueueAnswer{ok: true}
	return r
}

func newStepClaimRunner(t *testing.T, disp ApplyDispatcher, kid string) *ClaimRunner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewClaimRunner(ClaimDeps{
		Deps: Deps{
			Loader:      artifact.NewServiceLoader(t.TempDir(), nil),
			Topology:    topology.NewResolver(integrationPool, nil, nil),
			ServiceVars: servicevars.NewResolver(nil),
			Render:      render.NewPipeline(nil, engine, nil, nil),
			Outbound:    disp,
			DB:          integrationPool,
		},
		KID:   kid,
		Lease: 30 * time.Second,
		Batch: 10,
	})
}

// runAcolyte polls the queue with its own ClaimRunner until the test ends.
func runAcolyte(t *testing.T, disp ApplyDispatcher, kid string) {
	t.Helper()
	cr := newStepClaimRunner(t, disp, kid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = cr.Claim(ctx)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// queueSouls answers every ApplyRequest as the Souls of the run would: it records
// what each host is sent per step, writes the probe's register, and finishes each
// message after a pause — so a step sent before the barrier of the step before it
// is seen, and flagged, while that step still has a live row.
type queueSouls struct {
	t      *testing.T
	probe  string
	stdout map[string]string
	// extra — more fields of the probe's register, per host.
	extra map[string]map[string]any

	mu       sync.Mutex
	sent     map[int]map[string][]string // passage → sid → task names
	cmds     map[int]map[string][]string // passage → sid → `cmd` params
	sends    map[string]int              // sid/passage → ApplyRequests
	attempts map[string][]int32          // sid/passage → attempts sent
	pending  sync.WaitGroup
}

func newQueueSouls(t *testing.T, probe string, stdout map[string]string) *queueSouls {
	s := &queueSouls{
		t: t, probe: probe, stdout: stdout,
		sent: map[int]map[string][]string{}, cmds: map[int]map[string][]string{},
		sends: map[string]int{}, attempts: map[string][]int32{},
	}
	t.Cleanup(s.pending.Wait)
	return s
}

func stepKey(sid string, passage int) string { return fmt.Sprintf("%s/%d", sid, passage) }

func (s *queueSouls) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()
	if passage > 0 {
		statuses, err := applyrun.SelectStatusesByApplyID(ctx, integrationPool, applyID)
		if err != nil {
			s.t.Errorf("queueSouls: statuses: %v", err)
		}
		for _, st := range statuses {
			if st.Passage == passage-1 && !benignOrFailedTerminal(st.Status) {
				s.t.Errorf("step %d sent to %s while %s is %s at step %d - the barrier did not hold", passage, sid, st.SID, st.Status, passage-1)
			}
		}
	}

	s.mu.Lock()
	if s.sent[passage] == nil {
		s.sent[passage] = map[string][]string{}
		s.cmds[passage] = map[string][]string{}
	}
	for _, task := range req.GetTasks() {
		s.sent[passage][sid] = append(s.sent[passage][sid], task.GetName())
		s.cmds[passage][sid] = append(s.cmds[passage][sid], task.GetParams().GetFields()["cmd"].GetStringValue())
	}
	s.sends[stepKey(sid, passage)]++
	s.attempts[stepKey(sid, passage)] = append(s.attempts[stepKey(sid, passage)], req.GetAttempt())
	s.mu.Unlock()

	localIdx, planIdx := emitterIndices(req, s.probe)
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		time.Sleep(40 * time.Millisecond)
		bg := context.Background()
		if localIdx >= 0 {
			if err := applyrun.UpsertTaskRegister(bg, integrationPool, &applyrun.TaskRegister{
				ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx,
				RegisterData: s.register(sid),
				Passage:      passage,
			}); err != nil {
				s.t.Errorf("queueSouls: UpsertTaskRegister(%s): %v", sid, err)
			}
		}
		if err := applyrun.UpdateStatus(bg, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
			s.t.Errorf("queueSouls: UpdateStatus(%s, step %d): %v", sid, passage, err)
		}
	}()
	return nil
}

func (s *queueSouls) register(sid string) map[string]any {
	reg := map[string]any{"stdout": s.stdout[sid], "changed": false, "failed": false}
	for k, v := range s.extra[sid] {
		reg[k] = v
	}
	return reg
}

func benignOrFailedTerminal(st applyrun.Status) bool {
	switch st {
	case applyrun.StatusSuccess, applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusNoMatch, applyrun.StatusOrphaned:
		return true
	}
	return false
}

func (s *queueSouls) sentAt(sid string, passage int) []int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int32(nil), s.attempts[stepKey(sid, passage)]...)
}

func (s *queueSouls) sendCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.sends))
	for k, n := range s.sends {
		out[k] = n
	}
	return out
}

func (s *queueSouls) messages() map[int]map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedCopy(s.sent)
}

func (s *queueSouls) commands() map[int]map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedCopy(s.cmds)
}

func sortedCopy(in map[int]map[string][]string) map[int]map[string][]string {
	out := map[int]map[string][]string{}
	for p, bySID := range in {
		out[p] = map[string][]string{}
		for sid, names := range bySID {
			sorted := append([]string(nil), names...)
			sort.Strings(sorted)
			out[p][sid] = sorted
		}
	}
	return out
}

// stepRowView is one host's row of one step, as the queue left it.
type stepRowView struct {
	status  applyrun.Status
	attempt int
	kid     string // claim_by_kid; "" — no Acolyte ever held the row
	recipe  *applyrun.Recipe
}

func stepRows(t *testing.T, applyID string) map[int]map[string]stepRowView {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(),
		`SELECT sid, passage, status, attempt, COALESCE(claim_by_kid, ''), recipe FROM apply_runs WHERE apply_id = $1`, applyID)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	defer rows.Close()
	out := map[int]map[string]stepRowView{}
	for rows.Next() {
		var (
			sid, status string
			passage     int
			v           stepRowView
			raw         []byte
		)
		if err := rows.Scan(&sid, &passage, &status, &v.attempt, &v.kid, &raw); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		v.status = applyrun.Status(status)
		if len(raw) > 0 {
			v.recipe = &applyrun.Recipe{}
			if err := json.Unmarshal(raw, v.recipe); err != nil {
				t.Fatalf("recipe of %s/%d: %v", sid, passage, err)
			}
		}
		if out[passage] == nil {
			out[passage] = map[string]stepRowView{}
		}
		out[passage][sid] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func repoHead(t *testing.T, gitURL string) string {
	t.Helper()
	repo, err := git.PlainOpen(strings.TrimPrefix(gitURL, "file://"))
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	return head.Hash().String()
}

func startLinearRun(t *testing.T, r *Runner, gitURL, scenarioName string) string {
	t.Helper()
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "redis", Git: gitURL, Ref: "master"},
		ScenarioName:    scenarioName,
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return applyID
}

func seedTwoHosts(t *testing.T, hostA, hostB string) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, hostA, []string{"redis-prod"})
	seedConnectedSoul(t, hostB, []string{"redis-prod"})
}

// ★ GUARD (replaces NIM-944's "acolytes>0 + a linear run stays inline"). On a
// work-queue instance a linear run's step goes through the queue — one step, block
// or not — and the Acolyte's render chooses the hosts the run's own render chose.
// In the block case a claim render without the step shape would AND the block's
// where: into the child and evaluate it with the child's vars:, choosing no host.
//
// Mutations: keep a linear run off the step queue (no recipe for its steps) → the
// rows are never claimed; render the claim without the recipe's step shape → the
// block's child reaches no host.
func TestIntegration_LinearStepGoesThroughQueue(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	cases := map[string]string{
		"block": `name: rolled
description: one default block whose where reads a var its only child overrides
tasks:
  - name: pair
    vars: { mode: a }
    where: "vars.mode == 'a'"
    block:
      - name: a
        module: core.exec.run
        vars: { mode: b }
        params: { cmd: a }
`,
		"no block": `name: rolled
description: one task
tasks:
  - name: a
    module: core.exec.run
    params: { cmd: a }
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			seedTwoHosts(t, hostA, hostB)
			gitURL := linearServiceRepo(t, "rolled", body)
			souls := newQueueSouls(t, "", nil)
			r := newLinearQueueRunner(t, souls)
			runAcolyte(t, souls, "keeper-2")

			applyID := startLinearRun(t, r, gitURL, "rolled")
			waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

			want := map[int]map[string][]string{0: {hostA: {"a"}, hostB: {"a"}}}
			if got := souls.messages(); !reflect.DeepEqual(got, want) {
				t.Errorf("messages = %v, want %v", got, want)
			}
			for sid, row := range stepRows(t, applyID)[0] {
				if row.kid != "keeper-2" || row.attempt != 1 {
					t.Errorf("step 0 of %s: claimed by %q at attempt %d, want keeper-2 at 1 - the step did not go through the queue", sid, row.kid, row.attempt)
				}
			}
		})
	}
}

// ★ GUARD (NIM-961 acceptance, in-process HA). One instance owns a three-step run;
// two others, sharing nothing with it but PG, claim its step rows. Each step is
// sent once per host and only after the barrier of the step before it; step 1's
// render on another instance reads the register step 0 wrote (its where: and its
// params); a host the step does not target gets no row, and a host whose `when:`
// is false is closed by the run goroutine itself — no row is queued for it (user,
// 2026-10-08, option (i)). The step rows are pinned to the commit the run loaded.
//
// Mutations: render a claimed step without the earlier steps' registers → step 1
// fails on a missing key; write the step's rows for the whole roster → host B gets a
// step-1 row; queue the when-false host → its step-2 row is claimed; drop the commit
// pin → the recipe names the branch.
func TestIntegration_LinearRunStepsAcrossInstances(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	seedTwoHosts(t, hostA, hostB)
	gitURL := linearServiceRepo(t, "roll", `name: roll
description: a probe, then the master only, then the lagging replica only
tasks:
  - name: role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: probe }
  - name: restart
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params: { cmd: "restart ${ register.role.stdout }" }
  - name: resync
    module: core.exec.run
    when: "register.role.stdout == 'lagging'"
    params: { cmd: resync }
`)
	souls := newQueueSouls(t, "role", map[string]string{hostA: "master", hostB: "lagging"})
	r := newLinearQueueRunner(t, souls)
	runAcolyte(t, souls, "keeper-2")
	runAcolyte(t, souls, "keeper-3")

	applyID := startLinearRun(t, r, gitURL, "roll")
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	want := map[int]map[string][]string{
		0: {hostA: {"role"}, hostB: {"role"}},
		1: {hostA: {"restart"}},
		2: {hostB: {"resync"}},
	}
	if got := souls.messages(); !reflect.DeepEqual(got, want) {
		t.Errorf("messages = %v, want %v", got, want)
	}
	if got := souls.commands()[1][hostA]; !reflect.DeepEqual(got, []string{"restart master"}) {
		t.Errorf("step 1 cmd on %s = %v, want [restart master] - the claim did not read step 0's register", hostA, got)
	}
	for key, n := range souls.sendCounts() {
		if n != 1 {
			t.Errorf("%s sent %d times, want once", key, n)
		}
	}

	rows := stepRows(t, applyID)
	head := repoHead(t, gitURL)
	for _, queued := range []struct {
		passage int
		sid     string
	}{{0, hostA}, {0, hostB}, {1, hostA}, {2, hostB}} {
		row, ok := rows[queued.passage][queued.sid]
		if !ok {
			t.Errorf("step %d of %s: no row", queued.passage, queued.sid)
			continue
		}
		if row.status != applyrun.StatusSuccess || (row.kid != "keeper-2" && row.kid != "keeper-3") {
			t.Errorf("step %d of %s = %s claimed by %q, want success claimed by keeper-2 or keeper-3", queued.passage, queued.sid, row.status, row.kid)
		}
		if row.recipe == nil || row.recipe.Step == nil || row.recipe.ServiceRef.Ref != head {
			t.Errorf("step %d of %s: recipe %+v, want a step recipe pinned to %s", queued.passage, queued.sid, row.recipe, head)
		}
	}
	if row, ok := rows[1][hostB]; ok {
		t.Errorf("step 1 of %s has a row (%s) - a host the step does not target gets none", hostB, row.status)
	}
	if row := rows[2][hostA]; row.status != applyrun.StatusSuccess || row.kid != "" || row.recipe != nil {
		t.Errorf("step 2 of %s = %s claimed by %q (recipe %v), want success closed by the run goroutine with nothing queued", hostA, row.status, row.kid, row.recipe != nil)
	}
}

// ★ GUARD (NIM-961: `serial:` is decided per step). A step with `serial:` goes
// inline — distributed serial is ADR-027 Phase 3 — and the step after it, with
// none, still goes through the queue.
//
// Mutation: decide serial: for the whole run → step 1 goes inline too.
func TestIntegration_LinearSerialStepStaysInline(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	seedTwoHosts(t, hostA, hostB)
	gitURL := linearServiceRepo(t, "waves", `name: waves
description: a rolling step, then a plain one
tasks:
  - name: first
    module: core.exec.run
    serial: 1
    settings: { serial_scope: task }
    params: { cmd: first }
  - name: second
    module: core.exec.run
    params: { cmd: second }
`)
	souls := newQueueSouls(t, "", nil)
	r := newLinearQueueRunner(t, souls)
	runAcolyte(t, souls, "keeper-2")

	applyID := startLinearRun(t, r, gitURL, "waves")
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	rows := stepRows(t, applyID)
	for _, sid := range []string{hostA, hostB} {
		if row := rows[0][sid]; row.kid != "" || row.attempt != 0 {
			t.Errorf("step 0 of %s claimed by %q at attempt %d, want inline (serial:)", sid, row.kid, row.attempt)
		}
		if row := rows[1][sid]; row.kid != "keeper-2" {
			t.Errorf("step 1 of %s claimed by %q, want keeper-2 - a step without serial: goes through the queue", sid, row.kid)
		}
	}
}

// ★ GUARD (NIM-961 acceptance: an Acolyte killed between claim and send). Step 0's
// rows are claimed by an instance that dies before sending; the Reaper returns them
// to `planned` once the lease lapses, a live Acolyte sends each exactly once at the
// next attempt, and step 1 goes out at an attempt above it — the Soul fences by
// apply_id, so a lower one would be dropped.
//
// Mutation: drop the attempt raise in ClaimNext (NIM-960) → step 1 goes out at
// attempt 1, below step 0's 2.
func TestIntegration_LinearStepReclaimedAfterAcolyteDeath(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	seedTwoHosts(t, hostA, hostB)
	gitURL := linearServiceRepo(t, "pair", `name: pair
description: two steps
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: second
    module: core.exec.run
    params: { cmd: second }
`)
	souls := newQueueSouls(t, "", nil)
	r := newLinearQueueRunner(t, souls)
	applyID := startLinearRun(t, r, gitURL, "pair")

	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	var dead []*applyrun.ApplyRun
	for len(dead) < 2 && time.Now().Before(deadline) {
		got, err := applyrun.ClaimNext(ctx, integrationPool, "keeper-dead", 50*time.Millisecond, 10, "")
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}
		// The claim has no apply filter: a row of another run still alive in this
		// package is not this test's to count.
		for _, row := range got {
			if row.ApplyID == applyID {
				dead = append(dead, row)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(dead) != 2 || dead[0].Passage != 0 || dead[1].Passage != 0 || dead[0].SID == dead[1].SID {
		t.Fatalf("the dying Acolyte claimed %+v, want step 0 of both hosts", dead)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := reaper.NewPurger(integrationPool).ReclaimApplyRuns(ctx, time.Second, 10); err != nil {
		t.Fatalf("ReclaimApplyRuns: %v", err)
	}
	if rows := stepRows(t, applyID); rows[0][hostA].status != applyrun.StatusPlanned || rows[0][hostB].status != applyrun.StatusPlanned {
		t.Fatalf("after reclaim step 0 = %+v, want both rows back in planned", rows[0])
	}
	runAcolyte(t, souls, "keeper-2")
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	for _, sid := range []string{hostA, hostB} {
		if got := souls.sentAt(sid, 0); !reflect.DeepEqual(got, []int32{2}) {
			t.Errorf("step 0 of %s sent at attempts %v, want once at 2", sid, got)
		}
		if got := souls.sentAt(sid, 1); len(got) != 1 || got[0] <= 2 {
			t.Errorf("step 1 of %s sent at attempts %v, want once above 2", sid, got)
		}
	}
}

// insertStepRow writes a step row of a linear run as the run goroutine would,
// with the decisions in step.
func insertStepRow(t *testing.T, applyID, sid, gitURL, scenarioName string, passage int, step *applyrun.RecipeStep) {
	t.Helper()
	if err := applyrun.InsertPlanned(context.Background(), integrationPool, &applyrun.ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "redis-prod", Scenario: scenarioName, Passage: passage,
		Recipe: &applyrun.Recipe{
			ServiceRef:   artifact.ServiceRef{Name: "redis", Git: gitURL, Ref: "master"},
			ScenarioName: scenarioName,
			Step:         step,
		},
	}); err != nil {
		t.Fatalf("InsertPlanned %s step %d: %v", sid, passage, err)
	}
}

func claimAll(t *testing.T, cr *ClaimRunner) {
	t.Helper()
	if err := cr.Claim(context.Background()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
}

// seedMember seeds an incarnation member in the given status.
func seedMember(t *testing.T, sid string, status soul.Status) {
	t.Helper()
	if err := soul.Insert(context.Background(), integrationPool, &soul.Soul{SID: sid, Status: status}); err != nil {
		t.Fatalf("seed %s: %v", sid, err)
	}
	if err := incarnation.AddMembers(context.Background(), integrationPool, "redis-prod", []string{sid}, nil); err != nil {
		t.Fatalf("membership %s: %v", sid, err)
	}
}

// ★ GUARD (NIM-961: the claim sends only what the run decided). The claim renders
// over the run's roster — live or not — with the run's held block targets, and a
// claim whose render arrives at other tasks than the row names refuses the host and
// sends it nothing.
//
// Mutations: rebuild the held targets empty → host B passes the check with the
// block's child and is sent it; render over the live members → a lost host drops
// out of soulprint.hosts; render over all members → run_once: moves to host A and
// host B's row is refused; drop the check → host B is closed no_match instead of
// refused; check only that something is sent → a task the run did not decide goes
// out.
func TestIntegration_ClaimStepSendsOnlyWhatTheRunDecided(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	refused := func(t *testing.T, applyID, sid string) {
		t.Helper()
		var (
			status  string
			summary *string
		)
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT status, error_summary FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid).Scan(&status, &summary); err != nil {
			t.Fatalf("read %s: %v", sid, err)
		}
		if status != string(applyrun.StatusFailed) || summary == nil || !strings.Contains(*summary, "the run decided") {
			t.Errorf("%s = %s (%v), want failed naming what the run decided", sid, status, summary)
		}
	}

	t.Run("held block targets", func(t *testing.T) {
		seedTwoHosts(t, hostA, hostB)
		gitURL := linearServiceRepo(t, "held", `name: held
description: one default block on every host
tasks:
  - name: pair
    block:
      - name: a
        module: core.exec.run
        params: { cmd: a }
`)
		const applyID = "01HHELDTARGETS"
		step := &applyrun.RecipeStep{
			Roster:      []string{hostA, hostB},
			HeldTargets: map[string][]string{"0": {hostA}},
			Tasks:       []int{0},
		}
		insertStepRow(t, applyID, hostA, gitURL, "held", 0, step)
		insertStepRow(t, applyID, hostB, gitURL, "held", 0, step)

		souls := newQueueSouls(t, "", nil)
		claimAll(t, newStepClaimRunner(t, souls, "keeper-2"))

		if got, want := souls.messages(), (map[int]map[string][]string{0: {hostA: {"a"}}}); !reflect.DeepEqual(got, want) {
			t.Errorf("messages = %v, want %v", got, want)
		}
		refused(t, applyID, hostB)
	})

	t.Run("roster live or not", func(t *testing.T) {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "redis-prod")
		seedMember(t, hostA, soul.StatusDisconnected)
		seedConnectedSoul(t, hostB, []string{"redis-prod"})
		gitURL := linearServiceRepo(t, "count", `name: count
description: a step that reads the roster
tasks:
  - name: a
    module: core.exec.run
    params: { cmd: "${ string(size(soulprint.hosts)) }" }
`)
		const applyID = "01HROSTERLIVEORNOT"
		insertStepRow(t, applyID, hostB, gitURL, "count", 0, &applyrun.RecipeStep{Roster: []string{hostA, hostB}, Tasks: []int{0}})

		souls := newQueueSouls(t, "", nil)
		claimAll(t, newStepClaimRunner(t, souls, "keeper-2"))

		if got := souls.commands()[0][hostB]; !reflect.DeepEqual(got, []string{"2"}) {
			t.Errorf("cmd on %s = %v, want [2] - the run's roster holds the host that lost its lease", hostB, got)
		}
	})

	t.Run("roster as the run had it", func(t *testing.T) {
		seedTwoHosts(t, hostA, hostB)
		gitURL := linearServiceRepo(t, "once", `name: once
description: one host of the roster
tasks:
  - name: a
    module: core.exec.run
    run_once: true
    params: { cmd: a }
`)
		const applyID = "01HPINNEDROSTER"
		insertStepRow(t, applyID, hostB, gitURL, "once", 0, &applyrun.RecipeStep{Roster: []string{hostB}, Tasks: []int{0}})

		souls := newQueueSouls(t, "", nil)
		claimAll(t, newStepClaimRunner(t, souls, "keeper-2"))

		if got, want := souls.messages(), (map[int]map[string][]string{0: {hostB: {"a"}}}); !reflect.DeepEqual(got, want) {
			t.Errorf("messages = %v, want %v", got, want)
		}
	})

	t.Run("decided other tasks", func(t *testing.T) {
		seedTwoHosts(t, hostA, hostB)
		gitURL := linearServiceRepo(t, "one", `name: one
description: one task
tasks:
  - name: a
    module: core.exec.run
    params: { cmd: a }
`)
		const applyID = "01HDECIDEDOTHER"
		insertStepRow(t, applyID, hostB, gitURL, "one", 0, &applyrun.RecipeStep{Roster: []string{hostA, hostB}, Tasks: []int{7}})

		souls := newQueueSouls(t, "", nil)
		claimAll(t, newStepClaimRunner(t, souls, "keeper-2"))

		if got := souls.messages(); len(got) != 0 {
			t.Errorf("sent %v, want nothing - the run decided another task", got)
		}
		refused(t, applyID, hostB)
	})

	for name, when := range map[string]string{
		"when false at claim": "register.role.stdout == 'lagging'",
		"when fails at claim": "register.role.nope == 'lagging'",
	} {
		t.Run(name, func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "redis-prod")
			seedConnectedSoul(t, hostB, []string{"redis-prod"})
			gitURL := linearServiceRepo(t, "settle", `name: settle
description: a probe, then a step gated on it
tasks:
  - name: role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: probe }
  - name: resync
    module: core.exec.run
    when: "`+when+`"
    params: { cmd: resync }
`)
			ctx := context.Background()
			const applyID = "01HCLAIMDIFFERS"
			if _, err := integrationPool.Exec(ctx, `INSERT INTO apply_runs
				(apply_id, sid, incarnation_name, scenario, status, passage, attempt)
				VALUES ($1, $2, 'redis-prod', 'settle', 'success', 0, 1)`, applyID, hostB); err != nil {
				t.Fatalf("seed step 0: %v", err)
			}
			if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
				ApplyID: applyID, SID: hostB, PlanIndex: 0, TaskIdx: 0, Passage: 0,
				RegisterData: map[string]any{"stdout": "master", "changed": false, "failed": false},
			}); err != nil {
				t.Fatalf("seed register: %v", err)
			}
			insertStepRow(t, applyID, hostB, gitURL, "settle", 1, &applyrun.RecipeStep{
				Roster: []string{hostB}, RegisterNames: map[int]string{0: "role"}, Tasks: []int{1},
			})

			souls := newQueueSouls(t, "", nil)
			claimAll(t, newStepClaimRunner(t, souls, "keeper-2"))

			if got := souls.messages(); len(got) != 0 {
				t.Errorf("sent %v, want nothing", got)
			}
			if got := stepRows(t, applyID)[1][hostB].status; got != applyrun.StatusFailed {
				t.Errorf("step 1 = %s, want failed - the run decided to send the task", got)
			}
		})
	}
}

// ★ GUARD (NIM-961: a host the Keeper fails before sending does not cut the step
// short for the hosts queued beside it). Step 1's `when:` fails to evaluate on host
// A — its probe register lacks the key — and is true on host B: the run goroutine
// fails A without queueing it, queues B, and waits for B's result before the step
// fails, as in one inline wave.
//
// Mutation: end dispatchStepPlanned with the fail-fast waitBarrier → the step fails
// at once and B's row is swept to failed before its result.
func TestIntegration_LinearQueuedStepOutlivesAKeeperFailedHost(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	seedTwoHosts(t, hostA, hostB)
	gitURL := linearServiceRepo(t, "lag", `name: lag
description: a probe, then the lagging replica only
tasks:
  - name: role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: probe }
  - name: resync
    module: core.exec.run
    when: "register.role.lag == 'yes'"
    params: { cmd: resync }
`)
	souls := newQueueSouls(t, "role", map[string]string{hostA: "master", hostB: "slave"})
	souls.extra = map[string]map[string]any{hostB: {"lag": "yes"}}
	r := newLinearQueueRunner(t, souls)
	runAcolyte(t, souls, "keeper-2")

	applyID := startLinearRun(t, r, gitURL, "lag")
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)

	rows := stepRows(t, applyID)
	if row := rows[1][hostA]; row.status != applyrun.StatusFailed || row.kid != "" {
		t.Errorf("step 1 of %s = %s claimed by %q, want failed by the run goroutine", hostA, row.status, row.kid)
	}
	if row := rows[1][hostB]; row.status != applyrun.StatusSuccess || row.kid != "keeper-2" {
		t.Errorf("step 1 of %s = %s claimed by %q, want success through the queue - the step waited for it", hostB, row.status, row.kid)
	}
}

// ★ GUARD (NIM-961: the records a claim writes for the tasks it does not send). A
// step that sends a host one iteration of a loop and skips the other (the pattern of
// TestIntegration_LinearLoopIterationSkippedBesideOneThatRuns): the claim writes the
// skipped one's record, numbered after the task it sends, so a later step reads its
// register instead of a missing key.
//
// Mutation: drop the records the claim writes after MarkDispatched → no record.
func TestIntegration_ClaimStepRecordsWhatItSkips(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	seedTwoHosts(t, hostA, hostB)
	gitURL := linearServiceRepo(t, "each", `name: each
description: a loop whose iterations each run on one host
tasks:
  - name: each
    module: core.exec.run
    loop:
      items: ["`+hostA+`", "`+hostB+`"]
    vars: { target: "${ item }" }
    when: vars.target == soulprint.self.sid
    params: { cmd: "${ item }" }
`)
	souls := newQueueSouls(t, "", nil)
	r := newLinearQueueRunner(t, souls)
	runAcolyte(t, souls, "keeper-2")

	applyID := startLinearRun(t, r, gitURL, "each")
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	if got := souls.commands()[0]; !reflect.DeepEqual(got, map[string][]string{hostA: {hostA}, hostB: {hostB}}) {
		t.Fatalf("cmds = %v, want each host's own iteration", got)
	}
	for sid, row := range stepRows(t, applyID)[0] {
		if row.kid != "keeper-2" {
			t.Errorf("%s claimed by %q, want keeper-2 - the step did not go through the queue", sid, row.kid)
		}
	}
	regs, err := applyrun.SelectTaskRegistersByApplyIDUpToPassage(context.Background(), integrationPool, applyID, 1)
	if err != nil {
		t.Fatalf("registers: %v", err)
	}
	skipped := map[string]int{}
	for _, reg := range regs {
		if reg.RegisterData["skipped"] == true {
			skipped[reg.SID] = reg.TaskIdx
		}
	}
	if want := map[string]int{hostA: 1, hostB: 1}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped records by host (task_idx) = %v, want %v - numbered after the one task each host is sent", skipped, want)
	}
}
