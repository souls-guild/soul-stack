package config

// Scanning a DEFINITION for deprecated params (ADR-0076(v), NIM-268).
//
// The run-time channel (ADR-0076(u)) tells an operator that a run they are
// looking at passed a param on its way out. It cannot tell them where ELSE that
// is true, and the obvious source for that — the notices stored on past runs —
// answers a different question: "who passed it in the runs we happened to
// observe". An incarnation nobody has run this month is missing from such an
// answer although its definition will break at `removed_in`, and one fixed
// yesterday still appears because the old rows remain. Planning a migration
// against that is planning against noise.
//
// So the source is the definition — the text that will be rendered next time —
// and this file is the walk over it. Deliberately a sibling of the
// `introduced_in` walk in introduced.go rather than an extension of it: they
// share a shape (visit every task, look at what it actually passes, consult the
// manifest) but answer opposite questions, and one function returning both would
// be read at each call site by whichever half the caller did not want.
//
// A plugin module is resolved through the same [ModuleManifestResolver] the
// static check uses (NIM-228) — keeper snapshots it from its Sigil grants,
// soul-lint from `--modules`. Passing nil is legal and means "no plugin catalog
// here", which is NOT the same as "the plugins are clean": the walk reports what
// it SKIPPED alongside what it found, so "no deprecated params" and "I could not
// look" never reach a caller as the same silence. That distinction is the whole
// reason this returns a scan rather than a slice.

import (
	"sort"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// DeprecatedParamUse — one place a definition passes a param that its module's
// manifest marks deprecated.
//
// Carries the manifest's own block rather than a rendered sentence: a caller
// aggregating across a fleet sorts by RemovedIn ("what breaks first") and groups
// by Module+Param, and prose would have to be parsed apart again to do either.
// The sentence is available from [plugin.DeprecatedDef.Notice] wherever it is
// actually shown.
type DeprecatedParamUse struct {
	// Module — the full address the task named: <ns>.<module>.<state>.
	Module string
	// Param — the deprecated input parameter the task passes.
	Param string
	// Deprecated — the manifest block: since / removed_in / use.
	Deprecated plugin.DeprecatedDef
	// Where — the YAML-ish location inside the scanned task list, for a
	// diagnostic that can point at the line rather than just name the service.
	Where string
}

// UnresolvedModule — a module a definition uses whose contract this scan could
// not read, so nothing can be claimed about its params either way.
//
// Exists because the alternative is a lie by omission. Without it, a service
// whose every task targets plugin modules produces an empty finding list —
// indistinguishable from a service that is genuinely clean, and an operator
// planning a migration would read "nothing to do" from "nothing was checked".
type UnresolvedModule struct {
	// Module — the address whose manifest was not available.
	Module string
	// Reason — why, in one machine-readable token: `plugin_namespace` (the
	// manifest ships with the plugin, not with the definition) or
	// `unknown_core_module` (a core module this binary does not carry — an
	// engine older than the definition).
	Reason string
	// Where — the location of the task that used it.
	Where string
}

// Reasons a module's contract is unavailable to a definition scan.
const (
	// ReasonPluginNamespace — a plugin module whose manifest the supplied
	// catalog did not resolve (or no catalog was supplied at all). Deliberately
	// the same meaning as the static check's `plugin_params_unchecked`: nobody
	// looked, which is neither an error nor a clean bill of health.
	ReasonPluginNamespace = "plugin_namespace"
	// ReasonUnknownPluginState — the plugin manifest resolved but declares no
	// such state, so this task's params were not checked against anything.
	ReasonUnknownPluginState = "unknown_plugin_state"
	// ReasonUnknownCoreModule — namespace is `core` but this engine has no
	// manifest for it: the definition is newer than the binary scanning it.
	ReasonUnknownCoreModule = "unknown_core_module"
)

// DeprecatedScan — the result of walking one definition.
type DeprecatedScan struct {
	// Uses — every deprecated param actually passed, ordered by (Module, Param,
	// Where) so two scans of the same text agree.
	Uses []DeprecatedParamUse
	// Unresolved — modules whose contract could not be read, deduplicated by
	// address+reason. A NON-EMPTY value means Uses is a lower bound.
	Unresolved []UnresolvedModule
}

// Clean reports whether the scan both found nothing AND was able to look
// everywhere. A caller rendering "this service is fine" must ask THIS rather
// than `len(Uses) == 0`, or it will say "fine" about a definition it never read.
func (s DeprecatedScan) Clean() bool {
	return len(s.Uses) == 0 && len(s.Unresolved) == 0
}

// ScanTasksForDeprecated walks a flat task list (post-ExpandIncludes, the same
// input shape the render pipeline and the introduced_in walk take) and reports
// the deprecated params it passes.
//
// Only params the task ACTUALLY passes count: a deprecated param a definition
// never mentions is not a migration this definition has to make, and listing it
// would bury the real ones.
// modules resolves non-core namespaces; nil means no plugin catalog is
// available, and every plugin module then lands in Unresolved rather than being
// quietly treated as clean.
func ScanTasksForDeprecated(tasks []Task, modules ModuleManifestResolver) DeprecatedScan {
	return scanTasksForDeprecated(tasks, coremanifest.Default(), modules)
}

func scanTasksForDeprecated(tasks []Task, reg coreModuleLookup, modules ModuleManifestResolver) DeprecatedScan {
	var scan DeprecatedScan
	collectDeprecatedUses(tasks, "$.tasks", reg, modules, &scan)

	sort.Slice(scan.Uses, func(i, j int) bool {
		a, b := scan.Uses[i], scan.Uses[j]
		if a.Module != b.Module {
			return a.Module < b.Module
		}
		if a.Param != b.Param {
			return a.Param < b.Param
		}
		return a.Where < b.Where
	})
	scan.Unresolved = dedupeUnresolved(scan.Unresolved)
	return scan
}

func collectDeprecatedUses(tasks []Task, prefix string, reg coreModuleLookup, modules ModuleManifestResolver, scan *DeprecatedScan) {
	for i := range tasks {
		t := &tasks[i]
		where := prefix + "[" + scanIdx(i) + "]"
		if t.Module != nil {
			moduleDeprecatedUses(t.Module.Module, t.Module.Params, where, reg, modules, scan)
		}
		// A block's children are ordinary tasks and pass ordinary params; a
		// deprecation inside one is no less due for migration than at top level.
		if t.Block != nil {
			collectDeprecatedUses(t.Block.Block, where+".block", reg, modules, scan)
		}
	}
}

// moduleDeprecatedUses inspects one module task. Anything it cannot resolve is
// recorded in Unresolved rather than dropped — see [UnresolvedModule].
func moduleDeprecatedUses(addr string, params map[string]any, where string, reg coreModuleLookup, modules ModuleManifestResolver, scan *DeprecatedScan) {
	ns, mod, state, ok := splitModuleAddress(addr)
	if !ok {
		// A malformed address is already `module_format_invalid` from the
		// validator; it is not this walk's business to report it twice.
		return
	}
	if ns != "core" {
		pluginDeprecatedUses(addr, ns, mod, state, params, where, modules, scan)
		return
	}
	if reg == nil {
		return
	}
	name := ns + "." + mod
	if _, known := reg.Lookup(name); !known {
		scan.Unresolved = append(scan.Unresolved, UnresolvedModule{
			Module: addr, Reason: ReasonUnknownCoreModule, Where: where,
		})
		return
	}
	def, ok := reg.State(name, state)
	if !ok {
		// An unknown state of a known module is `module_state_unknown` from the
		// static check, with a position. Recorded as unresolved all the same:
		// its params were not checked, and that is what this field means.
		scan.Unresolved = append(scan.Unresolved, UnresolvedModule{
			Module: addr, Reason: ReasonUnknownCoreModule, Where: where,
		})
		return
	}

	collectDeprecatedParams(addr, def, params, where, scan)
}

// collectDeprecatedParams records every param the task passes that the state's
// input marks deprecated. Shared by both arms: core and plugin differ in where
// the StateDef came from, never in what a deprecation means.
func collectDeprecatedParams(addr string, def plugin.StateDef, params map[string]any, where string, scan *DeprecatedScan) {
	names := make([]string, 0, len(params))
	for p := range params {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		schema, known := def.Input[p]
		if !known || schema.Deprecated == nil {
			continue
		}
		scan.Uses = append(scan.Uses, DeprecatedParamUse{
			Module:     addr,
			Param:      p,
			Deprecated: *schema.Deprecated,
			Where:      where + ".params." + p,
		})
	}
}

// pluginDeprecatedUses is the plugin arm, resolved through the catalog NIM-228
// introduced. Structurally the same walk as core's — the four checks were never
// core-specific, and neither is this one: what differs is only AVAILABILITY of
// the manifest, which the resolver supplies.
//
// A resolver that cannot see the module is NOT an error and NOT a clean result:
// it is `plugin_namespace`, meaning nobody looked. Same for a state the manifest
// does not declare — its params went unchecked, which is what Unresolved says.
func pluginDeprecatedUses(addr, ns, mod, state string, params map[string]any, where string, modules ModuleManifestResolver, scan *DeprecatedScan) {
	if modules == nil {
		scan.Unresolved = append(scan.Unresolved, UnresolvedModule{
			Module: addr, Reason: ReasonPluginNamespace, Where: where,
		})
		return
	}
	man, ok := modules.ResolveModule(ns, mod)
	if !ok || man == nil {
		scan.Unresolved = append(scan.Unresolved, UnresolvedModule{
			Module: addr, Reason: ReasonPluginNamespace, Where: where,
		})
		return
	}
	def, known := man.Spec.States[state]
	if !known {
		scan.Unresolved = append(scan.Unresolved, UnresolvedModule{
			Module: addr, Reason: ReasonUnknownPluginState, Where: where,
		})
		return
	}
	collectDeprecatedParams(addr, def, params, where, scan)
}

// dedupeUnresolved collapses repeats by (Module, Reason), keeping the first
// location: twenty tasks using the same plugin module are one gap in coverage,
// not twenty, and a caller listing them wants the gap named once.
func dedupeUnresolved(in []UnresolvedModule) []UnresolvedModule {
	if len(in) == 0 {
		return nil
	}
	type key struct{ module, reason string }
	seen := make(map[key]struct{}, len(in))
	out := make([]UnresolvedModule, 0, len(in))
	for _, u := range in {
		k := key{u.Module, u.Reason}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// scanIdx — small non-negative int to string, avoiding a strconv import for the
// single use in a location string.
func scanIdx(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
