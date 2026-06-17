package beacon

import (
	"context"
	"fmt"
	"log/slog"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/protobuf/types/known/structpb"
)

// PluginBeaconSpawner — узкий контракт над pluginhost.Host для one-shot
// per-tick spawn beacon-плагина. В production — обёртка над
// *pluginhost.Host (Soul-side, kind=soul_beacon); в тестах — fake. Decouple
// держит beacon-пакет независимым от pluginhost-импорта (избегаем
// циклической ссылки и хост-deps в unit-тестах scheduler-а).
type PluginBeaconSpawner interface {
	SpawnBeacon(ctx context.Context, d sharedhost.Discovered) (PluginBeaconSession, error)
}

// PluginBeaconSession — узкий контракт над *pluginhost.BeaconPlugin для одного
// Check-вызова. Параллель [soul/internal/runtime.PluginSession] для SoulModule.
type PluginBeaconSession interface {
	Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error)
	Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error)
	Close() error
}

// PluginRegistry — реализация [BeaconLookup] над custom-beacon-плагинами,
// найденными pluginhost.Discover (kind=soul_beacon). Lookup возвращает обёртку
// [pluginBeacon], которая на каждый Check делает Spawn → Check → Close.
//
// Concurrency: read-only после конструктора, безопасен для конкурентных
// Lookup-ов. Spawn-сессии независимы — Host сериализует создание сокетов через
// atomic-counter (shared/pluginhost).
type PluginRegistry struct {
	spawner PluginBeaconSpawner
	beacons map[string]sharedhost.Discovered
	logger  *slog.Logger
}

// NewPluginRegistry собирает реестр. discovered — список плагинов
// kind=soul_beacon (caller отфильтровал по `d.Manifest.Kind`). Имя ключа —
// `<namespace>.<name>` (manifest.Address()), совпадает с VigilDef.check для
// plugin-beacon-адресов.
func NewPluginRegistry(spawner PluginBeaconSpawner, discovered []sharedhost.Discovered, logger *slog.Logger) *PluginRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	beacons := make(map[string]sharedhost.Discovered, len(discovered))
	for _, d := range discovered {
		if d.Manifest == nil || d.Manifest.Kind != sharedplugin.KindSoulBeacon {
			continue
		}
		beacons[d.Manifest.Address()] = d
	}
	return &PluginRegistry{spawner: spawner, beacons: beacons, logger: logger}
}

// Names — список зарегистрированных plugin-beacon-адресов.
func (r *PluginRegistry) Names() []string {
	out := make([]string, 0, len(r.beacons))
	for k := range r.beacons {
		out = append(out, k)
	}
	return out
}

// Lookup возвращает per-Vigil обёртку, реализующую [Beacon]. На каждый Check
// внутри обёртки идёт one-shot Spawn → Check → Close (ADR-020(d), parity с
// pluginSoulModule в runtime/pluginregistry.go).
func (r *PluginRegistry) Lookup(name string) (Beacon, bool) {
	d, ok := r.beacons[name]
	if !ok {
		return nil, false
	}
	return &pluginBeacon{
		discovered: d,
		spawner:    r.spawner,
		logger:     r.logger,
	}, true
}

// pluginBeacon — адаптер one-shot spawn-а под [Beacon]. Реализует только Check;
// scheduler не зовёт Validate напрямую (manifest-валидация на этапе создания
// Vigil оператором через OpenAPI).
type pluginBeacon struct {
	discovered sharedhost.Discovered
	spawner    PluginBeaconSpawner
	logger     *slog.Logger
}

// Check делает one-shot Spawn → SoulBeacon.Check → Close. Возвращаемые
// state/payload/error пробрасываются scheduler-у; non-fatal CheckReply.error
// (plugin-side soft error) транслируется в Go-ошибку, чтобы scheduler пропустил
// тик (baseline не трогается, parity с встроенным [Beacon].Check err).
func (p *pluginBeacon) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	sess, err := p.spawner.SpawnBeacon(ctx, p.discovered)
	if err != nil {
		return "", nil, fmt.Errorf("plugin_spawn: %w", err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			p.logger.Warn("beacon: plugin close error",
				slog.String("beacon", p.discovered.Manifest.Address()),
				slog.Any("error", cerr),
			)
		}
	}()
	reply, err := sess.Check(ctx, &pluginv1.CheckRequest{Params: params})
	if err != nil {
		return "", nil, fmt.Errorf("plugin_check_rpc: %w", err)
	}
	if reply.GetError() != "" {
		// Soft error от плагина — scheduler пропустит тик. Поднимаем как
		// ошибку Go (а не как state), чтобы baseline не двигался.
		return "", nil, fmt.Errorf("plugin_check_soft: %s", reply.GetError())
	}
	return reply.GetState(), reply.GetPayload(), nil
}
