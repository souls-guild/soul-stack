package handlers

import (
	"context"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/config"
)

// `GET /v1/services/{id}/compat` — the engine-compat window of a Service
// BEFORE any run (ADR-0076(h)).
//
// The window is computed here, from the per-entity declarations, and never
// stored: the same [config] helpers back this view and the render-path gate, so
// the number the UI shows cannot drift from the number that blocks a run. The UI
// renders `status` + `detail` as given — the compatibility verdict is a backend
// catalog value, not something the frontend re-derives from min/max.

// Compat status values reported by [ServiceHandler.ListServiceCompatTyped].
// A closed set; the UI keys its badge off these strings.
const (
	// CompatStatusOK — every declared window admits this keeper build (or nothing
	// was declared: an unbounded definition is compatible by construction).
	CompatStatusOK = "ok"
	// CompatStatusUnsupported — this keeper build is outside the effective window;
	// a run on THIS instance is rejected with `keeper_version_unsupported`.
	CompatStatusUnsupported = "unsupported"
	// CompatStatusNotEnforced — this keeper build carries no comparable version
	// (`0.0.0-dev`, a bare commit hash): the window is not enforced, and that is
	// said out loud rather than reported as a pass (ADR-0076(e)).
	CompatStatusNotEnforced = "not_enforced"
	// CompatStatusWindowEmpty — the intersection is unsatisfiable (`compat_window_empty`):
	// no keeper version can ever run this definition, so it is an authoring error
	// rather than an upgrade/downgrade decision.
	CompatStatusWindowEmpty = "window_empty"
)

// CompatWindowView — a declared or effective `[min, max)` window on the wire.
// Display carries the rendered half-open form (`[0.1.0, 0.3.0)`, `>= 0.2.0`) so
// the UI does not re-implement the notation; min/max stay separate for sorting
// and comparison. An absent bound is omitted (one-sided window).
type CompatWindowView struct {
	Min     string `json:"min,omitempty"`
	Max     string `json:"max,omitempty"`
	Display string `json:"display"`
}

// CompatEntityView — one contributor to the effective window: which artifact
// declared it and at which git ref (ADR-007). Window is null when the entity
// declares none (unbounded — the backcompat default). Compatible is the
// per-entity verdict for this keeper build; it is `true` for an unbounded entity
// and for a build with no comparable version (nothing was enforced).
type CompatEntityView struct {
	Kind       string            `json:"kind"`
	Name       string            `json:"id"`
	Ref        string            `json:"ref,omitempty"`
	Window     *CompatWindowView `json:"window"`
	Compatible bool              `json:"compatible"`
}

// ServiceCompatReply — `GET /v1/services/{id}/compat` body. Self-contained
// (like [ServiceTelemetryReply]): service + ref echo, snapshot sha1 (== ETag),
// the running keeper's version, the verdict, the effective window and the
// per-entity contributions that produced it.
//
// KeeperVersion is the RAW build string this instance reports; KeeperRelease is
// the comparable release core actually used for the comparison (empty when the
// build carries no version). EffectiveWindow is null when nothing declared a
// window. Entities is never null (the service itself is always one entry).
type ServiceCompatReply struct {
	Service         string             `json:"service"`
	Ref             string             `json:"ref"`
	SHA1            string             `json:"sha1"`
	KeeperVersion   string             `json:"keeper_version"`
	KeeperRelease   string             `json:"keeper_release,omitempty"`
	Enforced        bool               `json:"enforced"`
	Status          string             `json:"status"`
	Detail          string             `json:"detail,omitempty"`
	EffectiveWindow *CompatWindowView  `json:"effective_window"`
	Entities        []CompatEntityView `json:"entities"`
}

// ListServiceCompatTyped — `GET /v1/services/{id}/compat` (READ without
// audit): registry lookup + the declared windows of the service snapshot and of
// every destiny it pulls → the effective window and the verdict for THIS keeper
// instance (its build version comes from the handler, not the request). name/ref
// come in as arguments (ref="" → the registry default). Errors are *problemError
// (500 no lister / registry failure, 404 not-found, 502 loader failed); success is
// [ServiceCompatReply].
func (h *ServiceHandler) ListServiceCompatTyped(ctx context.Context, id, ref string) (ServiceCompatReply, error) {
	var zero ServiceCompatReply
	if h.compat == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "service compat lister not configured")}
	}

	entry, err := h.svc.GetService(ctx, id)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "service "+id+" not found")}
	default:
		h.logger.Error("service.compat: get service failed",
			slog.String("id", id),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get service failed")}
	}

	if ref == "" {
		ref = entry.Ref
	}

	catalog, err := h.compat.ListServiceCompat(ctx, entry.ID, entry.Git, ref)
	if err != nil {
		h.logger.Warn("service.compat: loader failed",
			slog.String("id", id),
			slog.String("git", entry.Git),
			slog.String("ref", ref),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "compat loader failed for service "+id+": "+err.Error())}
	}
	if catalog == nil {
		// Defensive: the lister must return non-nil when err=nil (the
		// ListServiceTelemetry pattern).
		h.logger.Error("service.compat: loader returned nil without error", slog.String("id", id))
		return zero, &problemError{problem.New(problem.TypeBadGateway, "", "compat loader returned empty result")}
	}

	return buildServiceCompatReply(entry.ID, ref, catalog, h.keeperVersion), nil
}

// buildServiceCompatReply assembles the wire view from the declared
// contributions. Split out from the endpoint so the verdict logic is unit-testable
// without a registry or git.
func buildServiceCompatReply(service, ref string, catalog *serviceregistry.CompatCatalog, keeperVersion string) ServiceCompatReply {
	release, comparable := config.NormalizeEngineVersion(keeperVersion)
	effective := config.IntersectKeeperWindows(catalog.Entities)

	entities := make([]CompatEntityView, 0, len(catalog.Entities))
	for _, e := range catalog.Entities {
		entities = append(entities, CompatEntityView{
			Kind:       e.Kind,
			Name:       e.Name,
			Ref:        e.Ref,
			Window:     toCompatWindowView(e.Window),
			Compatible: !comparable || e.Window.Contains(release),
		})
	}

	reply := ServiceCompatReply{
		Service:         service,
		Ref:             ref,
		SHA1:            catalog.SHA1,
		KeeperVersion:   keeperVersion,
		KeeperRelease:   release,
		Enforced:        comparable && effective.Declared(),
		EffectiveWindow: toCompatWindowView(effective),
		Entities:        entities,
	}

	// An unsatisfiable intersection outranks the version verdict: no keeper build
	// can ever satisfy it, so "upgrade/downgrade keeper" would be wrong advice.
	switch {
	case effective.IsEmpty():
		// Enforced stays as computed: an unsatisfiable intersection does not disable
		// the gate — the per-entity checks reject EVERY comparable version, so a run
		// on such a definition is blocked, and reporting enforced=false here would
		// read as "it would still run".
		reply.Status = CompatStatusWindowEmpty
		reply.Detail = "the declared windows do not overlap: effective " + effective.String() +
			" can never be satisfied (max is exclusive) - narrow one of the contributing declarations"
	case !effective.Declared():
		reply.Status = CompatStatusOK
		reply.Detail = "no entity declares a keeper window - unbounded (ADR-0076)"
	case !comparable:
		reply.Status = CompatStatusNotEnforced
		reply.Detail = "not enforced (" + keeperVersion + " carries no version): the declared window is " + effective.String()
	case effective.Contains(release):
		reply.Status = CompatStatusOK
		reply.Detail = "keeper " + keeperVersion + " is inside the declared window " + effective.String()
	default:
		reply.Status = CompatStatusUnsupported
		reply.Detail = "keeper " + keeperVersion + " is outside the declared window " + effective.String() +
			" - a run on this instance is rejected with keeper_version_unsupported"
		if bad := config.FirstIncompatibleEntity(catalog.Entities, release); bad != nil {
			reply.Detail = config.NewKeeperCompatError(*bad, keeperVersion).Error()
		}
	}
	return reply
}

// toCompatWindowView projects a declared window onto the wire; nil (unbounded)
// stays nil so the UI can distinguish "no declaration" from "any version".
func toCompatWindowView(w *config.VersionWindow) *CompatWindowView {
	if !w.Declared() {
		return nil
	}
	return &CompatWindowView{Min: w.Min, Max: w.Max, Display: w.String()}
}

// EarlyCompatCheck — the convenience check at service registration / pin change
// (ADR-0076(f)). The render path is the AUTHORITY: keeper is a rolling-upgraded
// cluster, so a verdict recorded at registration says nothing about the instance
// that renders a run months later. This check exists only to turn a mistake into
// a fast, actionable 4xx while the operator can still pick a different ref.
//
// Returns nil (allow) when:
//   - no compat lister is wired;
//   - the snapshot cannot be loaded — registration must NOT acquire a hard
//     dependency on git reachability, so an unreachable repo is logged and
//     allowed through exactly as before this check existed;
//   - this build carries no comparable version, or nothing declares a window.
//
// Returns a 422 *problemError when the declared window provably excludes this
// keeper, or when the declarations cannot overlap at all.
func (h *ServiceHandler) EarlyCompatCheck(ctx context.Context, op, id, gitURL, ref string) error {
	if h.compat == nil {
		return nil
	}
	catalog, err := h.compat.ListServiceCompat(ctx, id, gitURL, ref)
	if err != nil || catalog == nil {
		h.logger.Warn("service.compat: early window check skipped - snapshot unavailable (ADR-0076)",
			slog.String("op", op),
			slog.String("id", id),
			slog.String("git", gitURL),
			slog.String("ref", ref),
			slog.Any("error", err),
		)
		return nil
	}

	effective := config.IntersectKeeperWindows(catalog.Entities)
	if effective.IsEmpty() {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"compat_window_empty: the declared keeper windows of service "+id+" and its destinies do not overlap (effective "+
				effective.String()+", max is exclusive) - no keeper version can render this definition")}
	}

	release, comparable := config.NormalizeEngineVersion(h.keeperVersion)
	if !comparable {
		return nil
	}
	bad := config.FirstIncompatibleEntity(catalog.Entities, release)
	if bad == nil {
		return nil
	}
	return &problemError{problem.New(problem.TypeValidationFailed, "",
		config.NewKeeperCompatError(*bad, h.keeperVersion).Error())}
}
