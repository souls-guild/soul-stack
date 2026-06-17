package pushorch

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// destinyNamePlaceholder — маркер `{name}` в `default_destiny_source`. Дублирует
// scenario/destiny.go::destinyNamePlaceholder (нестабильный экспорт между
// пакетами; держим локально, защищено тестом на совпадение строки).
const destinyNamePlaceholder = "{name}"

// DestinyTemplateSource — источник шаблона URL `default_destiny_source` (тот же
// интерфейс, что scenario/destiny.go::DestinyTemplateSource). Прод-реализация —
// snapshot-чтение serviceregistry.Holder; шаблон читается ЛЕНИВО при каждом
// резолве, hot-reload скаляра keeper_settings прозрачен. Декларирован здесь
// (а не импортируется из scenario), чтобы pushorch не тянул scenario-pkg —
// семантика одна.
type DestinyTemplateSource interface {
	DefaultDestinySource() string
}

// DestinyArtifactLoader — узкая поверхность [artifact.DestinyLoader] для
// pushDestinyResolver. Сужено до Load — позволяет fake в unit-тестах без подъёма
// git. *artifact.DestinyLoader удовлетворяет автоматически.
type DestinyArtifactLoader interface {
	Load(ctx context.Context, ref artifact.DestinyRef) (*artifact.DestinyArtifact, error)
}

// pushDestinyResolver — push-сторонняя реализация [render.DestinyResolver].
//
// ОТЛИЧИЕ от scenario/destiny.go::destinyResolver: scenario-сторона резолвит
// destiny через `service.yml::destiny[]` зависимости (ref+опц.git-override),
// здесь destiny задаётся напрямую `<name>@<ref>` из запроса push.apply, и
// git-URL вытягивается ТОЛЬКО через `default_destiny_source` (per-entry
// git-override не применим — нет service-снапшота-источника).
//
// Render зовёт Resolve(ctx, name) ровно с тем `name`, который пришёл в
// синтетическом scenario {apply.destiny}. Проверяем равенство: разойтись они
// могут только при программной ошибке (caller передал чужой resolver).
type pushDestinyResolver struct {
	loader   DestinyArtifactLoader
	template DestinyTemplateSource
	name     string
	ref      string
}

// newPushDestinyResolver конструирует резолвер для одного push-прогона. Все
// поля обязательны: loader/template — runtime-зависимости daemon-а (production),
// name/ref — распарсенный `<name>@<ref>` запроса.
func newPushDestinyResolver(loader DestinyArtifactLoader, template DestinyTemplateSource, name, ref string) *pushDestinyResolver {
	return &pushDestinyResolver{loader: loader, template: template, name: name, ref: ref}
}

// Resolve реализует [render.DestinyResolver.Resolve]. Render-сторона передаёт
// `name` синтетического apply: ожидается совпадение с зафиксированным при
// конструкторе (defense-in-depth: подмена destiny из другого прогона = bug в
// orchestrator-е). git-URL — `default_destiny_source` + замена `{name}`.
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

	// .tmpl-файлы destiny лежат в её собственном снапшоте (одноуровневый резолв
	// без scenario-local-слоя; параллель с scenario/destiny.go).
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

// resolveGitURL вытягивает git-URL destiny из `default_destiny_source` через
// шаблон-источник. nil/пустой шаблон / отсутствие `{name}`-плейсхолдера —
// validation-failed: push-прогон без per-entry git-override (как в
// service.yml::destiny[]) не может резолвить destiny.
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
