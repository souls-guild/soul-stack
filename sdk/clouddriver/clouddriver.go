// Package clouddriver is the Soul Stack SDK for CloudDriver plugin authors
// (kind: cloud_driver, binaries soul-cloud-<provider>).
//
// Minimal plugin author path:
//
//	type AwsDriver struct { clouddriver.BaseDriver }
//
//	func (a *AwsDriver) Create(req *pluginv1.CreateRequest, stream pluginv1.CloudDriver_CreateServer) error {
//	    // ...
//	}
//
//	func main() {
//	    if err := clouddriver.Serve(&AwsDriver{}); err != nil { os.Exit(1) }
//	}
//
// BaseDriver provides no-op implementations of all RPCs (Schema/Validate
// return empty successful replies, Create/Destroy/List close the stream
// without events, Status returns an empty State, Resize is default-deny
// resize.unsupported); the author overrides only what's needed. Resize
// additionally requires declaring the Resizable marker interface
// (default-deny capability).
// Serve opens a Unix socket, performs the gRPC-stdio handshake, and handles
// SIGTERM (see sdk/handshake).
package clouddriver

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion is the MVP plugin protocol version (docs/keeper/plugins.md →
// Versioning). Symmetric with sdk/module.
const protocolVersion = 1

// CloudDriver is the interface implemented by the plugin author. Signatures
// mirror pluginv1.CloudDriverServer, but without the must-embed requirement
// for pluginv1.UnimplementedCloudDriverServer: the SDK takes forward-compat
// on itself via an internal adapter.
type CloudDriver interface {
	Schema(ctx context.Context, req *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error)
	Validate(ctx context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error)
	Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error
	Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error
	Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error)
	List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error

	// Resize expands VM resources (cpu/ram/disk, our units). Declared in the
	// interface for forward-compat (the gRPC contract already carries it), but
	// the capability is declared SEPARATELY via the [Resizable] marker
	// interface (default-deny): a driver that actually supports resize
	// overrides this method AND implements [Resizable]. A driver built on
	// [BaseDriver] gets a safe default-deny — the host (keeper) doesn't call
	// Resize and returns resize.unsupported instead. See [BaseDriver.Resize],
	// [Resizable].
	Resize(req *pluginv1.ResizeRequest, stream grpc.ServerStreamingServer[pluginv1.ResizeEvent]) error
}

// Resizable is an optional marker interface (default-deny, the
// [PlanReadSafe] pattern from sdk/module / ADR-031): a driver implements it
// to DECLARE that its Resize is a real implementation (able to change VM
// resources). The host (keeper, `core.cloud.provisioned` module,
// state=resized) checks the implementation via type assertion BEFORE calling
// Resize: a driver without [Resizable] gets default-deny — the host returns
// a clear `resize.unsupported`, not a raw gRPC Unimplemented or a false
// "success".
//
// An argument-less marker method: its presence is the capability
// declaration. [BaseDriver] DELIBERATELY does NOT implement it — a driver
// built on BaseDriver gets a safe default-deny on resize by default, with no
// action from the author.
type Resizable interface {
	// Resizable is a marker; called by the host as a type assertion, the body
	// doesn't matter.
	Resizable()
}

// BaseDriver is an embeddable default implementation of CloudDriver: all
// methods are no-ops. The author overrides only the RPCs it needs;
// unimplemented ones keep returning empty responses (Validate returns
// Ok=true, Create/Destroy/List close the stream without events). This is
// acceptable for test plugins and smoke tests.
type BaseDriver struct{}

func (BaseDriver) Schema(context.Context, *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return &pluginv1.SchemaReply{}, nil
}

func (BaseDriver) Validate(context.Context, *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return &pluginv1.ValidateProfileReply{Ok: true}, nil
}

func (BaseDriver) Create(*pluginv1.CreateRequest, grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	return nil
}

func (BaseDriver) Destroy(*pluginv1.DestroyRequest, grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	return nil
}

func (BaseDriver) Status(context.Context, *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return &pluginv1.StatusReply{}, nil
}

func (BaseDriver) List(*pluginv1.ListRequest, grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	return nil
}

// Resize is a default-deny no-op: sends a final ResizeEvent with
// failed=true and message resize.unsupported — NOT a panic, NOT a false
// "success". Normally the host never reaches this method (BaseDriver doesn't
// implement [Resizable] → the host applies default-deny). This fallback
// guards against a direct call that bypasses the marker check.
func (BaseDriver) Resize(_ *pluginv1.ResizeRequest, stream grpc.ServerStreamingServer[pluginv1.ResizeEvent]) error {
	return stream.Send(&pluginv1.ResizeEvent{
		Failed:  true,
		Message: "resize.unsupported: driver does not implement Resize (missing Resizable capability)",
	})
}

// Serve is the typical main() of a CloudDriver plugin: wraps
// sdk/handshake.Serve + registers the pluginv1.CloudDriver grpc-service with
// the author's impl.
func Serve(impl CloudDriver) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_CLOUD_DRIVER,
	}, func(s *grpc.Server) {
		pluginv1.RegisterCloudDriverServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter is the bridge between the SDK's CloudDriver interface and
// pluginv1.CloudDriverServer; embedding Unimplemented provides forward-compat
// when new RPCs are added in proto/plugin/v2/.
type serverAdapter struct {
	pluginv1.UnimplementedCloudDriverServer
	impl CloudDriver
}

func (a *serverAdapter) Schema(ctx context.Context, req *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return a.impl.Schema(ctx, req)
}

func (a *serverAdapter) Validate(ctx context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return a.impl.Validate(ctx, req)
}

func (a *serverAdapter) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	return a.impl.Create(req, stream)
}

func (a *serverAdapter) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	return a.impl.Destroy(req, stream)
}

func (a *serverAdapter) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return a.impl.Status(ctx, req)
}

func (a *serverAdapter) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	return a.impl.List(req, stream)
}

// Resize applies default-deny based on the [Resizable] marker interface (the
// PlanReadSafe pattern). The plugin lives in a separate process, so the host
// (keeper) can't check the marker directly via type assertion — the check
// happens here, in serverAdapter: if impl does NOT implement [Resizable], the
// adapter returns resize.unsupported without calling impl.Resize. This
// guarantees that a driver built on [BaseDriver] (or one that forgot to
// declare the capability) gets a clear refusal instead of accidentally
// running a no-op Resize.
func (a *serverAdapter) Resize(req *pluginv1.ResizeRequest, stream grpc.ServerStreamingServer[pluginv1.ResizeEvent]) error {
	if _, ok := a.impl.(Resizable); !ok {
		return stream.Send(&pluginv1.ResizeEvent{
			Failed:  true,
			Message: "resize.unsupported: driver does not declare Resizable capability",
		})
	}
	return a.impl.Resize(req, stream)
}
