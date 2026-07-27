package cloud

import (
	"errors"
	"io"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// fakeCreateStream is in-memory createEventStream: yields pre-set
// events one by one, then io.EOF. Optional recvErr returned INSTEAD OF
// EOF at end (transport-failure simulation).
type fakeCreateStream struct {
	events  []*pluginv1.CreateEvent
	idx     int
	recvErr error
}

func (s *fakeCreateStream) Recv() (*pluginv1.CreateEvent, error) {
	if s.idx < len(s.events) {
		ev := s.events[s.idx]
		s.idx++
		return ev, nil
	}
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return nil, io.EOF
}

// TestCollectCreateVMs_Failed — guard on core bug: driver returned
// failed=true (cluster read-only / quota / etc) → collectCreateVMs MUST
// return error with driver-message, not silently swallow (false success with 0 VM).
func TestCollectCreateVMs_Failed(t *testing.T) {
	stream := &fakeCreateStream{events: []*pluginv1.CreateEvent{
		{Message: "validating profile"},
		{Failed: true, Message: "cluster is read-only"},
	}}

	vms, err := collectCreateVMs(stream)
	if err == nil {
		t.Fatal("expected error on driver failed=true, got nil (silent false success)")
	}
	if vms != nil {
		t.Errorf("expected nil vms on failure, got %v", vms)
	}
	if !strings.Contains(err.Error(), "cluster is read-only") {
		t.Errorf("err = %q, want driver message propagated", err)
	}
}

// TestCollectCreateVMs_FailedDropsPartialVMs — partial success = error.
// Driver managed to report some VMs, then failed=true: do NOT
// return subset as success — provision as whole failed.
func TestCollectCreateVMs_FailedDropsPartialVMs(t *testing.T) {
	stream := &fakeCreateStream{events: []*pluginv1.CreateEvent{
		{Vms: []*pluginv1.VmInfo{{VmId: "i-1", Fqdn: "host-1.example.com"}}},
		{Failed: true, Message: "quota exceeded after 1 of 3"},
	}}

	vms, err := collectCreateVMs(stream)
	if err == nil {
		t.Fatal("expected error on partial failure, got nil")
	}
	if vms != nil {
		t.Errorf("expected nil vms on partial failure, got %v (must not onboard subset as success)", vms)
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("err = %q, want driver message propagated", err)
	}
}

// TestCollectCreateVMs_FailedNoMessage — failed=true without message: error still
// returned, with default text (don't lose failure fact itself).
func TestCollectCreateVMs_FailedNoMessage(t *testing.T) {
	stream := &fakeCreateStream{events: []*pluginv1.CreateEvent{
		{Failed: true},
	}}

	if _, err := collectCreateVMs(stream); err == nil {
		t.Fatal("expected error on failed=true even without message")
	}
}

// TestCollectCreateVMs_Happy — all VMs in final event, without failed →
// success, VMs aggregated.
func TestCollectCreateVMs_Happy(t *testing.T) {
	stream := &fakeCreateStream{events: []*pluginv1.CreateEvent{
		{Message: "provisioning"},
		{Vms: []*pluginv1.VmInfo{
			{VmId: "i-1", Fqdn: "host-1.example.com", PrimaryIp: "10.0.0.1"},
			{VmId: "i-2", Fqdn: "host-2.example.com", PrimaryIp: "10.0.0.2"},
		}},
	}}

	vms, err := collectCreateVMs(stream)
	if err != nil {
		t.Fatalf("unexpected error on happy path: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("len(vms) = %d, want 2", len(vms))
	}
	if vms[0].GetFqdn() != "host-1.example.com" || vms[1].GetFqdn() != "host-2.example.com" {
		t.Errorf("vms = %+v, want host-1/host-2", vms)
	}
}

// TestCollectCreateVMs_HappyMultiEvent — VMs arrived in multiple events:
// all are aggregated.
func TestCollectCreateVMs_HappyMultiEvent(t *testing.T) {
	stream := &fakeCreateStream{events: []*pluginv1.CreateEvent{
		{Vms: []*pluginv1.VmInfo{{VmId: "i-1", Fqdn: "host-1.example.com"}}},
		{Vms: []*pluginv1.VmInfo{{VmId: "i-2", Fqdn: "host-2.example.com"}}},
	}}

	vms, err := collectCreateVMs(stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("len(vms) = %d, want 2 (both events aggregated)", len(vms))
	}
}

// TestCollectCreateVMs_RecvError — transport-failure of stream (not EOF) →
// propagated as error.
func TestCollectCreateVMs_RecvError(t *testing.T) {
	wantErr := errors.New("connection reset")
	stream := &fakeCreateStream{
		events:  []*pluginv1.CreateEvent{{Message: "started"}},
		recvErr: wantErr,
	}

	_, err := collectCreateVMs(stream)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrap of %v", err, wantErr)
	}
}

// fakeDestroyStream replays a canned DestroyEvent sequence, then EOF.
type fakeDestroyStream struct {
	events []*pluginv1.DestroyEvent
	i      int
}

func (f *fakeDestroyStream) Recv() (*pluginv1.DestroyEvent, error) {
	if f.i < len(f.events) {
		ev := f.events[f.i]
		f.i++
		return ev, nil
	}
	return nil, io.EOF
}

// TestCollectDestroyed_FailedIsNotDeleted is the keeper half of NIM-191: a
// driver that could not confirm a teardown sends failed=true WITH the vm_id.
// Counting that id as destroyed would cascade souls→destroyed (ADR-017) over a
// VM that is still alive and billed.
func TestCollectDestroyed_FailedIsNotDeleted(t *testing.T) {
	stream := &fakeDestroyStream{events: []*pluginv1.DestroyEvent{
		{VmId: "vm-ok", Message: "destroyed"},
		{VmId: "vm-stuck", Failed: true, Message: "destroy not confirmed: the VM is still present"},
	}}

	destroyed, err := collectDestroyed(stream, 2)
	if err == nil {
		t.Fatal("an unconfirmed teardown must fail the call, not pass silently")
	}
	if len(destroyed) != 1 || destroyed[0] != "vm-ok" {
		t.Fatalf("destroyed=%v, want only the confirmed vm-ok", destroyed)
	}
	for _, want := range []string{"vm-stuck", "1 of 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err=%q, want it to mention %q", err, want)
		}
	}
}

// TestCollectDestroyed_AllConfirmed: the ordinary path still returns every id
// with no error.
func TestCollectDestroyed_AllConfirmed(t *testing.T) {
	stream := &fakeDestroyStream{events: []*pluginv1.DestroyEvent{
		{Message: "confirm-destroy: 0/2 gone (attempt 1/25)"}, // progress, no vm_id
		{VmId: "vm-1", Message: "destroyed"},
		{VmId: "vm-2", Message: "destroyed"},
	}}

	destroyed, err := collectDestroyed(stream, 2)
	if err != nil {
		t.Fatalf("collectDestroyed: %v", err)
	}
	if len(destroyed) != 2 {
		t.Fatalf("destroyed=%v, want both VMs", destroyed)
	}
}

// TestCollectDestroyed_PhaseFailureWithoutVMID: a stream-level failure event
// (no vm_id) must still fail the call rather than read as "nothing to do".
func TestCollectDestroyed_PhaseFailureWithoutVMID(t *testing.T) {
	stream := &fakeDestroyStream{events: []*pluginv1.DestroyEvent{
		{Failed: true, Message: "auth: wb-client: token expired"},
	}}

	if _, err := collectDestroyed(stream, 1); err == nil {
		t.Fatal("a driver-level destroy failure must surface as an error")
	}
}
