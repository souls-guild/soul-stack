package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Management-Service Oracle-реестров (operator-facing CRUD Vigil / Decree через
// OpenAPI / MCP, ADR-030 S3). Один источник правды для REST-handler-а и
// MCP-tool-а: оба вызывают этот Service, чтобы валидация и error-контракт не
// разъехались (паттерн [augur.Service] / [serviceregistry.Service]).
//
// ОТЛИЧИЕ от reactor-роутера ([Match] / match.go): тот резолвит Portent от
// Soul-а (S2, горячий путь); Service здесь — только управление записями
// реестров. Reactor этот тип НЕ использует.

// ServicePool — узкое подмножество pgxpool.Pool, нужное [Service]. Реальный
// `*pgxpool.Pool` удовлетворяет автоматически (через [ExecQueryRower]).
// Объявлено локально (как augur/serviceregistry), чтобы пакет не тянул pgxpool
// в публичную поверхность.
type ServicePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pool/Tx удовлетворяют ServicePool через ExecQueryRower.
var _ ServicePool = (ExecQueryRower)(nil)

// WhereCompiler — узкий контракт compile-проверки where-CEL Decree-а на create.
// Удовлетворяется [*WhereEvaluator] (тот же, что reactor-роутер использует на
// горячем пути): set один env, один кеш программ. Объявлен интерфейсом, чтобы
// Service не зависел от конструктора evaluator-а в тестах.
type WhereCompiler interface {
	CompileCheck(expr string) error
}

// ServiceDeps — зависимости [Service]. Все поля immutable после конструктора.
type ServiceDeps struct {
	Pool   ServicePool
	Where  WhereCompiler
	Logger *slog.Logger
}

// Service — бизнес-логика operator-facing CRUD Oracle-реестров. Безопасен для
// конкурентного использования: deps immutable, состояния между вызовами не
// держит.
type Service struct {
	pool   ServicePool
	where  WhereCompiler
	logger *slog.Logger
}

// NewService собирает management-Service. Pool и Where обязательны (Where —
// для compile-проверки where-CEL Decree-а на create).
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("oracle: ServiceDeps.Pool is nil")
	}
	if d.Where == nil {
		return nil, errors.New("oracle: ServiceDeps.Where is nil")
	}
	return &Service{pool: d.Pool, where: d.Where, logger: d.Logger}, nil
}

// --- Vigil ------------------------------------------------------------

// CreateVigilInput — параметры CreateVigil. Субъект — XOR Coven/SID. Params —
// сырой JSONB (форма зависит от Check; глубокая проверка params отложена вместе
// с typed-payload, ADR-030). CallerAID опционален (nil → created_by_aid IS NULL;
// transport заполняет caller-ом из claims).
type CreateVigilInput struct {
	Name      string
	Coven     []string
	SID       *string
	Interval  string
	Check     string
	Params    json.RawMessage
	Enabled   bool
	CallerAID *string
}

// CreateVigil валидирует поля ДО round-trip-а (better error, нет лишнего
// обращения на битом вводе), затем вставляет запись.
//
// Возврат:
//   - [ErrValidation] (wrapped) — битый name / interval / check / субъект (422);
//   - [ErrVigilAlreadyExists] — name занят (409);
//   - wrapped fmt.Errorf — FK/CHECK/инфра (500).
func (s *Service) CreateVigil(ctx context.Context, in CreateVigilInput) (*Vigil, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("%w: invalid vigil name %q (must match %s)", ErrValidation, in.Name, NamePattern)
	}
	if err := validateInterval(in.Interval); err != nil {
		return nil, err
	}
	if !ValidCheckAddr(in.Check) {
		return nil, fmt.Errorf("%w: unknown check %q (must be a known core.beacon address)", ErrValidation, in.Check)
	}
	if err := validateSubjectXOR(in.Coven, in.SID); err != nil {
		return nil, err
	}

	v := &Vigil{
		Name:         in.Name,
		Coven:        in.Coven,
		SID:          in.SID,
		IntervalSpec: in.Interval,
		CheckAddr:    in.Check,
		Params:       in.Params,
		Enabled:      in.Enabled,
		CreatedByAID: in.CallerAID,
	}
	if err := InsertVigil(ctx, s.pool, v); err != nil {
		return nil, err
	}
	return v, nil
}

// ListVigils возвращает страницу Vigil-ов и общее количество (sort created_at
// DESC, name ASC).
func (s *Service) ListVigils(ctx context.Context, offset, limit int) ([]*Vigil, int, error) {
	return SelectAllVigils(ctx, s.pool, offset, limit)
}

// GetVigil читает Vigil по PK. [ErrVigilNotFound] если нет.
func (s *Service) GetVigil(ctx context.Context, name string) (*Vigil, error) {
	return SelectVigilByName(ctx, s.pool, name)
}

// DeleteVigil удаляет Vigil по PK. [ErrVigilNotFound] если записи не было.
func (s *Service) DeleteVigil(ctx context.Context, name string) error {
	return DeleteVigil(ctx, s.pool, name)
}

// --- Decree -----------------------------------------------------------

// CreateDecreeInput — параметры CreateDecree. Субъект — XOR Coven/SID.
// IncarnationName — таргет-incarnation реакции (обязательно, ADR-030).
// WhereCEL — опц. предикат над event.data (compile-проверяется на create).
// ActionInput — сырой JSONB-вход сценария. CallerAID опционален.
type CreateDecreeInput struct {
	Name            string
	OnBeacon        string
	WhereCEL        *string
	Coven           []string
	SID             *string
	IncarnationName string
	ActionScenario  string
	ActionInput     json.RawMessage
	Cooldown        string
	Enabled         bool
	CallerAID       *string
}

// CreateDecree валидирует поля ДО round-trip-а, затем вставляет запись.
// where-CEL компилируется keeper-local sandbox-движком ([WhereCompiler]) — битый
// предикат отвергается 422, а не превращается в runtime-сюрприз (default-deny
// проглотил бы его как no-match без диагностики оператору).
//
// Возврат:
//   - [ErrValidation] (wrapped) — битый name / on_beacon / incarnation_name /
//     action_scenario / субъект / where-CEL / cooldown (422);
//   - [ErrDecreeAlreadyExists] — name занят (409);
//   - wrapped fmt.Errorf — FK/CHECK/инфра (500).
func (s *Service) CreateDecree(ctx context.Context, in CreateDecreeInput) (*Decree, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("%w: invalid decree name %q (must match %s)", ErrValidation, in.Name, NamePattern)
	}
	if !ValidName(in.OnBeacon) {
		return nil, fmt.Errorf("%w: invalid on_beacon %q (must match Vigil name %s)", ErrValidation, in.OnBeacon, NamePattern)
	}
	if !ValidIncarnationName(in.IncarnationName) {
		return nil, fmt.Errorf("%w: invalid incarnation_name %q (must match %s)", ErrValidation, in.IncarnationName, IncarnationPattern)
	}
	if !ValidScenario(in.ActionScenario) {
		return nil, fmt.Errorf("%w: invalid action_scenario %q (must match %s)", ErrValidation, in.ActionScenario, ScenarioPattern)
	}
	if err := validateSubjectXOR(in.Coven, in.SID); err != nil {
		return nil, err
	}
	if err := validateCooldown(in.Cooldown); err != nil {
		return nil, err
	}
	if in.WhereCEL != nil && *in.WhereCEL != "" {
		if err := s.where.CompileCheck(*in.WhereCEL); err != nil {
			return nil, fmt.Errorf("%w: invalid where-CEL: %s", ErrValidation, err)
		}
	}

	d := &Decree{
		Name:            in.Name,
		OnBeacon:        in.OnBeacon,
		WhereCEL:        in.WhereCEL,
		SubjectCoven:    in.Coven,
		SubjectSID:      in.SID,
		IncarnationName: in.IncarnationName,
		ActionScenario:  in.ActionScenario,
		ActionInput:     in.ActionInput,
		Cooldown:        in.Cooldown,
		Enabled:         in.Enabled,
		CreatedByAID:    in.CallerAID,
	}
	if err := InsertDecree(ctx, s.pool, d); err != nil {
		return nil, err
	}
	return d, nil
}

// ListDecrees возвращает страницу Decree-ов и общее количество (sort created_at
// DESC, name ASC).
func (s *Service) ListDecrees(ctx context.Context, offset, limit int) ([]*Decree, int, error) {
	return SelectAllDecrees(ctx, s.pool, offset, limit)
}

// GetDecree читает Decree по PK. [ErrDecreeNotFound] если нет.
func (s *Service) GetDecree(ctx context.Context, name string) (*Decree, error) {
	return SelectDecreeByName(ctx, s.pool, name)
}

// DeleteDecree удаляет Decree по PK (cooldown-state в oracle_fires уходит
// каскадом). [ErrDecreeNotFound] если записи не было.
func (s *Service) DeleteDecree(ctx context.Context, name string) error {
	return DeleteDecree(ctx, s.pool, name)
}
