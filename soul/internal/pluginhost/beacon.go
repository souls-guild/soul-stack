package pluginhost

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// BeaconPlugin — Soul-side handle спавн-сессии плагина kind=soul_beacon
// (ADR-030 V5-2). Embed-ит [sharedhost.BasePlugin] (manifest / conn / Close /
// StderrTail) и добавляет gRPC-клиент SoulBeacon для Validate / Check.
//
// Жизненный цикл — one-shot per Spawn (ADR-020(d)): caller вызывает
// [Host.SpawnBeacon] → серия RPC (обычно один Check) → [BeaconPlugin.Close].
// Beacon-scheduler оборачивает каждый per-tick Check в отдельный Spawn,
// поэтому connection-pool не нужен; задача оптимизации long-lived process для
// частых ticks — отдельная (см. ADR-030 V5-2 ТЗ, secondary item).
type BeaconPlugin struct {
	*sharedhost.BasePlugin
	client pluginv1.SoulBeaconClient
}

// newBeaconFromBase оборачивает generic [sharedhost.BasePlugin] в Soul-side
// kind-specific [BeaconPlugin]. Используется только из [Host.SpawnBeacon] —
// публичного конструктора нет (caller не должен спавнить BasePlugin сам).
func newBeaconFromBase(base *sharedhost.BasePlugin) *BeaconPlugin {
	return &BeaconPlugin{
		BasePlugin: base,
		client:     pluginv1.NewSoulBeaconClient(base.Conn()),
	}
}

// Validate — RPC SoulBeacon.Validate. Пробрасывается caller-у без оборачивания
// в TaskError — это задача scheduler-а (на ошибку validate scheduler логирует и
// не запускает Vigil, baseline не устанавливается).
func (p *BeaconPlugin) Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	return p.client.Validate(ctx, req)
}

// Check — RPC SoulBeacon.Check. Возвращает state + payload + state_cookie.
// Scheduler сравнивает state с last per-Vigil; при смене формирует Portent.
func (p *BeaconPlugin) Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	return p.client.Check(ctx, req)
}
