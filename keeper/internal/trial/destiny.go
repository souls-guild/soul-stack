package trial

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// destinyNamePlaceholder — маркер в default_destiny_source, заменяемый именем
// destiny при резолве URL. Зеркало прод-константы
// scenario.destinyNamePlaceholder (keeper/internal/scenario/destiny.go).
const destinyNamePlaceholder = "{name}"

// fileScheme — единственная схема URL, поддерживаемая в L0: герметичность
// требует, чтобы кейс читал destiny только из локального дерева, без git/сети.
const fileScheme = "file://"

// fixtureDestinyResolver — герметичная реализация [render.DestinyResolver] для
// L0, ЗЕРКАЛЯЩАЯ прод-резолв apply:destiny (slice A, ADR-023):
//
//  1. name → запись в service.yml::destiny[] → {name, ref, git?}; необъявленная
//     зависимость отвергается (apply:destiny ссылается только на декларированную
//     зависимость, ADR-007) — та же ошибка, что в проде scenario.destinyResolver;
//  2. URL: per-entry git override (если задан) побеждает, иначе name
//     подставляется в шаблон default_destiny_source (case.yml::fixtures).
//
// В L0 URL обязан быть `file://` (герметичность): не-file-схема отвергается
// явной ошибкой. Из URL снимается `file://`, путь резолвится относительно
// service-root кейса; securejoin клампит выход за пределы destiny-root, чтобы
// {name} не вырвался через `../` за объявленный каталог destiny.
//
// Эвристический перебор каталогов ([serviceRoot, parent, grandparent] +
// конвенция destiny-<name>/) удалён: он не отражал прод и врал на cross-location
// раскладке (сервис в одном поддереве, standalone destiny — в другом).
type fixtureDestinyResolver struct {
	// serviceRoot — абсолютный каталог сервиса (или _trial-обёртки) кейса; база,
	// относительно которой резолвится относительный file://-путь.
	serviceRoot string
	// template — шаблон URL default_destiny_source из case.yml (пустой допустим:
	// тогда резолвятся только зависимости с per-entry git override).
	template string
	// deps — destiny[]-зависимости из service.yml сервиса кейса, по имени.
	deps map[string]config.DependencyRef
}

// newFixtureDestinyResolver строит резолвер из service-root кейса, шаблона
// default_destiny_source и распарсенного service.yml::destiny[]. serviceRoot
// приводится к абсолютному пути: securejoin на относительной базе (`../...`)
// нормализует `..` и теряет ведущий выход вверх, ломая os.ReadFile.
func newFixtureDestinyResolver(serviceRoot, template string, deps []config.DependencyRef) *fixtureDestinyResolver {
	if abs, err := filepath.Abs(serviceRoot); err == nil {
		serviceRoot = abs
	}
	byName := make(map[string]config.DependencyRef, len(deps))
	for _, dep := range deps {
		byName[dep.Name] = dep
	}
	return &fixtureDestinyResolver{serviceRoot: serviceRoot, template: template, deps: byName}
}

// Resolve грузит destiny по имени, зеркаля прод-резолв: name → destiny[]-запись,
// URL по гибридному правилу, чтение из локального дерева, парсинг
// destiny.yml + tasks/main.yml.
func (r *fixtureDestinyResolver) Resolve(_ context.Context, name string) (*render.ResolvedDestiny, error) {
	dir, err := r.locate(name)
	if err != nil {
		return nil, err
	}

	manData, err := os.ReadFile(filepath.Join(dir, "destiny.yml"))
	if err != nil {
		return nil, fmt.Errorf("trial: чтение destiny.yml фикстуры %q: %w", name, err)
	}
	manifest, _, mDiags, err := config.LoadDestinyManifestFromBytes("destiny.yml", manData, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("trial: парсинг destiny.yml фикстуры %q: %w", name, err)
	}
	if diag.HasErrors(mDiags) {
		return nil, fmt.Errorf("trial: destiny.yml фикстуры %q невалиден: %s", name, formatDiags(mDiags))
	}

	tasksPath, err := securejoin.SecureJoin(dir, filepath.Join("tasks", "main.yml"))
	if err != nil {
		return nil, fmt.Errorf("trial: небезопасный путь tasks/main.yml фикстуры %q: %w", name, err)
	}
	tasksData, err := os.ReadFile(tasksPath)
	if err != nil {
		return nil, fmt.Errorf("trial: чтение tasks/main.yml фикстуры %q: %w", name, err)
	}
	tasks, tDiags, err := config.LoadDestinyTasksFromBytes("tasks/main.yml", tasksData, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("trial: парсинг tasks/main.yml фикстуры %q: %w", name, err)
	}
	if diag.HasErrors(tDiags) {
		return nil, fmt.Errorf("trial: tasks/main.yml фикстуры %q невалиден: %s", name, formatDiags(tDiags))
	}

	// within-destiny include (tasks/<sub>.yml) раскрывается до render — так же,
	// как в проде DestinyLoader.parseTasks (destiny/tasks.md §4).
	expanded, iDiags := config.ExpandIncludes(tasks, fixtureDestinyIncludeResolver(dir))
	if diag.HasErrors(iDiags) {
		return nil, fmt.Errorf("trial: раскрытие include в destiny %q: %s", name, formatDiags(iDiags))
	}

	// destiny-локалы vars.yml (docs/destiny/vars.md) — зеркало прода
	// (artifact.DestinyLoader.parseVars): тот же config.LoadDestinyVars, опционален
	// (нет файла → nil). securejoin клампит выход за пределы dir.
	varsPath, err := securejoin.SecureJoin(dir, "vars.yml")
	if err != nil {
		return nil, fmt.Errorf("trial: небезопасный путь vars.yml фикстуры %q: %w", name, err)
	}
	vars, err := config.LoadDestinyVars(varsPath)
	if err != nil {
		return nil, fmt.Errorf("trial: vars.yml фикстуры %q: %w", name, err)
	}

	// .tmpl destiny читаются из её фикстурного каталога dir (одноуровневый
	// резолв — у destiny нет scenario-local-слоя). securejoin клампит выход за
	// пределы dir, симметрично прод-снапшоту.
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return readWithin(dir, rel) },
		"",
	)
	return &render.ResolvedDestiny{Name: manifest.Name, Tasks: expanded, Input: manifest.Input, Vars: vars, Templates: templates}, nil
}

// locate резолвит каталог destiny по имени: name → destiny[]-запись → file://-URL
// → абсолютный путь под service-root. Зеркалит прод resolveURL + Load.
func (r *fixtureDestinyResolver) locate(name string) (string, error) {
	dep, ok := r.deps[name]
	if !ok {
		return "", fmt.Errorf("trial: destiny %q не объявлена в service.yml::destiny[] — apply:destiny ссылается только на декларированную зависимость (ADR-007)", name)
	}

	root, leaf, err := r.resolvePath(name, dep.Git)
	if err != nil {
		return "", err
	}

	// Граница безопасности — destiny-root (доверенная часть URL до сегмента с
	// {name}), а НЕ service-root: securejoin клампит подставленное имя так, чтобы
	// {name} не вырвался через `../` за объявленный каталог destiny. destiny-root
	// сам может лежать вне service-root (cross-location раскладка, шаблон вида
	// `file://../../destiny/...`) — это доверенный путь оператора, не имя destiny.
	full, err := securejoin.SecureJoin(root, leaf)
	if err != nil {
		return "", fmt.Errorf("trial: небезопасный путь destiny %q (имя %q под %q): %w", name, leaf, root, err)
	}
	info, serr := os.Stat(full)
	if serr != nil {
		return "", fmt.Errorf("trial: destiny %q не найдена по %q (резолв из default_destiny_source): %w", name, full, serr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("trial: destiny %q резолвится в %q — это не каталог", name, full)
	}
	return full, nil
}

// resolvePath выводит destiny-root (доверенный абсолютный каталог) и leaf
// (недоверенный сегмент-имя с подставленным {name}) из гибридного правила:
// per-entry git override побеждает, иначе используется шаблон
// default_destiny_source. Зеркалит прод scenario.DestinySource.resolveURL, но
// дополнительно разделяет URL на доверенную базу и подставляемое имя — чтобы
// граница securejoin прошла по destiny-root, а не по service-root.
//
// {name} обязан жить в ПОСЛЕДНЕМ сегменте пути (напр. `.../destiny-{name}` или
// `.../{name}`): база (всё до последнего `/`) — доверенный путь оператора и
// резолвится обычным Join (допускает `../` для cross-location), последний
// сегмент с подставленным именем — недоверенный и клампится securejoin-ом.
func (r *fixtureDestinyResolver) resolvePath(name, gitOverride string) (root, leaf string, err error) {
	tmpl := gitOverride
	if tmpl == "" {
		if r.template == "" {
			return "", "", fmt.Errorf("trial: default_destiny_source не задан в case.yml::fixtures, а destiny %q не указала per-entry git — резолв apply:destiny невозможен", name)
		}
		tmpl = r.template
	}

	rel, ok := strings.CutPrefix(tmpl, fileScheme)
	if !ok {
		return "", "", fmt.Errorf("trial: destiny %q резолвится в %q, но L0 герметичен — поддерживается только схема %s", name, tmpl, fileScheme)
	}

	baseTmpl, leafTmpl := filepath.Dir(rel), filepath.Base(rel)
	if !strings.Contains(leafTmpl, destinyNamePlaceholder) {
		return "", "", fmt.Errorf("trial: default_destiny_source %q должен нести %s в последнем сегменте пути (напр. %sdestiny-%s) — имя destiny некуда безопасно подставить", tmpl, destinyNamePlaceholder, fileScheme, destinyNamePlaceholder)
	}

	// База — доверенный путь оператора: резолвится относительно service-root
	// обычным Join (Clean внутри Join обрабатывает ведущие `../`).
	root = filepath.Join(r.serviceRoot, baseTmpl)
	leaf = strings.ReplaceAll(leafTmpl, destinyNamePlaceholder, name)
	return root, leaf, nil
}

// fixtureDestinyIncludeResolver — within-destiny [config.IncludeResolver] для L0:
// include-цели строго в каталоге `tasks/` фикстуры (securejoin клампит выход).
func fixtureDestinyIncludeResolver(destinyDir string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		rel := filepath.Join("tasks", name)
		full, err := securejoin.SecureJoin(destinyDir, rel)
		if err != nil {
			return nil, "", fmt.Errorf("небезопасный путь %q: %w", rel, err)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, "", err
		}
		return data, rel, nil
	}
}
