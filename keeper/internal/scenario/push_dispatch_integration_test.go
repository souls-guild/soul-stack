//go:build integration

// The live scenario push run: a real Postgres, a real sshd on a real OS, the
// real `soul` binary built from this tree, and the production wiring above
// them — roster → dispatchPassage → SshDispatcher → ShaDeliverer → `soul apply`
// → NDJSON TaskEvent/RunResult → apply_task_register + apply_runs → barrier.
//
// WHY IT EXISTS. NIM-870 shipped the `transport:` key and nothing in production
// set it: the scenario dispatcher had no push branch and `POST /v1/push/apply`
// carried no field for it. Half a feature that looks whole. NIM-880 added the
// branch, and the one question it had to answer is not whether the branch is
// reached — a mock proves that — but whether a task that runs over push leaves
// the same trace a streamed one does. So the assertions here are the two rows
// the barrier and a later `require:` actually read: the `apply_task_register`
// row carrying output the HOST produced, and the terminal on `apply_runs`.
//
// WHY IT CANNOT GO GREEN WITHOUT A HOST. The register value asserted is the
// hostname the container printed, and the file assertion is read back through
// the container rather than through the run's report. A run that never
// connected produces neither.
//
// Run:
//
//	cd keeper && go test -tags=integration -count=1 ./internal/scenario/ -run PushBranch

package scenario

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/sshdtest"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	pushBranchProvider = "live-ssh"
	// pushBranchProofPath is what the run's first task writes on the host. Read
	// back out-of-band it is the one assertion no keeper-side bookkeeping can
	// fake.
	pushBranchProofPath = "/tmp/nim880-scenario-push-reached-the-host"
)

// TestIntegration_PushBranch_ScenarioTaskRunsOverSSHAndFillsRegister is the
// acceptance for NIM-880.
//
// The scenario shape is the one the ticket is about: a task carrying
// `transport: ssh` with the Level 0 overrides on it, targeting a host whose
// registry row is `transport=ssh` and — the roster half — `pending`, the only
// status such a host ever holds.
//
// Three things are asserted, in the order they would break:
//
//  1. the barrier RELEASES. dispatchPassage returns nil only after the host's
//     `apply_runs` row reached a terminal, which on this branch is written by
//     the goroutine that ran the SSH session. A branch that dispatched and
//     forgot would hang here until the run timeout;
//  2. `register:` FILLED, with a value the host produced. This is the question
//     NIM-870 left open. The row has a foreign key to the `apply_runs` row this
//     path mints, so it lands only because the branch mints one;
//  3. the machine CHANGED, read back through the container.
func TestIntegration_PushBranch_ScenarioTaskRunsOverSSHAndFillsRegister(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContextFor(10 * time.Minute)
	defer cancel()
	resetAll(t)

	soulBinary := buildSoulBinaryForPushBranch(t)
	ca := sshdtest.NewCA(t)
	host := sshdtest.Start(ctx, t, ca, sshdtest.Options{})

	// The SID is the address: PGFallbackTargetResolver resolves SSHTarget.Host
	// from the SID, and `souls.ssh_target` has no address column at all. That is
	// a real constraint on push today (and the reason bootstrapping a bare VM is
	// separate scope), not a shortcut of this test.
	sid := host.Addr
	const incName = "push-branch-live"

	seedOperator(t, "archon-live")
	seedIncarnation(t, incName)
	seedPushSoulInIncarnation(t, sid, incName, host.Port)

	// ROSTER HALF: the same production resolver a run uses. Before NIM-880 this
	// returned nothing for this host — `pending` was excluded outright — so the
	// branch below could not have been reached by any configuration.
	hosts, err := topology.NewResolver(integrationPool, nil, nil).LoadIncarnationHosts(ctx, incName)
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != sid {
		t.Fatalf("roster = %v, want the pending transport=ssh member - the scenario roster's push half is not admitting it", hostSIDs(hosts))
	}

	dispatcher := livePushDispatcher(t, ca, host, soulBinary)

	// Two tasks, so the register the first one produces is a value the second
	// could consume and a `require:` could wait on. Both carry the key.
	tasks := []*render.RenderedTask{
		{
			Index: 0, Name: "probe", Module: "core.exec.run", Register: "probe",
			// argv-form, no shell: core.exec.run takes cmd/args, never a
			// command line (core/exec/README.md).
			Params: mustStructI(t, map[string]any{"cmd": "hostname"}),
		},
		{
			Index: 1, Name: "mark", Module: "core.exec.run",
			Params: mustStructI(t, map[string]any{
				"cmd":  "/bin/sh",
				"args": []any{"-c", "echo reached > " + pushBranchProofPath},
			}),
		},
	}
	plans := []render.DispatchPlan{
		{
			TaskIndex: 0, TargetSIDs: []string{sid},
			TransportName: config.TransportSSH,
			// The Level 0 overrides of ADR-0088, on the task. The registry row
			// seeded above carries a user sshd does not admit, so a dial that read
			// the registry instead of the task fails to authenticate — a green run
			// is evidence the task won, not evidence that neither mattered.
			TransportParams: map[string]any{
				config.TransportParamSSHProvider: pushBranchProvider,
				config.TransportParamUser:        host.User,
				config.TransportParamPort:        uint64(host.Port),
			},
		},
		{
			TaskIndex: 1, TargetSIDs: []string{sid},
			TransportName: config.TransportSSH,
			TransportParams: map[string]any{
				config.TransportParamSSHProvider: pushBranchProvider,
				config.TransportParamUser:        host.User,
				config.TransportParamPort:        uint64(host.Port),
			},
		},
	}

	runner := &Runner{
		deps: Deps{
			DB:        integrationPool,
			PushApply: dispatcher,
			Audit:     auditpg.NewWriter(integrationPool),
			// No PushRouter on purpose: the task named its own provider, so Level 0
			// must answer and the router must not be needed at all.
		},
		applyDB:      integrationPool,
		logger:       discardLog(),
		pollInterval: 200 * time.Millisecond,
	}
	spec := RunSpec{
		ApplyID:         "01NIM880PUSHBRANCHLIVE",
		IncarnationName: incName,
		ScenarioName:    "live",
		StartedByAID:    "archon-live",
	}

	// (1) THE BARRIER. Returns only once the host's row is terminal and benign.
	if err := runner.dispatchPassage(ctx, spec, discardLog(), 0, tasks, plans, nil, hosts); err != nil {
		t.Fatalf("dispatchPassage over push: %v\n\tapply_runs=%s", err, applyRunDump(ctx, t, spec.ApplyID))
	}

	// (2) THE REGISTER — the question NIM-870 left open. The value is the
	// container's hostname, which only the host can have produced.
	regs, err := applyrun.SelectTaskRegistersByApplyID(ctx, integrationPool, spec.ApplyID)
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	// The accumulator keys by plan_index and knows no register NAMES (the proto
	// carries indices only, ADR-012(d)) — the runner resolves the name from its
	// own []RenderedTask on read. So every task that produced output has a row,
	// not just the one carrying `register:`; what matters is that task 0's is
	// there and carries what the host printed.
	probe, found := registerFor(regs, sid, 0)
	if !found {
		t.Fatalf("no apply_task_register row for (%s, plan_index 0) in %d rows - a push run that fills no register leaves every `require:` over it unreleased", sid, len(regs))
	}
	stdout, _ := probe.RegisterData["stdout"].(string)
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("register_data has no stdout from the host: %v", probe.RegisterData)
	}

	// (3) THE MACHINE. The run's report says it succeeded; this says the host
	// changed.
	got, err := host.Exec(ctx, "cat "+pushBranchProofPath)
	if err != nil {
		t.Fatalf("the task's file is not on the host: %v", err)
	}
	if strings.TrimSpace(got) != "reached" {
		t.Errorf("%s = %q, want \"reached\"", pushBranchProofPath, strings.TrimSpace(got))
	}

	// The audit half of ADR-0088: the task's key made a third source of truth
	// for provider/user/port, so the run has to say which source answered.
	assertPushDispatchAudited(ctx, t, spec.ApplyID, sid, host.User, host.Port)
}

// TestIntegration_PushBranch_FailingTaskBreaksTheBarrierWithItsReason — the
// failure path of the same branch. A push run that fails must reach the barrier
// as a failed terminal carrying the task's own reason, not as a hang and not as
// a bare `failed`: those are the two ways a transport goes quiet.
func TestIntegration_PushBranch_FailingTaskBreaksTheBarrierWithItsReason(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContextFor(10 * time.Minute)
	defer cancel()
	resetAll(t)

	soulBinary := buildSoulBinaryForPushBranch(t)
	ca := sshdtest.NewCA(t)
	host := sshdtest.Start(ctx, t, ca, sshdtest.Options{})
	sid := host.Addr
	const incName = "push-branch-live-fail"

	seedOperator(t, "archon-live")
	seedIncarnation(t, incName)
	seedPushSoulInIncarnation(t, sid, incName, host.Port)

	hosts, err := topology.NewResolver(integrationPool, nil, nil).LoadIncarnationHosts(ctx, incName)
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}

	tasks := []*render.RenderedTask{{
		Index: 0, Name: "doomed", Module: "core.exec.run",
		Params: mustStructI(t, map[string]any{"cmd": "/bin/sh", "args": []any{"-c", "exit 7"}}),
	}}
	plans := []render.DispatchPlan{{
		TaskIndex: 0, TargetSIDs: []string{sid},
		TransportName: config.TransportSSH,
		TransportParams: map[string]any{
			config.TransportParamSSHProvider: pushBranchProvider,
			config.TransportParamUser:        host.User,
			config.TransportParamPort:        uint64(host.Port),
		},
	}}

	runner := &Runner{
		deps:         Deps{DB: integrationPool, PushApply: livePushDispatcher(t, ca, host, soulBinary)},
		applyDB:      integrationPool,
		logger:       discardLog(),
		pollInterval: 200 * time.Millisecond,
	}
	spec := RunSpec{
		ApplyID: "01NIM880PUSHBRANCHFAIL", IncarnationName: incName,
		ScenarioName: "live", StartedByAID: "archon-live",
	}

	err = runner.dispatchPassage(ctx, spec, discardLog(), 0, tasks, plans, nil, hosts)
	if err == nil {
		t.Fatal("a failing push task returned a green barrier")
	}
	// The reason comes from apply_runs.error_summary, which on this branch is
	// written by the sink from the host's own TaskEvent. A bare status here would
	// mean the TaskEvents never reached the accumulator.
	if !strings.Contains(err.Error(), "exit code 7") {
		t.Errorf("barrier error = %v\n\twant the failed task's own reason from its TaskEvent; a bare status would mean the events never reached the accumulator", err)
	}
}

// --- fixtures ---------------------------------------------------------

// seedPushSoulInIncarnation seeds the registry row a scenario push host needs:
// a `pending` soul with transport=ssh, bound to the incarnation, plus an
// ssh_target. The target's user is DELIBERATELY one sshd does not admit — the
// task's `transport: { ssh: { user: … } }` has to beat it, and a row seeded with
// the right user would prove nothing about the precedence.
func seedPushSoulInIncarnation(t *testing.T, sid, incName string, port int) {
	t.Helper()
	ctx := context.Background()
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID:       sid,
		Transport: soul.TransportSSH,
		Status:    soul.StatusPending,
	}); err != nil {
		t.Fatalf("seed push soul: %v", err)
	}
	if err := incarnation.AddMembers(ctx, integrationPool, incName, []string{sid}, nil); err != nil {
		t.Fatalf("bind push soul to %s: %v", incName, err)
	}
	if err := soul.UpdateSshTarget(ctx, integrationPool, sid, &soul.SSHTarget{
		SSHPort: port + 1,
		SSHUser: "not-the-user-sshd-admits",
	}); err != nil {
		t.Fatalf("seed ssh_target: %v", err)
	}
}

// livePushDispatcher builds the production SshDispatcher over the test CA and
// the container, with delivery driven by the same keeper.yml decision the
// daemon makes.
func livePushDispatcher(t *testing.T, ca sshdtest.CA, host *sshdtest.Host, soulBinary string) *push.SshDispatcher {
	t.Helper()
	deliverer, soulSpec, err := push.DeliveryFromConfig(&config.KeeperPush{SoulBinaryPath: soulBinary})
	if err != nil {
		t.Fatalf("DeliveryFromConfig: %v", err)
	}
	if deliverer == nil {
		t.Fatal("DeliveryFromConfig returned no Deliverer for a configured binary")
	}
	d, err := push.NewSshDispatcher(push.Deps{
		Providers: map[string]push.ProviderEntry{
			pushBranchProvider: {Provider: &sshdtest.Provider{CA: ca, User: host.User}},
		},
		Targets:         &push.PGFallbackTargetResolver{Reader: push.NewPGTargetReader(integrationPool)},
		Souls:           push.NewPGSoulLookup(integrationPool),
		HostAuthorities: []push.NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.Pub}},
		Deliverer:       deliverer,
		SoulSpec:        soulSpec,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout:     20 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSshDispatcher: %v", err)
	}
	return d
}

// assertPushDispatchAudited checks the `apply.dispatched` event carries the
// effective transport decision. Without it the task's override — which beats
// two other sources — is invisible to an incident review, and a run would read
// as if it had gone by the registry it never consulted.
func assertPushDispatchAudited(ctx context.Context, t *testing.T, applyID, sid, user string, port int) {
	t.Helper()
	rows, err := integrationPool.Query(ctx,
		`SELECT payload FROM audit_log WHERE correlation_id = $1 AND event_type = 'apply.dispatched'`, applyID)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload map[string]any
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("audit scan: %v", err)
		}
		if payload["sid"] != sid {
			continue
		}
		if payload["transport"] != config.TransportSSH {
			t.Errorf("audit transport = %v, want ssh", payload["transport"])
		}
		if payload["route_source"] != "task" {
			t.Errorf("audit route_source = %v, want task - the task named its own provider", payload["route_source"])
		}
		if payload["ssh_user"] != user {
			t.Errorf("audit ssh_user = %v, want %q", payload["ssh_user"], user)
		}
		if got, ok := payload["ssh_port"].(float64); !ok || int(got) != port {
			t.Errorf("audit ssh_port = %v, want %d", payload["ssh_port"], port)
		}
		return
	}
	t.Errorf("no apply.dispatched audit event for %s - the effective transport decision is invisible", sid)
}

// registerFor finds a host's accumulated register row by plan_index.
func registerFor(regs []applyrun.TaskRegister, sid string, planIndex int) (applyrun.TaskRegister, bool) {
	for _, r := range regs {
		if r.SID == sid && r.PlanIndex == planIndex {
			return r, true
		}
	}
	return applyrun.TaskRegister{}, false
}

func hostSIDs(hosts []*topology.HostFacts) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.SID)
	}
	return out
}

// applyRunDump renders the run's apply_runs rows for a failure message: a
// barrier that timed out says nothing by itself, and the row says what it was
// waiting for.
func applyRunDump(ctx context.Context, t *testing.T, applyID string) string {
	t.Helper()
	statuses, err := applyrun.SelectStatusesByApplyID(ctx, integrationPool, applyID)
	if err != nil {
		return "apply_runs unreadable: " + err.Error()
	}
	var b strings.Builder
	for _, s := range statuses {
		b.WriteString(s.SID + "=" + string(s.Status))
		if s.ErrorSummary != nil {
			b.WriteString(" (" + *s.ErrorSummary + ")")
		}
		b.WriteString("; ")
	}
	return b.String()
}

// buildSoulBinaryForPushBranch compiles the agent from this tree. CGO off so the
// binary is static and runs on the container's musl; GOOS pinned because a
// docker daemon on a mac or windows host still runs a linux container, and the
// failure of a wrong-platform binary arrives as a RunResult-less stream rather
// than as "wrong platform".
func buildSoulBinaryForPushBranch(t *testing.T) string {
	t.Helper()
	soulModule, err := filepath.Abs(filepath.Join("..", "..", "..", "soul"))
	if err != nil {
		t.Fatalf("resolve soul module: %v", err)
	}
	out := filepath.Join(t.TempDir(), "soul")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/soul")
	cmd.Dir = soulModule
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build soul: %v\n%s", err, combined)
	}
	return out
}
