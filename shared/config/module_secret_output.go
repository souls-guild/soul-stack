package config

import (
	"sort"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// SecretOutputFields returns the names of the output fields the module at task
// address addr declares `secret: true` ([ADR-0083] §8), sorted so the result is
// byte-stable across renders (it travels on the wire and into audit payloads).
//
// This is the replacement for `no_log`. The declaration lives with the module,
// which is the only party that knows the shape of what it returns; the task
// author — who previously had to know that shape to set `no_log:` — declares
// nothing. Being per-field rather than per-task, it also stops suppressing a
// whole task's diagnostics in order to hide one value.
//
// modules resolves non-core namespaces. Both a nil resolver and a module the
// resolver cannot see yield nil: masking cannot be applied to fields nobody can
// name. That gap is not silent — the same resolver drives
// [DiagPluginParamsUnchecked] at load, which is where an author is told the
// manifest did not resolve.
func SecretOutputFields(addr string, modules ModuleManifestResolver) []string {
	return secretOutputFields(addr, coremanifest.Default(), modules)
}

func secretOutputFields(addr string, reg coreModuleLookup, modules ModuleManifestResolver) []string {
	ns, mod, state, ok := splitModuleAddress(addr)
	if !ok {
		return nil
	}
	name := ns + "." + mod

	// Which arm to take is decided by the REGISTRY, not by the namespace string —
	// address level 1 is an operator's alias since NIM-377 (see moduleDeprecatedUses).
	builtin := false
	if reg != nil {
		_, builtin = reg.Lookup(name)
	}

	var def plugin.StateDef
	switch {
	case builtin:
		d, known := reg.State(name, state)
		if !known {
			return nil
		}
		def = d
	case plugin.IsReserved(ns), modules == nil:
		return nil
	default:
		man, resolved := modules.ResolveModule(ns, mod)
		if !resolved {
			return nil
		}
		d, known := man.States[state]
		if !known {
			return nil
		}
		def = d
	}

	var out []string
	for field, p := range def.Output {
		if p.Secret {
			out = append(out, field)
		}
	}
	sort.Strings(out)
	return out
}
