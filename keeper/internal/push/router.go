package push

// router.go — 3-tier resolver выбора SshProvider-плагина по SID (ADR-032
// amendment 2026-05-27, P2 W-3 Multi-provider routing).
//
// Контекст. До P2 single-provider pilot держал ровно одного SshProvider-плагина
// per-keeper: оператор настраивал `vault-bastion` ИЛИ `static`, не оба
// одновременно. P2 вводит карту провайдеров (W-2) и резолвер их выбора
// per-SID — оператор может одновременно поднять несколько SshProvider-ов и
// маршрутизировать SID-ы между ними (smoke prod-env через `static`, prod —
// через `vault-bastion`).
//
// Selector R1 (architect-decisions 2026-05-27, 3-tier resolve):
//
//	Level 1: souls.ssh_target.ssh_provider    (per-SID explicit)
//	Level 2: push.coven_default_providers     (per-coven default)
//	Level 3: push.cluster_default_provider    (cluster fallback)
//
// Tiebreak при множественном coven-match (Soul в нескольких ковенах, каждый
// настроен на свой провайдер): алфавитный порядок имён ковенов (детерминизм).
//
// Все три уровня пусты → ErrProviderNotRouted → fail per-host. БЕЗ
// provider-chain fallback: auth-perimeter разных providers разный, silent
// fallback ломает trust-инвариант (security-first).

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// ProviderRouter резолвит имя SshProvider-плагина для конкретного SID.
// Используется в pushorch.PushRun.executeAsync до dispatch-фазы.
//
// Сужено до одного метода — позволяет fake-ить в unit-тестах без подъёма PG.
type ProviderRouter interface {
	RouteFor(ctx context.Context, sid string) (providerName string, source RouteSource, err error)
}

// RouteSource — какой из трёх уровней резолва дал ответ. Несётся в audit-
// summary (`push_runs.summary.hosts[sid].route_source`) и в Prometheus-counter
// `keeper_push_provider_routed_total{provider, decision_source}` (низкая
// cardinality: per-provider × 3 labels = единицы серий).
type RouteSource int

const (
	// SourceUnknown — нулевое значение (программная ошибка, не валидный путь
	// резолва). Используется только как defensive default.
	SourceUnknown RouteSource = 0
	// SourceSoul — Level 1, per-SID explicit (`souls.ssh_target.ssh_provider`).
	SourceSoul RouteSource = iota
	// SourceCoven — Level 2, per-coven default
	// (`push.coven_default_providers[<coven>]`).
	SourceCoven
	// SourceCluster — Level 3, cluster fallback (`push.cluster_default_provider`).
	SourceCluster
)

// String — kebab-case label для логов / audit-payload.
func (s RouteSource) String() string {
	switch s {
	case SourceSoul:
		return "soul"
	case SourceCoven:
		return "coven"
	case SourceCluster:
		return "cluster"
	default:
		return "unknown"
	}
}

// ErrProviderNotRouted — sentinel: ни Level 1, ни Level 2, ни Level 3 не
// дали имя SshProvider. caller (pushorch) маппит на per-host status="error"
// + error_code="provider_not_routed".
var ErrProviderNotRouted = errors.New("push: SshProvider not routed (no per-SID / per-coven / cluster default)")

// PGRouterReader — узкая поверхность над storage-слоем soul.* для PG-резолва
// `ssh_target.ssh_provider` и списка `coven`-меток Soul-а. Сужено под router-у
// (TargetReader из target_pg.go нацелен на полный SSHTarget, а router-у нужны
// только два поля — выделено чтобы unit-тесты не тащили лишнее).
type PGRouterReader interface {
	// SelectSshTarget — для чтения `ssh_target.ssh_provider` (Level 1).
	SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error)
	// SelectCovens — для чтения списка `coven` Soul-а (Level 2 lookup
	// per-coven default-карты). Возврат пустого слайса при отсутствии меток.
	SelectCovens(ctx context.Context, sid string) ([]string, error)
}

// RouterConfig — read-only snapshot конфига cluster-defaults. Передаётся как
// функция-снимок, чтобы hot-reload (config.Store.OnReload) переустанавливал
// карту без пересоздания PGRouter-а: routing-decisions берут свежий снимок на
// каждый RouteFor.
//
// CovenDefaultProviders — map coven-имя → provider-имя.
// ClusterDefaultProvider — fallback при отсутствии match-а.
type RouterConfig struct {
	CovenDefaultProviders  map[string]string
	ClusterDefaultProvider string
}

// RouterConfigSource — источник свежего snapshot-а конфига. Реализуется
// daemon-wire-up-ом обёрткой над config.Store. nil-указатель опасен (router
// без конфига — фактически только Level 1 / fail) — caller обязан передать
// non-nil.
type RouterConfigSource interface {
	Snapshot() RouterConfig
}

// staticRouterConfig — static-snapshot для unit-тестов и единичного init-снимка
// при отсутствии hot-reload.
type staticRouterConfig struct {
	cfg RouterConfig
}

// NewStaticRouterConfigSource — обёртка для тестов.
func NewStaticRouterConfigSource(cfg RouterConfig) RouterConfigSource {
	return &staticRouterConfig{cfg: cfg}
}

func (s *staticRouterConfig) Snapshot() RouterConfig { return s.cfg }

// PGRouter — production-implementation [ProviderRouter] поверх storage-слоя
// soul.* и snapshot-конфига cluster-defaults.
//
// Алгоритм RouteFor:
//
//  1. SELECT souls.ssh_target.ssh_provider → если непустое → SourceSoul.
//  2. SELECT souls.coven[] → для каждого coven (alphabetical) lookup в
//     CovenDefaultProviders → первый match → SourceCoven.
//  3. ClusterDefaultProvider непустой → SourceCluster.
//  4. Иначе ErrProviderNotRouted.
type PGRouter struct {
	Reader PGRouterReader
	Config RouterConfigSource
}

// NewPGRouter валидирует зависимости и возвращает router.
func NewPGRouter(reader PGRouterReader, cfg RouterConfigSource) (*PGRouter, error) {
	if reader == nil {
		return nil, errors.New("push: PGRouter requires Reader")
	}
	if cfg == nil {
		return nil, errors.New("push: PGRouter requires Config")
	}
	return &PGRouter{Reader: reader, Config: cfg}, nil
}

// RouteFor реализует [ProviderRouter].
func (r *PGRouter) RouteFor(ctx context.Context, sid string) (string, RouteSource, error) {
	// Level 1: per-SID explicit. soul.ErrSoulNotFound пробрасываем — это
	// нештатный путь (caller обычно уже валидировал Soul-row), но не наша
	// ответственность маскировать.
	target, err := r.Reader.SelectSshTarget(ctx, sid)
	if err != nil {
		return "", SourceUnknown, fmt.Errorf("router: select ssh_target %s: %w", sid, err)
	}
	if target != nil && target.SSHProvider != nil && *target.SSHProvider != "" {
		return *target.SSHProvider, SourceSoul, nil
	}

	cfg := r.Config.Snapshot()

	// Level 2: per-coven default. Tiebreak — алфавитный порядок имён ковенов
	// (детерминизм). Линейный sort на короткой выборке (Soul обычно в 1-3
	// ковенах); скан карты — тоже короткий.
	if len(cfg.CovenDefaultProviders) > 0 {
		covens, err := r.Reader.SelectCovens(ctx, sid)
		if err != nil {
			return "", SourceUnknown, fmt.Errorf("router: select covens %s: %w", sid, err)
		}
		if len(covens) > 0 {
			sortedCovens := make([]string, len(covens))
			copy(sortedCovens, covens)
			sort.Strings(sortedCovens)
			for _, c := range sortedCovens {
				if provider, ok := cfg.CovenDefaultProviders[c]; ok && provider != "" {
					return provider, SourceCoven, nil
				}
			}
		}
	}

	// Level 3: cluster fallback.
	if cfg.ClusterDefaultProvider != "" {
		return cfg.ClusterDefaultProvider, SourceCluster, nil
	}

	return "", SourceUnknown, fmt.Errorf("%w: sid=%s", ErrProviderNotRouted, sid)
}
