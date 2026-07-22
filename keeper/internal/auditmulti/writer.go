// Package auditmulti — multi-writer fan-out [audit.Writer] for the
// dual-write audit pipeline (ADR-022(f)).
//
// Write-policy architecture decision (M0.4.1b):
//
//   - primary — synchronous, source of truth (Postgres impl
//     `keeper/internal/auditpg`). A primary failure is returned to the
//     caller **before** secondary-writes start — inconsistent state
//     (recorded only in OTel) is not acceptable.
//   - secondaries — asynchronous, best-effort (OTel impl
//     `keeper/internal/auditotel` and any future back-ends). A secondary
//     failure is logged via [slog.Warn] and does NOT affect the return
//     value of Write.
//   - shutdown — graceful drain via [sync.WaitGroup] + bounded timeout
//     ([WithShutdownDrain], default 5s). The caller invokes [Writer.Close],
//     typically from the main shutdown hook in `cmd/keeper`.
//
// The event is passed to secondary-writes as a **deep copy** of the
// payload: otherwise concurrent reads of the Payload map from multiple
// goroutines would be a data race. Secret masking is delegated to each
// writer (each runs [audit.MaskSecrets] on its own side) — the duplicate
// work is intentional: centralizing masking here would leak storage-format
// knowledge into the multi-writer.
//
// Context detach. Secondary writers receive [context.WithoutCancel] of the
// caller ctx: the caller (HTTP handler / RPC handler) typically cancels
// its own context right after Write returns, which is unacceptable for
// best-effort async fan-out (it would cancel an in-flight OTel export and
// lose debug data). The caller is responsible for shutdown via
// [Writer.Close], **not** via ctx-cancel.
package auditmulti

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// defaultShutdownDrain — bound for waiting on inflight secondary-writes in
// [Writer.Close]. Chosen based on typical OTel export behavior
// (BatchSpanProcessor flush ~1-3s in common configs); 5s leaves headroom
// for exporter connection timeouts.
const defaultShutdownDrain = 5 * time.Second

// Writer — multi-writer fan-out satisfying [audit.Writer]. One instance
// per Keeper process; safe for concurrent use.
//
// Returning the concrete type (not `audit.Writer`) is intentional: the
// caller needs access to [Writer.Close] for graceful drain. This is the
// standard Go "return concrete, accept interface" pattern.
type Writer struct {
	primary     audit.Writer
	secondaries []audit.Writer
	logger      *slog.Logger

	drain time.Duration

	// mu guards the critical section "check closed → wg.Add → start
	// goroutine" in Write and its mirror section in Close. Without it,
	// a race window remains between `select <-w.closed` and `wg.Add(1)`
	// where Close could close the channel and return before the
	// secondary goroutine starts (ADR-022(f), review.C major-1).
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

// Option — configuration helper for [New].
type Option func(*Writer)

// WithShutdownDrain sets the maximum time to wait for pending secondaries
// in [Writer.Close]. Values ≤0 are ignored — drain stays at the 5s
// default. For "no-wait" semantics, use
// `WithShutdownDrain(time.Microsecond)` or call `Close(ctx)` with an
// already-cancelled context.
func WithShutdownDrain(d time.Duration) Option {
	return func(w *Writer) {
		if d > 0 {
			w.drain = d
		}
	}
}

// WithLogger sets the [slog.Logger] used for secondary-failure and
// shutdown-timeout warnings. Default is `slog.Default()`.
func WithLogger(logger *slog.Logger) Option {
	return func(w *Writer) {
		if logger != nil {
			w.logger = logger
		}
	}
}

// New wraps primary (sync) + secondaries (async fan-out) into a single
// [audit.Writer]. If `secondaries` is empty, behavior is identical to
// calling primary directly.
//
// **Fail-fast validation.** A `nil` primary or any `nil` among
// secondaries is a configuration error, surfaced immediately at boot
// rather than on the first write (review.C/qa.C major-3). New panics
// with a specific message.
//
// Signature: `[]audit.Writer` (slice) instead of variadic, to avoid
// conflicting with variadic options. The caller builds the slice
// explicitly:
//
//	mw := auditmulti.New(pgWriter, []audit.Writer{otelWriter},
//	    auditmulti.WithShutdownDrain(10*time.Second))
func New(primary audit.Writer, secondaries []audit.Writer, opts ...Option) *Writer {
	if primary == nil {
		panic("auditmulti.New: primary writer is nil")
	}
	for i, sec := range secondaries {
		if sec == nil {
			panic(fmt.Sprintf("auditmulti.New: secondaries[%d] is nil", i))
		}
	}
	w := &Writer{
		primary:     primary,
		secondaries: secondaries,
		logger:      slog.Default(),
		drain:       defaultShutdownDrain,
		closed:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Write implements [audit.Writer]. See the package doc for policy.
//
// Behavior after [Writer.Close]: primary keeps working (graceful drain of
// final events before a clean shutdown), secondary fan-out is skipped.
func (w *Writer) Write(ctx context.Context, event *audit.Event) error {
	// Nil event — defensive guard. The shared/audit contract allows
	// validation at the writer level; here it matters more: otherwise
	// primary would get nil and behavior becomes impl-specific.
	if event == nil {
		return nil
	}

	// Primary — sync, no mutex needed: its own data race does not
	// violate multi-writer invariants.
	if err := w.primary.Write(ctx, event); err != nil {
		// Primary failure = the audit invariant is broken. Secondaries
		// are not started — otherwise OTel would have an event missing
		// from audit_log, and a reader couldn't cross-check it.
		return err
	}

	if len(w.secondaries) == 0 {
		return nil
	}

	// Critical section: check closed → wg.Add → start goroutine must be
	// atomic with respect to Close. Without the mutex, Close could close
	// the channel between our select and wg.Add(1), return success
	// (wg.Wait resolves instantly), and the secondary goroutine would
	// start after Close returned — breaking the "inflight completed"
	// contract.
	w.mu.Lock()
	defer w.mu.Unlock()

	select {
	case <-w.closed:
		return nil
	default:
	}

	// Detach from the caller ctx: the caller typically cancels ctx right
	// after Write returns (HTTP handler closing the request scope), but
	// our secondary-writes are best-effort and need to run to
	// completion. Shutdown goes through Close(), not cancel.
	detached := context.WithoutCancel(ctx)

	for _, sec := range w.secondaries {
		evCopy := cloneEvent(event)
		w.wg.Add(1)
		go func(s audit.Writer, ev *audit.Event, c context.Context) {
			defer w.wg.Done()
			if err := s.Write(c, ev); err != nil {
				w.logger.Warn(
					"audit secondary write failed",
					slog.String("event_type", string(ev.EventType)),
					slog.String("audit_id", ev.AuditID),
					slog.String("error", err.Error()),
				)
			}
		}(sec, evCopy, detached)
	}
	return nil
}

// Close waits for inflight secondary-writes to finish (or for the
// shutdown drain to expire — see [WithShutdownDrain]). Idempotent: repeat
// calls return nil immediately. After Close, new Writes keep going to
// primary, but secondary fan-out is no longer started (guards against
// utilizing stopped processors).
//
// If the drain expires, pending secondary goroutines **keep running
// asynchronously**. A caller owning secondary resources (e.g. an OTel
// TracerProvider) **must not** release them right after Close returns
// with a timeout — the typical pattern is to call your own
// `tracerProvider.Shutdown(ctx)` with its own bounded timeout after
// `multi.Close(ctx)`, which finalizes the batches.
//
// Returns:
//   - nil — all inflight writes finished within the drain.
//   - error("audit: secondary drain timeout") — drain expired.
//   - ctx.Err() — caller cancelled the context.
func (w *Writer) Close(ctx context.Context) error {
	// Close the channel under the mutex so any Write that already
	// acquired mu finishes its wg.Add first (giving Close something to
	// wait for), while the next Write sees the closed channel and skips
	// fan-out.
	w.once.Do(func() {
		w.mu.Lock()
		close(w.closed)
		w.mu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()

	deadline := time.NewTimer(w.drain)
	defer deadline.Stop()

	select {
	case <-done:
		return nil
	case <-deadline.C:
		w.logger.Warn(
			"audit multi-writer shutdown drain timed out",
			slog.Duration("max_wait", w.drain),
		)
		return errors.New("audit: secondary drain timeout")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cloneEvent makes a deep copy for safe handoff to a secondary goroutine.
// Payload is the only shared mutable field; the rest are value types
// (`time.Time`, strings, enum casts).
func cloneEvent(ev *audit.Event) *audit.Event {
	if ev == nil {
		return nil
	}
	out := *ev
	out.Payload = clonePayload(ev.Payload)
	return &out
}

func clonePayload(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneValue deep-copies only `map[string]any` and `[]any` — the
// canonical typed payload containers matching the [audit.MaskSecrets]
// contract in `shared/audit`. All other types (including
// `map[string]string`, `[]string`, structs, pointers) are copied
// shallow — the caller must build Payload from `map[string]any`+`[]any`,
// otherwise shared state between primary and secondary becomes possible.
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return clonePayload(x)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = cloneValue(el)
		}
		return out
	default:
		return v
	}
}
