package auditgate

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// recorder captures what actually reached the wrapped writer.
type recorder struct {
	got []audit.EventType
	err error
}

func (r *recorder) Write(_ context.Context, ev *audit.Event) error {
	r.got = append(r.got, ev.EventType)
	return r.err
}

func always(v bool) func() bool { return func() bool { return v } }

// master builds the gate as setupAudit does, with the transition marker on.
func master(next audit.Writer, enabled func() bool) audit.Writer {
	return New(Config{Next: next, Enabled: enabled, KID: "keeper-test"})
}

// The acceptance criterion of NIM-194: `audit.enabled: false` must actually stop
// the write, not merely be a documented field.
func TestGate_DisabledStopsTheWrite(t *testing.T) {
	rec := &recorder{}
	w := master(rec, always(false))

	if err := w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorCreated}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(rec.got) != 0 {
		t.Errorf("event reached the writer with audit disabled: %v", rec.got)
	}
}

func TestGate_EnabledPassesThrough(t *testing.T) {
	rec := &recorder{}
	w := master(rec, always(true))

	if err := w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorCreated}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(rec.got) != 1 || rec.got[0] != audit.EventOperatorCreated {
		t.Errorf("got %v, want one operator.created", rec.got)
	}
}

// The load-bearing half of the toggle: `Store.reload` swaps the snapshot before
// it emits the reload event, so the reload that DISABLES audit is journaled
// through an already-closed gate. Without the bypass, turning audit off would
// leave no record of it — working without traces.
func TestGate_DisabledStillWritesTheRecordOfItsOwnClosing(t *testing.T) {
	bypassed := []audit.EventType{
		audit.EventConfigReloadSucceeded,
		audit.EventConfigReloadFailed,
		audit.EventAuditDisabled,
	}

	for _, et := range bypassed {
		t.Run(string(et), func(t *testing.T) {
			rec := &recorder{}
			w := master(rec, always(false))

			if err := w.Write(context.Background(), &audit.Event{EventType: et}); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if len(rec.got) != 1 {
				t.Fatalf("%s was swallowed by the disabled gate", et)
			}
		})
	}
}

// A disabled gate is a configured outcome, not a failure: returning an error
// would fail the initiator's own operation (an HTTP request, a Reaper rule).
func TestGate_DisabledReportsSuccess(t *testing.T) {
	w := master(&recorder{err: errors.New("must not be called")}, always(false))

	if err := w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorCreated}); err != nil {
		t.Errorf("Write = %v, want nil", err)
	}
}

func TestGate_PropagatesWriterError(t *testing.T) {
	want := errors.New("pg down")
	w := master(&recorder{err: want}, always(true))

	if err := w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorCreated}); !errors.Is(err, want) {
		t.Errorf("Write = %v, want %v", err, want)
	}
}

// The secondary gate (`audit.otel_export`) has no bypass: Postgres stays the
// source of truth, so a suppressed span loses nothing.
func TestSecondaryGate_HasNoBypass(t *testing.T) {
	rec := &recorder{}
	w := NewSecondary(rec, always(false))

	for et := range AlwaysWrite {
		if err := w.Write(context.Background(), &audit.Event{EventType: et}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if len(rec.got) != 0 {
		t.Errorf("secondary gate bypassed events while disabled: %v", rec.got)
	}
}

func TestSecondaryGate_EnabledPassesThrough(t *testing.T) {
	rec := &recorder{}
	w := NewSecondary(rec, always(true))

	if err := w.Write(context.Background(), &audit.Event{EventType: audit.EventTaskExecuted}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(rec.got) != 1 {
		t.Errorf("got %v, want one event", rec.got)
	}
}

// The toggle is read per event, not captured at construction — that is what
// keeps `audit.enabled` hot-reloadable (ADR-022(i)).
func TestGate_TogglesLiveWithoutRewiring(t *testing.T) {
	rec := &recorder{}
	enabled := true
	w := master(rec, func() bool { return enabled })

	_ = w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorCreated})
	enabled = false
	_ = w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorRevoked})
	enabled = true
	_ = w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorTokenIssued})

	// operator.revoked is dropped; each flip leaves its marker where it happened,
	// so the trail reads "…created, audit off, audit on, token-issued" and the
	// gap between the markers is self-explaining.
	want := []audit.EventType{
		audit.EventOperatorCreated,
		audit.EventAuditDisabled,
		audit.EventAuditEnabled,
		audit.EventOperatorTokenIssued,
	}
	if len(rec.got) != len(want) {
		t.Fatalf("got %v, want %v", rec.got, want)
	}
	for i := range want {
		if rec.got[i] != want[i] {
			t.Fatalf("got %v, want %v", rec.got, want)
		}
	}
}

// A marker is written once per flip, not once per write while the toggle sits in
// its new state.
func TestGate_MarkerIsNotRepeatedWhileTheStateHolds(t *testing.T) {
	rec := &recorder{}
	enabled := true
	w := master(rec, func() bool { return enabled })

	enabled = false
	for range 3 {
		_ = w.Write(context.Background(), &audit.Event{EventType: audit.EventOperatorCreated})
	}

	markers := 0
	for _, et := range rec.got {
		if et == audit.EventAuditDisabled {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("wrote %d audit.disabled markers for one flip: %v", markers, rec.got)
	}
}

func TestGate_NilEventIsGatedNotDereferenced(t *testing.T) {
	rec := &recorder{}
	w := master(rec, always(false))

	if err := w.Write(context.Background(), nil); err != nil {
		t.Fatalf("Write(nil): %v", err)
	}
	if len(rec.got) != 0 {
		t.Error("nil event passed a disabled gate")
	}
}
