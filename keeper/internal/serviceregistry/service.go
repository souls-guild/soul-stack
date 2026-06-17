package serviceregistry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/config"
)

// ServicePool — узкое подмножество pgxpool.Pool, нужное [Service]. Реальный
// `*pgxpool.Pool` удовлетворяет автоматически. Объявлено локально (как
// rbac/augur), чтобы пакет не тянул operator/pgxpool в публичную поверхность.
type ServicePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pool/Tx удовлетворяют ServicePool через ExecQueryRower.
var _ ServicePool = (ExecQueryRower)(nil)

// Invalidator — поверхность cluster-wide инвалидации реестра (S2). После
// успешного commit-а CRUD-мутации (Insert/Update/Delete service / SetSetting)
// [Service] вызывает Invalidate, чтобы остальные Keeper-ноды near-instant
// перечитали снимок (вместо ожидания TTL-poll-а). Реализуется в `keeper run`
// адаптером поверх [keeperredis.PublishServiceInvalidate]; в single-Keeper/dev-
// режиме (без Redis) инвалидатор не подключён — работает только TTL-poll.
//
// Invalidate — best-effort: ошибку публикации НЕ возвращает (мутация уже
// зафиксирована в БД), реализация логирует и глотает. Паттерн идентичен
// [rbac.Invalidator] / [sigil.Invalidator].
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// ServiceDeps — зависимости [Service]. Все поля immutable после конструктора.
type ServiceDeps struct {
	Pool   ServicePool
	Logger *slog.Logger
}

// Service — бизнес-логика CRUD реестра Service-ов и cluster-wide настроек.
// Один источник правды для будущего transport-фасада (OpenAPI/MCP — slice S3);
// валидация (name-формат, непустые git/ref, формат refresh) живёт здесь,
// transport только декодирует input / кодирует output.
//
// Безопасен для конкурентного использования: deps immutable, состояние не
// держится; cluster-wide-инвалидация — через atomic-late-binding [SetInvalidator].
type Service struct {
	pool   ServicePool
	logger *slog.Logger

	// inv — опциональный cluster-wide invalidator (S2). Late-binding через
	// [Service.SetInvalidator]: Redis-клиент в `keeper run` поднимается ПОСЛЕ
	// NewService, поэтому инъекция отложена (паттерн rbac.Service.SetInvalidator
	// / sigil.Service.SetInvalidator). atomic.Pointer — конкурентная запись
	// сеттером vs. чтение из мутаций без отдельного mutex-а.
	inv atomic.Pointer[Invalidator]
}

// NewService собирает service. Pool обязателен.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("serviceregistry: ServiceDeps.Pool is nil")
	}
	return &Service{pool: d.Pool, logger: d.Logger}, nil
}

// SetInvalidator late-binding-ом подключает cluster-wide invalidator (S2).
// Вызывается из `keeper run` после подъёма Redis-клиента. nil — снять
// invalidator (вернуться к чистому TTL-poll-у). Идемпотентен, потокобезопасен.
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate шлёт cluster-wide invalidate-сигнал после успешного commit-а
// CRUD-мутации (S2). No-op, если invalidator не подключён (single-Keeper/dev).
// Best-effort: реализация Invalidate сама логирует и глотает ошибку publish-а —
// мутация уже зафиксирована, потеря сигнала компенсируется TTL-poll-ом.
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// CreateServiceInput — параметры CreateService. CallerAID опционален (nil →
// created_by_aid IS NULL для seed/системного создания; transport заполняет
// caller-ом из claims).
type CreateServiceInput struct {
	Name      string
	Git       string
	Ref       string
	Refresh   *string
	CallerAID *string
}

// CreateService создаёт запись Service-а. Валидация всех полей идёт ДО round-
// trip-а в БД (better error, нет лишнего обращения на битом вводе).
//
// Возврат:
//   - [ErrInvalidName] / [ErrInvalidGit] / [ErrInvalidRef] / [ErrInvalidRefresh] — 422;
//   - [ErrAlreadyExists] — name занят (409);
//   - [ErrOperatorNotFound] — CallerAID не существует в operators (FK).
func (s *Service) CreateService(ctx context.Context, in CreateServiceInput) (*ServiceEntry, error) {
	if err := validateFields(in.Name, in.Git, in.Ref, in.Refresh); err != nil {
		return nil, err
	}
	e := &ServiceEntry{
		Name:         in.Name,
		Git:          in.Git,
		Ref:          in.Ref,
		Refresh:      in.Refresh,
		CreatedByAID: in.CallerAID,
		// Первичное создание: updated_by_aid = created_by_aid (запись не
		// «менялась» отдельным оператором, но updated_at = created_at и автор
		// один). Симметрично updated_at DEFAULT NOW() в схеме.
		UpdatedByAID: in.CallerAID,
	}
	if err := InsertService(ctx, s.pool, e); err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return e, nil
}

// GetService читает запись Service-а по имени. [ErrNotFound] если нет.
func (s *Service) GetService(ctx context.Context, name string) (*ServiceEntry, error) {
	return GetService(ctx, s.pool, name)
}

// ListServices возвращает все записи Service-ов (sort name ASC).
func (s *Service) ListServices(ctx context.Context) ([]*ServiceEntry, error) {
	return ListServices(ctx, s.pool)
}

// UpdateServiceInput — параметры UpdateService. Заменяет mutable-поля записи
// (git/ref/refresh); name — ключ, не меняется.
type UpdateServiceInput struct {
	Name      string
	Git       string
	Ref       string
	Refresh   *string
	CallerAID *string
}

// UpdateService заменяет mutable-поля записи Service-а (replace-семантика).
// Валидация — ДО round-trip-а.
//
// CallerAID=nil → updated_by_aid обнуляется (set-семантика, прежний автор НЕ
// сохраняется); transport обычно заполняет CallerAID из claims.
//
// Возврат:
//   - validation-sentinel-ы (422);
//   - [ErrNotFound] — нет записи с таким name (404);
//   - [ErrOperatorNotFound] — CallerAID не существует (FK).
func (s *Service) UpdateService(ctx context.Context, in UpdateServiceInput) (*ServiceEntry, error) {
	if err := validateFields(in.Name, in.Git, in.Ref, in.Refresh); err != nil {
		return nil, err
	}
	e := &ServiceEntry{
		Name:         in.Name,
		Git:          in.Git,
		Ref:          in.Ref,
		Refresh:      in.Refresh,
		UpdatedByAID: in.CallerAID,
	}
	if err := UpdateService(ctx, s.pool, e); err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return e, nil
}

// DeleteService удаляет запись Service-а по имени. [ErrNotFound] если нет.
func (s *Service) DeleteService(ctx context.Context, name string) error {
	if err := DeleteService(ctx, s.pool, name); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// GetSetting читает cluster-wide настройку по ключу. Валидирует формат ключа до
// round-trip-а. [ErrSettingNotFound] если ключа нет.
func (s *Service) GetSetting(ctx context.Context, key string) (*Setting, error) {
	if !ValidSettingKey(key) {
		return nil, fmt.Errorf("%w: %q must match %s", ErrInvalidSettingKey, key, SettingKeyPattern)
	}
	return GetSetting(ctx, s.pool, key)
}

// SetSettingInput — параметры SetSetting. CallerAID опционален.
type SetSettingInput struct {
	Key       string
	Value     string
	CallerAID *string
}

// SetSetting upsert-ит cluster-wide настройку (key → value). Валидирует формат
// ключа до round-trip-а; value хранится как есть (семантика — на потребителе).
//
// Возврат:
//   - [ErrInvalidSettingKey] — ключ не по формату (422);
//   - [ErrOperatorNotFound] — CallerAID не существует (FK).
func (s *Service) SetSetting(ctx context.Context, in SetSettingInput) (*Setting, error) {
	if !ValidSettingKey(in.Key) {
		return nil, fmt.Errorf("%w: %q must match %s", ErrInvalidSettingKey, in.Key, SettingKeyPattern)
	}
	set := &Setting{Key: in.Key, Value: in.Value, UpdatedByAID: in.CallerAID}
	if err := SetSetting(ctx, s.pool, set); err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return set, nil
}

// validateFields — общая прикладная валидация полей Service-записи (create/
// update): name-формат, непустые git/ref, формат refresh через
// config.ParseDuration (если задан). Дублирует БД-CHECK-и для better-error до
// round-trip-а; refresh БД-CHECK-ом не ловится (как augur token_ttl) — только
// здесь.
func validateFields(name, git, ref string, refresh *string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidName, name, NamePattern)
	}
	if git == "" {
		return ErrInvalidGit
	}
	if ref == "" {
		return ErrInvalidRef
	}
	if refresh != nil {
		if _, err := config.ParseDuration(*refresh); err != nil {
			return fmt.Errorf("%w %q: %w", ErrInvalidRefresh, *refresh, err)
		}
	}
	return nil
}
