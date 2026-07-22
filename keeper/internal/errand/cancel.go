package errand

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// CancelRequest is the input to [Dispatcher.Cancel]. SID is not given —
// the dispatcher reads the errand row by ID and takes SID from there.
type CancelRequest struct {
	ErrandID    string
	RequestedBy string // archon AID of the initiator (for audit).
}

// Cancel is slice E5 ADR-033: cancel an in-flight Errand. Semantics are a
// best-effort signal: Keeper sends CancelErrand on the Soul's EventStream
// channel (local/remote by lease holder), the Soul-side errandrunner cancels
// the ctx of the active Run goroutine → it returns ErrandResult{status:
// CANCELLED} over the same EventStream, and the applybus-receiver
// (events_errand.go) transitions the errands row to status='cancelled' via
// MarkTerminal on its own.
//
// Cancel does NOT block and does NOT wait for ErrandResult: the HTTP handler
// returns 204 immediately. The operator sees the final status via
// GET /v1/errands/{id} (poll). If the Soul doesn't respond within the full
// TimeoutSec (300s max), purge_old_errands or sweep-on-restart move the row
// to timed_out via the usual path.
//
// Steps:
//  1. Lookup row by errand_id → 404 if none.
//  2. Check status='running' → 409 (ErrErrandTerminal) if already terminal.
//  3. Resolve holder lease, send CancelErrand local/remote.
//  4. Write audit `errand.cancelled` (event_type fixed, see event_types.go).
//
// Audit is written IMMEDIATELY on successful CancelErrand send — even if the
// Soul ends up ignoring it (race with its own completion). Intentional:
// audit records "the operator initiated cancel", not "the Soul actually
// cancelled". The final outcome shows up via GET
// (status=cancelled/success/failed/timed_out).
func (d *Dispatcher) Cancel(ctx context.Context, req CancelRequest) error {
	if req.ErrandID == "" {
		return ErrEmptyErrandID
	}

	row, err := d.deps.Store.Get(ctx, req.ErrandID)
	if err != nil {
		return fmt.Errorf("errand: cancel get: %w", err)
	}
	if row.Status != StatusRunning {
		// Terminal Errand — nothing to cancel. Idempotent: a duplicate cancel
		// of the same errand_id after a successful cancel also lands here
		// (status=cancelled); 409 is the correct response (not "ok, already cancelled").
		return fmt.Errorf("%w: status=%s", ErrErrandTerminal, row.Status)
	}

	if err := d.sendCancel(ctx, row.SID, row.ErrandID); err != nil {
		return err
	}

	d.writeCancelInitiated(ctx, row.ErrandID, row.SID, row.Module, req.RequestedBy)
	return nil
}

// sendCancel picks the CancelErrand delivery path: local
// (Outbound.SendCancelErrand) or remote (Publisher.PublishCancelErrand) by
// lease holder. Algorithm mirrors [Dispatcher.send] (for ErrandRequest);
// factored out separately so Cancel doesn't call buildProtoRequest (a
// different proto type).
func (d *Dispatcher) sendCancel(ctx context.Context, sid, errandID string) error {
	if d.deps.LeaseLookup == nil || d.deps.Publisher == nil {
		if err := d.deps.Outbound.SendCancelErrand(ctx, sid, errandID); err != nil {
			d.deps.Logger.Warn("errand: local-only cancel send failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}

	holder, err := d.deps.LeaseLookup.ReadHolder(ctx, sid)
	if err != nil {
		d.deps.Logger.Warn("errand: cancel lease lookup failed, fallback to local",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.Any("error", err))
		if sendErr := d.deps.Outbound.SendCancelErrand(ctx, sid, errandID); sendErr != nil {
			return ErrSoulNotConnected
		}
		return nil
	}
	if holder == "" {
		return ErrSoulNotConnected
	}
	if holder == d.deps.KID {
		if err := d.deps.Outbound.SendCancelErrand(ctx, sid, errandID); err != nil {
			d.deps.Logger.Warn("errand: cancel local send (holder=self) failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}
	if err := d.deps.Publisher.PublishCancelErrand(ctx, sid, errandID); err != nil {
		d.deps.Logger.Warn("errand: cancel remote publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.String("holder", holder),
			slog.Any("error", err))
		return ErrSoulNotConnected
	}
	return nil
}

// writeCancelInitiated writes an audit event `errand.cancelled` from the
// initiating Archon (source=api). The terminal `errand.cancelled` from the
// applybus-receiver (Soul sent ErrandResult{CANCELLED}) is also written by
// writeTerminal — for the UI these are two distinct events: "operator
// cancelled" + "Soul confirmed cancel". Compromise vs. dup: different
// source (api vs soul_grpc), same correlation_id — UI groups by it.
//
// nil audit-writer (test build) → drop.
func (d *Dispatcher) writeCancelInitiated(ctx context.Context, errandID, sid, module, aid string) {
	if d.deps.Audit == nil {
		return
	}
	ev := &audit.Event{
		EventType:     audit.EventTypeErrandCancelled,
		Source:        audit.SourceAPI,
		ArchonAID:     aid,
		CorrelationID: errandID,
		Payload: map[string]any{
			"sid":       sid,
			"module":    module,
			"errand_id": errandID,
		},
	}
	if err := d.deps.Audit.Write(ctx, ev); err != nil {
		d.deps.Logger.Warn("errand: audit cancelled (initiated) failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
}
