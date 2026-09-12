// Package coremanifest is the static registry of schema declarations for core modules.
//
// Core modules (ADR-015) are compiled into the `soul` and `keeper` binaries; they do not
// live on disk as an artifact next to a schema document like custom plugins. Yet their
// input schema must still be available to `soul-lint` for offline validation of each
// destiny/scenario task's `params:` (docs/soul/modules.md → "Core modules and manifest").
//
// Since NIM-377 a module's schema IS a Go value: one [schema.Module] per core module, in
// `mod_<name>.go` next to this file. That is the same model a plugin author writes in
// `sdk/module` and the same model `shared/plugin` reads out of an artifact — one
// definition of a parameter, a state and a module across the whole tree. What core does
// NOT need is the artifact machinery around it: nothing here is stamped, signed,
// approved or dispatched by subcommand, because there is no separate binary to approve.
//
// Placement in `shared/` (not `soul/`) is for isolation: both `soul` and `soul-lint`
// import `shared/` but do NOT import each other and do NOT pull in `keeper`. If the
// registry lived in an exported soul package, `soul-lint` would pull in the whole soul
// module (including coremod implementations with their runtime dependencies).
// `shared/coremanifest` depends only on `sdk/schema` — a neutral layer with
// compiler-guaranteed isolation.
//
// Declarations describe the **author-facing** input contract (what the operator writes in
// a task's `params:`), NOT the wire form of proto-params. For `core.file.rendered` that
// is `template:`+`vars:` (not the runtime `template_content`+`render_context` that Keeper
// substitutes after the render phases, ADR-010/ADR-012). Otherwise the linter would
// reject valid author-written destiny.
//
// Keeper-side core (`core.soul`/`core.bootstrap`/`core.vault`/`core.choir`/`core.state`, ADR-017/ADR-044/ADR-063/ADR-0083)
// are declared here by the same mechanism: a new `mod_<name>.go` + a line in [coreModules].
package coremanifest

import (
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// Namespace is address level 1 of every module in this registry.
//
// An artifact carries no self-name — level 1 comes from the alias an operator picks at
// registration (NIM-377). Core modules are never registered: they are compiled in, and
// `core` is the reserved name that addresses them. So the namespace lives here, once,
// rather than being repeated in each declaration.
const Namespace = "core"

// coreModules is the explicit list of core-module declarations. An explicit list (not a
// package-level scan or an init-time side effect in each file) keeps the set of core
// modules visible in code and catches a "forgot to add the module to the registry"
// mistake at review time, not at runtime.
var coreModules = []schema.Module{
	// Soul-side core (ADR-015) — statically compiled into the `soul` binary.
	modExec,
	modFile,
	modDirectory,
	modPkg,
	modService,
	modUser,
	modGroup,
	modCmd,
	modCron,
	modMount,
	modGit,
	modArchive,
	modSysctl,
	modURL,
	modLine,
	modRepo,
	modFirewall,
	modHTTP,
	modNoop,
	modModule,

	// Keeper-side core (ADR-017/ADR-044, on: keeper). State names aligned with the
	// actual dispatch of keeper-side coremods: core.soul.registered,
	// core.bootstrap.issued, core.ssh.run, core.vault.kv-read,
	// core.choir.present/absent, core.state.*.
	modSoul,
	modBootstrap,
	modSSH,
	modVault,
	modChoir,
	modState,
}

// Registry is an immutable set of "core-module name → declaration".
//
// The key is the canonical top-level name without a state suffix (`core.exec`,
// `core.file`), symmetric with soul/internal/coremod.Registry. State lookup is done via
// the State method over [schema.Module.States].
type Registry struct {
	mods map[string]schema.Module
}

// defaultRegistry is a singleton built from [coreModules] on first access. The build is
// idempotent and I/O-free; a panic is possible only on a programmer error (a duplicate
// entry, a declaration the validator rejects) — a build bug, not input.
var defaultRegistry = mustBuild()

// Default returns the shared registry of all core declarations. Lookup is O(1).
func Default() *Registry { return defaultRegistry }

func mustBuild() *Registry {
	if issues := schema.Validate(validationDocument()); schema.HasErrors(issues) {
		for _, i := range issues {
			if i.Level == schema.LevelError {
				panic(fmt.Sprintf("coremanifest: invalid core declaration at %s: %s", i.Path, i))
			}
		}
	}
	mods := make(map[string]schema.Module, len(coreModules))
	for _, m := range coreModules {
		addr := Namespace + "." + m.Name
		if _, dup := mods[addr]; dup {
			panic(fmt.Sprintf("coremanifest: duplicate core module %q", addr))
		}
		// Side is STAMPED from [keeperSideCore] rather than written on each
		// declaration (NIM-749). A plugin states its own side in its schema
		// document, so the field has to exist on [schema.Module] — and the moment
		// it does, a core declaration that left it empty would answer "soul" for
		// `core.state` while the catalog three files away answers "keeper". One
		// value, derived where it is already decided, is the only version of this
		// that cannot drift.
		m.Side = SideOf(addr)
		mods[addr] = m
	}
	return &Registry{mods: mods}
}

// validationDocument wraps the core declarations in a [schema.Document] so they can be
// checked by the SDK validator — the same one that judges a plugin artifact, so a core
// module and a third-party module are held to one rule set.
//
// The wrapper is a validation vehicle, not a claim that core ships as an artifact: no
// document is ever serialized, stamped or read back from here. Kind and protocol version
// are the values the hand-written core manifests carried before NIM-377.
func validationDocument() schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: schema.ProtocolVersion,
		Modules:         coreModules,
	}
}

// Lookup returns a core module's declaration by canonical name (`core.exec`) and a
// presence flag. The name has no state suffix.
func (r *Registry) Lookup(module string) (schema.Module, bool) {
	m, ok := r.mods[module]
	return m, ok
}

// State returns the state declaration for `module.state` (e.g. `core.exec` + `run`) and a
// presence flag. A convenience facade over Lookup + [schema.Module.States].
func (r *Registry) State(module, state string) (schema.State, bool) {
	m, ok := r.mods[module]
	if !ok {
		return schema.State{}, false
	}
	def, ok := m.States[state]
	return def, ok
}

// Names returns the names of registered core modules in deterministic (lexicographic)
// order. Used for diagnostic output — a stable order makes messages reproducible across
// runs.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
