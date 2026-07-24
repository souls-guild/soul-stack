package clouddriver

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// Wait-phase budget defaults. The wait phase (VM boot) and the API-retry phase
// (throttling on create/describe) have nothing in common except the backoff
// shape: a cloud API answers in seconds, a fresh VM boots minutes — more when a
// cluster topology brings up several at once. Sharing [DefaultBackoff] between
// the two made provisioning fail while the VMs were still booting normally.
const (
	// DefaultWaitBudget is the wall-clock time [DefaultWaitBackoff] polls
	// before giving up. Sized for a slow/loaded provider bringing up several
	// VMs at once, not for the fast path.
	DefaultWaitBudget = 10 * time.Minute

	// MaxWaitBudget caps [WaitBudgetEnv] so that a typo ("100h") cannot park a
	// provisioning run for days.
	MaxWaitBudget = 2 * time.Hour

	// WaitBudgetEnv overrides the wait budget for every driver in the process
	// (a Go duration, e.g. "20m"). Keeper passes its own environment to the
	// plugin, so this is set once on the Keeper unit. Invalid, zero or
	// negative values fall back to [DefaultWaitBudget].
	WaitBudgetEnv = "SOUL_CLOUD_WAIT_BUDGET"
)

// DefaultWaitBackoff is the backoff for the wait-until-ready phase: the
// [DefaultBackoff] shape (1s → 2s → … → 30s), but with enough attempts to
// cover [DefaultWaitBudget], overridable via [WaitBudgetEnv]. Drivers pass it
// to [WaitUntilReady]; [DefaultBackoff] stays for API retries, where a long
// budget would only prolong an outage.
func DefaultWaitBackoff() BackoffConfig { return WaitBackoffFor(waitBudgetFromEnv()) }

// WaitBackoffFor returns the [DefaultBackoff] shape sized to poll for at least
// budget (the minimal attempt count that covers it). For a driver whose VM boot
// time is known better than the default — e.g. read from the profile.
func WaitBackoffFor(budget time.Duration) BackoffConfig {
	cfg := DefaultBackoff()
	attempts, spent := 1, time.Duration(0)
	for spent < budget {
		spent += cfg.next(attempts - 1)
		attempts++
	}
	cfg.MaxAttempts = attempts
	return cfg
}

func waitBudgetFromEnv() time.Duration {
	raw := os.Getenv(WaitBudgetEnv)
	if raw == "" {
		return DefaultWaitBudget
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil || d <= 0:
		return DefaultWaitBudget
	case d > MaxWaitBudget:
		return MaxWaitBudget
	default:
		return d
	}
}

// ProbeResult is the outcome of a single VM readiness poll by the
// [WaitUntilReady] poller.
type ProbeResult struct {
	// Ready means the VM reached its target state (running + has an
	// IP/DNS): the poller stops polling it.
	Ready bool
	// Err is a terminal poll error (the VM went to error/terminated, or the
	// provider returned a deterministic failure). The poller stops polling
	// this VM and marks it failed. The driver must NOT return transient poll
	// errors here — it swallows them (returning Ready=false, Err=nil) and
	// the poller retries.
	Err error
	// State is the provider state observed by this poll ("creating",
	// "pending", …), optional but recommended: the poller tracks whether it
	// changes and reports "still booting" separately from "stuck" when the
	// budget runs out (see [WaitDeadlineError]).
	State string
}

// ReadyProbe is a per-provider readiness predicate for a single VM. The
// only thing a driver writes itself for the wait phase; the
// polling/backoff/ctx-cancel/anti-orphan loop is handled by the SDK. The
// driver polls the VM (DescribeInstances) and returns a [ProbeResult]. It
// doesn't need to honor ctx — the poller itself interrupts the wait between
// rounds based on ctx.
type ReadyProbe func(ctx context.Context, vmID string) ProbeResult

// WaitResult is the [WaitUntilReady] outcome for a single VM.
type WaitResult struct {
	VMID string
	// Ready means the VM reached readiness.
	Ready bool
	// Err is the terminal poll error, if any; nil for Ready and for VMs
	// interrupted by ctx (for those, Ready=false, Err=nil — distinguish by
	// WaitUntilReady's own return value = ctx.Err()).
	Err error
}

// WaitUntilReady polls all vmIDs with the probe predicate at backoff
// intervals until each VM becomes Ready or returns a terminal Err. progress
// is an optional diagnostic callback (message on each round); nil is fine.
//
// Anti-orphan (a reference technique for the lineup): on ctx-cancel/timeout
// the function does NOT throw everything away — it returns per-VM
// [WaitResult] for the VMs ALREADY polled (ready ones marked Ready=true,
// not-yet-ready ones Ready=false) + ctx.Err(). From this list the driver
// knows which VMs were created but didn't make it, and marks them failed
// with vm_id filled in — so Keeper can Destroy them (see the RunInstances
// flow in soul-cloud-aws). Without this, a ctx-cancel during the wait phase
// would leave Keeper with orphan VMs lacking a vm_id.
func WaitUntilReady(ctx context.Context, cfg BackoffConfig, vmIDs []string, probe ReadyProbe, progress func(string)) ([]WaitResult, error) {
	results := make([]WaitResult, len(vmIDs))
	for i, id := range vmIDs {
		results[i].VMID = id
	}

	pending := make(map[int]struct{}, len(vmIDs))
	for i := range vmIDs {
		pending[i] = struct{}{}
	}

	started := time.Now()
	seen := make([]stateTrack, len(vmIDs))

	attempt := 0
	for len(pending) > 0 {
		for i := range pending {
			res := probe(ctx, vmIDs[i])
			seen[i].observe(res.State)
			switch {
			case res.Err != nil:
				results[i].Err = res.Err
				delete(pending, i)
			case res.Ready:
				results[i].Ready = true
				delete(pending, i)
			}
		}
		if len(pending) == 0 {
			break
		}
		attempt++
		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			return results, newWaitDeadlineError(vmIDs, results, pending, seen, attempt, time.Since(started))
		}
		if progress != nil {
			progress(waitProgressMsg(len(pending), len(vmIDs), attempt, cfg.MaxAttempts, time.Since(started)))
		}
		// ctx-aware wait for the next round; cancellation → anti-orphan return.
		if err := sleepCtx(ctx, cfg.next(attempt-1)); err != nil {
			return results, err
		}
	}
	return results, nil
}

// stateTrack remembers the provider state a VM reported, to tell a VM that is
// merely booting slowly from one that never moved at all.
type stateTrack struct {
	last       string
	progressed bool
}

func (s *stateTrack) observe(state string) {
	if state == "" || state == s.last {
		return
	}
	if s.last != "" {
		s.progressed = true
	}
	s.last = state
}

// ErrWaitDeadline means the wait poller exhausted MaxAttempts without all VMs
// reaching readiness. Symmetric with ctx.DeadlineExceeded, but without
// depending on a deadline being present in ctx (the limit is set by attempt
// count). Matched with errors.Is; the concrete error carries the diagnostics
// (see [WaitDeadlineError]).
var ErrWaitDeadline = waitDeadlineError{}

type waitDeadlineError struct{}

func (waitDeadlineError) Error() string { return "wait-until-ready: max attempts exhausted" }

// PendingVM is a VM that had not reached readiness when the wait budget ran
// out.
type PendingVM struct {
	VMID string
	// LastState is the provider state last reported for it ("" when the
	// driver's probe doesn't fill [ProbeResult.State]).
	LastState string
	// Progressed means the state changed at least once during the wait: the
	// VM was coming up, just not fast enough for the budget. A VM that never
	// changed state is stuck — a larger budget will not save it.
	Progressed bool
}

// WaitDeadlineError is the [WaitUntilReady] outcome when the poll budget runs
// out: it names the VMs still pending, their last observed state and whether
// they were progressing, so an operator can tell "the boot budget is too
// small" from "this VM is never coming up". Matches errors.Is(err,
// [ErrWaitDeadline]).
type WaitDeadlineError struct {
	// Attempts is how many poll rounds ran, Elapsed how long they took.
	Attempts int
	Elapsed  time.Duration
	// Ready of Total VMs reached readiness before the budget ran out.
	Ready int
	Total int
	// Pending lists the VMs that did not (ordered as passed to the poller).
	Pending []PendingVM
}

// pendingInMessage caps how many pending VMs [WaitDeadlineError.Error] names
// before summarising the rest; the full list stays in Pending.
const pendingInMessage = 5

func (e *WaitDeadlineError) Is(target error) bool { return target == ErrWaitDeadline }

func (e *WaitDeadlineError) Error() string {
	var b strings.Builder
	// No "wait-until-ready:" prefix — drivers already pass that as the op to
	// [FailMessage], and a doubled phase name reads badly in the failed event.
	b.WriteString("wait budget exhausted after ")
	b.WriteString(e.Elapsed.Round(time.Second).String())
	b.WriteString(" (" + strconv.Itoa(e.Attempts) + " attempts): ")
	b.WriteString(strconv.Itoa(e.Ready) + "/" + strconv.Itoa(e.Total) + " ready; not ready: ")
	progressing := false
	for _, p := range e.Pending {
		progressing = progressing || p.Progressed
	}
	// A mass provision can leave dozens pending; name the first few and count
	// the rest rather than printing a wall of ids into the failed event.
	for i, p := range e.Pending {
		if i >= pendingInMessage {
			b.WriteString(", +" + strconv.Itoa(len(e.Pending)-i) + " more")
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.VMID)
		if p.LastState != "" {
			b.WriteString("(state=" + p.LastState + ")")
		}
	}
	if progressing {
		b.WriteString("; state was still advancing — the boot budget is likely too small, raise " + WaitBudgetEnv)
	} else {
		b.WriteString("; state never changed — the VM looks stuck, a larger budget will not help")
	}
	return b.String()
}

func newWaitDeadlineError(vmIDs []string, results []WaitResult, pending map[int]struct{}, seen []stateTrack, attempts int, elapsed time.Duration) *WaitDeadlineError {
	e := &WaitDeadlineError{Attempts: attempts, Elapsed: elapsed, Total: len(vmIDs)}
	for i := range vmIDs {
		if results[i].Ready {
			e.Ready++
			continue
		}
		// Terminal-error VMs are reported per-VM in results, not here.
		if _, still := pending[i]; still {
			e.Pending = append(e.Pending, PendingVM{VMID: vmIDs[i], LastState: seen[i].last, Progressed: seen[i].progressed})
		}
	}
	return e
}

func waitProgressMsg(pending, total, attempt, maxAttempts int, elapsed time.Duration) string {
	msg := "wait-until-ready: " +
		strconv.Itoa(total-pending) + "/" + strconv.Itoa(total) + " ready (attempt " + strconv.Itoa(attempt)
	if maxAttempts > 0 {
		msg += "/" + strconv.Itoa(maxAttempts)
	}
	return msg + ", elapsed " + elapsed.Round(time.Second).String() + ")"
}
