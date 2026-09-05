package config

import "strings"

// moduleInstalledAddr — Soul-side module for SoulModule plugin delivery (ADR-065).
const moduleInstalledAddr = "core.module.installed"

// SynthesizeModuleInstalls synthesizes Soul-side core.module.installed steps from
// `service.yml::modules[]` before the first consumer of each module (ADR-065);
// takeover (an explicit literal step), reserved names and modules with no consumers
// are skipped. Call AFTER [ExpandIncludes], BEFORE Stratify; with no synthesis the
// input is returned bit-for-bit. The second result is the aliases of the
// synthesized installs (for the log).
//
// What the synthesized step CARRIES is the alias, not the manifest entry's name.
// `modules[].name` is the alias itself, or the deprecated `<alias>.<module>`
// (NIM-829); the step's `params.name` is address level 1 either way, because that is
// the only identity core.module.installed has to work with — since NIM-377 the
// artifact carries no self-name, and the slot, the Sigil grant and every address
// derived from them agree on the registration alias. Passing the whole two-level name
// is what made every service declaring `modules:` unappliable on every host: the Soul
// side rejects a dot outright (NIM-524).
//
// EVERYTHING here is keyed on the alias, on both sides of the match: the manifest
// entry is reduced to it, and so is each consumer's address. One entry `redis` and
// six entries `redis.<object>` therefore synthesize the same single install, before
// the earliest consumer of ANY redis object — which is what lets the two declaration
// forms mean the same thing for the length of the transition window. Matching a
// consumer against the whole two-level entry, as this did before NIM-829, additionally
// made a plan's correctness depend on the manifest ENUMERATING every object it
// touches: a scenario calling `redis.sentinel.present` under a manifest that listed
// only `redis.instance` got no install, or got one placed after the consumer that
// needed it.
func SynthesizeModuleInstalls(tasks []Task, modules []DependencyRef) ([]Task, []string) {
	if len(modules) == 0 {
		return tasks, nil
	}

	firstConsumer := map[string]int{} // alias → top-level index of the first consumer's task
	takeover := map[string]bool{}     // address level 1 of the literal params.name of explicit install steps
	for i := range tasks {
		collectModuleUsage(&tasks[i], i, firstConsumer, takeover)
	}

	at := map[string]int{}     // alias → top-level index the install goes before
	ref := map[string]string{} // alias → ref carried into the pin check
	aliases := make([]string, 0, len(modules))
	for _, dep := range modules {
		if reservedModuleAddr(dep.Name) { // defense-in-depth: service.yml validation already forbids every reserved name
			continue
		}
		alias, ok := ModuleAlias(dep.Name)
		if !ok || takeover[alias] {
			continue
		}
		idx, used := firstConsumer[alias]
		if !used {
			continue
		}
		if _, planned := at[alias]; planned {
			// Sibling two-level entries of one artifact. No position to reconcile:
			// firstConsumer is already the earliest consumer of the whole alias, so
			// every one of them names the same index. The first entry's ref wins, and
			// a disagreement is `conflicting_module_ref` at validation.
			continue
		}
		at[alias], ref[alias] = idx, dep.Ref
		aliases = append(aliases, alias)
	}
	if len(aliases) == 0 {
		return tasks, nil
	}

	inserts := map[int][]Task{} // top-level index → synthesized steps (manifest order)
	for _, alias := range aliases {
		idx := at[alias]
		inserts[idx] = append(inserts[idx], Task{
			Name: "install " + alias + " (service manifest)",
			Module: &ModuleTask{
				Module: moduleInstalledAddr,
				Params: map[string]any{"name": alias, "ref": ref[alias]},
			},
		})
	}

	out := make([]Task, 0, len(tasks)+len(aliases))
	for i := range tasks {
		out = append(out, inserts[i]...)
		out = append(out, tasks[i])
	}
	return out, aliases
}

// ModuleAlias — the registration alias naming the slot a dependency's artifact
// installs into: the name itself, or address level 1 of the deprecated
// `<alias>.<module>` form (NIM-829). ok is false only for an empty alias, which is
// the one string that names no slot.
//
// A bare name is its OWN alias and not a refusal. It used to be one, on the ground
// that `reDependencyModuleName` "already rejects those in service.yml" — a defensive
// half that outlived the rule it was defending, and would now reject the canonical
// spelling.
func ModuleAlias(name string) (string, bool) {
	alias := addrLevel1(name)
	if alias == "" {
		return "", false
	}
	return alias, true
}

// addrLevel1 — everything before the first dot, or the whole string when there is
// none. The one reduction BOTH ends of the takeover comparison go through: the
// manifest declares `<alias>.<module>` and the explicit step writes a bare alias, so
// the halves only meet if each is cut down to the slot they name. Keying the two
// sides on their own raw spellings is what NIM-543 found — an author writing the
// pre-NIM-377 `name: community.redis` took over nothing, and the synthesizer added a
// second install of the same artifact beside theirs.
func addrLevel1(name string) string {
	level1, _, _ := strings.Cut(name, ".")
	return level1
}

// collectModuleUsage fills firstConsumer/takeover from one top-level task top
// (recursively via block: — a consumer inside a block addresses the whole block).
func collectModuleUsage(t *Task, top int, firstConsumer map[string]int, takeover map[string]bool) {
	if t.Module != nil {
		if t.Module.Module == moduleInstalledAddr {
			if name, ok := literalInstallName(t.Module.Params); ok {
				if alias := addrLevel1(name); alias != "" {
					takeover[alias] = true
				}
			}
		} else if name, _, ok := SplitModuleAddr(t.Module.Module); ok {
			// Keyed on the ALIAS, the same reduction the manifest side goes through:
			// what a task consumes is the artifact in the slot, and which of its
			// objects the address names decides nothing about installing it.
			if alias := addrLevel1(name); alias != "" {
				if _, seen := firstConsumer[alias]; !seen {
					firstConsumer[alias] = top
				}
			}
		}
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			collectModuleUsage(&t.Block.Block[i], top, firstConsumer, takeover)
		}
	}
}

// literalInstallName — the literal params.name of an explicit install step; a `${…}`
// name is statically unknown → not a takeover (ADR-010: a CEL value is not typed).
//
// The value is returned as written, NOT reduced to its alias. A step spelling the
// pre-NIM-377 `community.redis` is a takeover of the `community` slot — it names one
// artifact, and a synthesized second install of the same slot beside it helps nobody
// — but it is also an authoring error the offline check reports
// (`module_install_name_not_an_alias`, module_params.go). The reduction belongs to
// the caller, so the two facts stay separable.
func literalInstallName(params map[string]any) (string, bool) {
	v, ok := params["name"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || containsCELCell(s) {
		return "", false
	}
	return s, true
}

// containsCELCell — the value carries a `${…}` cell somewhere in it, so what it
// renders to is not knowable offline (ADR-010). PARTIAL interpolation counts:
// `acme-${ vars.env }` is legal (docs/templating.md §5(b)) and renders to an ordinary
// alias, so a whole-string test would deny it takeover here and reject it as malformed
// in the offline check — the two ends disagreeing on one field, which is the defect
// NIM-543 exists to close.
//
// The two callers of this are those two ends. Keep it one function.
func containsCELCell(s string) bool { return strings.Contains(s, "${") }
