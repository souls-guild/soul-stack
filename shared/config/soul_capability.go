package config

import "sort"

// Soul-capabilities are the canonical string names of what the Soul binary
// announces in Hello.capabilities (ADR-056 §S5 forward-compat, generalized into
// the Soul-side compatibility axis by ADR-0076(i)). Keeper persists the
// announcement next to presence and checks it BEFORE dispatching a run that
// depends on it. Registry — naming-rules.md → "Soul-capabilities".
//
// Three groups: Keeper↔Soul protocol features (passage, console), Soul-side DSL
// features the binary enforces itself (flow_control, retry, dry_run), and one
// `module:<name>` entry per core module the binary implements. The module group
// is what closes the silent-ignore mode of ADR-0076: an unannounced module is a
// per-host fail-closed rejection before dispatch instead of a task reporting
// OK/CHANGED with no effect.
//
// The constants live in shared/config (not keeper- or soul-internal): both keeper
// (the gates in run.go) and Soul (the grpc-client announcement) must
// reference the SAME string — a literal desync = a silent fail-closed on every
// staged run.
const (
	// CapabilityPassage — Soul echoes ApplyRequest.passage in TaskEvent/RunResult,
	// i.e. it can participate in staged render (N>1 Passage, ADR-056). A Soul
	// without this capability under a staged scenario is rejected by keeper BEFORE
	// dispatch (soul_passage_unsupported, fail-closed): otherwise the next Passage's
	// barrier would wait for a terminal the old binary never sends.
	CapabilityPassage = "passage"

	// CapabilityConsole — Soul understands the only-add `console_*` messages and
	// can host an interactive pty session. Keeper checks it BEFORE minting a
	// console session: an old binary would drop ConsoleOpen into the default
	// branch of its recv-loop and never answer, leaving the operator staring at a
	// dead terminal until an idle timeout. Same fail-closed shape as
	// CapabilityPassage.
	CapabilityConsole = "console"

	// CapabilityFlowControl — Soul evaluates the per-task flow-control CEL
	// predicates `when:`/`changed_when:`/`failed_when:` itself (ADR-012(d)): keeper
	// threads them through as strings, so a binary that ignores them would run a
	// gated-off task and report the raw module outcome — the silent-wrong-result
	// this axis exists to prevent (ADR-0076(i)).
	CapabilityFlowControl = "flow_control"

	// CapabilityRetry — Soul enforces the `retry:` loop (count/delay) and its
	// `until:` exit predicate (destiny/tasks.md §9). A binary without it makes one
	// attempt and reports FAILED where the author declared the failure to be
	// retryable.
	CapabilityRetry = "retry"

	// CapabilityDryRun — Soul honors ApplyRequest.dry_run by calling
	// SoulModule.Plan instead of Apply (Scry, ADR-031). Fail-closed matters most
	// here: a binary that ignores the flag would MUTATE the host during a
	// check-drift that promised a pure read.
	CapabilityDryRun = "dry_run"

	// CapabilityModulePrefix — namespace of the per-module capability
	// `module:<name>`, where name is the module address WITHOUT a state suffix
	// (`module:core.pkg`, not `module:core.pkg.installed`): the registry key is the
	// module, states are dispatched inside its implementation. Only core modules
	// are announced — a plugin module can be installed mid-run by
	// core.module.installed (ADR-065), long after the announcement was made at
	// connect time, so gating on one would reject a legitimate install-then-use
	// scenario. The plugin hole stays with param-level strictness (NIM-163).
	CapabilityModulePrefix = "module:"
)

// ModuleCapability is the capability announcing that a binary implements the
// given core module. Takes the module address without a state suffix.
func ModuleCapability(module string) string { return CapabilityModulePrefix + module }

// SoulCapabilities is the set of capabilities a soul-binary build announces in
// Hello.capabilities: the fixed protocol and DSL features THIS build implements,
// plus one [ModuleCapability] per name in coreModules (the caller's own module
// registry — the announcement must state what the binary actually carries, not
// what the catalog describes).
//
// The result is deduplicated and sorted, so the announcement is stable across
// reconnects (Registry.Names() iterates a map). A nil/empty coreModules is
// legitimate — a build with no modules announces only the feature set.
func SoulCapabilities(coreModules []string) []string {
	out := make([]string, 0, len(coreModules)+5)
	out = append(out, CapabilityPassage, CapabilityConsole, CapabilityFlowControl, CapabilityRetry, CapabilityDryRun)
	seen := make(map[string]struct{}, len(coreModules))
	for _, m := range coreModules {
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, ModuleCapability(m))
	}
	sort.Strings(out)
	return out
}
