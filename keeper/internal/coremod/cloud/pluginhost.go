package cloud

import (
	"context"
	"errors"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// PluginHost — узкое подмножество keeper/internal/pluginhost (ADR-020
// keeper-side runtime для CloudDriver-плагинов), нужное модулю
// `core.cloud.provisioned`. Прод-реализация — [PluginAdapter] поверх
// keeper/internal/pluginhost.Host (см. adapter.go). [StubHost] оставлен
// для unit-тестов модуля и для wire-сборок без discovered-плагинов.
//
// Cross-import между proto/keeper и proto/plugin запрещён (ADR-011 / ADR-012(g)),
// но keeper/internal/coremod/cloud — это keeper-side, не proto: импорт
// proto/plugin для типа VmInfo здесь легитимен (тот же модуль уже импортируется
// в soul/internal/pluginhost).
type PluginHost interface {
	// Create создаёт `count` VM через CloudDriver-плагин `driver` (= Provider.Type)
	// с заданным profile. credentials — plain-секрет провайдера (+region),
	// резолвленный Keeper-ом из Provider-реестра (A-flow); userdata — cloud-init
	// blob для bootstrap soul-агента. Stream-агрегация — на стороне реализации;
	// модуль получает финальный []VmInfo (или ошибку при stream-fail).
	Create(ctx context.Context, driver string, profile, credentials map[string]any, count int32, userdata string) ([]*pluginv1.VmInfo, error)

	// Destroy удаляет VM с указанными vm_id через `driver` (= Provider.Type).
	// credentials — plain-секрет провайдера (+region), резолвленный Keeper-ом.
	// Возвращает список фактически удалённых vm_id (provider может отвергнуть
	// подмножество).
	Destroy(ctx context.Context, driver string, credentials map[string]any, vmIDs []string) ([]string, error)

	// Status опрашивает состояние одной VM через `driver` (= Provider.Type).
	// credentials — тот же A-flow, что и в Create/Destroy (резолв из Provider-
	// реестра). Возвращает provider-specific state-строку + дополнительные
	// атрибуты VM.
	Status(ctx context.Context, driver string, credentials map[string]any, vmID string) (*pluginv1.StatusReply, error)

	// List перечисляет VM, известных провайдеру (отфильтрованные опц. фильтром).
	// credentials — тот же A-flow. Stream-агрегация — на стороне реализации;
	// модуль получает финальный []VmInfo.
	List(ctx context.Context, driver string, credentials, filter map[string]any) ([]*pluginv1.VmInfo, error)
}

// ErrPluginHostNotImplemented — sentinel-ошибка [StubHost], которую модуль
// маппит в `failed`-event с понятным message. В проде используется
// [PluginAdapter], который возвращает структурированные ошибки spawn-/RPC-фаз.
var ErrPluginHostNotImplemented = errors.New("cloud pluginhost: not implemented (using StubHost — wire PluginAdapter in main)")

// StubHost — minimal-реализация PluginHost для unit-тестов модуля и для
// сборок keeper без discovered-плагинов. Все методы возвращают
// [ErrPluginHostNotImplemented]; прод-сборка main-а инжектирует
// [PluginAdapter] вместо StubHost.
type StubHost struct{}

func (StubHost) Create(_ context.Context, _ string, _, _ map[string]any, _ int32, _ string) ([]*pluginv1.VmInfo, error) {
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
