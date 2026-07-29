package api

// GET /v1/deprecations — fleet-wide deprecation survey (ADR-0076(v), NIM-268).

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// DeprecationSiteEntry — one incarnation still passing a deprecated param, with
// the coordinates to go and fix it: which service at which ref, which scenario,
// and the YAML-ish location inside it.
type DeprecationSiteEntry struct {
	Incarnation    string `json:"incarnation"`
	Service        string `json:"service"`
	ServiceVersion string `json:"service_version,omitempty"`
	Scenario       string `json:"scenario"`
	Where          string `json:"where,omitempty"`
}

// DeprecationEntry — one deprecated param and every place it is still used.
// Grouped per param rather than per site because that is the unit of work an
// operator schedules: you migrate `address` → `addr` once, across N places.
type DeprecationEntry struct {
	Module    string                 `json:"module"`
	Param     string                 `json:"param"`
	Since     string                 `json:"since,omitempty"`
	RemovedIn string                 `json:"removed_in,omitempty"`
	Use       string                 `json:"use,omitempty"`
	Sites     []DeprecationSiteEntry `json:"sites"`
}

// DeprecationGapEntry — a place the survey could NOT look, and why. The presence
// of this list is what keeps the reply honest: without it an empty `items` reads
// as "nothing to migrate" when it may mean "nothing could be checked" — most
// often because a plugin module's manifest ships beside its binary rather than
// with the definition (NIM-228).
type DeprecationGapEntry struct {
	Scope        string   `json:"scope" enum:"service,module"`
	Subject      string   `json:"subject"`
	Reason       string   `json:"reason"`
	Detail       string   `json:"detail,omitempty"`
	Incarnations []string `json:"incarnations"`
}

// DeprecationsReply — the survey. items is ordered by `removed_in` (what breaks
// first), so the top of the list is what to migrate now.
type DeprecationsReply struct {
	Items []DeprecationEntry    `json:"items"`
	Gaps  []DeprecationGapEntry `json:"gaps"`
	// ScannedIncarnations / ScannedDefinitions — coverage denominators: how much
	// of the estate this answer actually covers.
	ScannedIncarnations int `json:"scanned_incarnations"`
	ScannedDefinitions  int `json:"scanned_definitions"`
	// Truncated — the estate is larger than one survey walks; the answer is a
	// partial view rather than a clean bill of health.
	Truncated bool `json:"truncated,omitempty"`
}

type deprecationsInput struct{}

type deprecationsOutput struct {
	Body DeprecationsReply
}

func deprecationsOperation() huma.Operation {
	return huma.Operation{
		OperationID: "listDeprecations",
		Method:      http.MethodGet,
		Path:        "/",
		Summary:     "Deprecated params still used across the estate",
		Description: "Which incarnations still pass a module param marked `deprecated:` in its manifest, and the release it stops working in (ADR-0076(v)). Sourced from the DEFINITIONS the incarnations are pinned to - NOT from run history, which would miss an incarnation nobody has run lately and still list one fixed yesterday. `items` is ordered by `removed_in`: what breaks first comes first. `gaps` lists what could not be checked (most often a plugin module, whose manifest ships beside its binary rather than with the definition) - an empty `items` with a non-empty `gaps` means \"not checked\", not \"clean\". Visibility scoped by RBAC exactly as the incarnation list (fail-closed: empty scope -> empty survey). Permission incarnation.list. Read-only.",
		Tags:        []string{"deprecations"},

		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

func registerHumaDeprecationsList(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, deprecationsOperation(), func(ctx context.Context, _ *deprecationsInput) (*deprecationsOutput, error) {
		reply, err := incH.DeprecationsTyped(ctx, claimsOrNil(ctx))
		if err != nil {
			return nil, incProblem(err)
		}
		return &deprecationsOutput{Body: newDeprecationsReply(reply)}, nil
	})
}

// newDeprecationsReply projects the domain view into the wire type. Slices are
// materialized non-nil so the body carries `[]` rather than `null` — a client
// distinguishing "no findings" from "field absent" should not have to.
func newDeprecationsReply(v handlers.DeprecationsReply) DeprecationsReply {
	items := make([]DeprecationEntry, 0, len(v.Items))
	for _, u := range v.Items {
		sites := make([]DeprecationSiteEntry, 0, len(u.Sites))
		for _, s := range u.Sites {
			sites = append(sites, DeprecationSiteEntry{
				Incarnation: s.Incarnation, Service: s.Service,
				ServiceVersion: s.ServiceVersion, Scenario: s.Scenario, Where: s.Where,
			})
		}
		items = append(items, DeprecationEntry{
			Module: u.Module, Param: u.Param,
			Since: u.Since, RemovedIn: u.RemovedIn, Use: u.Use,
			Sites: sites,
		})
	}
	gaps := make([]DeprecationGapEntry, 0, len(v.Gaps))
	for _, g := range v.Gaps {
		incs := g.Incarnations
		if incs == nil {
			incs = []string{}
		}
		gaps = append(gaps, DeprecationGapEntry{
			Scope: g.Scope, Subject: g.Subject, Reason: g.Reason,
			Detail: g.Detail, Incarnations: incs,
		})
	}
	return DeprecationsReply{
		Items:               items,
		Gaps:                gaps,
		ScannedIncarnations: v.ScannedIncarnations,
		ScannedDefinitions:  v.ScannedDefinitions,
		Truncated:           v.Truncated,
	}
}
