package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrPushProviderNotConfigured — sentinel-ошибка резолва env-payload params:
// в PG-таблице `push_providers` нет записи для имени плагина, и
// legacy-fallback запрещён (`push.allow_legacy_push_providers=false`).
//
// Не ошибка: pilot S6 + legacy-fallback false означал «нет env-payload —
// плагин стартует с дефолтами», поэтому setupPushDispatchers НЕ обязан
// трактовать это как ошибку старта. Sentinel выделен для diagnostic-сообщения
// в логе wire-up («provider X не сконфигурирован в PG, fallback запрещён —
// плагин стартует без env-payload»).
var ErrPushProviderNotConfigured = errors.New("push: provider not configured in PG and legacy fallback disabled")

// PushProviderResolver — узкая поверхность над storage push_providers,
// нужная PG-резолверу. Реализуется обёрткой над pgxpool.Pool (см.
// [NewPGPushProviderReader]); unit-тесты подставляют fake.
type PushProviderResolver interface {
	SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error)
}

// pgPoolPushProviderReader — production-implementation [PushProviderResolver]
// поверх pgxpool.Pool / соответствующего pushprovider.ExecQueryRower.
type pgPoolPushProviderReader struct {
	db pushprovider.ExecQueryRower
}

// NewPGPushProviderReader адаптирует pgxpool.Pool (или любой
// pushprovider.ExecQueryRower) под [PushProviderResolver]. Используется
// setupPushDispatchers в daemon-wire-up.
func NewPGPushProviderReader(db pushprovider.ExecQueryRower) PushProviderResolver {
	return &pgPoolPushProviderReader{db: db}
}

func (r *pgPoolPushProviderReader) SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error) {
	return pushprovider.SelectByName(ctx, r.db, name)
}

// LegacyPushProvidersFallback — узкая поверхность над config-резолвом
// `keeper.yml::push.providers[]`. Реализуется тонким wrapper-ом в
// daemon-wire-up над `[]config.KeeperPushProvider`.
type LegacyPushProvidersFallback interface {
	ResolveParams(name string) (map[string]any, bool)
}

// configProvidersFallback — production-implementation
// [LegacyPushProvidersFallback] поверх `[]config.KeeperPushProvider` (inline-
// форма из pilot S6). Lookup по name линейный: список короткий (1-2 плагина в
// типичной инсталляции), мапу не строим.
type configProvidersFallback struct {
	entries []config.KeeperPushProvider
}

// NewLegacyConfigProvidersFallback оборачивает `keeper.yml::push.providers[]`
// в LegacyPushProvidersFallback.
func NewLegacyConfigProvidersFallback(providers []config.KeeperPushProvider) LegacyPushProvidersFallback {
	return &configProvidersFallback{entries: providers}
}

func (f *configProvidersFallback) ResolveParams(name string) (map[string]any, bool) {
	for _, e := range f.entries {
		if e.Name == name {
			return e.Params, true
		}
	}
	return nil, false
}

// PGFallbackProviderResolver — PG-first резолвер env-payload params SSH-
// плагина с опциональным fallback на keeper.yml::push.providers[]
// (ADR-032 amendment 2026-05-26, S7-2).
//
// Алгоритм ResolveParams:
//
//  1. SELECT push_providers по name:
//     - запись найдена → возвращаем `params` (может быть пустой объектом).
//     - [pushprovider.ErrPushProviderNotFound] → переход к шагу 2.
//     - прочие ошибки → пробрасываем (PG недоступна).
//
//  2. `AllowLegacy=false` (default S7-2) → возвращаем
//     [ErrPushProviderNotConfigured]; caller (daemon-wire-up) при этой ошибке
//     спокойно стартует плагин без env-payload.
//     `AllowLegacy=true` → одноразовый WARN deprecation-log + делегируем в
//     [Fallback] (резолвер поверх keeper.yml::push.providers[]).
//
// Семантика fail-safe: provider не сконфигурирован — НЕ ошибка старта;
// security-инвариант (sensitive params как vault-refs) проверяется в
// pushprovider.Service.Create/Update, не здесь — здесь нет операторской
// атаки, только чтение.
type PGFallbackProviderResolver struct {
	Reader       PushProviderResolver
	Fallback     LegacyPushProvidersFallback
	AllowLegacy  bool
	Logger       *slog.Logger
	legacyWarned sync.Once
}

// ResolveParams возвращает env-payload params плагина с именем pluginName.
// Семантика — см. doc-comment типа.
func (r *PGFallbackProviderResolver) ResolveParams(ctx context.Context, pluginName string) (map[string]any, error) {
	p, err := r.Reader.SelectByName(ctx, pluginName)
	if err == nil {
		if p.Params == nil {
			return map[string]any{}, nil
		}
		return p.Params, nil
	}
	if !errors.Is(err, pushprovider.ErrPushProviderNotFound) {
		return nil, fmt.Errorf("push: read push_providers %q: %w", pluginName, err)
	}

	// PG-запись отсутствует: переключаемся на legacy-fallback при флаге.
	if !r.AllowLegacy || r.Fallback == nil {
		return nil, fmt.Errorf("%w: %s", ErrPushProviderNotConfigured, pluginName)
	}

	r.legacyWarned.Do(func() {
		if r.Logger != nil {
			r.Logger.Warn("push: S7-2 deprecation: keeper.yml::push.providers[] используется как fallback; мигрируйте на push_providers через POST /v1/push-providers",
				slog.String("trigger_plugin", pluginName))
		}
	})
	params, ok := r.Fallback.ResolveParams(pluginName)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPushProviderNotConfigured, pluginName)
	}
	return params, nil
}
