// Package coremanifest is the static registry of manifest declarations for core modules.
//
// Core modules (ADR-015) are statically compiled into the `soul` binary and do not live
// on disk next to a manifest.yaml like custom plugins. Yet their input schema must still
// be available to `soul-lint` for offline validation of each destiny/scenario task's
// `params:` (docs/soul/modules.md → "Core modules and manifest").
//
// Decision (architect, variant b2): a core-module declaration uses the same
// `kind: soul_module` format as a custom manifest (docs/keeper/plugins.md) and is
// embedded via go:embed next to the registry. The parser is the same `shared/plugin`,
// so the linter has one code path for both core and custom manifests.
//
// Placement in `shared/` (not `soul/`) is for isolation: both `soul` and `soul-lint`
// import `shared/` but do NOT import each other and do NOT pull in `keeper`. If the
// registry lived in an exported soul package, `soul-lint` would pull in the whole soul
// module (including coremod implementations with their runtime dependencies).
// `shared/coremanifest` depends only on `shared/plugin` and `shared/diag` — a neutral
// layer with compiler-guaranteed isolation.
//
// Manifests describe the **author-facing** input contract (what the operator writes in a
// task's `params:`), NOT the wire form of proto-params. For `core.file.rendered` that is
// `template:`+`vars:` (not the runtime `template_content`+`render_context` that Keeper
// substitutes after the render phases, ADR-010/ADR-012). Otherwise the linter would
// reject valid author-written destiny.
//
// Keeper-side core (`core.soul`/`core.cloud`/`core.vault`, ADR-017) are added here by the
// same mechanism (batch H5): a new `<module>.yaml` + a line in All().
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

// coreFiles is the explicit list of embedded core-manifest files. An explicit list
// (not an FS walk) keeps the set of core modules visible in code and catches a
// "forgot to add the file to the registry" mistake at review time, not at runtime.
var coreFiles = []string{
	// Soul-side core (ADR-015) — statically compiled into the `soul` binary.
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

	// Keeper-side core (ADR-017/ADR-044, on: keeper). State names aligned with the
	// actual dispatch of keeper-side coremods: core.soul.registered,
	// core.cloud.created/destroyed, core.vault.kv-read, core.choir.present/absent.
	"soul.yaml",
	"cloud.yaml",
	"vault.yaml",
	"choir.yaml",
}

// Registry is an immutable set of "core-module name → parsed Manifest".
//
// The key is the canonical top-level name without a state suffix (`core.exec`,
// `core.file`), symmetric with soul/internal/coremod.Registry. State lookup is done
// via the State method over manifest.Spec.States.
type Registry struct {
	mods map[string]*plugin.Manifest
}

// defaultRegistry is a singleton built from the embedded files on first access. The
// build is idempotent and I/O-free (embed is already in the binary); a panic is possible
// only on a programmer error (a broken embedded manifest) — a build bug, not input.
var defaultRegistry = mustBuild()

// Default returns the shared registry of all core manifests. Lookup is O(1).
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

// Lookup returns a core module's manifest by canonical name (`core.exec`) and a
// presence flag. The name has no state suffix.
func (r *Registry) Lookup(module string) (*plugin.Manifest, bool) {
	m, ok := r.mods[module]
	return m, ok
}

// State returns the state declaration for `module.state` (e.g. `core.exec` + `run`) and
// a presence flag. A convenience facade over Lookup + Spec.States.
func (r *Registry) State(module, state string) (plugin.StateDef, bool) {
	m, ok := r.mods[module]
	if !ok {
		return plugin.StateDef{}, false
	}
	def, ok := m.Spec.States[state]
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
