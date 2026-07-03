// Package coremod — соединяет core-модули Soul-стороны в единый Registry.
//
// Core-модули из ADR-015 статически встроены в `soul`-бинарь. Registry
// связывает каноническое имя верхнего уровня (`core.pkg` / `core.file` / …)
// с реализацией sdk/module.SoulModule.
//
// MVP — 20 Soul-side модулей: pkg / file / service / user / group (Core.a.1),
// exec / cmd / cron / mount (Core.a.2), git / archive / sysctl (Core.a.3),
// url (Core.a.4), line (Core.a.5 — пилот in-place построчной правки, ADR-015),
// repo / firewall (Core.a.6 — пакетный репозиторий + правило файрвола, ADR-015),
// http (Core.a.7 — read-probe HTTP, verb probe, changed=false, ADR-015),
// noop (ADR-015 — no-op/barrier-якорь, verb run, changed=false),
// augur (ADR-025 — read-probe живого доступа к внешней системе через брокер
// Augur, verb fetch, changed=false),
// module (ADR-065 — доставка SoulModule-плагина: allow-check → FetchModule →
// Sigil-verify → atomic install; host-зависимости через Deps).
package coremod

import (
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/archive"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/augur"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/cmd"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/cron"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/exec"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/file"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/firewall"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/git"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/group"
	httpmod "github.com/souls-guild/soul-stack/soul/internal/coremod/http"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/line"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/mount"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/noop"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/pkg"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/repo"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/service"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/sysctl"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/url"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/user"
)

// Registry — иммутабельный набор «имя модуля → реализация SoulModule».
//
// Лукап — за O(1). Ключ — полное имя модуля без state-суффикса
// (`core.pkg`, не `core.pkg.installed`); state-суффикс приходит в
// pluginv1.ApplyRequest.state и обрабатывается реализацией модуля.
type Registry struct {
	mods map[string]module.SoulModule
}

// Default возвращает Registry со всеми 20 Soul-side core-модулями MVP.
// Используется при wire-up в cmd/soul; в тестах удобнее собрать собственный
// Registry через NewRegistry с фиксированными зависимостями.
//
// install — host-зависимости core.module (ADR-065: Sigil-набор, trust-anchor-ы,
// корень кеша модулей). Zero-значение валидно (push-режим/тесты): install-шаг
// откажет fail-closed module_not_allowed.
func Default(install installmod.Deps) *Registry {
	return NewRegistry(map[string]module.SoulModule{
		augur.Name:      augur.New(),
		pkg.Name:        pkg.New(),
		file.Name:       file.New(),
		service.Name:    service.New(),
		user.Name:       user.New(),
		group.Name:      group.New(),
		line.Name:       line.New(),
		exec.Name:       exec.New(),
		cmd.Name:        cmd.New(),
		cron.Name:       cron.New(),
		mount.Name:      mount.New(),
		noop.Name:       noop.New(),
		git.Name:        git.New(),
		archive.Name:    archive.New(),
		sysctl.Name:     sysctl.New(),
		url.Name:        url.New(),
		repo.Name:       repo.New(),
		firewall.Name:   firewall.New(),
		httpmod.Name:    httpmod.New(),
		installmod.Name: installmod.New(install),
	})
}

// NewRegistry собирает Registry из произвольного набора реализаций. Все
// имена должны быть в форме `core.<module>` (без state-суффикса).
func NewRegistry(mods map[string]module.SoulModule) *Registry {
	cp := make(map[string]module.SoulModule, len(mods))
	for k, v := range mods {
		cp[k] = v
	}
	return &Registry{mods: cp}
}

// Lookup возвращает модуль по каноническому имени и флаг наличия.
// Возврат — readonly интерфейс, мутация со стороны вызывающего невозможна.
func (r *Registry) Lookup(name string) (module.SoulModule, bool) {
	m, ok := r.mods[name]
	return m, ok
}

// Names возвращает список зарегистрированных модулей в недетерминированном
// порядке (Go map iteration). Используется для diagnostic-вывода / healthz.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	return out
}
