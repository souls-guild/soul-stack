package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/pluginhost"
	"google.golang.org/grpc"
)

// PluginSpawner — узкий контракт над pluginhost.Host, нужен Registry для
// lazy-spawn-а sub-process плагинов. В продакшен-сборке реализуется *Host;
// в тестах — fake, чтобы не поднимать реальные бинари.
type PluginSpawner interface {
	Spawn(ctx context.Context, d pluginhost.Discovered) (PluginSession, error)
}

// PluginSession — узкий контракт над *pluginhost.Plugin для одного Apply-вызова.
// Объявлен здесь, чтобы можно было подменить в тестах без зависимости от
// сетевого подключения и subprocess-а.
type PluginSession interface {
	Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error)
	Close() error
}

// PluginHostSpawner — обёртка над *pluginhost.Host, удовлетворяющая
// PluginSpawner. Существует для адаптации типа: Host.Spawn возвращает
// *pluginhost.Plugin, а Registry оперирует интерфейсом PluginSession.
type PluginHostSpawner struct {
	Host *pluginhost.Host
}

func (s PluginHostSpawner) Spawn(ctx context.Context, d pluginhost.Discovered) (PluginSession, error) {
	p, err := s.Host.Spawn(ctx, d)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// PluginRegistry — реализация Registry над custom-modules, найденными
// pluginhost.Discover. Lookup возвращает обёртку pluginSoulModule, которая
// на каждый Apply-вызов делает Spawn → Apply → Close (one-shot, ADR-020(d)).
//
// Concurrency: mods защищена RWMutex — Rescan (hot-register из
// core.module.installed, ADR-065(d)) конкурентен с Lookup идущего прогона.
// Spawn-сессии независимы друг от друга — Host сам сериализует создание
// сокетов через atomic-counter.
type PluginRegistry struct {
	spawner PluginSpawner
	logger  *slog.Logger

	mu   sync.RWMutex
	mods map[string]pluginhost.Discovered
}

// NewPluginRegistry собирает registry. Имя ключа — `<namespace>.<name>`
// (manifest.Address()) — совпадает с тем, что приходит в RenderedTask.module
// до state-суффикса. Discovered с kind != soul_module пропускаются (defensive:
// Soul-host Discover может вернуть и soul_beacon, ADR-030 V5-2 — их регистрирует
// отдельный beacon-PluginRegistry).
func NewPluginRegistry(spawner PluginSpawner, discovered []pluginhost.Discovered, logger *slog.Logger) *PluginRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &PluginRegistry{spawner: spawner, mods: indexSoulModules(discovered), logger: logger}
}

func indexSoulModules(discovered []pluginhost.Discovered) map[string]pluginhost.Discovered {
	mods := make(map[string]pluginhost.Discovered, len(discovered))
	for _, d := range discovered {
		if d.Manifest == nil || d.Manifest.Kind != pluginhost.KindSoulModule {
			continue
		}
		mods[d.Manifest.Address()] = d
	}
	return mods
}

// Rescan — hot-register (ADR-065(d)): повторный полный discover каталога
// модулей и атомарная замена набора custom-модулей без рестарта демона.
// Возвращает discovery-warnings для логирования caller-ом (тем же стилем, что
// на старте). Beacon-реестр при Rescan НЕ пересобирается — MVP-ограничение
// ADR-065, hot-reload soul_beacon — post-MVP.
func (r *PluginRegistry) Rescan(modulesRoot string) ([]string, error) {
	discovered, warnings, err := pluginhost.Discover(modulesRoot)
	if err != nil {
		return warnings, err
	}
	mods := indexSoulModules(discovered)
	r.mu.Lock()
	r.mods = mods
	r.mu.Unlock()
	return warnings, nil
}

// Names возвращает список зарегистрированных custom-модулей.
func (r *PluginRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	return out
}

// Lookup возвращает обёртку SoulModule, которая делает one-shot spawn на
// каждый Apply. Возвращаемая module.SoulModule реализует только Apply;
// Validate/Plan возвращают BaseModule-defaults (apply-цикл MVP их не зовёт).
func (r *PluginRegistry) Lookup(name string) (module.SoulModule, bool) {
	r.mu.RLock()
	d, ok := r.mods[name]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return &pluginSoulModule{
		discovered: d,
		spawner:    r.spawner,
		logger:     r.logger,
	}, true
}

// pluginSoulModule — адаптер one-shot spawn-а под sdk/module.SoulModule.
// Apply-вызов делает spawn → Apply (stream) → пробрасывает ApplyEvent-ы в
// inProcApplyStream → Close. Любая ошибка stage-а превращается в error
// для runner-а (тот превратит в TaskEvent.failed=true).
type pluginSoulModule struct {
	module.BaseModule
	discovered pluginhost.Discovered
	spawner    PluginSpawner
	logger     *slog.Logger
}

func (m *pluginSoulModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	sess, err := m.spawner.Spawn(ctx, m.discovered)
	if err != nil {
		return fmt.Errorf("plugin_spawn: %w", err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			m.logger.Warn("plugin: close error",
				slog.String("module", m.discovered.Manifest.Address()),
				slog.Any("error", cerr),
			)
		}
	}()

	rpcStream, err := sess.Apply(ctx, req)
	if err != nil {
		return fmt.Errorf("plugin_apply_rpc: %w", err)
	}
	for {
		ev, err := rpcStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugin_apply_stream: %w", err)
		}
		if sendErr := stream.Send(ev); sendErr != nil {
			return fmt.Errorf("plugin_apply_forward: %w", sendErr)
		}
	}
}

// CompositeRegistry — Registry, проверяющий лукап последовательно по списку.
// Используется для соединения core + plugin: core имеет приоритет, чтобы
// custom-модуль с конфликтным именем (например, `core.pkg`) не подменил
// статический core. Конфликты логируются в Names() через лог-вызов
// конструктором cmd/soul.
type CompositeRegistry struct {
	layers []Registry
}

// NewCompositeRegistry порядок-зависимый: первый layer проверяется первым.
func NewCompositeRegistry(layers ...Registry) *CompositeRegistry {
	return &CompositeRegistry{layers: layers}
}

func (c *CompositeRegistry) Lookup(name string) (module.SoulModule, bool) {
	for _, l := range c.layers {
		if m, ok := l.Lookup(name); ok {
			return m, true
		}
	}
	return nil, false
}
