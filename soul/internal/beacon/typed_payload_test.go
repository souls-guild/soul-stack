package beacon

import (
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// L0 unit tests for typed PortentPayload (V5-1, ADR-030 amendment 2026-05-26):
// roundtrip each of the 6 typed payloads through proto Marshal/Unmarshal +
// dual-write data+typed (deprecation period).

func mustStruct(t *testing.T, fields map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// roundtripPortent marshals → unmarshals and returns the unmarshaled
// message. Any proto failure is a t.Fatal.
func roundtripPortent(t *testing.T, ev *keeperv1.PortentEvent) *keeperv1.PortentEvent {
	t.Helper()
	bytes, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	got := &keeperv1.PortentEvent{}
	if err := proto.Unmarshal(bytes, got); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	return got
}

func TestFillTypedPayload_FileChanged(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"path":   "/etc/passwd",
		"sha256": "deadbeef",
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.FileChanged, data)

	got := roundtripPortent(t, ev)
	fc := got.GetFileChanged()
	if fc == nil {
		t.Fatalf("typed payload not set after roundtrip")
	}
	if fc.GetPath() != "/etc/passwd" || fc.GetSha256() != "deadbeef" {
		t.Errorf("FileChangedPortent = %+v", fc)
	}
	// dual-write: the data branch is also preserved after roundtrip.
	if got.GetData() == nil || got.GetData().GetFields()["path"].GetStringValue() != "/etc/passwd" {
		t.Error("legacy data branch should be populated during the hand-off period")
	}
}

func TestFillTypedPayload_ServiceDown(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"service":     "nginx",
		"active":      false,
		"init_system": "systemd",
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.ServiceDown, data)

	got := roundtripPortent(t, ev)
	sd := got.GetServiceDown()
	if sd == nil {
		t.Fatal("ServiceDown typed payload not set")
	}
	if sd.GetService() != "nginx" || sd.GetActive() != false || sd.GetInitSystem() != "systemd" {
		t.Errorf("ServiceDownPortent = %+v", sd)
	}
}

func TestFillTypedPayload_PortClosed(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"host": "10.0.0.1",
		"port": 8443,
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.PortClosed, data)

	got := roundtripPortent(t, ev)
	pc := got.GetPortClosed()
	if pc == nil {
		t.Fatal("PortClosed typed payload not set")
	}
	if pc.GetHost() != "10.0.0.1" || pc.GetPort() != 8443 {
		t.Errorf("PortClosedPortent = %+v", pc)
	}
}

func TestFillTypedPayload_DiskFull(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"path":         "/var",
		"used_percent": 95.5,
		"threshold":    90.0,
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.DiskFull, data)

	got := roundtripPortent(t, ev)
	df := got.GetDiskFull()
	if df == nil {
		t.Fatal("DiskFull typed payload not set")
	}
	if df.GetPath() != "/var" || df.GetUsedPercent() != 95.5 || df.GetThreshold() != 90.0 {
		t.Errorf("DiskFullPortent = %+v", df)
	}
}

func TestFillTypedPayload_ProcessAbsent(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"pattern": "redis-server",
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.ProcessAbsent, data)

	got := roundtripPortent(t, ev)
	pa := got.GetProcessAbsent()
	if pa == nil {
		t.Fatal("ProcessAbsent typed payload not set")
	}
	if pa.GetPattern() != "redis-server" {
		t.Errorf("ProcessAbsentPortent = %+v", pa)
	}
}

func TestFillTypedPayload_HTTPUnhealthy(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"url":    "https://api.example.com/health",
		"status": 503,
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.HTTPUnhealthy, data)

	got := roundtripPortent(t, ev)
	hu := got.GetHttpUnhealthy()
	if hu == nil {
		t.Fatal("HttpUnhealthy typed payload not set")
	}
	if hu.GetUrl() != "https://api.example.com/health" || hu.GetStatus() != 503 {
		t.Errorf("HttpUnhealthyPortent = %+v", hu)
	}
}

func TestFillTypedPayload_Inotify(t *testing.T) {
	// data shape from core.beacon.inotify::inotifyData with two events.
	data := mustStruct(t, map[string]any{
		"path":  "/var/log/audit",
		"count": 2,
		"events": []any{
			map[string]any{"type": "created", "file": "audit.log.1", "at": 1700000000},
			map[string]any{"type": "modified", "file": "audit.log", "at": 1700000001},
		},
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.Inotify, data)

	got := roundtripPortent(t, ev)
	ino := got.GetInotify()
	if ino == nil {
		t.Fatal("Inotify typed payload not set")
	}
	if ino.GetPath() != "/var/log/audit" {
		t.Errorf("InotifyPortent.path=%q", ino.GetPath())
	}
	if ino.GetCount() != 2 {
		t.Errorf("InotifyPortent.count=%d, want 2", ino.GetCount())
	}
	if len(ino.GetEvents()) != 2 {
		t.Fatalf("InotifyPortent.events len=%d, want 2", len(ino.GetEvents()))
	}
	if e := ino.GetEvents()[0]; e.GetType() != "created" || e.GetFile() != "audit.log.1" || e.GetAt() != 1700000000 {
		t.Errorf("events[0] = %+v", e)
	}
	if e := ino.GetEvents()[1]; e.GetType() != "modified" || e.GetFile() != "audit.log" || e.GetAt() != 1700000001 {
		t.Errorf("events[1] = %+v", e)
	}
}

func TestFillTypedPayload_InotifyEmptyEvents(t *testing.T) {
	// Edge case: count=0, no events key. The payload branch is still set
	// (the scheduler decides whether to emit a Portent based on state).
	data := mustStruct(t, map[string]any{
		"path":  "/etc/x",
		"count": 0,
	})
	ev := &keeperv1.PortentEvent{BeaconName: "v1", Data: data}
	fillTypedPayload(ev, beaconaddr.Inotify, data)

	got := roundtripPortent(t, ev)
	ino := got.GetInotify()
	if ino == nil {
		t.Fatal("Inotify typed payload should be set even without events")
	}
	if ino.GetPath() != "/etc/x" {
		t.Errorf("InotifyPortent.path=%q", ino.GetPath())
	}
	if len(ino.GetEvents()) != 0 {
		t.Errorf("len(events)=%d, want 0", len(ino.GetEvents()))
	}
}

func TestFillTypedPayload_UnknownCheckNoop(t *testing.T) {
	// Plugin beacon (V5-2): an unknown check address must not set a typed
	// payload — the branch stays nil so the plugin-apply-loop can set
	// `custom: Struct` itself.
	data := mustStruct(t, map[string]any{"x": "y"})
	ev := &keeperv1.PortentEvent{BeaconName: "plugin-v1", Data: data}
	fillTypedPayload(ev, "soul_beacon.example", data)

	if ev.GetPayload() != nil {
		t.Errorf("unknown check should not set typed payload, got %T", ev.GetPayload())
	}
}

func TestFillTypedPayload_NilDataNoop(t *testing.T) {
	// nil data — the beacon returned a pure state change with no attributes;
	// don't fill payload (nothing to put there anyway).
	ev := &keeperv1.PortentEvent{BeaconName: "v1"}
	fillTypedPayload(ev, beaconaddr.FileChanged, nil)
	if ev.GetPayload() != nil {
		t.Errorf("nil data should leave payload empty, got %T", ev.GetPayload())
	}
}

// TestPortentEvent_OneofExclusive is a proto invariant: after roundtrip
// exactly one typed branch is set (oneof guarantees it on the wire), and
// GetData() preserves the dual-write value.
func TestPortentEvent_OneofExclusive(t *testing.T) {
	data := mustStruct(t, map[string]any{"path": "/x", "sha256": "abc"})
	ev := &keeperv1.PortentEvent{
		BeaconName: "v1",
		Data:       data,
		Payload: &keeperv1.PortentEvent_FileChanged{FileChanged: &keeperv1.FileChangedPortent{
			Path: "/x", Sha256: "abc",
		}},
	}
	got := roundtripPortent(t, ev)

	// Exactly one typed branch — FileChanged.
	if got.GetFileChanged() == nil {
		t.Error("file_changed branch is empty after roundtrip")
	}
	if got.GetServiceDown() != nil || got.GetPortClosed() != nil ||
		got.GetDiskFull() != nil || got.GetProcessAbsent() != nil ||
		got.GetHttpUnhealthy() != nil || got.GetCustom() != nil {
		t.Error("oneof violated: multiple typed branches after roundtrip")
	}
	// dual-write: the legacy data branch is also present.
	if got.GetData() == nil {
		t.Error("data branch lost after roundtrip")
	}
}
