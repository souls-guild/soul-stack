package pluginhost

import (
	"context"
	"fmt"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// CloudDriverPlugin is thin wrapper over [Plugin], tying base handle
// to CloudDriver gRPC client. Created via [NewCloudDriverPlugin] after
// successful [Host.Spawn]: caller verifies manifest.kind == cloud_driver,
// and wraps Plugin in CloudDriverPlugin.
//
// Apply-cycle keeper.cloud / scenario step `core.cloud.provisioned`
// (ADR-017) uses methods Create/Destroy/Status/List/Schema/Validate;
// stream-methods return grpc-stream directly — caller reads events.
//
// Close proxied to underlying Plugin.Close (idempotent).
type CloudDriverPlugin struct {
	*Plugin
	client pluginv1.CloudDriverClient
}

// NewCloudDriverPlugin wraps [Plugin] (from [Host.Spawn]) in kind-specific
// handle. Returns error if manifest.kind != cloud_driver: protection from
// accidental call on soul_module / ssh_provider binary.
func NewCloudDriverPlugin(p *Plugin) (*CloudDriverPlugin, error) {
	if p == nil {
		return nil, fmt.Errorf("pluginhost: nil Plugin")
	}
	if p.Manifest().Kind != KindCloudDriver {
		return nil, fmt.Errorf("pluginhost: expected kind=cloud_driver, manifest %s has kind=%q",
			p.Manifest().Address(), p.Manifest().Kind)
	}
	return &CloudDriverPlugin{
		Plugin: p,
		client: pluginv1.NewCloudDriverClient(p.Conn()),
	}, nil
}

// Schema is RPC CloudDriver.Schema. Returns plugin's profile_schema (should
// match manifest.spec.profile_schema; cross-check is keeper.cloud task).
func (c *CloudDriverPlugin) Schema(ctx context.Context, req *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return c.client.Schema(ctx, req)
}

// Validate is RPC CloudDriver.Validate. Runtime validation of profile parameters
// (quotas, image availability, subnet validity) — what JSON Schema can't express.
func (c *CloudDriverPlugin) Validate(ctx context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return c.client.Validate(ctx, req)
}

// Create is RPC CloudDriver.Create (server-streaming). Caller reads
// progress events until EOF.
func (c *CloudDriverPlugin) Create(ctx context.Context, req *pluginv1.CreateRequest) (grpc.ServerStreamingClient[pluginv1.CreateEvent], error) {
	return c.client.Create(ctx, req)
}

// Destroy is RPC CloudDriver.Destroy (server-streaming). Under guard-rails
// of keeper.cloud (see docs/keeper/cloud.md → Destroy Safety).
func (c *CloudDriverPlugin) Destroy(ctx context.Context, req *pluginv1.DestroyRequest) (grpc.ServerStreamingClient[pluginv1.DestroyEvent], error) {
	return c.client.Destroy(ctx, req)
}

// Status is RPC CloudDriver.Status. Queries state of specific VM.
func (c *CloudDriverPlugin) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return c.client.Status(ctx, req)
}

// List is RPC CloudDriver.List (server-streaming). Enumerates VMs known
// to provider; caller reads stream until EOF.
func (c *CloudDriverPlugin) List(ctx context.Context, req *pluginv1.ListRequest) (grpc.ServerStreamingClient[pluginv1.VmInfo], error) {
	return c.client.List(ctx, req)
}

// Resize is RPC CloudDriver.Resize (server-streaming). Expands VM resources;
// caller reads progress events by phases until EOF. Driver without capability
// `Resizable` (sdk/clouddriver) returns final ResizeEvent with failed=true and
// message resize.unsupported — this is NOT gRPC Unimplemented.
func (c *CloudDriverPlugin) Resize(ctx context.Context, req *pluginv1.ResizeRequest) (grpc.ServerStreamingClient[pluginv1.ResizeEvent], error) {
	return c.client.Resize(ctx, req)
}
