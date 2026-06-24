package serviceregistry

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"
)

// DefaultRefreshInterval — период TTL-перечита снимка реестра из БД (S2,
// паттерн rbac.DefaultRefreshInterval). CRUD-мутации реестра редки, окно
// устаревания мало — секунды приемлемы. Redis pub/sub-инвалидация поверх
// (см. [Holder.WatchInvalidations]) сокращает задержку до миллисекунд.
const DefaultRefreshInterval = 10 * time.Second

// InvalidationSource — поверхность подписки на cluster-wide инвалидацию реестра
// (S2). Реализуется в `keeper run` адаптером поверх
// [keeperredis.SubscribeServiceInvalidate]; объявлена интерфейсом, чтобы
// [Holder.WatchInvalidations] тестировался без Redis-а (fake-источник).
//
// Watch блокируется до ctx.Done(), вызывая onInvalidate на каждое полученное
// invalidate-сообщение (self-origin уже отфильтрован источником). Возврат
// ошибки — фатальная проблема подписки; caller (Holder) логирует и деградирует
// на чистый TTL-poll (fail-soft), симметрично rbac.InvalidationSource.
type InvalidationSource interface {
	Watch(ctx context.Context, onInvalidate func()) error
}

// SnapshotSource — поверхность загрузки снимка реестра из БД. Реализуется
// [PoolSource] поверх ListServices + GetSetting; объявлена интерфейсом, чтобы
// Holder тестировался без Postgres-а (fake-источник).
type SnapshotSource interface {
	Load(ctx context.Context) (*Snapshot, error)
}

// Snapshot — иммутабельный согласованный срез реестра на момент чтения из БД:
// каталог Service-ов по имени + скаляры keeper_settings. Строится в [PoolSource]
// и целиком atomic-swap-ается в [Holder]; после публикации НЕ мутируется
// (геттеры отдают по значению).
type Snapshot struct {
	// services — каталог по PK service_registry.name. Значения по значению
	// (ServiceEntry — value-тип), поэтому Resolve безопасно отдаёт копию без
	// риска внешней мутации общего снимка.
	services map[string]ServiceEntry

	// defaultDestinySource — скаляр keeper_settings[default_destiny_source];
	// "" = настройка не задана (строки в keeper_settings нет).
	defaultDestinySource string

	// provisioningMethods — политика provisioning_allowed_methods (set
	// разрешённых created_via-методов СОЗДАНИЯ оператора). nil-map =
	// настройка НЕ задана (ключа в keeper_settings нет) → всё разрешено
	// (back-compat). non-nil = политика задана, разрешены ровно эти методы.
	// Битый-непустой ключ Load НЕ публикует (возвращает ошибку), поэтому
	// non-nil здесь всегда непустой набор из домена {user,ldap,oidc}.
	provisioningMethods map[string]bool
}

// PoolSource — [SnapshotSource] поверх pgx-pool (реальный источник в `keeper
// run`). Читает каталог Service-ов и well-known скаляры одним проходом.
type PoolSource struct {
	DB ExecQueryRower
}

// Load строит снимок: ListServices (весь каталог) + GetSetting на каждый
// well-known скаляр. Отсутствие строки настройки (ErrSettingNotFound) — не
// ошибка: скаляр остаётся пустым. Любая другая ошибка БД пробрасывается (Holder
// оставит прежний снимок).
func (s PoolSource) Load(ctx context.Context) (*Snapshot, error) {
	entries, err := ListServices(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	services := make(map[string]ServiceEntry, len(entries))
	for _, e := range entries {
		services[e.Name] = *e
	}

	dds, err := loadSettingValue(ctx, s.DB, SettingDefaultDestinySource)
	if err != nil {
		return nil, err
	}

	// provisioning_allowed_methods: ErrSettingNotFound → политика не задана
	// (nil-map, всё разрешено, back-compat). Найден → парсим: битый/пустой
	// (ErrEmptyProvisioningMethods / ErrInvalidProvisioningMethod) пробрасываем,
	// чтобы Holder НЕ опубликовал битый снимок (на старте NewHolder → fatal —
	// anti-lockout «не стартуем с битой политикой»; в runtime недостижимо, т.к.
	// PUT валидирует ДО записи, см. ProvisioningPolicyHandler).
	provMethods, err := loadProvisioningMethods(ctx, s.DB)
	if err != nil {
		return nil, err
	}

	return &Snapshot{
		services:             services,
		defaultDestinySource: dds,
		provisioningMethods:  provMethods,
	}, nil
}

// loadProvisioningMethods читает и парсит политику provisioning_allowed_methods.
// ErrSettingNotFound → (nil, nil) (политика не задана, всё разрешено); найден →
// [ParseProvisioningMethods] (битый/пустой → ошибка), прочие ошибки БД —
// пробрасываются.
func loadProvisioningMethods(ctx context.Context, db ExecQueryRower) (map[string]bool, error) {
	set, err := GetSetting(ctx, db, SettingProvisioningAllowedMethods)
	if err != nil {
		if isSettingNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseProvisioningMethods(set.Value)
}

// loadSettingValue читает значение настройки по ключу; ErrSettingNotFound →
// ("", nil) (настройка просто не задана), прочие ошибки пробрасываются.
func loadSettingValue(ctx context.Context, db ExecQueryRower, key string) (string, error) {
	set, err := GetSetting(ctx, db, key)
	if err != nil {
		if isSettingNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return set.Value, nil
}

func isSettingNotFound(err error) bool {
	// GetSetting возвращает ErrSettingNotFound напрямую (sentinel), не wrapped.
	return err == ErrSettingNotFound //nolint:errorlint // sentinel из этого же пакета, не оборачивается
}

// Holder — owner текущего [*Snapshot] реестра, построенного из БД (S2).
//
// Стратегия обновления (паттерн rbac.Holder):
//   - На старте [NewHolder] синхронно строит первый снимок из БД (fatal на
//     ошибке — daemon не должен подниматься с пустым/битым реестром).
//   - Фоновая goroutine ([Run]) перечитывает снимок каждые refreshInterval.
//     Ошибка перечита не сбрасывает снимок: Holder оставляет прежний и пишет
//     warn (БД-сбой не должен обнулять каталог Service-ов).
//   - [WatchInvalidations] поверх — near-instant rebuild по Redis-сигналу.
//
// Геттеры ([Resolve]/[DefaultDestinySource]) СИНХРОННЫЕ (без ctx/error): читают
// текущий снимок через atomic.Pointer.Load без блокировки. Это важно для
// будущего переключения потребителей (S4): их синхронный интерфейс
// (ServiceRegistry/DestinySource) не меняется при замене cfg-источника на
// Holder.
//
// Конкурентность: снимок хранится в atomic.Pointer; refresh/rebuild делает swap
// целого указателя, читатели всегда видят согласованный срез. Снимок после
// публикации не мутируется.
type Holder struct {
	src      SnapshotSource
	interval time.Duration
	logger   *slog.Logger

	cur atomic.Pointer[Snapshot]

	// metrics — keeper_serviceregistry_*-дескриптор, инжектится через
	// [SetMetrics] в setup-фазе daemon-а (после создания registry, который
	// поднимается позже NewHolder). nil до wire-up и в тестах/bootstrap — все
	// Observe* no-op (метод на nil *RegistryMetrics — no-op).
	//
	// atomic.Pointer, а не обычное поле: SetMetrics вызывается уже ПОСЛЕ старта
	// Run-goroutine (go Holder.Run в setupServiceRegistry, а SetMetrics — позже в
	// setupMetricsRegistry). В этом окне Run конкурентно читает metrics из refresh
	// — обычное поле дало бы data race (паттерн rbac.Holder.metrics).
	metrics atomic.Pointer[RegistryMetrics]
}

// SetMetrics привязывает keeper_serviceregistry_*-дескриптор. Безопасно
// конкурентно с фоновыми читателями ([Run]/[WatchInvalidations]) через
// atomic.Pointer. nil-получатель — no-op. Вызывается из daemon
// `setupMetricsRegistry` после [RegisterRegistryMetrics]; до вызова метрики nil
// (Load вернёт nil) и не публикуются (паттерн rbac.Holder.SetMetrics).
func (h *Holder) SetMetrics(m *RegistryMetrics) {
	if h == nil {
		return
	}
	h.metrics.Store(m)
}

// NewHolder строит первоначальный снимок из БД. Ошибка первичной загрузки —
// fatal (caller-у возвращается err, daemon падает на старте при недоступном/
// битом реестре).
//
// nil-src разрешён — Holder ведёт себя как пустой снимок (нет Service-ов, пустые
// скаляры), фоновый refresh при этом no-op. Используется тестами и code-path-
// ами, где pool ещё не инициализирован; в `keeper run` src всегда non-nil.
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
		h.cur.Store(emptySnapshot())
		return h, nil
	}
	snap, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	h.cur.Store(snap)
	return h, nil
}

// emptySnapshot — снимок без Service-ов и с пустыми скалярами (nil-src / до
// первой загрузки). Map не nil, чтобы Resolve не паниковал.
func emptySnapshot() *Snapshot {
	return &Snapshot{services: map[string]ServiceEntry{}}
}

// Run запускает фоновый TTL-перечит снимка до отмены ctx. Блокирующий — caller
// запускает в отдельной goroutine. При nil-src сразу выходит (нечего
// перечитывать).
//
// Ошибка перечита логируется (warn) и НЕ меняет активный снимок — окно
// устаревания меньше зла «БД мигнула → каталог Service-ов пуст».
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

// WatchInvalidations подписывается на cluster-wide инвалидацию реестра (S2) и на
// каждый сигнал делает [Holder.refresh] (near-instant перечит из БД, вместо
// ожидания TTL-poll-а из [Run]). Блокирующий — caller запускает в отдельной
// goroutine, завершается по ctx.Done().
//
// TTL-poll ([Run]) НЕ заменяется: pub/sub без persistence (потеря сообщения,
// reconnect) → следующий тик [Run] всё равно перечитает снимок. Это fail-soft-
// слой поверх fallback-а.
//
// nil-src ([Holder] построен без БД-источника) или nil-инвалидатор → no-op.
// Ошибка подписки логируется (warn) и НЕ роняет daemon — реестр продолжает
// обновляться TTL-poll-ом.
func (h *Holder) WatchInvalidations(ctx context.Context, src InvalidationSource) {
	if h.src == nil || src == nil {
		return
	}
	err := src.Watch(ctx, func() {
		h.metrics.Load().ObserveInvalidation()
		h.refresh(ctx)
	})
	if err != nil && ctx.Err() == nil {
		h.logger.Warn("serviceregistry: подписка на cluster-инвалидацию завершилась с ошибкой, остаётся TTL-poll",
			slog.Any("error", err),
		)
	}
}

// Refresh принудительно перечитывает снимок (lazy-путь / тесты). Возвращает
// ошибку загрузки; при ошибке активный снимок не меняется.
//
// Метрики keeper_serviceregistry_snapshot_*: весь rebuild (src.Load) засекается,
// фаза отказа — load; на успехе пишутся timestamp + число Service-ов из
// построенного снимка (паттерн rbac.Holder.Refresh).
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
	m.ObserveRebuildSuccess(time.Since(start), len(snap.services))
	h.cur.Store(snap)
	return nil
}

// refresh — внутренний best-effort перечит для фоновых goroutine: ошибку
// логирует, не пробрасывает.
func (h *Holder) refresh(ctx context.Context) {
	if err := h.Refresh(ctx); err != nil {
		h.logger.Warn("serviceregistry: refresh снимка из БД не удался, оставлен прежний снимок",
			slog.Any("error", err),
		)
	}
}

// current возвращает актуальный снимок (atomic, без блокировки).
func (h *Holder) current() *Snapshot {
	return h.cur.Load()
}

// Resolve возвращает запись Service-а по имени из текущего снимка. Второй
// результат false — Service-а нет (вместо error: путь горячий, отсутствие —
// нормальный результат, не сбой). Геттер СИНХРОННЫЙ — это контракт потребителей
// S4.
func (h *Holder) Resolve(name string) (ServiceEntry, bool) {
	e, ok := h.current().services[name]
	return e, ok
}

// DefaultDestinySource возвращает скаляр keeper_settings[default_destiny_source]
// из текущего снимка; "" = настройка не задана. Геттер СИНХРОННЫЙ.
func (h *Holder) DefaultDestinySource() string {
	return h.current().defaultDestinySource
}

// ProvisioningMethodAllowed — разрешено ли СОЗДАВАТЬ оператора методом method
// текущей политикой provisioning_allowed_methods. Геттер СИНХРОННЫЙ
// (atomic-снимок без блокировки). Семантика:
//   - bootstrap/system → ВСЕГДА true (не гейтятся политикой никогда: bootstrap
//     первого Архонта через `keeper init`, system — внутренние записи);
//   - политика не задана (nil-map, ключа в keeper_settings нет) → true (дефолт
//     «всё разрешено», back-compat);
//   - политика задана → method ∈ set.
//
// nil-получатель (gate не сконфигурирован / тесты) — true (back-compat,
// gate==nil трактуется вызывающим как «пропускать»).
func (h *Holder) ProvisioningMethodAllowed(method string) bool {
	if method == "bootstrap" || method == "system" {
		return true
	}
	if h == nil {
		return true
	}
	methods := h.current().provisioningMethods
	if methods == nil {
		return true
	}
	return methods[method]
}

// ProvisioningPolicy возвращает текущую политику для GET-эндпоинта: отсортированный
// список разрешённых методов и флаг set (задана ли политика). set=false → политика
// не задана (дефолт «всё разрешено»), methods=nil. Геттер СИНХРОННЫЙ.
func (h *Holder) ProvisioningPolicy() (methods []string, set bool) {
	if h == nil {
		return nil, false
	}
	m := h.current().provisioningMethods
	if m == nil {
		return nil, false
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, true
}
