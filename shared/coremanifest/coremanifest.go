// Package coremanifest — статический реестр manifest-деклараций core-модулей.
//
// Core-модули (ADR-015) статически встроены в `soul`-бинарь и не лежат на диске
// рядом с manifest.yaml, как custom-плагины. Но их input-схема всё равно должна
// быть доступна `soul-lint`-у для офлайн-валидации `params:` каждой задачи
// destiny/scenario (docs/soul/modules.md → «Core-модули и manifest»).
//
// Решение (architect, Вариант б2): декларация core-модуля живёт в том же формате
// `kind: soul_module`, что и custom-manifest (docs/keeper/plugins.md), и
// эмбедится через go:embed рядом с реестром. Парсер — тот же `shared/plugin`,
// поэтому в линтере один кодопуть для core и custom манифестов.
//
// Размещение в `shared/` (а не в `soul/`) выбрано из-за изоляции: и `soul`, и
// `soul-lint` импортируют `shared/`, но НЕ импортируют друг друга и НЕ тянут
// `keeper`. Если бы реестр жил в экспортируемом soul-пакете, `soul-lint`
// притянул бы весь soul-модуль (включая coremod-реализации с их рантайм-
// зависимостями). `shared/coremanifest` зависит только от `shared/plugin` и
// `shared/diag` — нейтральный слой, компилятор-гарантированная изоляция.
//
// Manifest-ы описывают **author-facing** input-контракт (то, что оператор пишет
// в `params:` задачи), а НЕ wire-форму proto-params. Для `core.file.rendered`
// это `template:`+`vars:` (а не runtime `template_content`+`render_context`,
// которые Keeper подставляет после рендер-фаз, ADR-010/ADR-012). Иначе линтер
// ругался бы на корректные destiny, написанные автором.
//
// Keeper-side core (`core.soul`/`core.cloud`/`core.vault`, ADR-017) добавляются
// сюда тем же механизмом (тираж H5): новый `<module>.yaml` + строка в All().
package coremanifest

import (
	"embed"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

//go:embed *.yaml
var manifestFS embed.FS

// coreFiles — список embed-файлов core-манифестов. Явный список (а не walk по
// FS) делает набач core-модулей видимым в коде и ловит «забыли добавить файл в
// реестр» на этапе ревью, а не в рантайме.
var coreFiles = []string{
	// Soul-side core (ADR-015) — статически встроены в `soul`-бинарь.
	"exec.yaml",
	"file.yaml",
	"pkg.yaml",
	"service.yaml",
	"user.yaml",
	"group.yaml",
	"cmd.yaml",
	"cron.yaml",
	"mount.yaml",
	"git.yaml",
	"archive.yaml",
	"sysctl.yaml",
	"url.yaml",
	"line.yaml",
	"repo.yaml",
	"firewall.yaml",
	"http.yaml",
	"noop.yaml",
	"module.yaml",

	// Keeper-side core (ADR-017/ADR-044, on: keeper). Имена state выровнены на
	// фактический dispatch coremod-ов keeper-стороны: core.soul.registered,
	// core.cloud.created/destroyed, core.vault.kv-read, core.choir.present/absent.
	"soul.yaml",
	"cloud.yaml",
	"vault.yaml",
	"choir.yaml",
}

// Registry — иммутабельный набор «имя core-модуля → распарсенный Manifest».
//
// Ключ — каноническое имя верхнего уровня без state-суффикса (`core.exec`,
// `core.file`), симметрично soul/internal/coremod.Registry. State-лукап
// делается через метод State поверх manifest.Spec.States.
type Registry struct {
	mods map[string]*plugin.Manifest
}

// defaultRegistry — синглтон, собранный при первом обращении из embed-файлов.
// Сборка идемпотентна и без I/O (embed уже в бинаре); паника возможна только
// при программном расхождении (битый embed-манифест) — это баг сборки, не ввод.
var defaultRegistry = mustBuild()

// Default возвращает общий реестр всех core-манифестов. Лукап — O(1).
func Default() *Registry { return defaultRegistry }

func mustBuild() *Registry {
	mods := make(map[string]*plugin.Manifest, len(coreFiles))
	for _, name := range coreFiles {
		src, err := manifestFS.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("coremanifest: embed read %s: %v", name, err))
		}
		m, diags := plugin.LoadFromBytes(name, src)
		if diag.HasErrors(diags) {
			panic(fmt.Sprintf("coremanifest: %s is not a valid manifest: %v", name, diags))
		}
		addr := m.Namespace + "." + m.Name
		if _, dup := mods[addr]; dup {
			panic(fmt.Sprintf("coremanifest: duplicate core module %q", addr))
		}
		mods[addr] = m
	}
	return &Registry{mods: mods}
}

// Lookup возвращает manifest core-модуля по каноническому имени (`core.exec`)
// и флаг наличия. Имя — без state-суффикса.
func (r *Registry) Lookup(module string) (*plugin.Manifest, bool) {
	m, ok := r.mods[module]
	return m, ok
}

// State возвращает декларацию состояния `module.state` (например, `core.exec` +
// `run`) и флаг наличия. Удобный фасад над Lookup + Spec.States.
func (r *Registry) State(module, state string) (plugin.StateDef, bool) {
	m, ok := r.mods[module]
	if !ok {
		return plugin.StateDef{}, false
	}
	def, ok := m.Spec.States[state]
	return def, ok
}

// Names возвращает имена зарегистрированных core-модулей в детерминированном
// (лексикографическом) порядке. Используется для diagnostic-вывода — стабильный
// порядок делает сообщения воспроизводимыми между запусками.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
