package scenario

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// destinyNamePlaceholder — marker in `default_destiny_source`, replaced by the
// destiny name when resolving the git URL (keeper_settings::default_destiny_source).
const destinyNamePlaceholder = "{name}"

// DestinyTemplateSource — source of the `default_destiny_source` URL template.
// Implemented by a runtime snapshot [serviceregistry.Holder]
// (DefaultDestinySource reads the keeper_settings scalar, ADR-029). Declared as
// an interface so [DestinySource] can be tested without a DB (fixed template)
// and so the template is read LAZILY (from the current snapshot on every
// resolve) rather than fixed as a copy in the constructor — otherwise a
// hot-reload of the scalar wouldn't reach resolution.
type DestinyTemplateSource interface {
	DefaultDestinySource() string
}

// fixedTemplateSource — [DestinyTemplateSource] with a constant template (tests).
type fixedTemplateSource string

func (s fixedTemplateSource) DefaultDestinySource() string { return string(s) }

// DestinySource resolves the git coordinates of a destiny repo by name. ref
// comes from `service.yml → destiny[]` by name (ADR-007: version = git ref).
// The git URL follows a hybrid rule: a per-entry `destiny[].git` override
// (direct URL) takes priority, otherwise the name is substituted into the
// `default_destiny_source` template (read LAZILY from the snapshot source).
// Safe for concurrent use.
type DestinySource struct {
	loader   *artifact.DestinyLoader
	template DestinyTemplateSource
}

// NewDestinySource builds a destiny source from the loader and the
// `default_destiny_source` URL template snapshot source. The template is read
// lazily on every resolve (see resolveURL) — hot-reload of the keeper_settings
// scalar is transparent. An empty template is allowed: name-only dependencies
// then don't resolve, but destinies with a per-entry `git` override work
// without a template.
func NewDestinySource(loader *artifact.DestinyLoader, template DestinyTemplateSource) *DestinySource {
	return &DestinySource{loader: loader, template: template}
}

// resolveURL derives the destiny git URL by the hybrid rule: the per-entry
// `git` override takes priority (direct URL, no template), otherwise name is
// substituted into the `default_destiny_source` template. `gitOverride` is the
// `destiny[].git` value from service.yml (empty = override not set).
func (s *DestinySource) resolveURL(name, gitOverride string) (string, error) {
	if gitOverride != "" {
		return gitOverride, nil
	}
	var template string
	if s.template != nil {
		template = s.template.DefaultDestinySource()
	}
	if template == "" {
		return "", fmt.Errorf("scenario: default_destiny_source не задан (keeper_settings), а destiny %q не указала per-entry git — резолв apply:destiny невозможен", name)
	}
	if !strings.Contains(template, destinyNamePlaceholder) {
		return "", fmt.Errorf("scenario: default_destiny_source %q не содержит %s — имя destiny некуда подставить", template, destinyNamePlaceholder)
	}
	return strings.ReplaceAll(template, destinyNamePlaceholder, name), nil
}

// resolverFor builds a per-run [render.DestinyResolver] from the destiny[]
// dependencies of a specific service snapshot. ref/git come from
// manifest.Destiny[] by name; a destiny not declared in service.yml::destiny[]
// is rejected (apply:destiny can only reference a declared dependency, ADR-007).
func (s *DestinySource) resolverFor(manifest *config.ServiceManifest) *destinyResolver {
	deps := make(map[string]config.DependencyRef, len(manifest.Destiny))
	for _, dep := range manifest.Destiny {
		deps[dep.Name] = dep
	}
	return &destinyResolver{source: s, deps: deps}
}

// destinyResolver — per-run implementation of [render.DestinyResolver]: knows
// the destiny[] dependencies of the current service snapshot (ref + optional
// git override) and loads the destiny artifact via DestinyLoader.
type destinyResolver struct {
	source *DestinySource
	deps   map[string]config.DependencyRef
}

// Resolve loads a destiny by name: ref from service.yml::destiny[], git URL by
// the hybrid rule (per-entry git override → default_destiny_source + name).
// Returns the parsed tasks + input schema.
func (r *destinyResolver) Resolve(ctx context.Context, name string) (*render.ResolvedDestiny, error) {
	dep, ok := r.deps[name]
	if !ok {
		return nil, fmt.Errorf("scenario: destiny %q не объявлена в service.yml::destiny[] — apply:destiny ссылается только на декларированную зависимость (ADR-007)", name)
	}
	gitURL, err := r.source.resolveURL(name, dep.Git)
	if err != nil {
		return nil, err
	}
	art, err := r.source.loader.Load(ctx, artifact.DestinyRef{Name: name, Git: gitURL, Ref: dep.Ref})
	if err != nil {
		return nil, fmt.Errorf("scenario: load destiny %q: %w", name, err)
	}
	// .tmpl files of a destiny live in ITS OWN snapshot (art.LocalDir), not the
	// service snapshot. Single-level resolve (destiny has no scenario-local
	// layer): empty prefix.
	localDir := art.LocalDir
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(localDir, rel) },
		"",
	)
	return &render.ResolvedDestiny{
		Name:      art.Manifest.Name,
		Tasks:     art.Tasks,
		Input:     art.Manifest.Input,
		Vars:      art.Vars, // destiny-local vars.yml (docs/destiny/vars.md), raw
		Templates: templates,
	}, nil
}
