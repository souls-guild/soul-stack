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
// (exactly one of sid / service.incarnation / coven / trait, [subject.Selector])
// + action = ONLY named scenario (whitelist) + cooldown (loop-prevention). The
// subject's SID is authoritative from the mTLS peer cert, NOT from
// PortentEvent.sid (echo), and every other fact about that host is read from the
// registries by SID — never taken from the event.
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

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
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
// ADR-030). The subject is exactly one of four dimensions (CHECK
// vigils_subject_one_of, NIM-280) — see [subject.Selector] and [Vigil.Subject].
// Params is raw JSONB (its shape depends on CheckAddr, validated at the service
// layer S3). Read-only-by-construction of the Vigil is guaranteed by the Soul
// side (S1), not by this type.
type Vigil struct {
	Name         string          `json:"name"`
	SID          []string        `json:"sid,omitempty"`
	Service      *string         `json:"service,omitempty"`
	Incarnation  *string         `json:"incarnation,omitempty"`
	Coven        []string        `json:"coven,omitempty"`
	TraitKey     *string         `json:"trait_key,omitempty"`
	TraitValue   *string         `json:"trait_value,omitempty"`
	IntervalSpec string          `json:"interval"`
	CheckAddr    string          `json:"check"`
	Params       json.RawMessage `json:"params"`
	Enabled      bool            `json:"enabled"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
}

// Subject renders the Vigil's stored columns as the shared selector. One
// conversion per registry rather than one embedded type: the three tables spell
// the columns differently on purpose (a bare `incarnation` on `decrees` would
// sit next to `incarnation_name`, the ACTION target, and read as the same
// thing), and the shape they share is the semantics, not the names.
func (v *Vigil) Subject() subject.Selector {
	return subject.Selector{
		SIDs:        v.SID,
		Service:     deref(v.Service),
		Incarnation: deref(v.Incarnation),
		Covens:      v.Coven,
		TraitKey:    deref(v.TraitKey),
		TraitValue:  deref(v.TraitValue),
	}
}

// Decree is the runtime representation of a row in the `decrees` registry (a reactor rule,
// ADR-030). Default-deny: no matching Decree → the event triggers no action.
// The subject is exactly one of four dimensions (CHECK decrees_subject_one_of,
// NIM-280) — see [subject.Selector] and [Decree.Subject].
//
// WhereCEL is an optional predicate over the event payload (event.data); nil/empty →
// always match (the subject already filtered). IncarnationName is the target incarnation
// of the reaction (DECISION #1, option b): the scenario's ServiceRef is resolved FROM it
// at enqueue time, rather than being duplicated in the Decree. It is ALSO the
// membership gate — the reacting host must be a member of that incarnation
// (`incarnation_membership`, NIM-124), which is a separate question from whether
// the subject reached the host and stays fail-closed regardless of which
// dimension matched. ActionScenario is a named scenario (whitelist; a raw
// command was rejected, ADR-030(b)). Cooldown is a duration string
// (config.ParseDuration), the minimum interval between fires per-(decree, subject).
//
// The subject columns carry a `subject_` prefix precisely because of
// IncarnationName: a bare `incarnation` next to `incarnation_name` would read as
// the same thing, and they are opposite ends of the rule — who fires it versus
// what it acts on.
type Decree struct {
	Name               string          `json:"name"`
	OnBeacon           string          `json:"on_beacon"`
	WhereCEL           *string         `json:"where_cel,omitempty"`
	SubjectSID         []string        `json:"subject_sid,omitempty"`
	SubjectService     *string         `json:"subject_service,omitempty"`
	SubjectIncarnation *string         `json:"subject_incarnation,omitempty"`
	SubjectCoven       []string        `json:"subject_coven,omitempty"`
	SubjectTraitKey    *string         `json:"subject_trait_key,omitempty"`
	SubjectTraitValue  *string         `json:"subject_trait_value,omitempty"`
	IncarnationName    string          `json:"incarnation_name"`
	ActionScenario     string          `json:"action_scenario"`
	ActionInput        json.RawMessage `json:"action_input"`
	Cooldown           string          `json:"cooldown"`
	Enabled            bool            `json:"enabled"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	CreatedByAID       *string         `json:"created_by_aid,omitempty"`
}

// setSubject writes the selector back into the stored columns. Every unset
// dimension becomes NULL — which is what makes the SQL predicate exact: a row
// can only be reached through the dimension it was written with.
func (v *Vigil) setSubject(s subject.Selector) {
	v.SID, v.Coven = s.SIDs, s.Covens
	v.Service, v.Incarnation = ptr(s.Service), ptr(s.Incarnation)
	v.TraitKey, v.TraitValue = ptr(s.TraitKey), ptr(s.TraitValue)
}

// Subject renders the Decree's stored columns as the shared selector.
func (d *Decree) Subject() subject.Selector {
	return subject.Selector{
		SIDs:        d.SubjectSID,
		Service:     deref(d.SubjectService),
		Incarnation: deref(d.SubjectIncarnation),
		Covens:      d.SubjectCoven,
		TraitKey:    deref(d.SubjectTraitKey),
		TraitValue:  deref(d.SubjectTraitValue),
	}
}

// setSubject — the Decree half of [Vigil.setSubject].
func (d *Decree) setSubject(s subject.Selector) {
	d.SubjectSID, d.SubjectCoven = s.SIDs, s.Covens
	d.SubjectService, d.SubjectIncarnation = ptr(s.Service), ptr(s.Incarnation)
	d.SubjectTraitKey, d.SubjectTraitValue = ptr(s.TraitKey), ptr(s.TraitValue)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
