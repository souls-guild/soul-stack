package errand

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCancel_Happy — Errand at status=running, lease=self → SendCancelErrand
// is called, no error.
func TestCancel_Happy(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-test"}}

	d := buildTestDispatcher(store, bus, ob, ob, lease, time.Second)

	// Setup: insert a running row.
	row := Row{
		ErrandID:     "ERR1",
		SID:          "host.test",
		Module:       "core.cmd.shell",
		Status:       StatusRunning,
		StartedByAID: "archon-alice",
		StartedByKID: "kid-test",
		StartedAt:    time.Now(),
		TTLAt:        time.Now().Add(time.Hour),
	}
	if err := store.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := d.Cancel(context.Background(), CancelRequest{
		ErrandID:    "ERR1",
		RequestedBy: "archon-alice",
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(ob.cancelled) != 1 || ob.cancelled[0] != "ERR1" {
		t.Fatalf("ob.cancelled = %v, want [ERR1]", ob.cancelled)
	}
}

// TestCancel_NotFound — row doesn't exist → ErrNotFound.
func TestCancel_NotFound(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{}

	d := buildTestDispatcher(store, bus, ob, ob, lease, time.Second)
	err := d.Cancel(context.Background(), CancelRequest{ErrandID: "MISSING"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want wraps ErrNotFound", err)
	}
}

// TestCancel_TerminalStatus — row already success/failed/timed_out/cancelled →
// ErrErrandTerminal.
func TestCancel_TerminalStatus(t *testing.T) {
	for _, term := range []Status{StatusSuccess, StatusFailed, StatusTimedOut, StatusCancelled, StatusModuleNotAllowed} {
		t.Run(string(term), func(t *testing.T) {
			store := newFakeStore()
			bus := newFakeBus()
			ob := &fakeOutbound{}
			lease := &fakeLease{}

			d := buildTestDispatcher(store, bus, ob, ob, lease, time.Second)
			now := time.Now()
			row := Row{
				ErrandID:     "ERRT",
				SID:          "host.test",
				Module:       "core.cmd.shell",
				Status:       term,
				StartedByAID: "archon-alice",
				StartedByKID: "kid-test",
				StartedAt:    now,
				FinishedAt:   &now,
				TTLAt:        now.Add(time.Hour),
			}
			if err := store.Insert(context.Background(), row); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			err := d.Cancel(context.Background(), CancelRequest{ErrandID: "ERRT", RequestedBy: "archon-alice"})
			if !errors.Is(err, ErrErrandTerminal) {
				t.Fatalf("err = %v, want wraps ErrErrandTerminal", err)
			}
			if len(ob.cancelled) != 0 {
				t.Fatalf("ob.cancelled = %v, want empty", ob.cancelled)
			}
		})
	}
}

// TestCancel_EmptyErrandID — ErrEmptyErrandID before lookup.
func TestCancel_EmptyErrandID(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{}

	d := buildTestDispatcher(store, bus, ob, ob, lease, time.Second)
	err := d.Cancel(context.Background(), CancelRequest{ErrandID: ""})
	if !errors.Is(err, ErrEmptyErrandID) {
		t.Fatalf("err = %v, want ErrEmptyErrandID", err)
	}
}

// TestCancel_RemoteHolder — lease holds another KID → PublishCancelErrand path.
func TestCancel_RemoteHolder(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": "kid-other"}}

	d := buildTestDispatcher(store, bus, ob, ob, lease, time.Second)

	row := Row{
		ErrandID:     "ERR-REMOTE",
		SID:          "host.test",
		Module:       "core.cmd.shell",
		Status:       StatusRunning,
		StartedByAID: "archon-alice",
		StartedByKID: "kid-test",
		StartedAt:    time.Now(),
		TTLAt:        time.Now().Add(time.Hour),
	}
	if err := store.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := d.Cancel(context.Background(), CancelRequest{ErrandID: "ERR-REMOTE", RequestedBy: "archon-alice"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// SendCancelErrand or PublishCancelErrand — both land in the same fakeOutbound.cancelled slice.
	if len(ob.cancelled) != 1 || ob.cancelled[0] != "ERR-REMOTE" {
		t.Fatalf("ob.cancelled = %v, want [ERR-REMOTE]", ob.cancelled)
	}
}

// TestCancel_SoulNotConnected — lease holder empty → ErrSoulNotConnected.
func TestCancel_SoulNotConnected(t *testing.T) {
	store := newFakeStore()
	bus := newFakeBus()
	ob := &fakeOutbound{}
	lease := &fakeLease{holders: map[string]string{"host.test": ""}}

	d := buildTestDispatcher(store, bus, ob, ob, lease, time.Second)

	row := Row{
		ErrandID:     "ERR-NC",
		SID:          "host.test",
		Module:       "core.cmd.shell",
		Status:       StatusRunning,
		StartedByAID: "archon-alice",
		StartedByKID: "kid-test",
		StartedAt:    time.Now(),
		TTLAt:        time.Now().Add(time.Hour),
	}
	if err := store.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	err := d.Cancel(context.Background(), CancelRequest{ErrandID: "ERR-NC", RequestedBy: "archon-alice"})
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
}
