package config

// Soul-capabilities are the canonical string names of Keeper↔Soul protocol
// features that the Soul binary announces in Hello.capabilities (ADR-056 §S5
// forward-compat). Keeper persists the announcement next to presence and checks it
// BEFORE dispatching feature-dependent runs. Registry — naming-rules.md →
// "Soul-capabilities".
//
// The constants live in shared/config (not keeper- or soul-internal): both keeper
// (the staged gate in run.go) and Soul (the grpc-client announcement) must
// reference the SAME string — a literal desync = a silent fail-closed on every
// staged run.
const (
	// CapabilityPassage — Soul echoes ApplyRequest.passage in TaskEvent/RunResult,
	// i.e. it can participate in staged render (N>1 Passage, ADR-056). A Soul
	// without this capability under a staged scenario is rejected by keeper BEFORE
	// dispatch (soul_passage_unsupported, fail-closed): otherwise the next Passage's
	// barrier would wait for a terminal the old binary never sends.
	CapabilityPassage = "passage"
)

// SoulCapabilities is the set of capabilities THIS soul-binary build supports
// (announced in Hello.capabilities). All beta souls are built together with
// keeper, so the set is static; forward-safety is for future mixed-version fleets
// where an old binary sends an empty/reduced set.
func SoulCapabilities() []string {
	return []string{CapabilityPassage}
}
