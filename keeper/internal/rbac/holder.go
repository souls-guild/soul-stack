package rbac

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultRefreshInterval — период TTL-перечита RBAC-снимка из БД (ADR-028(d),
// Фаза 1 = B1 TTL-poll). Мутации ролей/membership редки, окно устаревания
// мало — секунды приемлемы. Redis pub/sub-инвалидация (B2) — Фаза 3,
// см. [Holder.WatchInvalidations].
const DefaultRefreshInterval = 10 * time.Second

// InvalidationSource — поверхность подписки на cluster-wide RBAC-инвалидацию
// (ADR-028(d), B2). Реализуется в `keeper run` адаптером поверх
// [keeperredis.SubscribeRBACInvalidate]; объявлена интерфейсом, чтобы
// [Holder.WatchInvalidations] тестировался без Redis-а (fake-источник).
//
// Watch блокируется до ctx.Done(), вызывая onInvalidate на каждое полученное
// invalidate-сообщение (self-origin уже отфильтрован источником). Возврат
// ошибки — фатальная проблема подписки; caller (Holder) логирует и
// деградирует на чистый TTL-poll (fail-soft).
type InvalidationSource interface {
	Watch(ctx context.Context, onInvalidate func()) error
}

// SnapshotSource — поверхность загрузки RBAC-снимка из БД. Реализуется
// функцией поверх [LoadSnapshot] + pool; объявлена интерфейсом, чтобы Holder
// тестировался без Postgres-а (fake-источник).
type SnapshotSource interface {
	Load(ctx context.Context) (*Snapshot, error)
}

// PoolSource — SnapshotSource поверх pgx-pool (реальный источник в `keeper run`).
type PoolSource struct {
	DB ExecQueryRower
}

// Load читает снимок тремя SELECT-ами через [LoadSnapshot].
func (s PoolSource) Load(ctx context.Context) (*Snapshot, error) {
	return LoadSnapshot(ctx, s.DB)
}

// Holder — owner текущего [*Enforcer], построенного из БД-снимка (ADR-028(d)).
//
// Стратегия обновления (Фаза 1 = B1, TTL-poll):
//   - На старте [NewHolder] синхронно строит первый Enforcer из БД (fatal на
//     ошибке — daemon не должен подниматься с пустым/битым RBAC).
//   - Фоновая goroutine ([Run]) перечитывает снимок каждые refreshInterval.
//     Ошибка перечита не сбрасывает снимок: Holder оставляет прежний Enforcer
//     и пишет warn (БД-сбой не должен превратить всех в default-deny).
//
// Redis pub/sub-инвалидация (B2) — Фаза 3, здесь НЕ реализована.
//
// Конкурентность: текущий Enforcer хранится под Mutex-ом; Check и refresh
// сериализуются на коротком критическом участке (swap указателя). Профиль
// использования — admin-API (десятки RPS максимум), не data-path.
type Holder struct {
	src      SnapshotSource
	interval time.Duration
	logger   *slog.Logger

	mu  sync.Mutex
	cur *Enforcer

	// metrics — keeper_rbac_*-дескриптор, инжектится через [SetMetrics] в
	// setup-фазе daemon-а (после создания registry, который поднимается позже
	// NewHolder). nil до wire-up и в тестах/bootstrap — все Observe* no-op
	// (метод на nil *RBACMetrics — no-op, поэтому nil-Load безопасен).
	//
	// atomic.Pointer, а не обычное поле: SetMetrics вызывается в setup-фазе
	// daemon-а уже ПОСЛЕ старта Run-goroutine (go Holder.Run в setupRBAC, а
	// SetMetrics — позже в setupMetricsRegistry). В этом окне Run конкурентно
	// читает metrics из Refresh/ObserveInvalidation — обычное поле даёт data
	// race. Check здесь горячий не мьютексом, а Load-ом (дешевле, не блокирует).
	metrics atomic.Pointer[RBACMetrics]
}

// SetMetrics привязывает keeper_rbac_*-дескриптор. Безопасно конкурентно с
// фоновыми читателями ([Run]/[WatchInvalidations]) через atomic.Pointer.
// nil-получатель — no-op (симметрия с прочими nil-safe-обёртками Holder-а).
// Вызывается из daemon `setupMetricsRegistry` после [RegisterRBACMetrics];
// до вызова метрики nil (Load вернёт nil) и не публикуются.
func (h *Holder) SetMetrics(m *RBACMetrics) {
	if h == nil {
		return
	}
	h.metrics.Store(m)
}

// NewHolder строит первоначальный Enforcer из БД-снимка. Ошибка первичной
// загрузки — fatal (caller-у возвращается err, daemon падает на старте при
// недоступной/битой RBAC-схеме).
//
// nil-src разрешён — Holder ведёт себя как пустой snapshot (default deny),
// фоновый refresh при этом no-op. Используется тестами и code-path-ами, где
// pool ещё не инициализирован; на практике в `keeper run` src всегда non-nil.
//
// interval <= 0 → [DefaultRefreshInterval]. logger nil → slog.Default().
func NewHolder(ctx context.Context, src SnapshotSource, interval time.Duration, logger *slog.Logger) (*Holder, error) {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	h := &Holder{
		src:      src,
		interval: interval,
		logger:   logger,
	}
	if src == nil {
		enf, err := NewEnforcerFromSnapshot(nil)
		if err != nil {
			return nil, err
		}
		h.cur = enf
		return h, nil
	}
	snap, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	enf, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		return nil, err
	}
	h.cur = enf
	return h, nil
}

// Run запускает фоновый TTL-перечит снимка до отмены ctx (Фаза 1, B1).
// Блокирующий — caller запускает в отдельной goroutine. При nil-src сразу
// выходит (нечего перечитывать).
//
// Ошибка перечита логируется (warn) и НЕ меняет активный Enforcer — окно
// устаревания меньше зла «БД мигнула → весь кластер default-deny».
func (h *Holder) Run(ctx context.Context) {
	if h.src == nil {
		return
	}
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.refresh(ctx)
		}
	}
}

// WatchInvalidations подписывается на cluster-wide RBAC-инвалидацию (B2 =
// B1 + pub/sub, ADR-028(d)) и на каждый сигнал делает [Holder.refresh]
// (near-instant перечит снимка из БД, вместо ожидания TTL-poll-а из [Run]).
// Блокирующий — caller запускает в отдельной goroutine, завершается по
// ctx.Done().
//
// TTL-poll ([Run]) НЕ заменяется: pub/sub без persistence (потеря сообщения,
// reconnect) → следующий тик [Run] всё равно перечитает снимок. Это
// fail-soft-слой поверх fallback-а.
//
// nil-src ([Holder] построен без БД-источника) или nil-инвалидатор → no-op
// (нечего перечитывать / некуда подписываться). Ошибка подписки логируется
// (warn) и НЕ роняет daemon — RBAC продолжает обновляться TTL-poll-ом.
func (h *Holder) WatchInvalidations(ctx context.Context, src InvalidationSource) {
	if h.src == nil || src == nil {
		return
	}
	err := src.Watch(ctx, func() {
		h.metrics.Load().ObserveInvalidation()
		h.refresh(ctx)
	})
	if err != nil && ctx.Err() == nil {
		h.logger.Warn("rbac: подписка на cluster-инвалидацию завершилась с ошибкой, остаётся TTL-poll",
			slog.Any("error", err),
		)
	}
}

// Refresh принудительно перечитывает снимок (lazy-путь / тесты). Возвращает
// ошибку загрузки/парсинга; при ошибке активный Enforcer не меняется.
//
// Метрики keeper_rbac_snapshot_*: весь rebuild (Load + NewEnforcerFromSnapshot)
// засекается, фаза отказа различается явно (load/parse), на успехе пишутся
// timestamp + counts ролей/операторов из построенного enforcer-а.
func (h *Holder) Refresh(ctx context.Context) error {
	if h.src == nil {
		return nil
	}
	m := h.metrics.Load()
	start := time.Now()
	snap, err := h.src.Load(ctx)
	if err != nil {
		m.ObserveRebuildError(time.Since(start), rebuildErrorLoad)
		return err
	}
	enf, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		m.ObserveRebuildError(time.Since(start), rebuildErrorParse)
		return err
	}
	m.ObserveRebuildSuccess(time.Since(start), enf.RoleCount(), enf.OperatorCount())
	h.mu.Lock()
	h.cur = enf
	h.mu.Unlock()
	return nil
}

// refresh — внутренний best-effort перечит для фоновой goroutine: ошибку
// логирует, не пробрасывает.
func (h *Holder) refresh(ctx context.Context) {
	if err := h.Refresh(ctx); err != nil {
		h.logger.Warn("rbac: TTL-refresh снимка из БД не удался, оставлен прежний enforcer",
			slog.Any("error", err),
		)
	}
}

// current возвращает актуальный Enforcer под Mutex-ом.
func (h *Holder) current() *Enforcer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}

// Check делегирует в текущий enforcer. См. [Enforcer.Check]. Это единственная
// точка permission-проверки в `keeper run` (api-middleware и MCP получают
// Holder как PermissionChecker), поэтому keeper_rbac_checks_total
// инкрементируется здесь — горячий путь не меняется, добавлен один nil-safe
// Inc на счётчик.
func (h *Holder) Check(aid, resource, action string, context map[string]string) error {
	err := h.current().Check(aid, resource, action, context)
	h.metrics.Load().ObserveCheck(err)
	return err
}

// HasWildcard — true, если у AID есть хотя бы одна `*`-permission через любую
// из ролей текущего снимка. См. [Enforcer.HasWildcard].
func (h *Holder) HasWildcard(aid string) bool {
	return h.current().HasWildcard(aid)
}

// ClusterAdmins — список AID-ов с активной wildcard-permission в текущем
// снимке. См. [Enforcer.ClusterAdmins].
func (h *Holder) ClusterAdmins() []string {
	return h.current().ClusterAdmins()
}

// IsRevoked — ревокнут ли AID в текущем снимке. См. [Enforcer.IsRevoked].
// Нужна обмену cookie→Bearer (POST /auth/token, NIM-77): in-memory revoked-чек.
func (h *Holder) IsRevoked(aid string) bool {
	return h.current().IsRevoked(aid)
}

// RolesOf — имена ролей AID-а в текущем снимке. См. [Enforcer.RolesOf].
func (h *Holder) RolesOf(aid string) []string {
	return h.current().RolesOf(aid)
}

// CovenScope — coven-scope AID-а для (resource, action) в текущем снимке.
// См. [Enforcer.CovenScope].
func (h *Holder) CovenScope(aid, resource, action string) ([]string, bool) {
	return h.current().CovenScope(aid, resource, action)
}

// ResolvePurview — scope-граница AID-а (Purview по измерениям) для
// (resource, action) в текущем снимке. См. [Enforcer.ResolvePurview]. Нужна
// scoped-видимости `GET /v1/souls` (ADR-047 S3b, keeper/internal/soulpurview).
func (h *Holder) ResolvePurview(aid, resource, action string) Purview {
	return h.current().ResolvePurview(aid, resource, action)
}

// HoldsAction — existence-gate read-эндпоинтов (ADR-047 §г amendment
// 2026-06-04): держит ли AID действие хоть в каком-то scope, в текущем снимке.
// См. [Enforcer.HoldsAction]. Нужна middleware-у [RequireAction] (G1/G2).
func (h *Holder) HoldsAction(aid, resource, action string) bool {
	return h.current().HoldsAction(aid, resource, action)
}

// PermissionsOf — эффективные права AID-а в текущем снимке (self-describing
// `GET /v1/me/permissions`). См. [Enforcer.PermissionsOf].
func (h *Holder) PermissionsOf(aid string) []EffectivePermission {
	return h.current().PermissionsOf(aid)
}
