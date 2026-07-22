// Package coremod wires Soul-side core modules into a single Registry.
//
// Core modules from ADR-015 are statically compiled into the `soul` binary.
// Registry maps a canonical top-level name (`core.pkg` / `core.file` / …) to
// an sdk/module.SoulModule implementation.
//
// MVP — 21 Soul-side modules: pkg / file / directory / service / user / group
// (Core.a.1), exec / cmd / cron / mount (Core.a.2), git / archive / sysctl (Core.a.3),
// url (Core.a.4), line (Core.a.5 — pilot in-place line editing, ADR-015),
// repo / firewall (Core.a.6 — package repository + firewall rule, ADR-015),
// http (Core.a.7 — read-probe HTTP, verb probe, changed=false, ADR-015),
// noop (ADR-015 — no-op/barrier anchor, verb run, changed=false),
// augur (ADR-025 — read-probe live access to an external system via the
// Augur broker, verb fetch, changed=false),
// module (ADR-065 — SoulModule plugin delivery: allow-check → FetchModule →
// Sigil-verify → atomic install; host dependencies via Deps).
package coremod

import (
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/archive"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/augur"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/cmd"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/cron"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/directory"
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

// Registry is an immutable "module name → SoulModule implementation" map.
//
// Lookup is O(1). The key is the full module name without a state suffix
// (`core.pkg`, not `core.pkg.installed`); the state suffix arrives in
// pluginv1.ApplyRequest.state and is handled by the module implementation.
type Registry struct {
	mods map[string]module.SoulModule
}

// Default returns a Registry with all 21 Soul-side core modules of the MVP.
// Used for wire-up in cmd/soul; in tests it's more convenient to build your
// own Registry via NewRegistry with fixed dependencies.
//
// install — host dependencies for core.module (ADR-065: Sigil set, trust
// anchors, module cache root). The zero value is valid (push mode/tests): the
// install step will fail-closed with module_not_allowed.
func Default(install installmod.Deps) *Registry {
	return NewRegistry(map[string]module.SoulModule{
		augur.Name:      augur.New(),
		pkg.Name:        pkg.New(),
		file.Name:       file.New(),
		directory.Name:  directory.New(),
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

// NewRegistry builds a Registry from an arbitrary set of implementations. All
// names must be in the form `core.<module>` (without a state suffix).
func NewRegistry(mods map[string]module.SoulModule) *Registry {
	cp := make(map[string]module.SoulModule, len(mods))
	for k, v := range mods {
		cp[k] = v
	}
	return &Registry{mods: cp}
}

// Lookup returns the module by canonical name and a presence flag.
// The returned value is a read-only interface, callers cannot mutate it.
func (r *Registry) Lookup(name string) (module.SoulModule, bool) {
	m, ok := r.mods[name]
	return m, ok
}

// Names returns the registered modules in non-deterministic order (Go map
// iteration). Used for diagnostic output / healthz.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	return out
}
