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

// PluginAdapter реализует [PluginHost] поверх keeper/internal/pluginhost.
// Заменяет [StubHost] в проде (см. wire-up в keeper/cmd/keeper/main.go).
//
// Lookup провайдера — по `manifest.name` среди discovery-кеша, который уже
// отфильтрован [pluginhost.FilterByCatalog] по `keeper.yml::plugins.cloud_drivers[].name`
// (PM-decision delegation.md #1). Сравнение CASE-sensitive, как и в каталоге.
//
// Spawn-цикл — one-shot per RPC (ADR-020(d), PM-decision delegation.md #3):
// Spawn → Create/Destroy → Close. Никаких long-lived connections;
// изоляция между задачами гарантируется новым процессом плагина.
type PluginAdapter struct {
	host      *pluginhost.Host
	providers map[string]pluginhost.Discovered
}

// NewPluginAdapter индексирует переданный discovery-список по `manifest.name`
// для O(1) lookup-а в Create/Destroy. Дубликаты имён в discovery недопустимы:
// FilterByCatalog не дедуплицирует, но caller (wire-up в main.go) подаёт
// только cloud_driver-плагины. При коллизии имени возвращается ошибка —
// это конфигурационная проблема (две записи с одинаковым `name`), не runtime.
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

// encodeStruct кодирует map[string]any в *structpb.Struct; nil/пустой map →
// nil (proto-поле остаётся unset). `field` — имя для error-контекста.
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

// Providers возвращает список индексированных имён провайдеров. Используется
// для диагностики при unknown-provider-ошибке и в logging-выводе main-а.
func (a *PluginAdapter) Providers() []string {
	out := make([]string, 0, len(a.providers))
	for name := range a.providers {
		out = append(out, name)
	}
	return out
}

// Create — реализация [PluginHost.Create]. Spawn one-shot, server-stream
// Read до EOF, аггрегация всех VmInfo из всех событий стрима. `driver` —
// имя CloudDriver-плагина (= Provider.Type), под которым он в discovery-кеше.
func (a *PluginAdapter) Create(ctx context.Context, driver string, profile, credentials map[string]any, count int32, userdata string) ([]*pluginv1.VmInfo, error) {
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

// createEventStream — узкое подмножество grpc.ServerStreamingClient[CreateEvent]
// (только Recv), достаточное для агрегации Create-стрима. Узкий интерфейс
// позволяет покрыть [collectCreateVMs] unit-тестом без живого gRPC-плагина.
type createEventStream interface {
	Recv() (*pluginv1.CreateEvent, error)
}

// collectCreateVMs читает Create-стрим драйвера до EOF и агрегирует созданные
// VM. Driver-failure ОБЯЗАН пропагироваться ошибкой (а не молча терять VM):
// CreateEvent.failed=true — это сбой всей операции (cluster read-only, quota,
// и т.п.), драйвер закрывает стрим после первого такого события (контракт
// proto/plugin/v1/clouddriver.proto → CreateEvent). Симметрично stream-level
// обработке failed в [PluginAdapter.Resize].
//
// Возврат ошибки здесь → applyCreated отдаёт failed-event → шаг
// `core.cloud.created` падает → incarnation error_locked (НЕ ложный
// operational с 0 VM). Если драйвер прислал failed=true, частично собранные
// vms НЕ возвращаются: провижн как целое провалился, нельзя онбордить
// подмножество как успех.
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

// Destroy — реализация [PluginHost.Destroy]. Spawn one-shot, server-stream
// Read до EOF, аггрегация `vm_id` из всех событий — это «фактически
// удалённые» (provider может отвергнуть подмножество, см. контракт).
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

// Status — реализация [PluginHost.Status]. Spawn one-shot, unary RPC.
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

// List — реализация [PluginHost.List]. Spawn one-shot, server-stream Read до
// EOF, аггрегация всех VmInfo.
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

// Resize — реализация [PluginHost.Resize]. Spawn one-shot, server-stream Read
// до EOF, агрегация per-vm результатов из финального события. Stream-level
// failed=true (включая resize.unsupported от драйвера без Resizable-capability)
// превращается в ошибку с message события — модуль маппит её в failed-event.
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
			// Stream-level сбой всей операции (включая resize.unsupported от
			// драйвера без Resizable). Превращаем в ошибку — модуль выдаст
			// failed-event с этим message.
			return nil, fmt.Errorf("%s", ev.GetMessage())
		}
		if len(ev.GetResults()) > 0 {
			results = append(results, ev.GetResults()...)
		}
	}
	return results, nil
}
