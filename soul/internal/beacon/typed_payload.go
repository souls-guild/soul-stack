package beacon

import (
	"log/slog"
	"sync"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"google.golang.org/protobuf/types/known/structpb"
)

// Typed-payload mapper (V5-1, ADR-030 amendment 2026-05-26): projects the
// data *structpb.Struct from a built-in core-beacon into the typed
// PortentEvent.payload (oneof). Soul-side fills BOTH branches (event.Data +
// event.Payload) during a 1-release deprecation period — backward-compat for
// existing where-CEL `event.data.<field>`. After that release, the `data`
// branch is removed in a hard cut (S5-final, parity with the push
// S7-decision).
//
// The mapping is flat: a switch on the check address from
// VigilDef.GetCheck() picks the concrete builder. An unknown check (e.g.
// plugin-beacon V5-2) leaves payload unset; the data branch still carries
// the raw Struct.

// deprecationWarnOnce fires exactly once per process on the first Portent
// emitted with BOTH branches filled (data + payload), so the operator sees
// the hand-off period once in the logs. Log-spam at thousands of
// Portents/hour is unacceptable.
var deprecationWarnOnce sync.Once

// fillTypedPayload sets PortentEvent.Payload (oneof) from the Vigil's
// check address using the data *structpb.Struct returned by that beacon's
// Check. nil data → no-op (Payload stays nil). Unknown check → no-op (for
// plugin-beacon V5-2, the `custom` branch is filled separately by the
// plugin's apply loop, not here). A local function in this package — the
// private oneof interface keeperv1.isPortentEvent_Payload is accessible
// here via direct assignment of the concrete type to the Payload field.
func fillTypedPayload(ev *keeperv1.PortentEvent, check string, data *structpb.Struct) {
	if data == nil {
		return
	}
	switch check {
	case beaconaddr.FileChanged:
		ev.Payload = &keeperv1.PortentEvent_FileChanged{FileChanged: &keeperv1.FileChangedPortent{
			Path:   getString(data, "path"),
			Sha256: getString(data, "sha256"),
		}}
	case beaconaddr.ServiceDown:
		ev.Payload = &keeperv1.PortentEvent_ServiceDown{ServiceDown: &keeperv1.ServiceDownPortent{
			Service:    getString(data, "service"),
			Active:     getBool(data, "active"),
			InitSystem: getString(data, "init_system"),
		}}
	case beaconaddr.PortClosed:
		ev.Payload = &keeperv1.PortentEvent_PortClosed{PortClosed: &keeperv1.PortClosedPortent{
			Host: getString(data, "host"),
			Port: int32(getNumber(data, "port")),
		}}
	case beaconaddr.DiskFull:
		ev.Payload = &keeperv1.PortentEvent_DiskFull{DiskFull: &keeperv1.DiskFullPortent{
			Path:        getString(data, "path"),
			UsedPercent: getNumber(data, "used_percent"),
			Threshold:   getNumber(data, "threshold"),
		}}
	case beaconaddr.ProcessAbsent:
		ev.Payload = &keeperv1.PortentEvent_ProcessAbsent{ProcessAbsent: &keeperv1.ProcessAbsentPortent{
			Pattern: getString(data, "pattern"),
		}}
	case beaconaddr.HTTPUnhealthy:
		ev.Payload = &keeperv1.PortentEvent_HttpUnhealthy{HttpUnhealthy: &keeperv1.HttpUnhealthyPortent{
			Url:    getString(data, "url"),
			Status: int32(getNumber(data, "status")),
		}}
	case beaconaddr.Inotify:
		ev.Payload = &keeperv1.PortentEvent_Inotify{Inotify: buildInotifyPayload(data)}
	}
}

// buildInotifyPayload builds an InotifyPortent from the data Struct (V5-3).
// The events list arrives via `data.events: []map{type,file,at}` — a
// projection of one node into a repeated typed message. Empty/missing
// events → empty repeated, but a Portent is only ever emitted at
// state="events" (scheduler invariant), so an empty list shouldn't occur.
func buildInotifyPayload(data *structpb.Struct) *keeperv1.InotifyPortent {
	out := &keeperv1.InotifyPortent{
		Path:  getString(data, "path"),
		Count: int32(getNumber(data, "count")),
	}
	lv, ok := data.GetFields()["events"]
	if !ok || lv == nil {
		return out
	}
	list := lv.GetListValue()
	if list == nil {
		return out
	}
	for _, item := range list.GetValues() {
		s := item.GetStructValue()
		if s == nil {
			continue
		}
		out.Events = append(out.Events, &keeperv1.InotifyEvent{
			Type: getString(s, "type"),
			File: getString(s, "file"),
			At:   int64(getNumber(s, "at")),
		})
	}
	return out
}

// getString reads a string field from the Struct; missing/non-string → "".
func getString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	v, ok := s.GetFields()[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}

// getNumber reads a numeric field; missing/non-number → 0. proto-json
// marshals all numbers as float64 (NumberValue), so one function covers both
// double and int (via explicit cast int32(getNumber)).
func getNumber(s *structpb.Struct, key string) float64 {
	if s == nil {
		return 0
	}
	v, ok := s.GetFields()[key]
	if !ok {
		return 0
	}
	return v.GetNumberValue()
}

// getBool reads a bool field; missing/non-bool → false.
func getBool(s *structpb.Struct, key string) bool {
	if s == nil {
		return false
	}
	v, ok := s.GetFields()[key]
	if !ok {
		return false
	}
	return v.GetBoolValue()
}

// emitDeprecationWarnOnce logs a WARN once about the data+payload dual-write.
// Called from emit() after successfully queuing a Portent with typed payload.
func emitDeprecationWarnOnce(logger *slog.Logger) {
	deprecationWarnOnce.Do(func() {
		logger.Warn("beacon: PortentEvent.data заполняется параллельно с typed payload — deprecated, 1-release WARN, удалится hard-cut в S5-final (V5-1 ADR-030 amendment 2026-05-26)")
	})
}
