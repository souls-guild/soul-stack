package pushorch

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// destinyNamePlaceholder is the marker `{name}` in `default_destiny_source`. Duplicates
// scenario/destiny.go::destinyNamePlaceholder (unstable export between packages;
// held locally, protected by test for string equivalence).
const destinyNamePlaceholder = "{name}"

// DestinyTemplateSource is a source of the `default_destiny_source` URL template
// (same interface as scenario/destiny.go::DestinyTemplateSource). Production
// implementation reads snapshot from serviceregistry.Holder; template is read
// lazily on each resolve, hot-reload of keeper_settings scalar is transparent.
// Declared here (not imported from scenario) so pushorch does not pull scenario
// package — semantics are identical.
type DestinyTemplateSource interface {
	DefaultDestinySource() string
}

// DestinyArtifactLoader is a narrow interface of [artifact.DestinyLoader] for
// pushDestinyResolver. Narrowed to Load only — allows mocking in unit tests
// without raising git. *artifact.DestinyLoader satisfies it automatically.
type DestinyArtifactLoader interface {
	Load(ctx context.Context, ref artifact.DestinyRef) (*artifact.DestinyArtifact, error)
}

// pushDestinyResolver is the push-side implementation of [render.DestinyResolver].
//
// DIFFERENCE from scenario/destiny.go::destinyResolver: the scenario side resolves
// destiny via `service.yml::destiny[]` dependencies (ref + optional git-override);
// here destiny is specified directly as `<name>@<ref>` from push.apply request, and
// git-URL is retrieved ONLY through `default_destiny_source` (per-entry git-override
// does not apply — no service-snapshot source available).
//
// Render calls Resolve(ctx, name) with exactly the `name` that came in the synthetic
// scenario {apply.destiny}. We verify equality: they can only diverge on programmer
// error (caller passed wrong resolver).
type pushDestinyResolver struct {
	loader   DestinyArtifactLoader
	template DestinyTemplateSource
	name     string
	ref      string
}

// newPushDestinyResolver constructs a resolver for one push run. All fields are
// required: loader/template are runtime dependencies of the daemon (production),
// name/ref are parsed `<name>@<ref>` from the request.
func newPushDestinyResolver(loader DestinyArtifactLoader, template DestinyTemplateSource, name, ref string) *pushDestinyResolver {
	return &pushDestinyResolver{loader: loader, template: template, name: name, ref: ref}
}

// Resolve implements [render.DestinyResolver.Resolve]. Render side passes the
// `name` of the synthetic apply: it must match the one fixed at constructor
// time (defense-in-depth: substituting destiny from another run would be a bug
// in the orchestrator). git-URL is `default_destiny_source` with `{name}` replaced.
func (r *pushDestinyResolver) Resolve(ctx context.Context, name string) (*render.ResolvedDestiny, error) {
	if name != r.name {
		return nil, fmt.Errorf("pushorch: destiny resolver invoked with name %q, expected %q (programmer error)", name, r.name)
	}

	gitURL, err := r.resolveGitURL()
	if err != nil {
		return nil, err
	}

	art, err := r.loader.Load(ctx, artifact.DestinyRef{Name: name, Git: gitURL, Ref: r.ref})
	if err != nil {
		return nil, fmt.Errorf("pushorch: load destiny %q@%s: %w", name, r.ref, err)
	}

	// .tmpl files of destiny are in its own snapshot (single-level resolve without
	// scenario-local layer; parallel to scenario/destiny.go).
	localDir := art.LocalDir
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(localDir, rel) },
		"",
	)
	return &render.ResolvedDestiny{
		Name:      art.Manifest.Name,
		Tasks:     art.Tasks,
		Input:     art.Manifest.Input,
		Templates: templates,
	}, nil
}

// resolveGitURL pulls destiny git-URL from `default_destiny_source` via the template
// source. nil/empty template / missing `{name}` placeholder — validation failed:
// a push run without per-entry git-override (as in service.yml::destiny[])
// cannot resolve destiny.
func (r *pushDestinyResolver) resolveGitURL() (string, error) {
	var template string
	if r.template != nil {
		template = r.template.DefaultDestinySource()
	}
	if template == "" {
		return "", fmt.Errorf("pushorch: default_destiny_source не задан (keeper_settings) — резолв destiny %q невозможен", r.name)
	}
	if !strings.Contains(template, destinyNamePlaceholder) {
		return "", fmt.Errorf("pushorch: default_destiny_source %q не содержит %s — имя destiny некуда подставить", template, destinyNamePlaceholder)
	}
	return strings.ReplaceAll(template, destinyNamePlaceholder, r.name), nil
}
