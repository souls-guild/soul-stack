package scenario

// Live preview of the incarnation id a create scenario COMPOSES from its
// `id_template` (ADR-0079). The create form calls it as the operator types the
// `input:` components, so the id is on screen before the request that would make
// it permanent — the id is the immutable primary key, and a wrong one costs a
// destroy and a re-create.
//
// The composition itself is NOT reimplemented here: [PreviewID] renders through
// [ComposeID], the same function the create path reaches via
// composeIncarnationID. Blocks in a template are CEL, and a second evaluator —
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

// IDPreview is what [PreviewID] resolved for one (scenario, input) pair.
//
//   - Composes=false: the scenario declares no `id_template` — the operator
//     names the incarnation themselves and there is nothing to preview. Every
//     other field is zero.
//   - Valid=true: ID is the identifier a create with this input would produce.
//   - Valid=false: the id could not be composed, or was composed into something
//     the grammar rejects. Reason says which, in the operator's terms. ID still
//     carries the offending string when there IS one (over-long, bad character),
//     so the form can show it and its length instead of a blank box; it is empty
//     only when the render itself failed and there is no string to show.
//
// The template TEXT is deliberately NOT a field: the operator is shown the
// resulting id, not the formula (NIM-340), and a client holding the expression
// would be one step from evaluating it — the divergence this whole path exists to
// avoid.
//
// A failed preview is a NORMAL state, not an error: the operator has not finished
// typing. Callers surface it, they do not reject on it.
type IDPreview struct {
	Composes bool
	ID       string
	Valid    bool
	Reason   string
}

// PreviewID composes the id scenario scenarioName of service ref would give an
// incarnation created with input `provided`, without creating anything.
//
// The returned error is INFRASTRUCTURE only — snapshot load, unreadable or
// unparseable manifest (caller → 500). A template that does not render, a
// component the operator has not filled in, an id over the ceiling: all of those
// come back as an IDPreview with Valid=false and a Reason, because they are what
// a preview normally sees.
//
// The caller is responsible for having established that scenarioName is an
// eligible create scenario of ref ([ValidateCreateScenarioChoice]) — this function
// answers about the id, not about the choice.
func PreviewID(ctx context.Context, loader InputScenarioLoader, ref artifact.ServiceRef, scenarioName string, provided map[string]any) (IDPreview, error) {
	scn, err := loadScenarioManifest(ctx, loader, ref, scenarioName, "preview id")
	if err != nil {
		return IDPreview{}, err
	}
	if scn.IDTemplate == "" {
		// No template — a free-text id. Not an error and not an empty preview:
		// the form needs this answer to keep showing its id field.
		return IDPreview{}, nil
	}

	out := IDPreview{Composes: true}
	merged := config.MergeInputDefaults(scn.Input, provided)

	composed, cerr := ComposeID(scn.IDTemplate, merged)
	out.ID = composed
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
// "scenario: id composed from id_template is not a valid incarnation id:"
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
	case errors.Is(err, config.ErrIDTemplateRender):
		return fmt.Sprintf("the id cannot be composed yet — %s", trimSentinel(err, config.ErrIDTemplateRender))
	case errors.Is(err, ErrComposedIDInvalid):
		return trimSentinel(err, ErrComposedIDInvalid)
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
