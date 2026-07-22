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
