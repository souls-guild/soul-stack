package pluginhost

import (
	"context"
	"fmt"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/grpc"
)

// Plugin — Soul-side handle спавн-сессии плагина kind=soul_module. Embed-ит
// [sharedhost.BasePlugin] (manifest / conn / Close / StderrTail) и добавляет
// gRPC-клиент SoulModule для Validate / Plan / Apply.
//
// Жизненный цикл — one-shot per Spawn (ADR-020(d)): caller вызывает [Host.Spawn]
// → серия RPC → [Plugin.Close]. Connection-pooling не предусмотрен.
type Plugin struct {
	*sharedhost.BasePlugin
	client pluginv1.SoulModuleClient
}

// newPluginFromBase оборачивает generic [sharedhost.BasePlugin] в Soul-side
// kind-specific [Plugin]. Используется только из [Host.Spawn] — публичного
// конструктора нет (caller не должен спавнить BasePlugin сам).
func newPluginFromBase(base *sharedhost.BasePlugin) *Plugin {
	return &Plugin{
		BasePlugin: base,
		client:     pluginv1.NewSoulModuleClient(base.Conn()),
	}
}

// errKindMismatch — ошибка Spawn-метода при попытке запустить плагин чужого
// kind-а в kind-specific wrap. Указывает на ошибку конструкции Discovered
// (например, в тесте) или на drift между Discover-фильтром и manifest-ом
// плагина.
func errKindMismatch(want, got string) error {
	return fmt.Errorf("pluginhost: expected kind=%s, got kind=%q", want, got)
}

// Validate — RPC SoulModule.Validate. Пробрасывается caller-у без оборачивания
// в TaskError — это задача apply-цикла (Core.b / M2.1.b.3).
func (p *Plugin) Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return p.client.Validate(ctx, req)
}

// Plan — RPC SoulModule.Plan. Возвращает stream — caller сам читает и
// агрегирует PlanEvent-ы.
func (p *Plugin) Plan(ctx context.Context, req *pluginv1.PlanRequest) (grpc.ServerStreamingClient[pluginv1.PlanEvent], error) {
	return p.client.Plan(ctx, req)
}

// Apply — RPC SoulModule.Apply. Возвращает stream — caller сам читает все
// ApplyEvent-ы (последний — финальный, с changed/failed/output согласно
// docs/destiny/tasks.md).
func (p *Plugin) Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error) {
	return p.client.Apply(ctx, req)
}
