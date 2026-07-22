package herald

// DeliveryWorker is the claim-queue worker for real webhook delivery (ADR-052(d),
// S3). at-least-once: it claims a job from the Redis queue, resolves the Herald
// channel, performs an SSRF-guarded webhook POST, retries with backoff on failure,
// and emits audit `herald.delivered`/`herald.failed` at terminal state.
//
// Concurrent workers (several per instance plus N instances) are safe: claim is
// atomic (BRPOPLPUSH), duplicate delivery is acceptable (at-least-once). The lease
// key plus mini-reaper ([RequeueExpired]) return jobs orphaned after a crash.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/netguard"
)

// defaultRetryBackoff is the delay before redelivery by attempt number
// (ADR-052(d): retry with backoff). Slice length is the number of attempts after
// the first one (total 1 + len = 4 attempts). The first retry is quick because a
// flickering endpoint often recovers within seconds; the exponential tail covers
// short receiver downtime (restart/deploy) without a retry storm.
//
//	attempt 0 -> immediately (first delivery)
//	attempt 1 -> +5s
//	attempt 2 -> +30s
//	attempt 3 -> +2m   (last attempt; then terminal fail)
var defaultRetryBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// claimBlockTimeout is the blocking claim timeout (BRPOPLPUSH). On expiry, worker
// checks ctx.Done and starts a new claim. It is finite, not 0, so shutdown is
// observed even with an empty queue.
const claimBlockTimeout = 2 * time.Second

// leaseTTL is the TTL of the claimed job lease key. It must cover the longest
// delivery (delivery timeout + backoff sleep for the current attempt) with margin,
// otherwise mini-reaper may take a still-processing job and create a duplicate. It
// is the maximum backoff plus a generous margin for the HTTP POST itself.
const leaseTTL = 5 * time.Minute

// leaseRenewInterval is the lease-key renewal period while a job is processed.
// backoff sleep + POST must not outlive leaseTTL, so we renew.
const leaseRenewInterval = leaseTTL / 3

// DeliveryWorker is one claim loop. daemon starts several per instance.
type DeliveryWorker struct {
	Queue    queueBackend
	Heralds  HeraldReader
	KV       KVReader
	Audit    audit.Writer
	Logger   *slog.Logger
	Metrics  *DeliveryMetrics
	Resolver netguard.Resolver
	// Timeout is the overall timeout for one webhook POST (0 uses DefaultDeliveryTimeout).
	Timeout time.Duration
	// Backoff is the delay between attempts (nil uses defaultRetryBackoff). It is
	// injected for tests (fast delays) and future config; len defines the number of
	// retries, total attempts = 1 + len(Backoff).
	Backoff []time.Duration
}

// backoff returns the effective backoff slice (nil means default).
func (w *DeliveryWorker) backoff() []time.Duration {
	if w.Backoff == nil {
		return defaultRetryBackoff
	}
	return w.Backoff
}

// retryMax is the maximum number of attempts (1 first + len(backoff) retries).
// attempt+1 >= retryMax means terminal fail without requeue.
func (w *DeliveryWorker) retryMax() int {
	return 1 + len(w.backoff())
}

func (w *DeliveryWorker) validate() error {
	if w.Queue == nil {
		return errors.New("herald: DeliveryWorker.Queue is required")
	}
	if w.Heralds == nil {
		return errors.New("herald: DeliveryWorker.Heralds is required")
	}
	if w.Logger == nil {
		return errors.New("herald: DeliveryWorker.Logger is required")
	}
	if w.Resolver == nil {
		w.Resolver = netguard.DefaultResolver
	}
	return nil
}

// Run loops claims until ctx is cancelled. Returning on ctx.Done without an error
// is graceful shutdown. invalid-config returns an error as a wire-up bug.
func (w *DeliveryWorker) Run(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		claimed, err := w.Queue.Claim(ctx, claimBlockTimeout)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			w.Logger.Warn("herald: claim failed", slog.Any("error", err))
			if !sleepCtx(ctx, time.Second) {
				return nil
			}
			continue
		}
		if claimed == nil {
			continue // Empty queue: claim again.
		}
		w.handle(ctx, claimed.Payload)
	}
}

// handle processes one claimed job: parse, lease renewal, delivery, retry or
// terminal. Bad payload is dropped (Ack without requeue): only marshalJob could
// put it there, so do not loop the anomaly.
func (w *DeliveryWorker) handle(ctx context.Context, payload []byte) {
	job, err := unmarshalJob(payload)
	if err != nil {
		w.Logger.Warn("herald: dropping unparsable delivery job", slog.Any("error", err))
		_ = w.Queue.Ack(ctx, "", payload)
		return
	}

	// Lease renewal while processing the job (backoff sleep + POST). Stop renewal
	// before terminal handling: stopRenew cancels renewCtx and waits for the
	// goroutine to exit, so an in-flight SetLease cannot recreate the lease key
	// after Ack-DEL (stray key until TTL expiry). Cancel without synchronization
	// does not guarantee this; the goroutine may have passed select and entered
	// SetLease already.
	renewCtx, cancelRenew := context.WithCancel(ctx)
	if err := w.Queue.SetLease(renewCtx, job.ID, leaseTTL); err != nil {
		w.Logger.Warn("herald: set lease failed", slog.String("job_id", job.ID), slog.Any("error", err))
	}
	renewDone := make(chan struct{})
	go w.renewLease(renewCtx, job.ID, renewDone)
	stopRenew := func() {
		cancelRenew()
		<-renewDone
	}
	defer stopRenew()

	// Backoff before attempt>0. Requeue puts the job back immediately; keep the
	// delay here to avoid delay queues. Interruptible by ctx.
	bo := w.backoff()
	if job.Attempt > 0 && job.Attempt-1 < len(bo) {
		if !sleepCtx(ctx, bo[job.Attempt-1]) {
			return // Shutdown: job stays in processing; mini-reaper will return it.
		}
	}

	w.Metrics.observeAttempt(job.Herald)
	statusCode, derr := w.deliver(ctx, job)
	if derr == nil {
		stopRenew() // Renewal stops before Ack-DEL to avoid a stray lease key.
		w.terminalDelivered(ctx, job, statusCode, payload)
		return
	}

	// Delivery failure. Terminal without retry: stable error (channel deleted or
	// disabled, SSRF guard rejected URL, bad payload), so retry cannot help.
	// Otherwise retry with backoff until retryMax is exhausted.
	if isTerminalNoRetry(derr) || job.Attempt+1 >= w.retryMax() {
		stopRenew() // Renewal stops before Ack-DEL to avoid a stray lease key.
		w.terminalFailed(ctx, job, derr, payload)
		return
	}
	w.requeue(ctx, job, payload, derr)
}

// deliver delivers one job. It returns (statusCode, nil) on success; otherwise an
// error (SSRF guard / transport / non-2xx), and caller decides retry vs terminal.
// For email, statusCode is always 0 because SMTP has no HTTP status.
//
// Two-class model (ADR-052 amendment): channel resolution and Enabled check are
// shared. Then dispatch by class: email -> [deliverEmail] (own SMTP branch, own
// SSRF guard/transport); otherwise HTTP class -> [resolveDelivery] builds
// httpDelivery, while deliver itself calls the single SSRF guard
// (validateDeliveryEndpoint + guardedDeliveryClient) and client.Do. A new HTTP
// type cannot bypass the guard by construction. non-2xx classification
// (isTerminalStatus) is shared for HTTP.
func (w *DeliveryWorker) deliver(ctx context.Context, job *DeliveryJob) (int, error) {
	h, err := w.Heralds.HeraldByName(ctx, job.Herald)
	if err != nil {
		if errors.Is(err, ErrHeraldNotFound) {
			// Channel was deleted between enqueue and delivery: terminal fail because
			// there is nowhere to retry. Return a no-retry error; otherwise handle
			// would exhaust attempts and hit not-found each time.
			return 0, fmt.Errorf("%w", errTerminalNoRetry{err})
		}
		return 0, fmt.Errorf("herald: resolve channel: %w", err)
	}
	if !h.Enabled {
		// Channel disabled: do not deliver (terminal, no retry).
		return 0, errTerminalNoRetry{fmt.Errorf("herald: channel %q disabled", h.Name)}
	}

	// SMTP class has its own branch (own net/smtp transport + own SSRF guard by
	// resolved IP). No httpDelivery/HTTP client.
	if h.Type == HeraldEmail {
		if err := deliverEmail(ctx, h, job, w.KV, w.Resolver); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// HTTP class: driver builds httpDelivery, guard+dial are shared.
	hd, err := resolveDelivery(ctx, h, job, w.KV)
	if err != nil {
		// Driver already classified the error (terminal-no-retry for bad config /
		// transient for Vault failure), so pass it through.
		return 0, err
	}

	// SSRF URL validation before the request (config may have changed after create):
	// the single point for all HTTP types.
	if err := validateDeliveryEndpoint(hd.url, hd.httpAllowed, hd.allowPrivate); err != nil {
		// Guard rejected it: stable config error, retry is pointless.
		return 0, errTerminalNoRetry{err}
	}

	req, err := buildHTTPRequest(ctx, hd)
	if err != nil {
		return 0, errTerminalNoRetry{fmt.Errorf("herald: build request: %w", err)}
	}

	client := guardedDeliveryClient(w.Resolver, hd.allowPrivate, w.Timeout)
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("herald: delivery %s: %w", req.Method, err)
	}
	defer resp.Body.Close()
	// Drain the body with a limit so the keep-alive connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("herald: delivery returned status %d", resp.StatusCode)
		if isTerminalStatus(resp.StatusCode) {
			// 4xx except 408/429 is a stable client error (auth/route/payload):
			// retry cannot help, terminal without retry.
			return resp.StatusCode, errTerminalNoRetry{err}
		}
		// 408/429/5xx is transient (rate limit / overload / receiver restart):
		// retry with backoff.
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// isTerminalStatus reports that an HTTP status is a stable error where retry makes
// no sense: any 4xx except 408 Request Timeout and 429 Too Many Requests. Those
// two are transient and retry with 5xx/timeouts/transport failures.
func isTerminalStatus(code int) bool {
	if code < 400 || code >= 500 {
		return false
	}
	return code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// buildPayload builds the webhook POST JSON body. Format is fixed by
// ADR-052(d)/(h)/(i):
//
//	{
//	  "event_type":   "<area>.<action>",
//	  "occurred_at":  "<RFC3339>",
//	  "herald":       "<channel-name>",
//	  "tiding":       "<rule-name>",
//	  "payload":      { ... },  // audit-event payload, optionally narrowed by projection
//	  "annotations":  { ... }   // optional operator static fields, omitted when empty
//	}
//
// No secret enrichment: payload is a copy of the already masked audit payload
// (invariant A ADR-027); run MaskSecrets again before sending as defence in depth.
// Even if a job is constructed without audit masking, no secret leaves the system.
//
// Order (ADR-052(h)): MaskSecrets -> projection -> annotations. projection applies
// to the already masked payload, so the allow-list cannot extract a field removed
// by masking and secret hygiene is preserved. annotations are operator static
// fields and merge as a separate top-level key, not into payload. Signature, when
// signingKey is set, is calculated by caller ([deliver]) over this final body
// after projection+annotations.
func buildPayload(job *DeliveryJob) ([]byte, error) {
	payload := audit.MaskSecrets(job.PayloadCopy)
	if len(job.Projection) > 0 {
		payload = projectPayload(payload, job.Projection)
	}
	out := webhookPayload{
		EventType:   string(job.EventType),
		OccurredAt:  job.OccurredAt.UTC().Format(time.RFC3339),
		Herald:      job.Herald,
		Tiding:      job.Tiding,
		Payload:     payload,
		Annotations: job.Annotations,
	}
	return marshalWebhookPayload(out)
}

// projectPayload builds a subset of src by the projection path allow-list
// (ADR-052(h)). Paths use dotted notation (`summary.succeeded`, `voyage_id`);
// syntax was already validated at CRUD ([ValidateProjection]), so this only
// resolves against the actual payload shape.
//
// Result shape is nested: path `summary.succeeded` -> {"summary": {"succeeded": N}}.
// The receiver can parse projected payload with the same code as the full shape:
// the same key contract, only fewer fields. A flat shape ("summary.succeeded": N)
// would introduce a second incompatible format for the same field and break the
// receiver when switching a rule between projection and full.
//
// Missing path is skipped, not error or null: the operator subscribed to a field
// absent from this concrete event, which is normal because payload forms vary by
// EventType. If all paths miss, an empty object is returned.
func projectPayload(src map[string]any, paths []string) map[string]any {
	out := map[string]any{}
	for _, path := range paths {
		val, ok := resolvePath(src, strings.Split(path, "."))
		if !ok {
			continue // Path is absent in this payload: skip.
		}
		// Deep-copy the leaf before insertion. Otherwise, with prefix collision
		// (`summary` + `summary.failed`), the broad path would place a reference
		// to a nested src map into out, and the later deep insertion would mutate
		// it, corrupting src. buildPayload is called again for every retry attempt
		// over the same job.PayloadCopy, so src must stay unchanged: projectPayload
		// never mutates src for any path set.
		insertPath(out, strings.Split(path, "."), deepCopyValue(val))
	}
	return out
}

// deepCopyValue recursively copies a value that enters projection, so out shares
// no nested map/slice with src. Scalar leaves (string/number/bool/nil) are
// immutable and returned as-is. It covers only forms that actually occur in audit
// payloads (nested map[string]any and []any); other leaf types are copied by value.
func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, el := range x {
			out[k] = deepCopyValue(el)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = deepCopyValue(el)
		}
		return out
	default:
		return v
	}
}

// resolvePath descends through segments in m one segment at a time. ok=false means
// an intermediate segment is absent or not an object (a leaf was reached before a
// deeper path). It resolves only through map[string]any: projection into array
// elements is not supported. Projection syntax allows only `[a-z0-9_]` segments,
// no indexes, see [ValidateProjection].
func resolvePath(m map[string]any, segments []string) (any, bool) {
	cur := any(m)
	for _, seg := range segments {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// insertPath restores a nested structure for segments in dst, placing val at the
// leaf. Intermediate objects are created as needed; collision (a leaf from a
// shorter projection path already lies on the path) is resolved in favor of the
// deeper insertion. Projection paths should not be prefixes of each other in
// practice, but deterministic behavior is guaranteed.
func insertPath(dst map[string]any, segments []string, val any) {
	for i := 0; i < len(segments)-1; i++ {
		seg := segments[i]
		next, ok := dst[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			dst[seg] = next
		}
		dst = next
	}
	dst[segments[len(segments)-1]] = val
}

// renewLease extends the job lease key while renewCtx is alive and handle is
// processing the job. Best-effort: renewal errors are logged at debug and do not
// fail delivery. Worst case, mini-reaper takes a still-live job and delivers it
// again, which at-least-once permits. close(done) on exit is the sync point for
// handle: after it returns, no SetLease is still running, avoiding a stray lease
// key after terminal Ack-DEL.
func (w *DeliveryWorker) renewLease(ctx context.Context, jobID string, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(leaseRenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Queue.SetLease(ctx, jobID, leaseTTL); err != nil {
				if w.Logger != nil {
					w.Logger.Debug("herald: lease renew failed", slog.String("job_id", jobID), slog.Any("error", err))
				}
			}
		}
	}
}

// requeue returns the job for retry with incremented attempt. Backoff is applied
// at the next claim; handle sleeps before attempt>0.
func (w *DeliveryWorker) requeue(ctx context.Context, job *DeliveryJob, oldPayload []byte, cause error) {
	next := *job
	next.Attempt = job.Attempt + 1
	newPayload, err := marshalJob(&next)
	if err != nil {
		// Marshaling should not fail because the job is already parsed; fail
		// terminally just in case.
		w.terminalFailed(ctx, job, fmt.Errorf("requeue marshal: %w", err), oldPayload)
		return
	}
	if err := w.Queue.Requeue(ctx, job.ID, oldPayload, newPayload); err != nil {
		w.Logger.Warn("herald: requeue failed", slog.String("job_id", job.ID), slog.Any("error", err))
		return
	}
	w.Metrics.observeRetry(job.Herald)
	w.Logger.Info("herald: delivery failed, scheduled retry",
		slog.String("herald", job.Herald), slog.String("tiding", job.Tiding),
		slog.Int("attempt", next.Attempt), slog.String("error", maskErr(cause)))
}

// terminalDelivered handles successful delivery: Ack + audit herald.delivered + metric.
func (w *DeliveryWorker) terminalDelivered(ctx context.Context, job *DeliveryJob, statusCode int, payload []byte) {
	_ = w.Queue.Ack(ctx, job.ID, payload)
	w.Metrics.observeSucceeded(job.Herald)
	w.emitAudit(audit.EventHeraldDelivered, job, map[string]any{
		"herald":      job.Herald,
		"tiding":      job.Tiding,
		"event_type":  string(job.EventType),
		"attempt":     job.Attempt,
		"status_code": statusCode,
	})
	w.Logger.Info("herald: notification delivered",
		slog.String("herald", job.Herald), slog.String("tiding", job.Tiding),
		slog.Int("attempt", job.Attempt), slog.Int("status_code", statusCode))
}

// terminalFailed handles retry exhaustion / no-retry error: Ack + audit herald.failed.
func (w *DeliveryWorker) terminalFailed(ctx context.Context, job *DeliveryJob, cause error, payload []byte) {
	_ = w.Queue.Ack(ctx, job.ID, payload)
	w.Metrics.observeFailed(job.Herald)
	w.emitAudit(audit.EventHeraldFailed, job, map[string]any{
		"herald":        job.Herald,
		"tiding":        job.Tiding,
		"event_type":    string(job.EventType),
		"attempt":       job.Attempt,
		"error_message": maskErr(cause),
	})
	w.Logger.Warn("herald: notification delivery failed terminally",
		slog.String("herald", job.Herald), slog.String("tiding", job.Tiding),
		slog.Int("attempt", job.Attempt), slog.String("error", maskErr(cause)))
}

// emitAudit writes a terminal delivery event. Background ctx: emit outside
// claim-ctx, which may have been cancelled during shutdown. source=keeper_internal,
// archon_aid="" (NULL), correlation_id = run event correlation_id.
func (w *DeliveryWorker) emitAudit(et audit.EventType, job *DeliveryJob, payload map[string]any) {
	if w.Audit == nil {
		return
	}
	ev := &audit.Event{
		EventType:     et,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: job.CorrelationID,
		Payload:       payload,
	}
	if err := w.Audit.Write(context.Background(), ev); err != nil {
		w.Logger.Warn("herald: terminal audit write failed",
			slog.String("event_type", string(et)), slog.Any("error", err))
	}
}

// errTerminalNoRetry wraps an error where retry makes no sense (stable
// configuration/state: channel deleted/disabled, SSRF guard rejected URL, bad
// payload). handle recognizes it and forces terminal fail without retries.
type errTerminalNoRetry struct{ err error }

func (e errTerminalNoRetry) Error() string { return e.err.Error() }
func (e errTerminalNoRetry) Unwrap() error { return e.err }

func isTerminalNoRetry(err error) bool {
	var t errTerminalNoRetry
	return errors.As(err, &t)
}

// maskErr returns error text passed through MaskSecrets; cause can transitively
// carry a vault-ref in the message. audit.MaskSecrets works with payload maps, so
// wrap the string in a map and read it back.
func maskErr(err error) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"e": err.Error()})
	if s, ok := masked["e"].(string); ok {
		return s
	}
	return "<masked>"
}

// sleepCtx waits for d or ctx.Done. false means ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
