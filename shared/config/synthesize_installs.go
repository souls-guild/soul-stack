package config

import "strings"

// moduleInstalledAddr — Soul-side модуль доставки SoulModule-плагина (ADR-065).
const moduleInstalledAddr = "core.module.installed"

// SynthesizeModuleInstalls синтезирует Soul-side шаги core.module.installed из
// `service.yml::modules[]` перед первым потребителем каждого модуля (ADR-065);
// takeover (явный литеральный шаг), core.* и модули без потребителей — skip.
// Вызывать ПОСЛЕ [ExpandIncludes], ДО Stratify; без синтеза вход возвращается
// бит-в-бит. Второй результат — имена синтезированных модулей (для лога).
func SynthesizeModuleInstalls(tasks []Task, modules []DependencyRef) ([]Task, []string) {
	if len(modules) == 0 {
		return tasks, nil
	}

	firstConsumer := map[string]int{} // "<ns>.<module>" → индекс top-level задачи первого потребителя
	takeover := map[string]bool{}     // литеральные params.name явных install-шагов
	for i := range tasks {
		collectModuleUsage(&tasks[i], i, firstConsumer, takeover)
	}

	inserts := map[int][]Task{} // top-level индекс → синтез-шаги (порядок манифеста)
	var names []string
	for _, dep := range modules {
		if strings.HasPrefix(dep.Name, "core.") { // defense-in-depth: валидация service.yml уже запрещает
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

// collectModuleUsage наполняет firstConsumer/takeover по одной top-level задаче
// top (рекурсивно через block: — потребитель внутри block адресует весь block).
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

// literalInstallName — литеральное params.name явного install-шага; `${…}`-имя
// статически неизвестно → не takeover (ADR-010: CEL-значение не типизируется).
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
