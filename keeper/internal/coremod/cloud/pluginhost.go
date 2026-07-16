package cloud

import (
	"context"
	"errors"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// PluginHost is a narrow subset of keeper/internal/pluginhost (ADR-020
// keeper-side runtime for CloudDriver plugins), needed by the
// `core.cloud.provisioned` module. Production implementation is [PluginAdapter]
// over keeper/internal/pluginhost.Host (see adapter.go). [StubHost] is kept
// for module unit tests and for wire builds without discovered plugins.
//
// Cross-import between proto/keeper and proto/plugin is prohibited (ADR-011 /
// ADR-012(g)), but keeper/internal/coremod/cloud is keeper-side, not proto:
// importing proto/plugin for VmInfo type is legitimate here (the same module
// is already imported in soul/internal/pluginhost).
type PluginHost interface {
	// Create instantiates `count` VMs via CloudDriver plugin `driver` (=
	// Provider.Type) with the given profile. credentials is the provider's
	// plain-secret (+region), resolved by Keeper from the Provider registry
	// (A-flow); userdata is a cloud-init blob for bootstrapping the soul agent.
	// name is the base name for the VM batch (self-onboard Variant T, ADR-017(h)):
	// the driver names the i-th VM as `<name>-<index>` so FQDN is predictable
	// to Keeper; empty → driver names at its discretion. Stream aggregation is
	// on the implementation side; the module receives final []VmInfo (or error
	// on stream-fail).
	Create(ctx context.Context, driver string, profile, credentials map[string]any, count int32, userdata, name string) ([]*pluginv1.VmInfo, error)

	// Destroy removes VMs with the given vm_id via `driver` (= Provider.Type).
	// credentials is the provider's plain-secret (+region), resolved by Keeper.
	// Returns a list of actually deleted vm_id (provider may reject a subset).
	Destroy(ctx context.Context, driver string, credentials map[string]any, vmIDs []string) ([]string, error)

	// Status queries the state of a single VM via `driver` (= Provider.Type).
	// credentials is the same A-flow as in Create/Destroy (resolved from Provider
	// registry). Returns provider-specific state string + additional VM attributes.
	Status(ctx context.Context, driver string, credentials map[string]any, vmID string) (*pluginv1.StatusReply, error)

	// List enumerates VMs known to the provider (optionally filtered).
	// credentials is the same A-flow. Stream aggregation is on the implementation
	// side; the module receives final []VmInfo.
	List(ctx context.Context, driver string, credentials, filter map[string]any) ([]*pluginv1.VmInfo, error)

	// Resize expands VM resources (cpu/ram/disk, in our units) via CloudDriver
	// plugin `driver` (= Provider.Type). desired is the target spec (fields with 0
	// do not change); allowDowntime permits stop/start. credentials is the same
	// A-flow. Stream aggregation is on the implementation side; the module receives
	// per-vm results (or error on stream-fail / resize.unsupported).
	Resize(ctx context.Context, driver string, credentials map[string]any, vmIDs []string, desired *pluginv1.ResizeSpec, allowDowntime bool) ([]*pluginv1.VmResizeResult, error)
}

// ErrPluginHostNotImplemented is a sentinel error for [StubHost], which the
// module maps to a `failed`-event with a readable message. Production uses
// [PluginAdapter], which returns structured errors for spawn/RPC phases.
var ErrPluginHostNotImplemented = errors.New("cloud pluginhost: not implemented (using StubHost — wire PluginAdapter in main)")

// StubHost is a minimal PluginHost implementation for module unit tests and for
// Keeper builds without discovered plugins. All methods return
// [ErrPluginHostNotImplemented]; production main wiring injects [PluginAdapter]
// instead of StubHost.
type StubHost struct{}

func (StubHost) Create(_ context.Context, _ string, _, _ map[string]any, _ int32, _, _ string) ([]*pluginv1.VmInfo, error) {
	return nil, ErrPluginHostNotImplemented
}

func (StubHost) Destroy(_ context.Context, _ string, _ map[string]any, _ []string) ([]string, error) {
	return nil, ErrPluginHostNotImplemented
}

func (StubHost) Status(_ context.Context, _ string, _ map[string]any, _ string) (*pluginv1.StatusReply, error) {
	return nil, ErrPluginHostNotImplemented
}

func (StubHost) List(_ context.Context, _ string, _, _ map[string]any) ([]*pluginv1.VmInfo, error) {
	return nil, ErrPluginHostNotImplemented
}

func (StubHost) Resize(_ context.Context, _ string, _ map[string]any, _ []string, _ *pluginv1.ResizeSpec, _ bool) ([]*pluginv1.VmResizeResult, error) {
	return nil, ErrPluginHostNotImplemented
}
