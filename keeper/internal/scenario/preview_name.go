package scenario

// Live preview of the incarnation name a create scenario COMPOSES from its
// `name_template` (ADR-0079). The create form calls it as the operator types the
// `input:` components, so the name is on screen before the request that would make
// it permanent — the name is the immutable primary key, and a wrong one costs a
// destroy and a re-create.
//
// The composition itself is NOT reimplemented here: [PreviewName] renders through
// [ComposeName], the same function the create path reaches via
// composeIncarnationName. Blocks in a template are CEL, and a second evaluator —
// client-side above all — would coerce numbers and bools by its own rules and
// compose a DIFFERENT string from the same input. The operator would then be shown
// one identity and handed another, silently. One evaluator per system is the whole
// point of routing the preview back through the server.
//
// The ONE deliberate difference from the create path is the input gate: a preview
// runs on half-typed input by definition, so it merges defaults
// ([config.MergeInputDefaults]) without the required/validate phases that would
// reject every keystroke. Merge is also the only phase that CHANGES a value —
// require and validate merely reject — so whenever the create WOULD have been
// accepted, both paths compose over the same map and produce the same string. That
// equality is pinned by a guard test rather than left to inspection.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// NamePreview is what [PreviewName] resolved for one (scenario, input) pair.
//
//   - Composes=false: the scenario declares no `name_template` — the operator
//     types the name themselves and there is nothing to preview. Every other
//     field is zero.
//   - Valid=true: Name is the name a create with this input would produce.
//   - Valid=false: the name could not be composed, or was composed into something
//     the grammar rejects. Reason says which, in the operator's terms. Name still
//     carries the offending string when there IS one (over-long, bad character),
//     so the form can show it and its length instead of a blank box; it is empty
//     only when the render itself failed and there is no string to show.
//
// The template TEXT is deliberately NOT a field: the operator is shown the
// resulting name, not the formula (NIM-340), and a client holding the expression
// would be one step from evaluating it — the divergence this whole path exists to
// avoid.
//
// A failed preview is a NORMAL state, not an error: the operator has not finished
// typing. Callers surface it, they do not reject on it.
type NamePreview struct {
	Composes bool
	Name     string
	Valid    bool
	Reason   string
}

// PreviewName composes the name scenario scenarioName of service ref would give an
// incarnation created with input `provided`, without creating anything.
//
// The returned error is INFRASTRUCTURE only — snapshot load, unreadable or
// unparseable manifest (caller → 500). A template that does not render, a
// component the operator has not filled in, a name over the ceiling: all of those
// come back as a NamePreview with Valid=false and a Reason, because they are what
// a preview normally sees.
//
// The caller is responsible for having established that scenarioName is an
// eligible create scenario of ref ([ValidateCreateScenarioChoice]) — this function
// answers about the name, not about the choice.
func PreviewName(ctx context.Context, loader InputScenarioLoader, ref artifact.ServiceRef, scenarioName string, provided map[string]any) (NamePreview, error) {
	scn, err := loadScenarioManifest(ctx, loader, ref, scenarioName, "preview name")
	if err != nil {
		return NamePreview{}, err
	}
	if scn.NameTemplate == "" {
		// No template — a free-text name. Not an error and not an empty preview:
		// the form needs this answer to keep showing its name field.
		return NamePreview{}, nil
	}

	out := NamePreview{Composes: true}
	merged := config.MergeInputDefaults(scn.Input, provided)

	name, cerr := ComposeName(scn.NameTemplate, merged)
	out.Name = name
	if cerr == nil {
		out.Valid = true
		return out, nil
	}
	out.Reason = previewReason(cerr)
	return out, nil
}

// previewReason phrases a composition failure for someone mid-typing.
//
// The sentinel prefixes are stripped: they exist so callers can branch with
// errors.Is, and this string is read by an operator in a form, where
// "scenario: name composed from name_template is not a valid incarnation name:"
// in front of the actual sentence is noise they have to read past.
//
// A render failure is the common one, and its cel-go tail ("no such key: project")
// names the component without saying what to do, so it is framed as the unfinished
// input it almost always is. A grammar failure already arrives phrased for the
// operator — the composed value, its length, the ceiling, which component to
// shorten — and only loses its prefix. Anything else keeps its own message rather
// than being flattened into a generic one: an unexplained blank preview is the
// exact failure this endpoint exists to remove.
func previewReason(err error) string {
	switch {
	case errors.Is(err, config.ErrNameTemplateRender):
		return fmt.Sprintf("the name cannot be composed yet — %s", trimSentinel(err, config.ErrNameTemplateRender))
	case errors.Is(err, ErrComposedNameInvalid):
		return trimSentinel(err, ErrComposedNameInvalid)
	default:
		return err.Error()
	}
}

// trimSentinel drops the leading "<sentinel>: " that fmt.Errorf("%w: …") put on the
// message, leaving the part written for a human. Falls back to the whole message if
// the wrapping ever stops being a prefix — losing the prefix must never cost the
// explanation.
func trimSentinel(err, sentinel error) string {
	msg := err.Error()
	if trimmed, ok := strings.CutPrefix(msg, sentinel.Error()+": "); ok {
		return trimmed
	}
	return msg
}
