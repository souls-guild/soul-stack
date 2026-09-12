package scenario

// The keeper-side half of the `validate:` incarnation context (NIM-833): what the
// day-2 request path is willing to answer for. The stance itself, and why the
// create path answers for less, live in shared/config/validate_scope.go.

import (
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// DayTwoIncarnation builds the `validate:` incarnation context from a loaded
// incarnation row, for the run path (REST and MCP both reach it).
//
// The field set is deliberately a SUBSET of what the run's own `incarnation.*`
// carries (render.incarnationVars): id, service, service_version, state. A rule
// that passes this gate must not be able to read a field the run then lacks, and
// the one field the run has and this does not — `host_count` — is a roster
// question, which is not knowable on the request path and belongs in `assert:`
// anyway.
//
// `label` is NOT here and must not be ([ADR-0085]): a caption is mutable, and an
// expression that reads one turns editing a screen into changing what a run does.
// `validate:` is the fourth CEL environment that invariant has to hold in — see
// validate_scope_guard_test.go beside this file, and its three siblings named in
// keeper/internal/render/label_invariant_guard_test.go.
//
// The ADR-0085 `incarnation.name` alias comes from [cel.IncarnationRoot], the one
// statement of that compatibility window, rather than being restated here.
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
func DayTwoIncarnation(id, service, serviceVersion string, state map[string]any) config.ValidateContext {
	m := map[string]any{
		"id":              id,
		"service":         service,
		"service_version": serviceVersion,
	}
	// A NULL state column omits the KEY but not the FIELD: the day-2 path answers
	// for `state` on every request, so `incarnation.state.<x>` over an empty row is
	// the ordinary no-such-key the run would give, not "this path does not have
	// that fact". Deriving the answerable set from the row instead made the same
	// scenario report broken for one incarnation and fine for its sibling.
	if state != nil {
		m["state"] = state
	}
	return config.LoadedIncarnation(dayTwoFields, cel.IncarnationRoot(m))
}

// dayTwoFields is what the day-2 request path answers for — fixed, request- and
// row-independent. A strict subset of the run's `incarnation.*`
// (render.incarnationVars) plus the ADR-0085 `name` alias, which
// [cel.IncarnationRoot] derives from `id`.
var dayTwoFields = []string{
	cel.IncarnationIDField, cel.LegacyIncarnationIDField,
	"service", "service_version", "state",
}
