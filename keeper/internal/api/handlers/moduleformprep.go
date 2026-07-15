// Module-form-prep-handler Operator API (`POST /v1/modules/{name}/form-prep`) —
// the shared resolver of source catalogs for the module UI form (ADR-045 S3). The UI builds
// the Run→Command form from the schema of GET /v1/modules/{name} (S2); fields with
// `input.source` (incarnation_hosts / choir) need a live list of SIDs for
// autocomplete. This endpoint is the only resolver of such source catalogs.
//
// Cluster-aware: SIDs come from `souls` (the whole-cluster registry), not from
// a single request's scope. Prefix filter + a hard cap (souls up to 100k — returning
// the whole list is not allowed): first narrow by prefix, then cut by cap and
// signal truncated.
//
// RBAC — incarnation.run (the endpoint serves Run→Command preparation:
// whoever launches the run also resolves the SIDs for its fields). No new permission is
// introduced (reuse the run permission, the module-catalog → service.list pattern).
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// formPrepSIDCap — the upper bound on the number of SIDs in one response (DoS guard, souls
// up to 100k). The resolver fetches one more to tell "exactly cap" from "there is more"
// (truncated). UI autocomplete narrows the output with a prefix; the cap backstops an empty
// prefix on a large incarnation.
const formPrepSIDCap = 50

// FormPrepInput — the NATIVE request shape of POST /v1/modules/{name}/form-prep (handler-native
// T5d-2c-full). Replaces ModuleFormPrepRequest: huma-input (package api) binds/validates
// the body and projects it into these fields. Source — a discriminator (exactly one non-empty variant,
// XOR checked by the handler → 422); Prefix — an optional LIKE prefix.
type FormPrepInput struct {
	Source FormPrepSourceInput
	Prefix string
}

// FormPrepSourceInput — a NATIVE source discriminator: incarnation_hosts (incarnation name)
// XOR choir (Choir-source coordinates). Empty fields = "not set" (parity omitempty).
type FormPrepSourceInput struct {
	IncarnationHosts string
	Choir            *FormPrepChoirSource
}

// FormPrepChoirSource — Choir-source coordinates: incarnation + Choir name. Used
// both in the internal [FormPrepFilter] (passed to the SQL resolver) and as a source sub-object.
type FormPrepChoirSource struct {
	Incarnation string
	Name        string
}

// FormPrepResult — the NATIVE result of POST /v1/modules/{name}/form-prep (handler-native).
// A sorted slice of SIDs (non-nil) + a cap-truncation flag. Package api projects it
// into the native schema ModuleFormPrepReply (register func huma_module.go).
type FormPrepResult struct {
	Sids      []string
	Truncated bool
}

// FormPrepFilter — a resolved source for [FormPrepSIDResolver]. Exactly one of
// IncarnationHosts / Choir is non-empty (the handler guarantees it). Prefix is optional.
// An internal domain type (not wire): the handler collapses the source discriminator into
// this flat shape for the resolver.
type FormPrepFilter struct {
	IncarnationHosts string
	Choir            *FormPrepChoirSource
	Prefix           string
}

// FormPrepSIDResolver — resolves a form source catalog into live SIDs. Returns
// a slice sorted (by SID) of ≤ cap and truncated=true if it hit the cap.
type FormPrepSIDResolver interface {
	ResolveSIDs(ctx context.Context, filter FormPrepFilter) (sids []string, truncated bool, err error)
}

// ModuleFormPrepHandler — `POST /v1/modules/{name}/form-prep`.
//
// {name} in the path is currently unused during resolution (the source catalog does not depend
// on the module), but stays in the contract: the form is built per-module, and the endpoint
// logically belongs to the module. Dependencies immutable; safe for concurrent use.
type ModuleFormPrepHandler struct {
	resolver FormPrepSIDResolver
	logger   *slog.Logger
}

// NewModuleFormPrepHandler creates the handler. resolver is required for the
// production route (the router mounts the route only for a non-nil handler).
// logger nil → io.Discard.
func NewModuleFormPrepHandler(resolver FormPrepSIDResolver, logger *slog.Logger) *ModuleFormPrepHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ModuleFormPrepHandler{resolver: resolver, logger: logger}
}

// ModuleFormPrepSpecStub — a non-nil *ModuleFormPrepHandler stub for generating the
// huma OpenAPI fragment (parity [RoleSpecStub]). resolver nil — the handler never
// executes in spec mode.
func ModuleFormPrepSpecStub() *ModuleFormPrepHandler {
	return &ModuleFormPrepHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// FormPrepTyped — the domain function `POST /v1/modules/{name}/form-prep` (handler-native):
// resolves the source catalog without an http boundary. req — the native request shape (huma package api
// binds/validates the body and projects into it; huma rejects unknown → 400 before the call).
// Errors — *problemError (422 invalid source / 500 resolve failure); success —
// [FormPrepResult] (sids non-nil, sorted).
func (h *ModuleFormPrepHandler) FormPrepTyped(ctx context.Context, req FormPrepInput) (FormPrepResult, error) {
	filter, perr := toFilter(req)
	if perr != "" {
		return FormPrepResult{}, &problemError{problem.New(problem.TypeValidationFailed, "", perr)}
	}
	sids, truncated, err := h.resolver.ResolveSIDs(ctx, filter)
	if err != nil {
		h.logger.Error("module.form-prep: resolve sids failed", slog.Any("error", err))
		return FormPrepResult{}, &problemError{problem.New(problem.TypeInternalError, "", "resolve form source failed")}
	}
	if sids == nil {
		sids = []string{}
	}
	return FormPrepResult{Sids: sids, Truncated: truncated}, nil
}

// toFilter validates source (exactly one non-empty variant) and assembles [FormPrepFilter].
// Returns the validation error text (empty → ok). source fields — values (huma package api
// has already projected pointer-optional into the flat native shape).
func toFilter(req FormPrepInput) (FormPrepFilter, string) {
	inc := req.Source.IncarnationHosts
	prefix := req.Prefix
	hasInc := inc != ""
	hasChoir := req.Source.Choir != nil

	switch {
	case hasInc && hasChoir:
		return FormPrepFilter{}, "source must specify exactly one of incarnation_hosts/choir"
	case hasInc:
		if !incarnation.ValidName(inc) {
			return FormPrepFilter{}, "invalid incarnation name"
		}
		return FormPrepFilter{IncarnationHosts: inc, Prefix: prefix}, ""
	case hasChoir:
		c := req.Source.Choir
		if c.Incarnation == "" || c.Name == "" {
			return FormPrepFilter{}, "choir source requires incarnation and name"
		}
		if !incarnation.ValidName(c.Incarnation) {
			return FormPrepFilter{}, "invalid incarnation name"
		}
		return FormPrepFilter{Choir: c, Prefix: prefix}, ""
	default:
		return FormPrepFilter{}, "source must specify one of incarnation_hosts/choir"
	}
}

// --- production implementation over pgxpool.Pool ---

// FormPrepPGResolver — a production implementation of [FormPrepSIDResolver] over
// `souls` / `incarnation_choir_voices`. Cluster-wide source → SID[] resolution.
// Presence filter — `souls.status IN ('connected','dormant')` (parity with
// [VoyageCommandPGResolver]: a lightweight SQL snapshot without a Redis lease — form
// autocomplete carries no run security invariant, presence accuracy is not
// critical here).
type FormPrepPGResolver struct {
	db voyageResolverDB
}

// NewFormPrepPGResolver constructs the resolver. db is required.
func NewFormPrepPGResolver(db voyageResolverDB) *FormPrepPGResolver {
	return &FormPrepPGResolver{db: db}
}

// incarnationHostsSQL — live SIDs of incarnation hosts: souls with the Coven label
// `$1 = ANY(coven)` (ADR-008: incarnation.name is the root Coven label),
// online snapshot, optional prefix filter ($2 = ” → no filter), cap+1 ($3) to
// detect truncated. ORDER BY sid — determinism + stable autocomplete.
const formPrepIncarnationHostsSQL = `
SELECT sid FROM souls
WHERE $1 = ANY(coven)
  AND status IN ('connected', 'dormant')
  AND ($2 = '' OR sid LIKE $2 || '%')
ORDER BY sid ASC
LIMIT $3
`

// choirVoicesSQL — live SIDs of a Choir's Voices: join souls with
// incarnation_choir_voices on (incarnation_name, choir_name) (ADR-044).
// Cross-incarnation isolation — filter by incarnation_name. Presence/prefix/cap —
// as in incarnationHostsSQL.
const formPrepChoirVoicesSQL = `
SELECT s.sid FROM souls s
JOIN incarnation_choir_voices v ON v.sid = s.sid
WHERE v.incarnation_name = $1
  AND v.choir_name = $2
  AND s.status IN ('connected', 'dormant')
  AND ($3 = '' OR s.sid LIKE $3 || '%')
ORDER BY s.sid ASC
LIMIT $4
`

// ResolveSIDs resolves source → ≤ cap sorted SIDs + truncated.
// Fetches cap+1 rows: if more than cap arrived — cut to cap and truncated=true.
func (r *FormPrepPGResolver) ResolveSIDs(ctx context.Context, filter FormPrepFilter) ([]string, bool, error) {
	const limit = formPrepSIDCap + 1

	var (
		rows pgx.Rows
		err  error
	)
	if filter.Choir != nil {
		rows, err = r.db.Query(ctx, formPrepChoirVoicesSQL,
			filter.Choir.Incarnation, filter.Choir.Name, filter.Prefix, limit)
	} else {
		rows, err = r.db.Query(ctx, formPrepIncarnationHostsSQL,
			filter.IncarnationHosts, filter.Prefix, limit)
	}
	if err != nil {
		return nil, false, errors.Join(errors.New("form-prep resolver: query souls"), err)
	}
	defer rows.Close()

	out := make([]string, 0, formPrepSIDCap)
	truncated := false
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, false, errors.Join(errors.New("form-prep resolver: scan"), err)
		}
		if len(out) == formPrepSIDCap {
			truncated = true // the (cap+1)th row arrived → there is more.
			break
		}
		out = append(out, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.Join(errors.New("form-prep resolver: iter"), err)
	}
	return out, truncated, nil
}
