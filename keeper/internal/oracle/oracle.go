// Package oracle is the Keeper-side reactor router for the beacons contour (ADR-030, slice
// S2). Accepts a Portent (a beacon event from the Soul), matches it against the
// Decree registry, and enqueues a named scenario in the work-queue (ADR-027). It doesn't
// execute apply itself — only routes.
//
// Contents of slice S2:
//   - Vigil / Decree / Fire — runtime types for the `vigils` / `decrees` /
//     `oracle_fires` registries (migration 041);
//   - repository (crud.go): SelectActiveVigilsForSubject (VigilSnapshot resolve),
//     SelectDecreesByBeacon (hot path of match), cooldown read/record;
//   - match logic (match.go): subject-match + where-CEL + cooldown-check,
//     default-deny.
//
// Security (ADR-030(b)): Portent is untrusted input (the Soul could be
// compromised). Defense in layers: default-deny Decree + subject binding
// (coven XOR sid) + action = ONLY named scenario (whitelist) + cooldown
// (loop-prevention). The subject's SID is authoritative from the mTLS peer cert, NOT from
// PortentEvent.sid (echo).
//
// What's NOT in S2 (later slices): OpenAPI/MCP CRUD for Vigil/Decree + RBAC perms
// (S3); circuit breaker + metrics (S4); inotify / soul_beacon plugins /
// typed payload (S5).
package oracle

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecQueryRower is a narrow subset of pgxpool.Pool needed by the oracle repository.
// Symmetric with [augur.ExecQueryRower] / [applyrun.ExecQueryRower]: unit tests
// go through a fake without spinning up PG, production supplies a real pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

// Vigil is the runtime representation of a row in the `vigils` registry (Soul-side check,
// ADR-030). Subject is strictly XOR: exactly one of Coven / SID is non-empty (CHECK
// vigils_subject_xor). Params is raw JSONB (its shape depends on CheckAddr,
// validated at the service layer S3). Read-only-by-construction of the Vigil
// is guaranteed by the Soul side (S1), not by this type.
type Vigil struct {
	Name         string          `json:"name"`
	Coven        []string        `json:"coven,omitempty"`
	SID          *string         `json:"sid,omitempty"`
	IntervalSpec string          `json:"interval"`
	CheckAddr    string          `json:"check"`
	Params       json.RawMessage `json:"params"`
	Enabled      bool            `json:"enabled"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
}

// Decree is the runtime representation of a row in the `decrees` registry (a reactor rule,
// ADR-030). Default-deny: no matching Decree → the event triggers no action.
// Subject is strictly XOR (like Rite): SubjectCoven OR SubjectSID is non-empty.
// WhereCEL is an optional predicate over the event payload (event.data); nil/empty →
// always match (the subject already filtered). IncarnationName is the target incarnation
// of the reaction (DECISION #1, option b): the scenario's ServiceRef is resolved FROM it
// at enqueue time, rather than being duplicated in the Decree; it's also the subject's
// root Coven label (ADR-008), used for the membership check. ActionScenario is a named
// scenario (whitelist; a raw command was rejected, ADR-030(b)). Cooldown is
// a duration string (config.ParseDuration), the minimum interval between
// fires per-(decree, subject).
type Decree struct {
	Name            string          `json:"name"`
	OnBeacon        string          `json:"on_beacon"`
	WhereCEL        *string         `json:"where_cel,omitempty"`
	SubjectCoven    []string        `json:"subject_coven,omitempty"`
	SubjectSID      *string         `json:"subject_sid,omitempty"`
	IncarnationName string          `json:"incarnation_name"`
	ActionScenario  string          `json:"action_scenario"`
	ActionInput     json.RawMessage `json:"action_input"`
	Cooldown        string          `json:"cooldown"`
	Enabled         bool            `json:"enabled"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	CreatedByAID    *string         `json:"created_by_aid,omitempty"`
}
