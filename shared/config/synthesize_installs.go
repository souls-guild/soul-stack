package config

import "strings"

// moduleInstalledAddr — Soul-side module for SoulModule plugin delivery (ADR-065).
const moduleInstalledAddr = "core.module.installed"

// SynthesizeModuleInstalls synthesizes Soul-side core.module.installed steps from
// `service.yml::modules[]` before the first consumer of each module (ADR-065);
// takeover (an explicit literal step), core.* and modules with no consumers are
// skipped. Call AFTER [ExpandIncludes], BEFORE Stratify; with no synthesis the
// input is returned bit-for-bit. The second result is the names of synthesized
// modules (for the log).
func SynthesizeModuleInstalls(tasks []Task, modules []DependencyRef) ([]Task, []string) {
	if len(modules) == 0 {
		return tasks, nil
	}

	firstConsumer := map[string]int{} // "<ns>.<module>" → top-level index of the first consumer's task
	takeover := map[string]bool{}     // literal params.name of explicit install steps
	for i := range tasks {
		collectModuleUsage(&tasks[i], i, firstConsumer, takeover)
	}

	inserts := map[int][]Task{} // top-level index → synthesized steps (manifest order)
	var names []string
	for _, dep := range modules {
		if strings.HasPrefix(dep.Name, "core.") { // defense-in-depth: service.yml validation already forbids this
			continue
		}
		idx, used := firstConsumer[dep.Name]
		if !used || takeover[dep.Name] {
			continue
		}
		inserts[idx] = append(inserts[idx], Task{
			Name: "install " + dep.Name + " (service manifest)",
			Module: &ModuleTask{
				Module: moduleInstalledAddr,
				Params: map[string]any{"name": dep.Name, "ref": dep.Ref},
			},
		})
		names = append(names, dep.Name)
	}
	if len(names) == 0 {
		return tasks, nil
	}

	out := make([]Task, 0, len(tasks)+len(names))
	for i := range tasks {
		out = append(out, inserts[i]...)
		out = append(out, tasks[i])
	}
	return out, names
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

// literalInstallName — the literal params.name of an explicit install step; a
// `${…}` name is statically unknown → not a takeover (ADR-010: a CEL value is not typed).
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
