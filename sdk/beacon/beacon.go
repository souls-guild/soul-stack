// Package beacon is the Soul Stack SDK for SoulBeacon plugin authors
// (kind: soul_beacon, binaries soul-beacon-<name>, ADR-030 V5-2).
//
// Minimal path for a plugin author:
//
//	type ZFSDegraded struct { beacon.BaseBeacon }
//
//	func (z *ZFSDegraded) Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
//	    // ...
//	}
//
//	func main() {
//	    if err := beacon.Serve(&ZFSDegraded{}); err != nil { os.Exit(1) }
//	}
//
// BaseBeacon provides no-op implementations of Validate (Ok=true) and Check
// (State="unknown"); the author only overrides the method they need. Serve
// opens a Unix socket, performs the gRPC-stdio handshake, and handles
// SIGTERM (see sdk/handshake).
//
// Beacon is read-only by construction (ADR-030): Check observes host state
// and reports it back, but never mutates the system. Any write to the system
// is a plugin bug.
package beacon

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion is the MVP plugin-protocol version (docs/keeper/plugins.md
// → Versioning). Mirrors sdk/module / sdk/clouddriver / sdk/sshprovider.
const protocolVersion = 1

// Beacon is the interface a plugin author implements. Its signatures mirror
// pluginv1.SoulBeaconServer, but without the must-embed requirement for
// pluginv1.UnimplementedSoulBeaconServer: the SDK takes forward-compat on
// itself via an internal adapter.
type Beacon interface {
	Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error)
	Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error)
}

// BaseBeacon is an embeddable default implementation of Beacon: Validate
// returns Ok=true, Check returns State="unknown" with no payload. The author
// overrides only the RPCs they need; unimplemented ones keep returning the
// expected "safe" responses. Fine for test plugins and smoke tests.
type BaseBeacon struct{}

func (BaseBeacon) Validate(context.Context, *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	return &pluginv1.ValidateVigilReply{Ok: true}, nil
}

func (BaseBeacon) Check(context.Context, *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	return &pluginv1.CheckReply{State: "unknown"}, nil
}

// Serve is the typical main() of a SoulBeacon plugin: it wraps
// sdk/handshake.Serve and registers the pluginv1.SoulBeacon grpc-service
// with the author's impl.
func Serve(impl Beacon) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_SOUL_BEACON,
	}, func(s *grpc.Server) {
		pluginv1.RegisterSoulBeaconServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter bridges the SDK's Beacon interface and
// pluginv1.SoulBeaconServer; embedding Unimplemented provides forward-compat
// when new RPCs are added in proto/plugin/v2/.
type serverAdapter struct {
	pluginv1.UnimplementedSoulBeaconServer
	impl Beacon
}

func (a *serverAdapter) Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	return a.impl.Validate(ctx, req)
}

func (a *serverAdapter) Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	return a.impl.Check(ctx, req)
}
