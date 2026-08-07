package config

import "strings"

// moduleInstalledAddr — Soul-side module for SoulModule plugin delivery (ADR-065).
const moduleInstalledAddr = "core.module.installed"

// SynthesizeModuleInstalls synthesizes Soul-side core.module.installed steps from
// `service.yml::modules[]` before the first consumer of each module (ADR-065);
// takeover (an explicit literal step), core.* and modules with no consumers are
// skipped. Call AFTER [ExpandIncludes], BEFORE Stratify; with no synthesis the
// input is returned bit-for-bit. The second result is the aliases of the
// synthesized installs (for the log).
//
// What the synthesized step CARRIES is the alias, not the manifest entry's name.
// `modules[].name` is a two-level address prefix `<alias>.<module>`; the step's
// `params.name` is address level 1 alone, because that is the only identity
// core.module.installed has to work with — since NIM-377 the artifact carries no
// self-name, and the slot, the Sigil grant and every address derived from them
// agree on the registration alias. Passing the whole two-level name is what made
// every service declaring `modules:` unappliable on every host: the Soul side
// rejects a dot outright (NIM-524).
//
// Two entries of the same artifact (one alias serving `redis` and `sentinel`)
// therefore collapse to ONE install, placed before the earliest of their
// consumers — a second step would install the same bytes into the same slot.
func SynthesizeModuleInstalls(tasks []Task, modules []DependencyRef) ([]Task, []string) {
	if len(modules) == 0 {
		return tasks, nil
	}

	firstConsumer := map[string]int{} // "<alias>.<module>" → top-level index of the first consumer's task
	takeover := map[string]bool{}     // literal params.name (an ALIAS) of explicit install steps
	for i := range tasks {
		collectModuleUsage(&tasks[i], i, firstConsumer, takeover)
	}

	at := map[string]int{}     // alias → top-level index the install goes before
	ref := map[string]string{} // alias → ref carried into the pin check
	aliases := make([]string, 0, len(modules))
	for _, dep := range modules {
		if strings.HasPrefix(dep.Name, "core.") { // defense-in-depth: service.yml validation already forbids this
			continue
		}
		idx, used := firstConsumer[dep.Name]
		if !used {
			continue
		}
		alias, ok := ModuleAlias(dep.Name)
		if !ok || takeover[alias] {
			continue
		}
		if prev, planned := at[alias]; planned {
			if idx < prev {
				at[alias] = idx
			}
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

// ModuleAlias — address level 1 of a `<alias>.<module>` dependency name, i.e. the
// registration alias naming the slot the artifact installs into. ok is false for a
// name with no level 2; `reDependencyModuleName` already rejects those in
// service.yml, so this is the defensive half of the same rule.
func ModuleAlias(name string) (string, bool) {
	alias, _, found := strings.Cut(name, ".")
	if !found || alias == "" {
		return "", false
	}
	return alias, true
}

// collectModuleUsage fills firstConsumer/takeover from one top-level task top
// (recursively via block: — a consumer inside a block addresses the whole block).
func collectModuleUsage(t *Task, top int, firstConsumer map[string]int, takeover map[string]bool) {
	if t.Module != nil {
		if t.Module.Module == moduleInstalledAddr {
			if name, ok := literalInstallName(t.Module.Params); ok {
				takeover[name] = true
			}
		} else if name, _, ok := SplitModuleAddr(t.Module.Module); ok {
			if _, seen := firstConsumer[name]; !seen {
				firstConsumer[name] = top
			}
		}
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			collectModuleUsage(&t.Block.Block[i], top, firstConsumer, takeover)
		}
	}
}

// literalInstallName — the literal params.name of an explicit install step, which
// is an ALIAS; a `${…}` name is statically unknown → not a takeover (ADR-010: a CEL
// value is not typed).
func literalInstallName(params map[string]any) (string, bool) {
	v, ok := params["name"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || strings.Contains(s, "${") {
		return "", false
	}
	return s, true
}
