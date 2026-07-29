package scenario

// Fleet-wide deprecation survey (ADR-0076(v), NIM-268): which incarnations still
// pass a param that is on its way out, and by which release it stops working.
//
// SOURCE IS THE DEFINITION, not the runs. `apply_runs.notices` (ADR-0076(u))
// records what observed runs reported, which answers a neighbouring but wrong
// question: an incarnation nobody has run this month is absent from it although
// its definition breaks at `removed_in`, and one fixed yesterday is still
// present because the old rows remain. A migration planned against that is
// planned against noise. So this walks the text that will be rendered NEXT time.
//
// Definitions are loaded per (service, ref) rather than per incarnation: twenty
// incarnations of one service at one ref are one git snapshot and one parse, and
// the loader caches the snapshot anyway. What varies per incarnation is only
// which group it belongs to.
//
// Plugin modules are resolved through the catalog NIM-228 introduced (keeper
// snapshots it from its Sigil grants), so the survey covers them on the same
// terms as core. What it still cannot read it REPORTS rather than skips — a
// service whose snapshot fails to load, a scenario that will not parse, a plugin
// the catalog does not carry. The alternative is an empty finding list that
// reads as "nothing to migrate" when it means "nothing was looked at", which is
// the failure this whole surface exists to avoid.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// DeprecationUsage — one deprecated param, and every incarnation whose
// definition passes it. Grouped this way because that is the unit of work: an
// operator migrates a param, and needs to know which incarnations that touches.
type DeprecationUsage struct {
	Module     string
	Param      string
	Deprecated plugin.DeprecatedDef
	Sites      []DeprecationSite
}

// DeprecationSite — one incarnation passing the param, with enough coordinates
// to go and fix it: which service at which ref, which scenario, which line-ish
// location inside it.
type DeprecationSite struct {
	Incarnation    string
	Service        string
	ServiceVersion string
	Scenario       string
	Where          string
}

// DeprecationGap — a place the survey could not look. Reported beside the
// findings so a caller never mistakes "clean" for "unchecked".
type DeprecationGap struct {
	// Scope — what the gap covers: `service` (the whole definition failed to
	// load or parse) or `module` (one module's contract is unreadable).
	Scope string
	// Subject — the service ref or the module address the gap is about.
	Subject string
	// Reason — machine-readable: `plugin_namespace` / `unknown_core_module`
	// (from the definition walk), `load_failed` / `parse_failed` (here).
	Reason string
	// Detail — the error text, when there is one worth showing.
	Detail string
	// Incarnations — which incarnations are affected by this gap.
	Incarnations []string
}

// Gap reasons owned by this survey (the module-level ones come from
// [config.DeprecatedScan]).
const (
	// GapLoadFailed — the service snapshot could not be materialized: the git
	// ref is gone, the remote is unreachable, credentials changed.
	GapLoadFailed = "load_failed"
	// GapParseFailed — the snapshot loaded but a scenario would not parse, so
	// its tasks were never inspected.
	GapParseFailed = "parse_failed"
)

// DeprecationSurvey — the whole answer: what is deprecated and still used, what
// could not be checked, and how much was covered.
type DeprecationSurvey struct {
	Usages []DeprecationUsage
	Gaps   []DeprecationGap
	// ScannedIncarnations / ScannedDefinitions — coverage denominators. A
	// caller showing "3 deprecated params across 12 incarnations" needs the 12
	// to come from the survey, not from a separate count that may disagree.
	ScannedIncarnations int
	ScannedDefinitions  int
}

// DeprecationScanner surveys definitions of the incarnations it is given.
//
// It takes the incarnations ALREADY narrowed by the caller's RBAC scope, rather
// than resolving them itself: the incarnation list endpoint owns that narrowing
// (incarnation.ListScope, including the boolean-scope predicate of NIM-128), and
// a second implementation of the same rule is how the two drift apart into a
// leak. This type never widens the set it is handed.
type DeprecationScanner struct {
	loader SnapshotReader
	logger *slog.Logger
}

// SnapshotReader — the only thing the survey (and the scenario parse helpers it
// reuses) needs of a service loader: read a file out of a materialized snapshot.
// Narrow on purpose — the handler already holds a loader behind its own
// interface, and widening this to the concrete *artifact.ServiceLoader would
// force a second wiring path for no capability.
type SnapshotReader interface {
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
}

// SnapshotResolver materializes the definition behind one incarnation. Supplied
// by the caller rather than resolved here, because resolving a service ref is
// the registry's job and the caller already holds it — and because a survey that
// could reach for arbitrary definitions on its own would be a second, unaudited
// path to the same artifacts.
//
// An error becomes a [DeprecationGap] rather than failing the survey: one
// unreachable git remote must not blank the report for every other service.
type SnapshotResolver func(ctx context.Context, inc incarnation.Incarnation) (*artifact.ServiceArtifact, error)

// NewDeprecationScanner builds a scanner over the shared service loader (its
// snapshot cache is what makes a per-(service, ref) walk affordable).
func NewDeprecationScanner(loader SnapshotReader, logger *slog.Logger) *DeprecationScanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeprecationScanner{loader: loader, logger: logger}
}

// definitionKey — one service snapshot: several incarnations of the same service
// at the same ref share a parse.
type definitionKey struct {
	service string
	version string
}

// Survey walks the definitions behind incs and returns what is deprecated, what
// could not be read, and how much was covered.
//
// Errors are per-definition, never fatal: one unreachable git remote must not
// blank the report for every other service. Such a failure becomes a
// [DeprecationGap], which is the difference between "we checked and it is fine"
// and "we could not check".
func (s *DeprecationScanner) Survey(ctx context.Context, incs []incarnation.Incarnation, snapshotFor SnapshotResolver, modules config.ModuleManifestResolver) DeprecationSurvey {
	groups := map[definitionKey][]incarnation.Incarnation{}
	order := make([]definitionKey, 0)
	for _, inc := range incs {
		k := definitionKey{service: inc.Service, version: inc.ServiceVersion}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], inc)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].service != order[j].service {
			return order[i].service < order[j].service
		}
		return order[i].version < order[j].version
	})

	survey := DeprecationSurvey{ScannedIncarnations: len(incs)}
	byParam := map[string]*DeprecationUsage{}
	gaps := map[string]*DeprecationGap{}

	for _, k := range order {
		members := groups[k]
		names := incarnationNames(members)
		art, err := snapshotFor(ctx, members[0])
		if err != nil {
			addGap(gaps, "service", k.service+"@"+k.version, GapLoadFailed, err.Error(), names)
			continue
		}
		survey.ScannedDefinitions++
		s.surveyDefinition(art, k, members, names, modules, byParam, gaps)
	}

	survey.Usages = flattenUsages(byParam)
	survey.Gaps = flattenGaps(gaps)
	return survey
}

// surveyDefinition walks every scenario of one service snapshot. Every scenario
// counts, not only the ones anybody runs today: a deprecated param in a rarely
// used scenario breaks just as hard the day it IS run, and the point of the
// window is to have migrated before then.
func (s *DeprecationScanner) surveyDefinition(
	art *artifact.ServiceArtifact, k definitionKey,
	members []incarnation.Incarnation, names []string,
	modules config.ModuleManifestResolver,
	byParam map[string]*DeprecationUsage, gaps map[string]*DeprecationGap,
) {
	scenarioNames, err := s.scenarioDirs(art)
	if err != nil {
		addGap(gaps, "service", k.service+"@"+k.version, GapParseFailed, err.Error(), names)
		return
	}
	for _, name := range scenarioNames {
		tasks, err := s.scenarioTasks(art, name, modules)
		if err != nil {
			addGap(gaps, "service", k.service+"@"+k.version+"/"+name,
				GapParseFailed, err.Error(), names)
			continue
		}
		scan := config.ScanTasksForDeprecated(tasks, modules)
		for _, u := range scan.Uses {
			key := u.Module + "\x00" + u.Param
			agg, seen := byParam[key]
			if !seen {
				agg = &DeprecationUsage{Module: u.Module, Param: u.Param, Deprecated: u.Deprecated}
				byParam[key] = agg
			}
			for _, inc := range members {
				agg.Sites = append(agg.Sites, DeprecationSite{
					Incarnation:    inc.Name,
					Service:        k.service,
					ServiceVersion: k.version,
					Scenario:       name,
					Where:          u.Where,
				})
			}
		}
		for _, un := range scan.Unresolved {
			addGap(gaps, "module", un.Module, un.Reason, "", names)
		}
	}
}

// scenarioDirs lists the scenario names present in the snapshot by reading the
// directory, NOT via [artifact.ListScenarios].
//
// That helper has partial-success semantics on purpose — a scenario whose YAML
// is broken is logged and dropped, so a UI dropdown still shows the others. For
// a survey that behavior is exactly wrong: the definition it cannot parse would
// vanish from the walk and the service would be reported as having nothing to
// migrate, which is the silence this whole surface exists to remove. Reading the
// directory keeps every name, and the ones that will not parse become gaps a few
// lines below.
func (s *DeprecationScanner) scenarioDirs(art *artifact.ServiceArtifact) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(art.LocalDir, "scenario"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A service with no scenario/ directory is valid and has nothing to
			// scan — not a gap.
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// scenarioTasks reads one scenario and expands its includes, so a deprecated
// param inside an included fragment is found too — an include is ordinary task
// text, and missing it would under-report exactly the definitions that factored
// their work out.
func (s *DeprecationScanner) scenarioTasks(art *artifact.ServiceArtifact, scenarioName string, modules config.ModuleManifestResolver) ([]config.Task, error) {
	scn, err := parseScenarioFromArtifact(s.loader, art, scenarioName, false, modules)
	if err != nil {
		return nil, err
	}
	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(s.loader, art, scenarioName))
	if diag.HasErrors(idiags) {
		return nil, fmt.Errorf("expanding include: %s", firstError(idiags))
	}
	return expanded, nil
}

func incarnationNames(incs []incarnation.Incarnation) []string {
	out := make([]string, 0, len(incs))
	for _, i := range incs {
		out = append(out, i.Name)
	}
	sort.Strings(out)
	return out
}

// addGap merges by (scope, subject, reason): one plugin module used by twenty
// tasks in ten incarnations is ONE coverage gap, listing the incarnations it
// affects.
func addGap(gaps map[string]*DeprecationGap, scope, subject, reason, detail string, incs []string) {
	key := scope + "\x00" + subject + "\x00" + reason
	g, seen := gaps[key]
	if !seen {
		g = &DeprecationGap{Scope: scope, Subject: subject, Reason: reason, Detail: detail}
		gaps[key] = g
	}
	g.Incarnations = appendUnique(g.Incarnations, incs...)
}

func appendUnique(dst []string, add ...string) []string {
	seen := make(map[string]struct{}, len(dst))
	for _, d := range dst {
		seen[d] = struct{}{}
	}
	for _, a := range add {
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		dst = append(dst, a)
	}
	sort.Strings(dst)
	return dst
}

// flattenUsages orders by what breaks FIRST: an operator reads this to decide
// what to migrate now, and `removed_in` is the deadline. Ties fall back to the
// address so two surveys of the same estate agree.
func flattenUsages(byParam map[string]*DeprecationUsage) []DeprecationUsage {
	out := make([]DeprecationUsage, 0, len(byParam))
	for _, u := range byParam {
		sort.Slice(u.Sites, func(i, j int) bool {
			if u.Sites[i].Incarnation != u.Sites[j].Incarnation {
				return u.Sites[i].Incarnation < u.Sites[j].Incarnation
			}
			if u.Sites[i].Scenario != u.Sites[j].Scenario {
				return u.Sites[i].Scenario < u.Sites[j].Scenario
			}
			return u.Sites[i].Where < u.Sites[j].Where
		})
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Deprecated.RemovedIn != out[j].Deprecated.RemovedIn {
			return out[i].Deprecated.RemovedIn < out[j].Deprecated.RemovedIn
		}
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		return out[i].Param < out[j].Param
	})
	return out
}

func flattenGaps(gaps map[string]*DeprecationGap) []DeprecationGap {
	out := make([]DeprecationGap, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
