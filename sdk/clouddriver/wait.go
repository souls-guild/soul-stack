package clouddriver

import (
	"context"
	"strconv"
)

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

	attempt := 0
	for len(pending) > 0 {
		for i := range pending {
			res := probe(ctx, vmIDs[i])
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
		if progress != nil {
			progress(waitProgressMsg(len(pending), len(vmIDs), attempt))
		}
		// ctx-aware wait for the next round; cancellation → anti-orphan return.
		if err := sleepCtx(ctx, cfg.next(attempt)); err != nil {
			return results, err
		}
		attempt++
		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			return results, ErrWaitDeadline
		}
	}
	return results, nil
}

// ErrWaitDeadline means the wait poller exhausted MaxAttempts without all
// VMs reaching readiness. Symmetric with ctx.DeadlineExceeded, but without
// depending on a deadline being present in ctx (the limit is set by attempt
// count).
var ErrWaitDeadline = waitDeadlineError{}

type waitDeadlineError struct{}

func (waitDeadlineError) Error() string { return "wait-until-ready: max attempts exhausted" }

func waitProgressMsg(pending, total, attempt int) string {
	return "wait-until-ready: " +
		strconv.Itoa(total-pending) + "/" + strconv.Itoa(total) + " ready (attempt " + strconv.Itoa(attempt+1) + ")"
}
