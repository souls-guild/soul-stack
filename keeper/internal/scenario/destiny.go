package scenario

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// destinyNamePlaceholder — маркер в `default_destiny_source`, заменяемый именем
// destiny при резолве git-URL (keeper_settings::default_destiny_source).
const destinyNamePlaceholder = "{name}"

// DestinyTemplateSource — источник шаблона URL `default_destiny_source`.
// Реализуется runtime-снимком [serviceregistry.Holder] (DefaultDestinySource
// читает скаляр keeper_settings, ADR-029). Объявлен интерфейсом, чтобы
// [DestinySource] тестировался без БД (fixed-шаблон) и чтобы шаблон читался
// ЛЕНИВО (на каждый резолв из текущего снимка), а не фиксировался копией в
// конструкторе — иначе hot-reload скаляра не доезжал бы до резолва.
type DestinyTemplateSource interface {
	DefaultDestinySource() string
}

// fixedTemplateSource — [DestinyTemplateSource] с константным шаблоном (тесты).
type fixedTemplateSource string

func (s fixedTemplateSource) DefaultDestinySource() string { return string(s) }

// DestinySource резолвит git-координаты destiny-репо по имени. ref — из
// `service.yml → destiny[]` по имени (ADR-007: version = git ref). git-URL —
// по гибридному правилу: per-entry `destiny[].git` override (прямой URL) имеет
// приоритет, иначе имя подставляется в шаблон `default_destiny_source`
// (читается ЛЕНИВО из snapshot-источника). Safe for concurrent use.
type DestinySource struct {
	loader   *artifact.DestinyLoader
	template DestinyTemplateSource
}

// NewDestinySource собирает источник destiny из загрузчика и snapshot-источника
// шаблона URL `default_destiny_source`. Шаблон читается лениво на каждый резолв
// (см. resolveURL) — hot-reload скаляра keeper_settings прозрачен. Пустой шаблон
// допустим: name-only зависимости тогда не резолвятся, но destiny с per-entry
// `git` override работают без шаблона.
func NewDestinySource(loader *artifact.DestinyLoader, template DestinyTemplateSource) *DestinySource {
	return &DestinySource{loader: loader, template: template}
}

// resolveURL выводит git-URL destiny по гибридному правилу: per-entry override
// `git` имеет приоритет (прямой URL без шаблона), иначе name подставляется в
// шаблон `default_destiny_source`. `gitOverride` — значение `destiny[].git` из
// service.yml (пустое = override не задан).
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

// resolverFor строит per-run [render.DestinyResolver] из destiny[]-зависимостей
// конкретного service-снапшота. ref/git берутся из manifest.Destiny[] по имени;
// destiny, не объявленная в service.yml::destiny[], отвергается (apply:destiny
// может ссылаться только на декларированную зависимость, ADR-007).
func (s *DestinySource) resolverFor(manifest *config.ServiceManifest) *destinyResolver {
	deps := make(map[string]config.DependencyRef, len(manifest.Destiny))
	for _, dep := range manifest.Destiny {
		deps[dep.Name] = dep
	}
	return &destinyResolver{source: s, deps: deps}
}

// destinyResolver — per-run реализация [render.DestinyResolver]: знает
// destiny[]-зависимости текущего service-снапшота (ref + опц. git-override) и
// грузит destiny-артефакт через DestinyLoader.
type destinyResolver struct {
	source *DestinySource
	deps   map[string]config.DependencyRef
}

// Resolve грузит destiny по имени: ref из service.yml::destiny[], git-URL по
// гибридному правилу (per-entry git override → default_destiny_source + name).
// Возвращает распарсенные задачи + input-схему.
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
	// .tmpl destiny живут в ЕЁ снапшоте (art.LocalDir), не в снапшоте сервиса.
	// Одноуровневый резолв (scenario-local-слоя у destiny нет): пустой prefix.
	localDir := art.LocalDir
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(localDir, rel) },
		"",
	)
	return &render.ResolvedDestiny{
		Name:      art.Manifest.Name,
		Tasks:     art.Tasks,
		Input:     art.Manifest.Input,
		Vars:      art.Vars, // destiny-локалы vars.yml (docs/destiny/vars.md), raw
		Templates: templates,
	}, nil
}
