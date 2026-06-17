package augur

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Management-Service Augur-реестра (operator-facing CRUD Omen / Rite через
// OpenAPI / MCP, augur.md §4 / rbac.md §Augur). Один источник правды для REST-
// handler-а и MCP-tool-а: оба вызывают этот Service, чтобы валидация и error-
// контракт не разъехались (паттерн serviceregistry.Service).
//
// ОТЛИЧИЕ от брокера (broker*.go / resolve.go): тот резолвит AugurRequest от
// Soul-а (auth + egress); Service здесь — только управление записями реестра.
// Брокер этот тип НЕ использует.

// ErrValidation — sentinel для service-валидации, которую management-Service
// проводит ДО round-trip-а в БД (формат name / source_type / auth_ref / allow /
// субъект / token-поля). Transport маппит в 422 (REST TypeValidationFailed /
// MCP validation-failed). Конкретный диагностический текст — в обёрнутой
// ошибке (errors.Unwrap), для лога; клиенту отдаётся wrapped-сообщение целиком
// (оно уже public — формируется здесь, без internal SQL/stack-деталей).
var ErrValidation = errors.New("augur: validation failed")

// ServicePool — узкое подмножество pgxpool.Pool, нужное [Service]. Реальный
// `*pgxpool.Pool` удовлетворяет автоматически (через [ExecQueryRower]).
// Объявлено локально (как serviceregistry/rbac), чтобы пакет не тянул
// pgxpool в публичную поверхность.
type ServicePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pool/Tx удовлетворяют ServicePool через ExecQueryRower.
var _ ServicePool = (ExecQueryRower)(nil)

// ServiceDeps — зависимости [Service]. Все поля immutable после конструктора.
type ServiceDeps struct {
	Pool   ServicePool
	Logger *slog.Logger
}

// Service — бизнес-логика operator-facing CRUD Augur-реестра. Безопасен для
// конкурентного использования: deps immutable, состояния между вызовами не
// держит.
type Service struct {
	pool   ServicePool
	logger *slog.Logger
}

// NewService собирает management-Service. Pool обязателен.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("augur: ServiceDeps.Pool is nil")
	}
	return &Service{pool: d.Pool, logger: d.Logger}, nil
}

// --- Omen -------------------------------------------------------------

// CreateOmenInput — параметры CreateOmen. CallerAID опционален (nil →
// created_by_aid IS NULL; transport заполняет caller-ом из claims).
type CreateOmenInput struct {
	Name       string
	SourceType string
	Endpoint   string
	AuthRef    string
	CallerAID  *string
}

// CreateOmen валидирует поля ДО round-trip-а (better error, нет лишнего
// обращения на битом вводе), затем вставляет запись.
//
// Возврат:
//   - [ErrValidation] (wrapped) — битый name / source_type / endpoint / auth_ref (422);
//   - [ErrOmenAlreadyExists] — name занят (409);
//   - wrapped fmt.Errorf — FK/CHECK/инфра (500).
func (s *Service) CreateOmen(ctx context.Context, in CreateOmenInput) (*Omen, error) {
	src := SourceType(in.SourceType)
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("%w: invalid omen name %q (must match %s)", ErrValidation, in.Name, NamePattern)
	}
	if !ValidSourceType(src) {
		return nil, fmt.Errorf("%w: invalid source_type %q (must be vault/prometheus/elk)", ErrValidation, in.SourceType)
	}
	if in.Endpoint == "" {
		return nil, fmt.Errorf("%w: endpoint is empty", ErrValidation)
	}
	if !ValidAuthRef(in.AuthRef) {
		return nil, fmt.Errorf("%w: invalid auth_ref %q (must be a vault-ref vault:<mount>/<path>)", ErrValidation, in.AuthRef)
	}

	o := &Omen{
		Name:         in.Name,
		SourceType:   src,
		Endpoint:     in.Endpoint,
		AuthRef:      in.AuthRef,
		CreatedByAID: in.CallerAID,
	}
	if err := InsertOmen(ctx, s.pool, o); err != nil {
		return nil, err
	}
	return o, nil
}

// ListOmens возвращает страницу Omen-ов и общее количество (sort created_at
// DESC, name ASC).
func (s *Service) ListOmens(ctx context.Context, offset, limit int) ([]*Omen, int, error) {
	return SelectAllOmens(ctx, s.pool, offset, limit)
}

// GetOmen читает Omen по PK. [ErrOmenNotFound] если нет.
func (s *Service) GetOmen(ctx context.Context, name string) (*Omen, error) {
	return SelectOmenByName(ctx, s.pool, name)
}

// DeleteOmen удаляет Omen по PK (Rite-ы уходят каскадом). [ErrOmenNotFound]
// если записи не было.
func (s *Service) DeleteOmen(ctx context.Context, name string) error {
	return DeleteOmen(ctx, s.pool, name)
}

// --- Rite -------------------------------------------------------------

// CreateRiteInput — параметры CreateRite. Субъект — XOR Coven/SID. allow —
// сырой JSONB (форма проверяется против source_type Omen-а). TokenTTL /
// TokenNumUses осмысленны только для vault-Omen с Delegate=true.
type CreateRiteInput struct {
	Omen         string
	Coven        *string
	SID          *string
	Allow        json.RawMessage
	Delegate     bool
	TokenTTL     *string
	TokenNumUses *int
	CallerAID    *string
}

// CreateRite валидирует субъект (XOR) ДО round-trip-а, затем вставляет.
// allow-shape по source_type и token-поля довалидирует [InsertRite] (требует
// резолва Omen-а тем же db) — её ошибки тоже оборачиваются в [ErrValidation].
//
// Возврат:
//   - [ErrValidation] (wrapped) — битый субъект / allow / token-поля (422);
//   - [ErrOmenNotFound] — Omen не существует (404);
//   - wrapped fmt.Errorf — FK/CHECK/инфра (500).
func (s *Service) CreateRite(ctx context.Context, in CreateRiteInput) (*Rite, error) {
	r := &Rite{
		Omen:         in.Omen,
		Coven:        in.Coven,
		SID:          in.SID,
		Allow:        in.Allow,
		Delegate:     in.Delegate,
		TokenTTL:     in.TokenTTL,
		TokenNumUses: in.TokenNumUses,
		CreatedByAID: in.CallerAID,
	}
	if r.Omen == "" {
		return nil, fmt.Errorf("%w: omen is empty", ErrValidation)
	}
	if err := ValidateSubjectXOR(r); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrValidation, err.Error())
	}
	if len(r.Allow) == 0 {
		return nil, fmt.Errorf("%w: allow is empty", ErrValidation)
	}

	if err := InsertRite(ctx, s.pool, r); err != nil {
		// InsertRite резолвит Omen и довалидирует allow-shape / token-поля. Эти
		// ошибки помечены sentinel-ами ErrAllowShape / ErrTokenFields — это
		// service-валидация, а не инфра; маппим в ErrValidation. ErrOmenNotFound /
		// FK-violation пробрасываются как есть (404 / 500).
		if isRiteValidationError(err) {
			return nil, fmt.Errorf("%w: %s", ErrValidation, err.Error())
		}
		return nil, err
	}
	return r, nil
}

// ListRitesByOmen возвращает все Rite-ы одного Omen-а (sort created_at DESC,
// id ASC). Фильтр CRUD list-by-omen (augur.md §6).
func (s *Service) ListRitesByOmen(ctx context.Context, omen string) ([]*Rite, error) {
	return SelectRitesByOmen(ctx, s.pool, omen)
}

// DeleteRite удаляет Rite по суррогатному PK. [ErrRiteNotFound] если записи не
// было.
func (s *Service) DeleteRite(ctx context.Context, id int64) error {
	return DeleteRite(ctx, s.pool, id)
}

// isRiteValidationError отличает service-валидацию InsertRite (allow-shape /
// token-поля) от not-found / инфра. allow/token-валидаторы ([ValidateAllow] /
// [ValidateTokenFields]) живут в storage-слайсе и не знают про management-
// Service [ErrValidation], поэтому помечают свои ошибки sentinel-ами
// [ErrAllowShape] / [ErrTokenFields]; матчим через errors.Is, а не по
// строковому префиксу текста (переименование диагностики не должно молча
// сломать маппинг 422→500).
func isRiteValidationError(err error) bool {
	return errors.Is(err, ErrAllowShape) || errors.Is(err, ErrTokenFields)
}
