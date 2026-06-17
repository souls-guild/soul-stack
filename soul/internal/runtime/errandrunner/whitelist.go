package errandrunner

import (
	"fmt"

	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
)

// hardcodedWhitelist — verb-модули, императивные by-design (shell-exec), для
// которых Errand-runner допускает вызов по имени БЕЗ marker-check-а
// ([sdkmodule.ErrandReadSafe]). ADR-033 §2: жёсткий список — core.cmd.shell и
// core.exec.run; всё остальное (включая будущие custom-плагины) проходит
// через marker-interface default-deny.
//
// Сравнение по точному совпадению полного адреса `<ns>.<name>.<state>`: state
// важен (`core.cmd.shell` допущен, гипотетический `core.cmd.foo` — нет).
var hardcodedWhitelist = map[string]struct{}{
	"core.cmd.shell": {},
	"core.exec.run":  {},
}

// IsAllowed проверяет, что модуль mod, адресованный fullName-ом
// (`<namespace>.<name>.<state>`), безопасно вызвать через Errand. Возвращает
// (ok, reason). reason всегда `errand_module_not_allowed: <module>` для
// форматной совместимости с error-кодами [docs/naming-rules.md].
//
// Алгоритм (ADR-033 §2):
//  1. Жёсткий список (verb-модули shell/exec, императивные by-design).
//  2. [sdkmodule.ErrandReadSafe] marker — модуль сам декларирует «безопасен к
//     ad-hoc invocation» (BaseModule этот интерфейс НЕ реализует, поэтому
//     custom-плагин на BaseModule по умолчанию получает default-deny).
//  3. Иначе reject.
//
// nil-mod — defensive reject (caller обязан вызывать после Lookup; здесь
// перестраховка).
func IsAllowed(fullName string, mod sdkmodule.SoulModule) (bool, string) {
	if _, ok := hardcodedWhitelist[fullName]; ok {
		return true, ""
	}
	if mod == nil {
		return false, fmt.Sprintf("errand_module_not_allowed: %s", fullName)
	}
	if _, ok := mod.(sdkmodule.ErrandReadSafe); ok {
		return true, ""
	}
	return false, fmt.Sprintf("errand_module_not_allowed: %s", fullName)
}
