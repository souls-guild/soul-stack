package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// PluginAdapter implements [PluginHost] on top of keeper/internal/pluginhost.
// Replaces [StubHost] in prod (see wire-up in keeper/cmd/keeper/main.go).
//
// Provider lookup is by `manifest.name` among the discovery cache, which is
// already filtered by [pluginhost.FilterByCatalog] against
// `keeper.yml::plugins.cloud_drivers[].name` (PM-decision delegation.md #1).
// Comparison is CASE-sensitive, same as in the catalog.
//
// Spawn cycle is one-shot per RPC (ADR-020(d), PM-decision delegation.md #3):
// Spawn → Create/Destroy → Close. No long-lived connections;
// isolation between tasks is guaranteed by a fresh plugin process.
type PluginAdapter struct {
	host      *pluginhost.Host
	providers map[string]pluginhost.Discovered
}

// NewPluginAdapter indexes the given discovery list by `manifest.name` for
// O(1) lookup in Create/Destroy. Duplicate names in discovery are not
// allowed: FilterByCatalog doesn't deduplicate, but the caller (wire-up in
// main.go) only feeds it cloud_driver plugins. A name collision returns an
// error — that's a configuration problem (two entries with the same
// `name`), not a runtime one.
func NewPluginAdapter(host *pluginhost.Host, discovered []pluginhost.Discovered) (*PluginAdapter, error) {
	if host == nil {
		return nil, errors.New("cloud adapter: pluginhost.Host is nil")
	}
	providers := make(map[string]pluginhost.Discovered, len(discovered))
	for _, d := range discovered {
		if d.Manifest == nil {
			continue
		}
		if d.Manifest.Kind != pluginhost.KindCloudDriver {
			continue
		}
		name := d.Manifest.Name
		if _, dup := providers[name]; dup {
			return nil, fmt.Errorf("cloud adapter: duplicate provider name %q in discovery", name)
		}
		providers[name] = d
	}
	return &PluginAdapter{host: host, providers: providers}, nil
}

// encodeStruct encodes map[string]any into *structpb.Struct; a nil/empty
// map → nil (the proto field stays unset). `field` is the name used for
// error context.
func encodeStruct(m map[string]any, field string) (*structpb.Struct, error) {
	if len(m) == 0 {
		return nil, nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: encode %s: %w", field, err)
	}
	return s, nil
}

// Providers returns the list of indexed provider names. Used for
// diagnostics on unknown-provider errors and in main's logging output.
func (a *PluginAdapter) Providers() []string {
	out := make([]string, 0, len(a.providers))
	for name := range a.providers {
		out = append(out, name)
	}
	return out
}

// Create is the [PluginHost.Create] implementation. One-shot spawn,
// server-stream Read until EOF, aggregating all VmInfo from every stream
// event. `driver` is the CloudDriver plugin name (= Provider.Type) it is
// registered under in the discovery cache.
func (a *PluginAdapter) Create(ctx context.Context, driver string, profile, credentials map[string]any, count int32, userdata, name string) ([]*pluginv1.VmInfo, error) {
	d, ok := a.providers[driver]
	if !ok {
		return nil, fmt.Errorf("cloud adapter: unknown driver %q (known: %v)", driver, a.Providers())
	}
	plugin, err := a.host.Spawn(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: spawn %s: %w", d.Manifest.Address(), err)
	}
	defer func() { _ = plugin.Close() }()

	cd, err := pluginhost.NewCloudDriverPlugin(plugin)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: wrap %s: %w", d.Manifest.Address(), err)
	}

	profileStruct, err := encodeStruct(profile, "profile")
	if err != nil {
		return nil, err
	}
	credsStruct, err := encodeStruct(credentials, "credentials")
	if err != nil {
		return nil, err
	}

	stream, err := cd.Create(ctx, &pluginv1.CreateRequest{
		Profile:     profileStruct,
		Count:       count,
		Credentials: credsStruct,
		Userdata:    userdata,
		Name:        name,
	})
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: create rpc %s: %w", d.Manifest.Address(), err)
	}

	vms, err := collectCreateVMs(stream)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: create stream %s: %w (stderr-tail: %s)",
			d.Manifest.Address(), err, plugin.StderrTail())
	}
	return vms, nil
}

// createEventStream is a narrow subset of
// grpc.ServerStreamingClient[CreateEvent] (Recv only), enough to aggregate
// the Create stream. The narrow interface lets [collectCreateVMs] be
// covered by a unit test without a live gRPC plugin.
type createEventStream interface {
	Recv() (*pluginv1.CreateEvent, error)
}

// collectCreateVMs reads the driver's Create stream until EOF and
// aggregates the created VMs. A driver failure MUST propagate as an error
// (never silently drop VMs): CreateEvent.failed=true means the whole
// operation failed (cluster read-only, quota, etc.), and the driver closes
// the stream after the first such event (contract in
// proto/plugin/v1/clouddriver.proto → CreateEvent). Symmetric with the
// stream-level handling of failed in [PluginAdapter.Resize].
//
// Returning an error here → applyCreated emits a failed-event → the
// `core.cloud.created` step fails → incarnation goes error_locked (NOT a
// false operational with 0 VMs). If the driver sent failed=true, the
// partially collected vms are NOT returned: provisioning failed as a
// whole, so a subset can't be onboarded as a success.
func collectCreateVMs(stream createEventStream) ([]*pluginv1.VmInfo, error) {
	var vms []*pluginv1.VmInfo
	for {
		ev, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		if ev.GetFailed() {
			msg := ev.GetMessage()
			if msg == "" {
				msg = "driver reported create failure without message"
			}
			return nil, fmt.Errorf("driver create failed: %s", msg)
		}
		if len(ev.GetVms()) > 0 {
			vms = append(vms, ev.GetVms()...)
		}
	}
	return vms, nil
}

// Destroy is the [PluginHost.Destroy] implementation. One-shot spawn,
// server-stream Read until EOF, aggregating `vm_id` from every event —
// these are the "actually deleted" ones (the provider may reject a
// subset, see the contract).
func (a *PluginAdapter) Destroy(ctx context.Context, driver string, credentials map[string]any, vmIDs []string) ([]string, error) {
	d, ok := a.providers[driver]
	if !ok {
		return nil, fmt.Errorf("cloud adapter: unknown driver %q (known: %v)", driver, a.Providers())
	}
	plugin, err := a.host.Spawn(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: spawn %s: %w", d.Manifest.Address(), err)
	}
	defer func() { _ = plugin.Close() }()

	cd, err := pluginhost.NewCloudDriverPlugin(plugin)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: wrap %s: %w", d.Manifest.Address(), err)
	}

	credsStruct, err := encodeStruct(credentials, "credentials")
	if err != nil {
		return nil, err
	}

	stream, err := cd.Destroy(ctx, &pluginv1.DestroyRequest{VmIds: vmIDs, Credentials: credsStruct})
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: destroy rpc %s: %w", d.Manifest.Address(), err)
	}

	var destroyed []string
	for {
		ev, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, fmt.Errorf("cloud adapter: destroy stream %s: %w (stderr-tail: %s)",
				d.Manifest.Address(), recvErr, plugin.StderrTail())
		}
		if id := ev.GetVmId(); id != "" {
			destroyed = append(destroyed, id)
		}
	}
	return destroyed, nil
}

// Status is the [PluginHost.Status] implementation. One-shot spawn, unary RPC.
func (a *PluginAdapter) Status(ctx context.Context, driver string, credentials map[string]any, vmID string) (*pluginv1.StatusReply, error) {
	d, ok := a.providers[driver]
	if !ok {
		return nil, fmt.Errorf("cloud adapter: unknown driver %q (known: %v)", driver, a.Providers())
	}
	plugin, err := a.host.Spawn(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: spawn %s: %w", d.Manifest.Address(), err)
	}
	defer func() { _ = plugin.Close() }()

	cd, err := pluginhost.NewCloudDriverPlugin(plugin)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: wrap %s: %w", d.Manifest.Address(), err)
	}

	credsStruct, err := encodeStruct(credentials, "credentials")
	if err != nil {
		return nil, err
	}

	rep, err := cd.Status(ctx, &pluginv1.StatusRequest{VmId: vmID, Credentials: credsStruct})
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: status rpc %s: %w (stderr-tail: %s)",
			d.Manifest.Address(), err, plugin.StderrTail())
	}
	return rep, nil
}

// List is the [PluginHost.List] implementation. One-shot spawn,
// server-stream Read until EOF, aggregating all VmInfo.
func (a *PluginAdapter) List(ctx context.Context, driver string, credentials, filter map[string]any) ([]*pluginv1.VmInfo, error) {
	d, ok := a.providers[driver]
	if !ok {
		return nil, fmt.Errorf("cloud adapter: unknown driver %q (known: %v)", driver, a.Providers())
	}
	plugin, err := a.host.Spawn(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: spawn %s: %w", d.Manifest.Address(), err)
	}
	defer func() { _ = plugin.Close() }()

	cd, err := pluginhost.NewCloudDriverPlugin(plugin)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: wrap %s: %w", d.Manifest.Address(), err)
	}

	credsStruct, err := encodeStruct(credentials, "credentials")
	if err != nil {
		return nil, err
	}
	filterStruct, err := encodeStruct(filter, "filter")
	if err != nil {
		return nil, err
	}

	stream, err := cd.List(ctx, &pluginv1.ListRequest{Filter: filterStruct, Credentials: credsStruct})
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: list rpc %s: %w", d.Manifest.Address(), err)
	}

	var vms []*pluginv1.VmInfo
	for {
		vm, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, fmt.Errorf("cloud adapter: list stream %s: %w (stderr-tail: %s)",
				d.Manifest.Address(), recvErr, plugin.StderrTail())
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

// Resize is the [PluginHost.Resize] implementation. One-shot spawn,
// server-stream Read until EOF, aggregating per-vm results from the final
// event. Stream-level failed=true (including resize.unsupported from a
// driver without the Resizable capability) becomes an error carrying the
// event's message — the module maps it to a failed-event.
func (a *PluginAdapter) Resize(ctx context.Context, driver string, credentials map[string]any, vmIDs []string, desired *pluginv1.ResizeSpec, allowDowntime bool) ([]*pluginv1.VmResizeResult, error) {
	d, ok := a.providers[driver]
	if !ok {
		return nil, fmt.Errorf("cloud adapter: unknown driver %q (known: %v)", driver, a.Providers())
	}
	plugin, err := a.host.Spawn(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: spawn %s: %w", d.Manifest.Address(), err)
	}
	defer func() { _ = plugin.Close() }()

	cd, err := pluginhost.NewCloudDriverPlugin(plugin)
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: wrap %s: %w", d.Manifest.Address(), err)
	}

	credsStruct, err := encodeStruct(credentials, "credentials")
	if err != nil {
		return nil, err
	}

	stream, err := cd.Resize(ctx, &pluginv1.ResizeRequest{
		VmIds:         vmIDs,
		Desired:       desired,
		AllowDowntime: allowDowntime,
		Credentials:   credsStruct,
	})
	if err != nil {
		return nil, fmt.Errorf("cloud adapter: resize rpc %s: %w", d.Manifest.Address(), err)
	}

	var results []*pluginv1.VmResizeResult
	for {
		ev, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, fmt.Errorf("cloud adapter: resize stream %s: %w (stderr-tail: %s)",
				d.Manifest.Address(), recvErr, plugin.StderrTail())
		}
		if ev.GetFailed() {
			// Stream-level failure of the whole operation (including
			// resize.unsupported from a driver without Resizable). Turn it
			// into an error — the module will emit a failed-event with
			// this message.
			return nil, fmt.Errorf("%s", ev.GetMessage())
		}
		if len(ev.GetResults()) > 0 {
			results = append(results, ev.GetResults()...)
		}
	}
	return results, nil
}
