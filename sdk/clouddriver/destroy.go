package clouddriver

import (
	"context"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// GoneResult is the outcome of a single teardown poll by [ConfirmDestroy].
type GoneResult struct {
	// Gone means the VM is no longer there — the provider reports it missing
	// (a not-found read is the usual signal). The poller stops polling it and
	// the destroy counts as confirmed.
	Gone bool
	// Err is a terminal poll error (no permission to read the VM, a
	// deterministic provider failure). The poller stops polling this VM and
	// reports the error. Transient read errors must NOT go here — swallow
	// them (Gone=false, Err=nil) and the poller retries.
	Err error
	// State is the provider state observed by this poll ("DELETING",
	// "DELETE_FAILED", …), optional but recommended: it lands in the
	// diagnostics when the budget runs out.
	State string
	// DeleteFailed means the provider actively refused or failed the deletion
	// (e.g. WB parks such a VM in DELETE_FAILED) rather than still working on
	// it. A VM in that state never leaves it on its own, so the poller issues
	// the delete again. Leave false while a deletion is merely in flight —
	// re-issuing then only spams the API.
	DeleteFailed bool
}

// GoneProbe is a per-provider "is this VM actually gone" predicate — the only
// thing a driver writes itself for the teardown phase; the
// delete/poll/re-issue/ctx-cancel loop is handled by the SDK. The driver reads
// the VM (DescribeInstances/DetailVm/...) and returns a [GoneResult]; a
// not-found read means Gone.
type GoneProbe func(ctx context.Context, vmID string) GoneResult

// DeleteFunc issues the provider's delete call for one VM. [ConfirmDestroy]
// calls it once up front and again whenever the probe reports the deletion
// failed. Drivers usually wrap their API call in [Retry] here, so transient
// API errors are absorbed before the poller sees them.
type DeleteFunc func(ctx context.Context, vmID string) error

// DestroyResult is the [ConfirmDestroy] outcome for a single VM.
type DestroyResult struct {
	VMID string
	// Gone means the deletion was CONFIRMED by a poll — not merely accepted by
	// the provider.
	Gone bool
	// Err is the terminal probe error or, for a VM that never disappeared, the
	// last delete error; nil when Gone.
	Err error
}

// ConfirmDestroy deletes every vmID and polls until the provider confirms each
// one is actually gone.
//
// Why this exists: provider delete calls are asynchronous — a successful RPC
// means "accepted", not "deleted". A driver that reports success right after
// the call can leave a VM alive (WB parks a VM deleted mid-create in
// DELETE_FAILED), while Keeper's cascade already marked the soul destroyed —
// a silent registry/cloud divergence, and precisely a failure of the
// anti-orphan cleanup, since the VMs it exists to remove are the ones caught
// mid-create.
//
// The loop is delete → poll → re-delete-if-refused, bounded by cfg (reuse the
// wait budget, [DefaultWaitBackoff]: teardown of a VM stuck mid-create has to
// wait out the creation first). progress is an optional per-round callback;
// nil is fine.
//
// Anti-orphan, symmetric with [WaitUntilReady]: on ctx-cancel or an exhausted
// budget the per-VM results still carry every vm_id, so the caller can report
// what it could not confirm instead of losing track of it.
func ConfirmDestroy(ctx context.Context, cfg BackoffConfig, vmIDs []string, del DeleteFunc, probe GoneProbe, progress func(string)) ([]DestroyResult, error) {
	results := make([]DestroyResult, len(vmIDs))
	for i, id := range vmIDs {
		results[i].VMID = id
	}
	if len(vmIDs) == 0 {
		return results, nil
	}

	pending := make(map[int]struct{}, len(vmIDs))
	for i := range vmIDs {
		pending[i] = struct{}{}
	}

	// Issue the first delete for everything, then confirm.
	delErr := make([]error, len(vmIDs))
	for i, id := range vmIDs {
		delErr[i] = del(ctx, id)
	}

	started := time.Now()
	seen := make([]stateTrack, len(vmIDs))
	rejected := make([]bool, len(vmIDs))

	attempt := 0
	for len(pending) > 0 {
		reissue := make([]int, 0, len(pending))
		for i := range pending {
			res := probe(ctx, vmIDs[i])
			seen[i].observe(res.State)
			rejected[i] = res.DeleteFailed
			switch {
			case res.Gone:
				results[i].Gone = true
				results[i].Err = nil
				delete(pending, i)
			case res.Err != nil:
				results[i].Err = res.Err
				delete(pending, i)
			case res.DeleteFailed:
				// The provider refused the deletion; it will not retry itself.
				reissue = append(reissue, i)
			}
		}
		if len(pending) == 0 {
			break
		}
		attempt++
		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			for i := range pending {
				if results[i].Err == nil {
					results[i].Err = delErr[i]
				}
			}
			return results, newDestroyDeadlineError(vmIDs, results, pending, seen, rejected, attempt, time.Since(started))
		}
		if progress != nil {
			progress(destroyProgressMsg(len(pending), len(vmIDs), attempt, cfg.MaxAttempts, time.Since(started)))
		}
		if err := sleepCtx(ctx, cfg.next(attempt-1)); err != nil {
			for i := range pending {
				if results[i].Err == nil {
					results[i].Err = delErr[i]
				}
			}
			return results, err
		}
		for _, i := range reissue {
			delErr[i] = del(ctx, vmIDs[i])
		}
	}
	return results, nil
}

// ErrDestroyDeadline means [ConfirmDestroy] ran out of budget before every VM
// was confirmed gone. Matched with errors.Is; the concrete error carries the
// diagnostics (see [DestroyDeadlineError]).
var ErrDestroyDeadline = destroyDeadlineError{}

type destroyDeadlineError struct{}

func (destroyDeadlineError) Error() string { return "destroy: max attempts exhausted" }

// PresentVM is a VM that was still there when the destroy budget ran out.
type PresentVM struct {
	VMID string
	// LastState is the provider state last reported for it ("" when the
	// driver's probe doesn't fill [GoneResult.State]).
	LastState string
	// DeleteRejected means the provider kept failing the deletion rather than
	// still working on it — more budget will not help, the VM needs attention.
	DeleteRejected bool
}

// DestroyDeadlineError is the [ConfirmDestroy] outcome when the budget runs
// out: it names the VMs still present and whether the provider was refusing
// the deletion, so an operator can tell "teardown is slow" from "these VMs are
// stuck and still being billed". Matches errors.Is(err, [ErrDestroyDeadline]).
type DestroyDeadlineError struct {
	// Attempts is how many poll rounds ran, Elapsed how long they took.
	Attempts int
	Elapsed  time.Duration
	// Gone of Total VMs were confirmed destroyed.
	Gone  int
	Total int
	// Pending lists the VMs that were not (ordered as passed to the poller).
	Pending []PresentVM
}

func (e *DestroyDeadlineError) Is(target error) bool { return target == ErrDestroyDeadline }

func (e *DestroyDeadlineError) Error() string {
	var b strings.Builder
	b.WriteString("destroy budget exhausted after ")
	b.WriteString(e.Elapsed.Round(time.Second).String())
	b.WriteString(" (" + strconv.Itoa(e.Attempts) + " attempts): ")
	b.WriteString(strconv.Itoa(e.Gone) + "/" + strconv.Itoa(e.Total) + " gone; still present: ")
	anyRejected := false
	for _, p := range e.Pending {
		anyRejected = anyRejected || p.DeleteRejected
	}
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
	if anyRejected {
		b.WriteString("; the provider kept refusing the deletion — these VMs are still alive and billed, " +
			"a larger budget will not help (a VM deleted mid-create has to finish creating first)")
	} else {
		b.WriteString("; teardown was still in flight — raise " + WaitBudgetEnv + " if the provider is simply slow")
	}
	return b.String()
}

func newDestroyDeadlineError(vmIDs []string, results []DestroyResult, pending map[int]struct{}, seen []stateTrack, rejected []bool, attempts int, elapsed time.Duration) *DestroyDeadlineError {
	e := &DestroyDeadlineError{Attempts: attempts, Elapsed: elapsed, Total: len(vmIDs)}
	for i := range vmIDs {
		if results[i].Gone {
			e.Gone++
			continue
		}
		// VMs closed by a terminal probe error are reported per-VM in results.
		if _, still := pending[i]; still {
			e.Pending = append(e.Pending, PresentVM{
				VMID:           vmIDs[i],
				LastState:      seen[i].last,
				DeleteRejected: rejected[i],
			})
		}
	}
	return e
}

func destroyProgressMsg(pending, total, attempt, maxAttempts int, elapsed time.Duration) string {
	msg := "confirm-destroy: " +
		strconv.Itoa(total-pending) + "/" + strconv.Itoa(total) + " gone (attempt " + strconv.Itoa(attempt)
	if maxAttempts > 0 {
		msg += "/" + strconv.Itoa(maxAttempts)
	}
	return msg + ", elapsed " + elapsed.Round(time.Second).String() + ")"
}

// ReportDestroy turns [ConfirmDestroy] results into the driver's DestroyEvent
// stream, so every provider reports teardown the same way instead of inventing
// its own dialect.
//
// Per VM: confirmed gone → "destroyed"; a terminal error → failed with the
// classified message; still present without an error → failed with an explicit
// "not confirmed" (never a silent success). When the whole phase ended on a
// budget/ctx failure, a final event carries that diagnosis. Returns the first
// stream error, if any.
func ReportDestroy(stream grpc.ServerStreamingServer[pluginv1.DestroyEvent], results []DestroyResult, phaseErr error, classify ClassifyFunc) error {
	var firstErr error
	send := func(e *pluginv1.DestroyEvent) {
		if err := stream.Send(e); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, r := range results {
		switch {
		case r.Gone:
			send(&pluginv1.DestroyEvent{VmId: r.VMID, Message: "destroyed"})
		case r.Err != nil:
			send(&pluginv1.DestroyEvent{
				VmId:    r.VMID,
				Message: FailMessage(Classify(classify, r.Err), "destroy", r.Err),
				Failed:  true,
			})
		default:
			send(&pluginv1.DestroyEvent{
				VmId:    r.VMID,
				Message: "destroy not confirmed: the VM is still present after the delete was accepted",
				Failed:  true,
			})
		}
	}
	if phaseErr != nil {
		send(&pluginv1.DestroyEvent{
			Message: FailMessage(Classify(classify, phaseErr), "confirm-destroy", phaseErr),
			Failed:  true,
		})
	}
	return firstErr
}
