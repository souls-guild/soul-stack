package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

// Management-Service for Oracle registries (operator-facing CRUD for Vigil /
// Decree via OpenAPI / MCP, ADR-030 S3). A single source of truth for the
// REST handler and the MCP tool: both call this Service, so validation and
// the error contract don't drift apart (the [augur.Service] /
// [serviceregistry.Service] pattern).
//
// DIFFERENCE from the reactor router ([Match] / match.go): that one resolves
// a Portent from a Soul (S2, hot path); the Service here is only registry
// row management. The reactor does NOT use this type.

// ServicePool — a narrow subset of pgxpool.Pool needed by [Service]. A real
// `*pgxpool.Pool` satisfies it automatically (via [ExecQueryRower]).
// Declared locally (like augur/serviceregistry) so the package doesn't pull
// pgxpool into its public surface.
type ServicePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pool/Tx satisfy ServicePool via ExecQueryRower.
var _ ServicePool = (ExecQueryRower)(nil)

// WhereCompiler — a narrow contract for compile-checking a Decree's
// where-CEL on create. Satisfied by [*WhereEvaluator] (the same one the
// reactor router uses on the hot path): one env, one program cache.
// Declared as an interface so Service doesn't depend on the evaluator's
// constructor in tests.
type WhereCompiler interface {
	CompileCheck(expr string) error
}

// ServiceDeps — dependencies of [Service]. All fields are immutable after construction.
type ServiceDeps struct {
	Pool   ServicePool
	Where  WhereCompiler
	Logger *slog.Logger
}

// Service — business logic for operator-facing CRUD over Oracle registries.
// Safe for concurrent use: deps are immutable, holds no state between
// calls.
type Service struct {
	pool   ServicePool
	where  WhereCompiler
	logger *slog.Logger
}

// NewService assembles the management-Service. Pool and Where are required
// (Where is for compile-checking a Decree's where-CEL on create).
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

// CreateVigilInput — parameters for CreateVigil. Subject carries exactly one of
// the four dimensions ([subject.Selector]). Params is raw JSONB (shape depends
// on Check; deep params validation is deferred along with typed-payload,
// ADR-030). CallerAID is optional (nil → created_by_aid IS NULL; the transport
// fills it in from claims).
type CreateVigilInput struct {
	Name string
	// Label is the optional display caption ([ADR-0085]): free text, set here at
	// registration and changed afterwards by [Service.SetVigilLabel]. nil/blank
	// stores NULL and the consumer shows Name.
	//
	// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
	Label     *string
	Subject   subject.Selector
	Interval  string
	Check     string
	Params    json.RawMessage
	Enabled   bool
	CallerAID *string
}

// CreateVigil validates fields BEFORE the round-trip (better error, no
// wasted call on bad input), then inserts the row.
//
// Returns:
//   - [ErrValidation] (wrapped) — bad name / interval / check / subject (422);
//   - [ErrVigilAlreadyExists] — name is taken (409);
//   - a wrapped fmt.Errorf — FK/CHECK/infra (500).
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
	if err := s.validateSubject(ctx, in.Subject); err != nil {
		return nil, err
	}

	v := &Vigil{
		Name:         in.Name,
		Label:        in.Label,
		IntervalSpec: in.Interval,
		CheckAddr:    in.Check,
		Params:       in.Params,
		Enabled:      in.Enabled,
		CreatedByAID: in.CallerAID,
	}
	v.setSubject(in.Subject)
	if err := InsertVigil(ctx, s.pool, v); err != nil {
		return nil, err
	}
	return v, nil
}

// ListVigils returns a page of Vigils and the total count (sort created_at
// DESC, name ASC).
func (s *Service) ListVigils(ctx context.Context, offset, limit int) ([]*Vigil, int, error) {
	return SelectAllVigils(ctx, s.pool, offset, limit)
}

// GetVigil reads a Vigil by PK. [ErrVigilNotFound] if it doesn't exist.
func (s *Service) GetVigil(ctx context.Context, name string) (*Vigil, error) {
	return SelectVigilByName(ctx, s.pool, name)
}

// SetVigilLabel replaces the display caption of one Vigil and returns the row as
// it now reads ([ADR-0085], permission vigil.label-set, audit
// vigil.label_changed).
//
// This is the registry's ONLY operator mutation: a Vigil's interval, check and
// subject stay immutable (the Souls holding a VigilSnapshot were told what to
// run, and there is no rule-update push), and a caption is the one field for
// which that argument does not apply, because nothing in the snapshot reads it.
//
// [ErrVigilNotFound] if the row doesn't exist.
func (s *Service) SetVigilLabel(ctx context.Context, name string, label *string) (*Vigil, error) {
	if err := UpdateVigilLabel(ctx, s.pool, name, label); err != nil {
		return nil, err
	}
	return SelectVigilByName(ctx, s.pool, name)
}

// DeleteVigil removes a Vigil by PK. [ErrVigilNotFound] if the row didn't exist.
func (s *Service) DeleteVigil(ctx context.Context, name string) error {
	return DeleteVigil(ctx, s.pool, name)
}

// --- Decree -----------------------------------------------------------

// CreateDecreeInput — parameters for CreateDecree. Subject carries exactly one
// of the four dimensions — WHO may fire the rule ([subject.Selector]).
// IncarnationName is the opposite end: the TARGET incarnation the reaction acts
// on (required, ADR-030). WhereCEL — an optional predicate over event.data
// (compile-checked on create). ActionInput — the raw JSONB scenario input.
// CallerAID is optional.
type CreateDecreeInput struct {
	Name string
	// Label is the optional display caption ([ADR-0085]), changed afterwards by
	// [Service.SetDecreeLabel]. Not OnBeacon and not IncarnationName: neither is
	// derived from it.
	Label           *string
	OnBeacon        string
	WhereCEL        *string
	Subject         subject.Selector
	IncarnationName string
	ActionScenario  string
	ActionInput     json.RawMessage
	Cooldown        string
	Enabled         bool
	CallerAID       *string
}

// CreateDecree validates fields BEFORE the round-trip, then inserts the row.
// where-CEL is compiled by the keeper-local sandbox engine ([WhereCompiler])
// — a bad predicate is rejected with 422, instead of turning into a runtime
// surprise (default-deny would swallow it as no-match with no diagnostic
// for the operator).
//
// Returns:
//   - [ErrValidation] (wrapped) — bad name / on_beacon / incarnation_name /
//     action_scenario / subject / where-CEL / cooldown (422);
//   - [ErrDecreeAlreadyExists] — name is taken (409);
//   - a wrapped fmt.Errorf — FK/CHECK/infra (500).
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
	if err := s.validateSubject(ctx, in.Subject); err != nil {
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
		Label:           in.Label,
		WhereCEL:        in.WhereCEL,
		IncarnationName: in.IncarnationName,
		ActionScenario:  in.ActionScenario,
		ActionInput:     in.ActionInput,
		Cooldown:        in.Cooldown,
		Enabled:         in.Enabled,
		CreatedByAID:    in.CallerAID,
	}
	d.setSubject(in.Subject)
	if err := InsertDecree(ctx, s.pool, d); err != nil {
		return nil, err
	}
	return d, nil
}

// ListDecrees returns a page of Decrees and the total count (sort created_at
// DESC, name ASC).
func (s *Service) ListDecrees(ctx context.Context, offset, limit int) ([]*Decree, int, error) {
	return SelectAllDecrees(ctx, s.pool, offset, limit)
}

// GetDecree reads a Decree by PK. [ErrDecreeNotFound] if it doesn't exist.
func (s *Service) GetDecree(ctx context.Context, name string) (*Decree, error) {
	return SelectDecreeByName(ctx, s.pool, name)
}

// SetDecreeLabel replaces the display caption of one Decree and returns the row
// as it now reads ([ADR-0085], permission decree.label-set, audit
// decree.label_changed).
//
// The reactor is untouched: `oracle_fires` and `oracle_circuit` are keyed on the
// Decree's NAME, so a caption edit moves no cooldown state and resets no circuit
// breaker.
//
// [ErrDecreeNotFound] if the row doesn't exist.
func (s *Service) SetDecreeLabel(ctx context.Context, name string, label *string) (*Decree, error) {
	if err := UpdateDecreeLabel(ctx, s.pool, name, label); err != nil {
		return nil, err
	}
	return SelectDecreeByName(ctx, s.pool, name)
}

// DeleteDecree removes a Decree by PK (cooldown state in oracle_fires cascades
// away). [ErrDecreeNotFound] if the row didn't exist.
func (s *Service) DeleteDecree(ctx context.Context, name string) error {
	return DeleteDecree(ctx, s.pool, name)
}
