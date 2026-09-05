package clouddriver

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClassify_CtxAndNil(t *testing.T) {
	if got := Classify(nil, nil); got != FailUnknown {
		t.Errorf("nil err class=%v, want unknown", got)
	}
	if got := Classify(nil, context.Canceled); got != FailTransient {
		t.Errorf("ctx.Canceled class=%v, want transient", got)
	}
	if got := Classify(nil, context.DeadlineExceeded); got != FailTransient {
		t.Errorf("ctx.DeadlineExceeded class=%v, want transient", got)
	}
	// fn=nil + arbitrary error → unknown (driver didn't set a classifier).
	if got := Classify(nil, errors.New("x")); got != FailUnknown {
		t.Errorf("nil fn class=%v, want unknown", got)
	}
}

func TestClassify_DelegatesToFn(t *testing.T) {
	fn := func(err error) FailClass {
		if err.Error() == "Throttling" {
			return FailTransient
		}
		return FailAuth
	}
	if got := Classify(fn, errors.New("Throttling")); got != FailTransient {
		t.Errorf("class=%v, want transient", got)
	}
	if got := Classify(fn, errors.New("AccessDenied")); got != FailAuth {
		t.Errorf("class=%v, want auth", got)
	}
}

func TestFailClass_TransientAndString(t *testing.T) {
	cases := map[FailClass]struct {
		str   string
		trans bool
	}{
		FailUnknown:       {"unknown", false},
		FailNotFound:      {"not_found", false},
		FailQuota:         {"quota_exceeded", false},
		FailAuth:          {"auth", false},
		FailInvalidParams: {"invalid_params", false},
		FailTransient:     {"transient", true},
	}
	for c, want := range cases {
		if c.String() != want.str {
			t.Errorf("%d.String()=%q, want %q", c, c.String(), want.str)
		}
		if c.Transient() != want.trans {
			t.Errorf("%d.Transient()=%v, want %v", c, c.Transient(), want.trans)
		}
	}
}

func TestFailMessage(t *testing.T) {
	msg := FailMessage(FailQuota, "RunInstances", errors.New("limit reached"))
	want := "quota_exceeded: RunInstances: limit reached"
	if msg != want {
		t.Errorf("msg=%q, want %q", msg, want)
	}
}

func TestRetry_SucceedsAfterTransient(t *testing.T) {
	classify := func(err error) FailClass {
		if err.Error() == "throttle" {
			return FailTransient
		}
		return FailAuth
	}
	calls := 0
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 5}
	err := Retry(context.Background(), cfg, classify, func() error {
		calls++
		if calls < 3 {
			return errors.New("throttle")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls=%d, want 3", calls)
	}
}

func TestRetry_NonTransientReturnsImmediately(t *testing.T) {
	classify := func(error) FailClass { return FailAuth }
	calls := 0
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 5}
	wantErr := errors.New("denied")
	err := Retry(context.Background(), cfg, classify, func() error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Errorf("calls=%d, want 1 (non-transient must not retry)", calls)
	}
}

func TestRetry_ExhaustsAttempts(t *testing.T) {
	classify := func(error) FailClass { return FailTransient }
	calls := 0
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	err := Retry(context.Background(), cfg, classify, func() error {
		calls++
		return errors.New("throttle")
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if calls != 3 {
		t.Errorf("calls=%d, want 3 (MaxAttempts)", calls)
	}
}

func TestRetry_CtxCancelDuringBackoff(t *testing.T) {
	classify := func(error) FailClass { return FailTransient }
	ctx, cancel := context.WithCancel(context.Background())
	cfg := BackoffConfig{Initial: 50 * time.Millisecond, Max: time.Second, Factor: 2, MaxAttempts: 10}
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	err := Retry(ctx, cfg, classify, func() error { return errors.New("throttle") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

func TestWaitUntilReady_AllBecomeReady(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 20}
	polls := map[string]int{}
	probe := func(_ context.Context, vmID string) ProbeResult {
		polls[vmID]++
		return ProbeResult{Ready: polls[vmID] >= 2}
	}
	res, err := WaitUntilReady(context.Background(), cfg, []string{"i-1", "i-2"}, probe, nil)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	for _, r := range res {
		if !r.Ready || r.Err != nil {
			t.Errorf("vm %s: ready=%v err=%v, want ready", r.VMID, r.Ready, r.Err)
		}
	}
}

func TestWaitUntilReady_TerminalErr(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 20}
	probeErr := errors.New("instance went to error state")
	probe := func(_ context.Context, vmID string) ProbeResult {
		if vmID == "i-bad" {
			return ProbeResult{Err: probeErr}
		}
		return ProbeResult{Ready: true}
	}
	res, err := WaitUntilReady(context.Background(), cfg, []string{"i-ok", "i-bad"}, probe, nil)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	byID := map[string]WaitResult{}
	for _, r := range res {
		byID[r.VMID] = r
	}
	if !byID["i-ok"].Ready {
		t.Error("i-ok should be ready")
	}
	if byID["i-bad"].Err == nil || byID["i-bad"].Ready {
		t.Errorf("i-bad should carry terminal err, got %+v", byID["i-bad"])
	}
}

// TestWaitUntilReady_CtxCancel_AntiOrphan covers the key reference technique:
// on ctx-cancel, per-VM results are returned for VMs already polled (with
// VMID filled in), so the driver can mark the ones that didn't make it as
// failed → Keeper can Destroy them.
func TestWaitUntilReady_CtxCancel_AntiOrphan(t *testing.T) {
	cfg := BackoffConfig{Initial: 50 * time.Millisecond, Max: time.Second, Factor: 2, MaxAttempts: 50}
	ctx, cancel := context.WithCancel(context.Background())
	probe := func(_ context.Context, _ string) ProbeResult {
		return ProbeResult{Ready: false} // never ready
	}
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	res, err := WaitUntilReady(ctx, cfg, []string{"i-1", "i-2"}, probe, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if len(res) != 2 {
		t.Fatalf("res len=%d, want 2 (anti-orphan: all vm_ids returned)", len(res))
	}
	for _, r := range res {
		if r.VMID == "" {
			t.Error("anti-orphan: WaitResult must carry vm_id even when cancelled")
		}
		if r.Ready {
			t.Errorf("vm %s reported ready unexpectedly", r.VMID)
		}
	}
}

func TestWaitUntilReady_MaxAttemptsDeadline(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	probe := func(_ context.Context, _ string) ProbeResult { return ProbeResult{Ready: false} }
	res, err := WaitUntilReady(context.Background(), cfg, []string{"i-1"}, probe, nil)
	if !errors.Is(err, ErrWaitDeadline) {
		t.Fatalf("err=%v, want ErrWaitDeadline", err)
	}
	if len(res) != 1 || res[0].Ready {
		t.Errorf("res=%+v, want one not-ready entry", res)
	}
}

// TestBackoffBudget_MatchesSleepCount pins the contract Budget() states: both
// Retry and WaitUntilReady sleep MaxAttempts-1 times, so the budget is the sum
// of exactly that many delays.
func TestBackoffBudget_MatchesSleepCount(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Second, Max: 30 * time.Second, Factor: 2, MaxAttempts: 4}
	if got, want := cfg.Budget(), 1*time.Second+2*time.Second+4*time.Second; got != want {
		t.Errorf("Budget()=%v, want %v", got, want)
	}
	for _, n := range []int{0, 1} {
		c := cfg
		c.MaxAttempts = n
		if got := c.Budget(); got != 0 {
			t.Errorf("MaxAttempts=%d: Budget()=%v, want 0", n, got)
		}
	}
}

// TestWaitBackoffFor_CoversBudget: the wait backoff must cover the requested
// wall-clock budget and be minimal — one attempt less must fall short.
func TestWaitBackoffFor_CoversBudget(t *testing.T) {
	for _, budget := range []time.Duration{30 * time.Second, 3 * time.Minute, 10 * time.Minute, time.Hour} {
		cfg := WaitBackoffFor(budget)
		if got := cfg.Budget(); got < budget {
			t.Errorf("budget %v: WaitBackoffFor covers only %v", budget, got)
		}
		shorter := cfg
		shorter.MaxAttempts--
		if got := shorter.Budget(); got >= budget {
			t.Errorf("budget %v: MaxAttempts=%d is not minimal (%d attempts already cover %v)",
				budget, cfg.MaxAttempts, shorter.MaxAttempts, got)
		}
	}
}

func TestWaitBackoffFor_NonPositiveBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Minute} {
		if got := WaitBackoffFor(budget).MaxAttempts; got != 1 {
			t.Errorf("budget %v: MaxAttempts=%d, want 1 (single probe, no waiting)", budget, got)
		}
	}
}

// TestDefaultWaitBackoff_OutlivesRetryBudget is the NIM-173 guard: a fresh VM
// boots minutes (more when a cluster topology brings up 3+ at once), so the
// wait phase must not be capped by the API-retry budget it used to share.
func TestDefaultWaitBackoff_OutlivesRetryBudget(t *testing.T) {
	wait, retry := DefaultWaitBackoff(), DefaultBackoff()
	if wait.MaxAttempts <= retry.MaxAttempts {
		t.Errorf("wait MaxAttempts=%d, want more than retry's %d", wait.MaxAttempts, retry.MaxAttempts)
	}
	if got := wait.Budget(); got < DefaultWaitBudget {
		t.Errorf("wait budget=%v, want at least %v", got, DefaultWaitBudget)
	}
	if retry.Budget() >= wait.Budget() {
		t.Errorf("retry budget %v must stay well under the wait budget %v", retry.Budget(), wait.Budget())
	}
}

func TestDefaultWaitBackoff_EnvOverride(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration // minimum budget the resulting backoff must cover
	}{
		{"20m", 20 * time.Minute},
		{"45s", 45 * time.Second},
		{"not-a-duration", DefaultWaitBudget}, // garbage → default, never zero
		{"0s", DefaultWaitBudget},
		{"-5m", DefaultWaitBudget},
		{"", DefaultWaitBudget},
	}
	for _, tc := range cases {
		t.Setenv(WaitBudgetEnv, tc.env)
		if got := DefaultWaitBackoff().Budget(); got < tc.want {
			t.Errorf("%s=%q: budget=%v, want at least %v", WaitBudgetEnv, tc.env, got, tc.want)
		}
	}
	// A typo like "100h" must not park a provisioning run for days.
	t.Setenv(WaitBudgetEnv, "100h")
	if got := DefaultWaitBackoff().Budget(); got > MaxWaitBudget+30*time.Second {
		t.Errorf("budget=%v, want clamped to %v", got, MaxWaitBudget)
	}
}

// TestWaitUntilReady_ReadyOnFinalAttempt covers the success side of the
// deadline boundary: a VM that becomes ready on the very last allowed probe is
// a success, not a false "did not make it".
func TestWaitUntilReady_ReadyOnFinalAttempt(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 4}
	polls := 0
	probe := func(_ context.Context, _ string) ProbeResult {
		polls++
		return ProbeResult{Ready: polls >= cfg.MaxAttempts, State: "creating"}
	}
	res, err := WaitUntilReady(context.Background(), cfg, []string{"i-1"}, probe, nil)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if polls != cfg.MaxAttempts {
		t.Errorf("polls=%d, want %d (the last allowed attempt must still be probed)", polls, cfg.MaxAttempts)
	}
	if !res[0].Ready {
		t.Errorf("res=%+v, want ready", res[0])
	}
}

// TestWaitUntilReady_DeadlineDiagnostics_StateNeverChanged: a VM whose observed
// state never moves must NOT be diagnosed as stuck. "creating" spans the whole
// boot, so a VM slower than the budget never leaves it — the message that ruled
// out a larger budget sent an operator hunting a broken VM that was booting
// normally (NIM-787, found live on soul-cloud-wb where a boot takes 60-90s).
func TestWaitUntilReady_DeadlineDiagnostics_StateNeverChanged(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	probe := func(_ context.Context, vmID string) ProbeResult {
		if vmID == "i-ok" {
			return ProbeResult{Ready: true, State: "running"}
		}
		return ProbeResult{State: "creating"}
	}
	_, err := WaitUntilReady(context.Background(), cfg, []string{"i-ok", "i-creating"}, probe, nil)
	if !errors.Is(err, ErrWaitDeadline) {
		t.Fatalf("err=%v, want ErrWaitDeadline", err)
	}
	var de *WaitDeadlineError
	if !errors.As(err, &de) {
		t.Fatalf("err=%T, want *WaitDeadlineError", err)
	}
	if de.Ready != 1 || de.Total != 2 || de.Attempts != cfg.MaxAttempts {
		t.Errorf("ready=%d total=%d attempts=%d, want 1/2 after %d attempts", de.Ready, de.Total, de.Attempts, cfg.MaxAttempts)
	}
	if de.Elapsed <= 0 {
		t.Error("Elapsed must report how long the wait actually ran")
	}
	if len(de.Pending) != 1 || de.Pending[0].VMID != "i-creating" {
		t.Fatalf("pending=%+v, want only i-creating", de.Pending)
	}
	if de.Pending[0].LastState != "creating" || de.Pending[0].Progressed {
		t.Errorf("pending=%+v, want last state creating and no progress", de.Pending[0])
	}
	msg := err.Error()
	if !strings.Contains(msg, "i-creating") || !strings.Contains(msg, "state=creating") {
		t.Errorf("message=%q, want it to name the pending VM and its last state", msg)
	}
	wantDiagnosis(t, msg, "no state change was observed — a boot slower than the budget looks exactly like this, "+
		"so raise "+WaitBudgetEnv+" before treating the VM as stuck")
}

// TestWaitUntilReady_DeadlineDiagnostics_NoStateReported: a driver whose probe
// leaves [ProbeResult.State] empty gives the poller no progress signal at all.
// The diagnosis must say that, not report "state never changed" about a state
// nobody ever observed.
func TestWaitUntilReady_DeadlineDiagnostics_NoStateReported(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	probe := func(_ context.Context, _ string) ProbeResult { return ProbeResult{} }
	_, err := WaitUntilReady(context.Background(), cfg, []string{"i-mute"}, probe, nil)
	var de *WaitDeadlineError
	if !errors.As(err, &de) {
		t.Fatalf("err=%v (%T), want *WaitDeadlineError", err, err)
	}
	if len(de.Pending) != 1 || de.Pending[0].LastState != "" || de.Pending[0].Progressed {
		t.Fatalf("pending=%+v, want i-mute with no state and no progress", de.Pending)
	}
	wantDiagnosis(t, err.Error(), "the driver reported no state — nothing observed here separates a slow boot "+
		"from a stuck VM, raise "+WaitBudgetEnv+" to tell them apart")
}

// TestWaitDeadlineError_VerdictHoldsForTheWholeSet: the branch is chosen by an
// OR over every pending VM, so a set whose FIRST VM is unrepresentative must
// still get a verdict true of all of them. This guards the selection itself —
// every single-VM case above passes just as well when only Pending[0] is read.
func TestWaitDeadlineError_VerdictHoldsForTheWholeSet(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	advancing := []string{"queued", "creating", "booting"}

	for _, tc := range []struct {
		name  string
		vmIDs []string
		want  string
	}{
		{
			// "the driver reported no state" would be false of i-creating.
			name:  "mute VM ahead of one whose state sat still",
			vmIDs: []string{"i-mute", "i-creating"},
			want: "no state change was observed — a boot slower than the budget looks exactly like this, " +
				"so raise " + WaitBudgetEnv + " before treating the VM as stuck",
		},
		{
			// The advancing VM behind the still one still sets the verdict.
			name:  "still VM ahead of an advancing one",
			vmIDs: []string{"i-creating", "i-advancing"},
			want:  "state was still advancing — the boot budget is likely too small, raise " + WaitBudgetEnv,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			polls := 0
			probe := func(_ context.Context, vmID string) ProbeResult {
				switch vmID {
				case "i-mute":
					return ProbeResult{}
				case "i-advancing":
					s := advancing[min(polls, len(advancing)-1)]
					polls++
					return ProbeResult{State: s}
				default:
					return ProbeResult{State: "creating"}
				}
			}
			_, err := WaitUntilReady(context.Background(), cfg, tc.vmIDs, probe, nil)
			var de *WaitDeadlineError
			if !errors.As(err, &de) {
				t.Fatalf("err=%v (%T), want *WaitDeadlineError", err, err)
			}
			if len(de.Pending) != len(tc.vmIDs) || de.Pending[0].VMID != tc.vmIDs[0] {
				t.Fatalf("pending=%+v, want %v in order", de.Pending, tc.vmIDs)
			}
			wantDiagnosis(t, err.Error(), tc.want)
		})
	}
}

// diagnosisTail returns the verdict a deadline message ends with — the sentence
// an operator acts on, as opposed to the counters and vm-id list before it.
func diagnosisTail(msg string) string {
	i := strings.LastIndex(msg, "; ")
	if i < 0 {
		return msg
	}
	return msg[i+2:]
}

// wantDiagnosis pins that verdict exactly. Golden on purpose: the sentence is
// itself the deliverable of NIM-787, and a checklist of forbidden phrases is not
// a guard — "raising SOUL_CLOUD_WAIT_BUDGET is pointless" says the same wrong
// thing in words no blacklist anticipates. Rewording is meant to fail here, so
// that changing an operator-facing verdict is a decision someone makes rather
// than a side effect. When updating a want, check the new sentence claims
// nothing the poller cannot observe: what a state string means is the driver's
// vocabulary, so from an unchanged state the SDK cannot rule out a slow boot.
func wantDiagnosis(t *testing.T, msg, want string) {
	t.Helper()
	if got := diagnosisTail(msg); got != want {
		t.Errorf("diagnosis:\n got %q\nwant %q", got, want)
	}
}

// TestWaitUntilReady_DeadlineDiagnostics_Progressing: a VM still moving between
// states when the budget runs out is diagnosed as too-slow-for-the-budget, and
// the message points at the knob that fixes it.
func TestWaitUntilReady_DeadlineDiagnostics_Progressing(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	states := []string{"queued", "creating", "booting"}
	polls := 0
	probe := func(_ context.Context, _ string) ProbeResult {
		s := states[min(polls, len(states)-1)]
		polls++
		return ProbeResult{State: s}
	}
	_, err := WaitUntilReady(context.Background(), cfg, []string{"i-slow"}, probe, nil)
	var de *WaitDeadlineError
	if !errors.As(err, &de) {
		t.Fatalf("err=%v (%T), want *WaitDeadlineError", err, err)
	}
	if len(de.Pending) != 1 || !de.Pending[0].Progressed || de.Pending[0].LastState != "booting" {
		t.Fatalf("pending=%+v, want i-slow progressed with last state booting", de.Pending)
	}
	wantDiagnosis(t, err.Error(), "state was still advancing — the boot budget is likely too small, raise "+WaitBudgetEnv)
}

// TestWaitDeadlineError_CapsPendingList: a mass provision must not print a wall
// of ids into the failed event — the message names a few and counts the rest,
// while Pending keeps every VM for the caller's anti-orphan handling.
func TestWaitDeadlineError_CapsPendingList(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 2}
	ids := make([]string, 0, 12)
	for i := range 12 {
		ids = append(ids, "i-"+strconv.Itoa(i))
	}
	probe := func(_ context.Context, _ string) ProbeResult { return ProbeResult{State: "creating"} }
	_, err := WaitUntilReady(context.Background(), cfg, ids, probe, nil)
	var de *WaitDeadlineError
	if !errors.As(err, &de) {
		t.Fatalf("err=%v, want *WaitDeadlineError", err)
	}
	if len(de.Pending) != len(ids) {
		t.Errorf("Pending len=%d, want all %d VMs", len(de.Pending), len(ids))
	}
	if msg := err.Error(); !strings.Contains(msg, "+7 more") {
		t.Errorf("message=%q, want the tail summarised as \"+7 more\"", msg)
	}
}

// TestWaitUntilReady_ProgressReportsBudget: the per-round progress line carries
// attempt/budget context, so a slow boot is visible in the Create event stream
// before it turns into a failure.
func TestWaitUntilReady_ProgressReportsBudget(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	var msgs []string
	probe := func(_ context.Context, _ string) ProbeResult { return ProbeResult{State: "creating"} }
	_, _ = WaitUntilReady(context.Background(), cfg, []string{"i-1"}, probe, func(m string) { msgs = append(msgs, m) })
	if len(msgs) == 0 {
		t.Fatal("no progress messages")
	}
	if !strings.Contains(msgs[0], "0/1 ready") || !strings.Contains(msgs[0], "/3") {
		t.Errorf("progress=%q, want ready counter and attempt/MaxAttempts", msgs[0])
	}
}
