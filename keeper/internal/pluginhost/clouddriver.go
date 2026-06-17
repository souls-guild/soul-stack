package pluginhost

import (
	"context"
	"fmt"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// CloudDriverPlugin — тонкая обёртка над [Plugin], привязывающая базовый handle
// к gRPC-клиенту CloudDriver. Создаётся через [NewCloudDriverPlugin] после
// успешного [Host.Spawn]: caller проверяет, что manifest.kind == cloud_driver,
// и оборачивает Plugin в CloudDriverPlugin.
//
// Apply-цикл keeper.cloud / scenario step `core.cloud.provisioned`
// (ADR-017) использует методы Create/Destroy/Status/List/Schema/Validate;
// stream-методы возвращают grpc-stream напрямую — caller сам читает события.
//
// Close проксируется на underlying Plugin.Close (идемпотентен).
type CloudDriverPlugin struct {
	*Plugin
	client pluginv1.CloudDriverClient
}

// NewCloudDriverPlugin оборачивает [Plugin] (из [Host.Spawn]) в kind-specific
// handle. Возвращает ошибку, если manifest.kind != cloud_driver: это защита от
// случайного вызова на soul_module / ssh_provider бинаре.
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

// Schema — RPC CloudDriver.Schema. Возвращает profile_schema плагина (должен
// совпадать с manifest.spec.profile_schema; cross-check — задача keeper.cloud).
func (c *CloudDriverPlugin) Schema(ctx context.Context, req *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return c.client.Schema(ctx, req)
}

// Validate — RPC CloudDriver.Validate. Runtime-проверки параметров профиля
// (квоты, доступность образа, валидность subnet) — то, что не выражается
// JSON Schema.
func (c *CloudDriverPlugin) Validate(ctx context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return c.client.Validate(ctx, req)
}

// Create — RPC CloudDriver.Create (server-streaming). Caller читает события
// прогресса до EOF.
func (c *CloudDriverPlugin) Create(ctx context.Context, req *pluginv1.CreateRequest) (grpc.ServerStreamingClient[pluginv1.CreateEvent], error) {
	return c.client.Create(ctx, req)
}

// Destroy — RPC CloudDriver.Destroy (server-streaming). Под guard-rails
// keeper.cloud (см. docs/keeper/cloud.md → Безопасность destroy).
func (c *CloudDriverPlugin) Destroy(ctx context.Context, req *pluginv1.DestroyRequest) (grpc.ServerStreamingClient[pluginv1.DestroyEvent], error) {
	return c.client.Destroy(ctx, req)
}

// Status — RPC CloudDriver.Status. Опрос состояния конкретной VM.
func (c *CloudDriverPlugin) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return c.client.Status(ctx, req)
}

// List — RPC CloudDriver.List (server-streaming). Перечисление VM, известных
// провайдеру; caller читает stream до EOF.
func (c *CloudDriverPlugin) List(ctx context.Context, req *pluginv1.ListRequest) (grpc.ServerStreamingClient[pluginv1.VmInfo], error) {
	return c.client.List(ctx, req)
}
