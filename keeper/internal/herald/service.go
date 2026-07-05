package herald

import (
	"context"
	"errors"
	"log/slog"
)

// Service — бизнес-логика CRUD реестров `heralds`/`tidings` + инвалидация
// снимка правил dispatcher-а (ADR-052, S4). Один источник правды для
// HTTP-handler-ов (OpenAPI) и MCP-tool-handler-ов: transport-фасад только
// декодирует input и кодирует output, инварименты и инвалидация живут здесь.
//
// Инвалидация двухуровневая (как pushprovider.Service):
//   - in-process — [Invalidator.InvalidateRules] (тот же процесс мгновенно
//     перечитывает enabled-Tiding-снимок dispatcher-а на ближайшем матче);
//   - cross-keeper — [RedisInvalidator.PublishHeraldInvalidate] (другая нода
//     по подписке `herald:invalidate` дёргает свой InvalidateRules).
//
// Mutations, влияющие на матч (любой CRUD Herald/Tiding), сбрасывают кэш:
// disabled→enabled-переход, смена event_types/фильтров/herald-привязки —
// всё видно dispatcher-у сразу, а не через TTL DefaultRuleCacheTTL.
type Service struct {
	pool        ExecQueryRower
	invalidator Invalidator
	redis       RedisInvalidator
	logger      *slog.Logger
	// secretWriter/acceptPlaintext — dual-mode приём plaintext-секрета (ADR-064,
	// NIM-11). secretWriter=nil ИЛИ acceptPlaintext=false → plaintext отвергается,
	// принимаются только *_ref (secure default).
	secretWriter    SecretWriter
	acceptPlaintext bool
}

// Invalidator — узкая поверхность in-process-сброса кэша правил dispatcher-а.
// Реализуется [*Dispatcher] (метод InvalidateRules). nil-safe nop по умолчанию.
type Invalidator interface {
	InvalidateRules()
}

// RedisInvalidator — узкая поверхность cross-keeper-публикации invalidate-
// сигнала. Реализуется keeperredis-обёрткой (метод-адаптер в daemon, чтобы
// herald-пакет не зависел напрямую от keeperredis). nil → no-op (Redis
// выключен / unit-тест без брокера): single-instance деградирует на TTL+
// in-process invalidate, кластерная сходимость — через DefaultRuleCacheTTL.
type RedisInvalidator interface {
	PublishHeraldInvalidate(ctx context.Context, name string) error
}

// ServiceDeps — зависимости [Service]. Pool обязателен; Invalidator/Redis
// опциональны (nil → no-op-инвалидация, сходимость через TTL).
type ServiceDeps struct {
	Pool        ExecQueryRower
	Invalidator Invalidator
	Redis       RedisInvalidator
	Logger      *slog.Logger
	// SecretWriter — материализация plaintext-секрета в Vault (dual-mode, ADR-064).
	// nil → plaintext-приём недоступен (принимаются только *_ref).
	SecretWriter SecretWriter
	// AcceptPlaintext — разрешён ли приём plaintext (ADR-064 митигация a: TLS-фронт).
	// false (default) → plaintext отвергается 422.
	AcceptPlaintext bool
}

// NewService собирает service. Pool обязателен (nil → ошибка). nil Invalidator/
// Redis подменяются no-op-реализациями.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("herald: ServiceDeps.Pool is nil")
	}
	inv := d.Invalidator
	if inv == nil {
		inv = nopInvalidator{}
	}
	redis := d.Redis
	if redis == nil {
		redis = nopRedisInvalidator{}
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		pool:            d.Pool,
		invalidator:     inv,
		redis:           redis,
		logger:          logger,
		secretWriter:    d.SecretWriter,
		acceptPlaintext: d.AcceptPlaintext,
	}, nil
}

// --- Herald ----------------------------------------------------------

// CreateHerald вставляет Herald-канал + инвалидирует снимок правил.
// Возвращает заполненную запись (с created_at/updated_at). Sentinel-ошибки —
// [ErrHeraldExists] (409). Валидация name/type/config/secret_ref — внутри
// [InsertHerald] (до записи).
func (s *Service) CreateHerald(ctx context.Context, h *Herald) (*Herald, error) {
	// Dual-mode: plaintext-секреты → Vault, замена на внутренний ref (ADR-064).
	// ДО Insert (в PG идёт только ref); plaintext стирается из h.
	if err := materializeHeraldSecrets(ctx, s.secretWriter, s.acceptPlaintext, h); err != nil {
		return nil, err
	}
	if err := InsertHerald(ctx, s.pool, h); err != nil {
		return nil, err
	}
	s.invalidate(ctx, h.Name)
	return h, nil
}

// GetHerald читает Herald по PK. [ErrHeraldNotFound] при отсутствии.
func (s *Service) GetHerald(ctx context.Context, name string) (*Herald, error) {
	return SelectHeraldByName(ctx, s.pool, name)
}

// ListHeralds — страница каналов + total.
func (s *Service) ListHeralds(ctx context.Context, offset, limit int) ([]*Herald, int, error) {
	return SelectAllHeralds(ctx, s.pool, offset, limit)
}

// UpdateHerald заменяет mutable-поля канала (replace). Перечитывает запись после
// апдейта, чтобы отдать актуальные timestamps. [ErrHeraldNotFound] при отсутствии.
func (s *Service) UpdateHerald(ctx context.Context, h *Herald) (*Herald, error) {
	// Dual-mode: plaintext → Vault (idempotent-write по тому же пути), ADR-064.
	if err := materializeHeraldSecrets(ctx, s.secretWriter, s.acceptPlaintext, h); err != nil {
		return nil, err
	}
	if err := UpdateHerald(ctx, s.pool, h); err != nil {
		return nil, err
	}
	updated, err := SelectHeraldByName(ctx, s.pool, h.Name)
	if err != nil {
		return nil, err
	}
	updated.SecretWritten = h.SecretWritten // маркер write-path переживает re-read
	s.invalidate(ctx, h.Name)
	return updated, nil
}

// DeleteHerald удаляет канал (его Tiding-ы уходят каскадом) + инвалидирует.
// [ErrHeraldNotFound] при отсутствии.
func (s *Service) DeleteHerald(ctx context.Context, name string) error {
	if err := DeleteHerald(ctx, s.pool, name); err != nil {
		return err
	}
	s.invalidate(ctx, name)
	return nil
}

// --- Tiding ----------------------------------------------------------

// CreateTiding вставляет Tiding-правило + инвалидирует. Sentinel-ошибки —
// [ErrTidingExists] (409), [ErrHeraldNotFound] (404 — FK на несуществующий
// herald). Валидация name/herald/event_types — внутри [InsertTiding].
func (s *Service) CreateTiding(ctx context.Context, t *Tiding) (*Tiding, error) {
	if err := InsertTiding(ctx, s.pool, t); err != nil {
		return nil, err
	}
	s.invalidate(ctx, t.Name)
	return t, nil
}

// GetTiding читает Tiding по PK. [ErrTidingNotFound] при отсутствии.
func (s *Service) GetTiding(ctx context.Context, name string) (*Tiding, error) {
	return SelectTidingByName(ctx, s.pool, name)
}

// ListTidings — страница правил + total. includeEphemeral=false (default)
// скрывает разовые правила (ADR-052(g)); true — отдаёт все (отладка).
func (s *Service) ListTidings(ctx context.Context, includeEphemeral bool, offset, limit int) ([]*Tiding, int, error) {
	return SelectAllTidings(ctx, s.pool, includeEphemeral, offset, limit)
}

// UpdateTiding заменяет mutable-поля правила (replace). Перечитывает запись.
// [ErrTidingNotFound] (404) при отсутствии PK; [ErrHeraldNotFound] (404) при
// FK на несуществующий herald.
func (s *Service) UpdateTiding(ctx context.Context, t *Tiding) (*Tiding, error) {
	if err := UpdateTiding(ctx, s.pool, t); err != nil {
		return nil, err
	}
	updated, err := SelectTidingByName(ctx, s.pool, t.Name)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, t.Name)
	return updated, nil
}

// DeleteTiding удаляет правило + инвалидирует. [ErrTidingNotFound] при отсутствии.
func (s *Service) DeleteTiding(ctx context.Context, name string) error {
	if err := DeleteTiding(ctx, s.pool, name); err != nil {
		return err
	}
	s.invalidate(ctx, name)
	return nil
}

// InvalidateTidings — публичная точка двухуровневой инвалидации для внешних
// пишущих путей, которые создают Tiding-правила В ОБХОД CRUD этого Service
// (voyage.create вставляет ephemeral-Tiding-и прямым herald.InsertTiding в свою
// voyage-tx — ADR-052(g)). Без этого вызова dispatcher диспетчит терминал
// быстрого прогона против устаревшего TTL-снимка, и разовое уведомление молча
// промахивается. Звать СТРОГО ПОСЛЕ tx.Commit (при rollback правила в БД нет —
// инвалидировать нечего). Best-effort, nil-safe (см. invalidate).
func (s *Service) InvalidateTidings(ctx context.Context, name string) {
	if s == nil {
		return
	}
	s.invalidate(ctx, name)
}

// --- helpers ---------------------------------------------------------

// invalidate сбрасывает кэш правил в этом процессе и публикует cross-keeper-
// сигнал. Обе операции best-effort: мутация уже committed, потеря invalidate
// компенсируется TTL-сходимостью (DefaultRuleCacheTTL). Redis-ошибка
// логируется, но не возвращается caller-у.
func (s *Service) invalidate(ctx context.Context, name string) {
	s.invalidator.InvalidateRules()
	if err := s.redis.PublishHeraldInvalidate(ctx, name); err != nil {
		s.logger.Warn("herald: publish herald:invalidate failed",
			slog.String("name", name), slog.Any("error", err))
	}
}

// nopInvalidator / nopRedisInvalidator — no-op-реализации для случаев без
// dispatcher-а / без Redis (unit-тест / single-instance dev).
type nopInvalidator struct{}

func (nopInvalidator) InvalidateRules() {}

type nopRedisInvalidator struct{}

func (nopRedisInvalidator) PublishHeraldInvalidate(context.Context, string) error { return nil }

// compile-time check: *Dispatcher удовлетворяет Invalidator.
var _ Invalidator = (*Dispatcher)(nil)
