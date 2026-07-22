// Package module is the Soul Stack SDK for SoulModule plugin authors
// (kind: soul_module, binaries soul-mod-<name>).
//
// Minimal path for a plugin author:
//
//	type RedisFailover struct { module.BaseModule }
//
//	func (r *RedisFailover) Apply(req *pluginv1.ApplyRequest, stream pluginv1.SoulModule_ApplyServer) error {
//	    // ...
//	}
//
//	func main() {
//	    if err := module.Serve(&RedisFailover{}); err != nil { os.Exit(1) }
//	}
//
// BaseModule provides no-op implementations of Validate (ok=true) and Plan
// (empty stream); the author only overrides Apply. Serve opens a Unix
// socket, performs the gRPC-stdio handshake, and handles SIGTERM (see
// sdk/handshake).
package module

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion is the MVP plugin-protocol version (docs/keeper/plugins.md
// → Versioning; the only version hosts support in SupportedProtocolVersions).
const protocolVersion = 1

// SoulModule is the interface a plugin author implements. Its signatures
// mirror pluginv1.SoulModuleServer, but without the must-embed requirement
// for pluginv1.UnimplementedSoulModuleServer: the SDK takes forward-compat
// on itself via an internal adapter.
//
// Plan's contract (ADR-031 Scry) is a pure-read dry-run: the module READS
// the resource's current state (the same read Apply starts with) and sends a
// final PlanEvent with a machine-readable `changed` — "would Apply change
// the resource?" (drift). Plan MUST NOT MUTATE the host: no install/write/
// start. On dry_run, the host (Soul) calls Plan INSTEAD OF Apply. A module
// without a genuine pure-read Plan must declare that by NOT implementing
// [PlanReadSafe] — the host then applies default-deny (Plan isn't called,
// the task gets an explicit "drift not supported" instead of a false "no
// drift"). See [PlanReadSafe].
//
// **Invariant (read-safe Plan):** a Plan implementation that declares
// [PlanReadSafe] MUST send EXACTLY ONE final PlanEvent with a
// machine-readable `changed` BEFORE returning from the method (with no
// error). The host treats a `nil` return with no final event as FAILED
// `plan.no_result` (a guard against a misbehaving module that silently
// "goes clean"). Returning an error yields FAILED `plan.error`.
type SoulModule interface {
	Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error)
	Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error
	Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error
}

// PlanReadSafe is an optional marker interface (ADR-031 Scry, default-deny):
// a module implements it to DECLARE that its Plan is a genuine pure-read
// implementation, safe to call on dry_run (reads state, does NOT mutate the
// host). On dry_run, the host (Soul) calls Plan ONLY for modules that
// implement this interface; everything else (a custom plugin on BaseModule,
// a core module without a pure-read Plan) gets default-deny: Plan isn't
// called, the task gets an explicit "drift not supported" refusal, NOT a
// false clean.
//
// A no-argument marker method: its mere presence declares the capability.
// Only modules with a verified pure-read Plan implement it; BaseModule does
// NOT implement it (its no-op Plan doesn't determine drift), so a plugin
// built on BaseModule gets safe default-deny with no action from the author.
type PlanReadSafe interface {
	// PlanReadSafe is the marker; the host invokes it via type assertion, its body doesn't matter.
	PlanReadSafe()
}

// ErrandReadSafe is an optional marker interface (ADR-033 Errand,
// default-deny): a module implements it to DECLARE that its Apply is safe to
// invoke through the Errand pull-ad-hoc path (doesn't mutate
// incarnation.state, has no side effects beyond those declared in the
// manifest's `side_effects`). The Soul-side Errand runner checks the
// implementation BEFORE calling Apply and REJECTS unmarked modules with
// `ErrandResult.status = MODULE_NOT_ALLOWED` (defense-in-depth, mirroring
// [PlanReadSafe] from ADR-031).
//
// The hardcoded `core.cmd.shell` / `core.exec.run` list bypasses this
// interface — verb modules are imperative by design, and the Errand runner
// allows them by name without a marker check (ADR-033 §2).
//
// BaseModule does NOT implement [ErrandReadSafe] — a plugin built on
// BaseModule gets safe default-deny on an Errand call by default. An author
// whose Apply really is safe for ad-hoc invocation overrides Apply AND
// implements ErrandReadSafe explicitly.
type ErrandReadSafe interface {
	// ErrandReadSafe is the marker; the host invokes it via type assertion, its body doesn't matter.
	ErrandReadSafe()
}

// BaseModule is an embeddable default implementation of SoulModule: Validate
// returns Ok=true, Plan sends no events, Apply is a TODO for the author.
//
// Apply is intentionally a no-op here too: the embedder must override it, or
// the plugin does nothing useful. Fine for test plugins and smoke tests.
//
// BaseModule deliberately does NOT implement [PlanReadSafe]: its Plan is a
// no-op (doesn't determine drift), and a plugin built on BaseModule should
// get safe default-deny on dry_run by default (the host doesn't call Plan →
// explicit "drift not supported"), rather than silently reporting "no
// drift". An author with a genuine pure-read Plan overrides Plan AND
// implements PlanReadSafe explicitly (ADR-031 Scry).
type BaseModule struct{}

func (BaseModule) Validate(context.Context, *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Plan is a no-op default: sends no events, doesn't determine drift. The
// host applies default-deny to a module without [PlanReadSafe] — this Plan
// isn't called on dry_run.
func (BaseModule) Plan(*pluginv1.PlanRequest, grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (BaseModule) Apply(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return nil
}

// Serve is the typical main() of a SoulModule plugin: it wraps
// sdk/handshake.Serve and registers the pluginv1.SoulModule grpc-service
// with the author's impl.
func Serve(impl SoulModule) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
	}, func(s *grpc.Server) {
		pluginv1.RegisterSoulModuleServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter bridges the SDK's SoulModule interface and
// pluginv1.SoulModuleServer; embedding Unimplemented provides forward-compat
// when new RPCs are added in proto/plugin/v2/.
type serverAdapter struct {
	pluginv1.UnimplementedSoulModuleServer
	impl SoulModule
}

func (a *serverAdapter) Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return a.impl.Validate(ctx, req)
}

func (a *serverAdapter) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return a.impl.Plan(req, stream)
}

func (a *serverAdapter) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return a.impl.Apply(req, stream)
}
