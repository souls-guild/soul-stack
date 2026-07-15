// Passage stratification ([ADR-056](../../../docs/adr/0056-staged-render-passage.md)).
// The canonical logic lives in shared/config (passage.go) — ONE register
// dependency graph for the keeper runtime, keeper tests, and the OFFLINE
// soul-lint (a duplicate risks silent-wrong-target). This file is thin
// aliases onto the shared symbols so keeper-side call sites (run.go, render
// tests) don't need rewriting.
package render

import (
	"github.com/souls-guild/soul-stack/shared/config"
)

// Passage aliases the canonical [config.Passage] (a run's stratification plan).
type Passage = config.Passage

// StratifyError codes — re-exported from the shared/config canon.
const (
	StratifyCycle                     = config.StratifyCycle
	StratifyUnknownRegister           = config.StratifyUnknownRegister
	CodeWithinBlockRegisterDependency = config.CodeWithinBlockRegisterDependency
)

// WithinBlockInfo aliases [config.WithinBlockInfo] (coordinates of a
// within-block register dependency). Kept for keeper-side call sites/tests.
type WithinBlockInfo = config.WithinBlockInfo

// WithinBlockRegisterDependency aliases the canonical detector
// [config.WithinBlockRegisterDependency]: a block: child reading the
// register of a sibling child in the same block (silent-wrong-target).
func WithinBlockRegisterDependency(tasks []config.Task) (config.WithinBlockInfo, bool) {
	return config.WithinBlockRegisterDependency(tasks)
}

// errStratify aliases [config.StratifyError] (carries Code/Msg). Name kept
// for keeper-side tests (errors.As on *errStratify).
type errStratify = config.StratifyError

// Stratify aliases the canonical [config.Stratify]: computes passage indices
// for a run's task plan from the cross-task register dependency graph.
func Stratify(tasks []config.Task) (Passage, error) {
	return config.Stratify(tasks)
}
