package handlers

// GET /v1/deprecations — the fleet-wide answer to "which incarnations still pass
// a param that is on its way out, and by when does it stop working"
// (ADR-0076(v), NIM-268).
//
// Sourced from DEFINITIONS, not from run history. The notices stored on past
// runs (ADR-0076(u)) answer a neighbouring question — who passed it in the runs
// we happened to observe — which is blind to an incarnation nobody has run this
// month and still accuses one fixed yesterday. This walks the text that will be
// rendered next time.
//
// RBAC: the incarnation set is narrowed by the SAME resolveListScope the
// incarnation list uses, and the survey is handed that narrowed set — it never
// resolves incarnations itself. A second implementation of a scope rule is how
// two of them drift apart into a leak (NIM-219, NIM-209), so there is only one.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
)

// maxDeprecationScanIncarnations caps how many incarnations one survey walks.
// Each distinct (service, ref) costs a snapshot materialization, so an unbounded
// scan on a large estate is a slow request holding a connection. The cap is
// REPORTED (Truncated) rather than silently applied: a survey that quietly
// stopped early would understate the fleet, which is the one thing this endpoint
// must never do.
const maxDeprecationScanIncarnations = 500

// errServiceNotRegistered — the incarnation names a service the registry does
// not resolve. Becomes a load_failed gap, not a failed request: one dangling
// service must not blank the report for every other one.
var errServiceNotRegistered = errors.New("service is not registered")

// DeprecationSiteView — one incarnation passing a deprecated param, with the
// coordinates needed to go and fix it.
type DeprecationSiteView struct {
	Incarnation    string
	Service        string
	ServiceVersion string
	Scenario       string
	Where          string
}

// DeprecationUsageView — one deprecated param and everywhere it is still passed.
// The window is flattened into Since/RemovedIn/Use because that is how the wire
// carries it; the grouping (one row per param, not per site) is the unit of work
// an operator actually schedules.
type DeprecationUsageView struct {
	Module    string
	Param     string
	Since     string
	RemovedIn string
	Use       string
	Sites     []DeprecationSiteView
}

// DeprecationGapView — a place the survey could not look. Present so a caller
// never reads "clean" off "unchecked".
type DeprecationGapView struct {
	Scope        string
	Subject      string
	Reason       string
	Detail       string
	Incarnations []string
}

// DeprecationsReply — the survey as the API returns it.
type DeprecationsReply struct {
	Items []DeprecationUsageView
	Gaps  []DeprecationGapView
	// ScannedIncarnations / ScannedDefinitions — coverage denominators, so a
	// reader can tell "3 findings across 12 incarnations" from "3 findings, and
	// we only managed to look at 2 of them".
	ScannedIncarnations int
	ScannedDefinitions  int
	// Truncated — the estate is larger than one survey walks; the answer is a
	// partial view.
	Truncated bool
}

// DeprecationsTyped is the domain function of GET /v1/deprecations.
func (h *IncarnationHandler) DeprecationsTyped(ctx context.Context, claims *jwt.Claims) (DeprecationsReply, error) {
	empty := DeprecationsReply{Items: []DeprecationUsageView{}, Gaps: []DeprecationGapView{}}
	if h.services == nil || h.loader == nil {
		return empty, &problemError{problem.New(problem.TypeInternalError, "", "service registry is not wired")}
	}

	scope, ok := h.resolveListScope(ctx, claims, "list", "")
	if !ok {
		// fail-closed: an undefined scope yields an empty survey, never the
		// whole estate.
		return empty, nil
	}

	incs, _, err := incarnation.SelectAll(ctx, h.db, incarnation.ListFilter{}, scope, 0, maxDeprecationScanIncarnations+1)
	if err != nil {
		h.logger.Error("deprecations: list incarnations failed", slog.Any("error", err))
		return empty, &problemError{problem.New(problem.TypeInternalError, "", "list incarnations failed")}
	}
	truncated := len(incs) > maxDeprecationScanIncarnations
	if truncated {
		incs = incs[:maxDeprecationScanIncarnations]
	}
	flat := make([]incarnation.Incarnation, 0, len(incs))
	for _, inc := range incs {
		if inc != nil {
			flat = append(flat, *inc)
		}
	}

	// One snapshot for the whole survey, taken here rather than per definition:
	// every incarnation is then judged against the same catalog, so the answer is
	// coherent instead of reflecting an allow-list that moved mid-scan. A nil
	// source (no Sigil service) yields a nil resolver, and plugin modules land in
	// gaps rather than being counted clean.
	modules := artifact.SnapshotModuleManifests(ctx, h.moduleManifests)
	scanner := scenario.NewDeprecationScanner(h.loader, h.logger)
	survey := scanner.Survey(ctx, flat, h.snapshotForIncarnation, modules)

	return DeprecationsReply{
		Items:               toDeprecationUsageViews(survey.Usages),
		Gaps:                toDeprecationGapViews(survey.Gaps),
		ScannedIncarnations: survey.ScannedIncarnations,
		ScannedDefinitions:  survey.ScannedDefinitions,
		Truncated:           truncated,
	}, nil
}

// snapshotForIncarnation materializes the definition an incarnation is pinned
// to: the registry resolves the service's git coordinates, and the incarnation's
// own ServiceVersion overrides the ref — the survey must read the text THIS
// incarnation would render, not whatever the registry's default ref points at.
func (h *IncarnationHandler) snapshotForIncarnation(ctx context.Context, inc incarnation.Incarnation) (*artifact.ServiceArtifact, error) {
	ref, found := h.services.Resolve(inc.Service)
	if !found {
		return nil, errServiceNotRegistered
	}
	if inc.ServiceVersion != "" {
		ref.Ref = inc.ServiceVersion
	}
	return h.loader.Load(ctx, ref)
}

func toDeprecationUsageViews(in []scenario.DeprecationUsage) []DeprecationUsageView {
	out := make([]DeprecationUsageView, 0, len(in))
	for _, u := range in {
		sites := make([]DeprecationSiteView, 0, len(u.Sites))
		for _, s := range u.Sites {
			sites = append(sites, DeprecationSiteView{
				Incarnation: s.Incarnation, Service: s.Service,
				ServiceVersion: s.ServiceVersion, Scenario: s.Scenario, Where: s.Where,
			})
		}
		out = append(out, DeprecationUsageView{
			Module: u.Module, Param: u.Param,
			Since: u.Deprecated.Since, RemovedIn: u.Deprecated.RemovedIn, Use: u.Deprecated.Use,
			Sites: sites,
		})
	}
	return out
}

func toDeprecationGapViews(in []scenario.DeprecationGap) []DeprecationGapView {
	out := make([]DeprecationGapView, 0, len(in))
	for _, g := range in {
		out = append(out, DeprecationGapView{
			Scope: g.Scope, Subject: g.Subject, Reason: g.Reason,
			Detail: g.Detail, Incarnations: g.Incarnations,
		})
	}
	return out
}
