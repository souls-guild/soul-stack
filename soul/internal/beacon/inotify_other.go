//go:build !linux

package beacon

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"google.golang.org/protobuf/types/known/structpb"
)

// InotifyName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
// On non-Linux platforms the beacon itself always errors, but the address
// constant stays available for the unified keeper-enum / soul-registry
// source-of-truth.
const InotifyName = beaconaddr.Inotify

// InotifyBeacon is the stub implementation on non-Linux platforms (V5-3,
// ADR-030 amendment 2026-05-26). Any Check returns a "platform not
// supported" error: the scheduler logs it and skips the tick (baseline isn't
// set, no Portent is emitted — a check error != a host state change). The
// beacon is registered on all platforms (a Default vs beaconaddr.All
// mismatch would be a build bug), but Vigils won't actually run on non-Linux.
type InotifyBeacon struct{}

// NewInotify builds the stub beacon. Allocates no resources.
func NewInotify() *InotifyBeacon { return &InotifyBeacon{} }

func (*InotifyBeacon) Check(_ context.Context, _ *structpb.Struct) (State, *structpb.Struct, error) {
	return "", nil, fmt.Errorf("core.beacon.inotify: platform not supported (Linux-only, V5-3)")
}
