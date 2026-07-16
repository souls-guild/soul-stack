//go:build e2e

package harness

// YAML loader for fixtures/<*>.yaml and expectations/<*>.yaml — typed
// structures reflecting the spec format (see docs/testing/e2e.md).
//
// In the pilot phase the types are defined, reading from YAML is not
// implemented (L3a-implementation slice). Consistency of the types with
// docs/testing/e2e.md is caught statically: editing the spec without editing
// the types is caught by a smoke test (the loader fails a ParseError on
// mismatch).

// SoulsFixture — `tests/e2e/<name>/fixtures/souls.yaml`. Describes the set of
// soul stubs the harness registers with the Keeper before a run.
type SoulsFixture []SoulFixtureEntry

// SoulFixtureEntry — one row of SoulsFixture.
//
// Status — the desired souls.<sid>.status once the Stack is ready
// ("connected" is the standard happy path). Covens — Coven membership for
// `where:` targeting. Soulprint — soulprint_facts contents, written to the DB
// via the same path as the Soul-side SoulprintReport (but without gRPC, a
// direct INSERT).
type SoulFixtureEntry struct {
	SID       string         `yaml:"sid"`
	Status    string         `yaml:"status"`
	Covens    []string       `yaml:"covens"`
	Soulprint map[string]any `yaml:"soulprint"`
}

// StubResponsesFixture — `tests/e2e/<name>/fixtures/stub-responses.yaml`.
// A script of soul-stub responses: per scenario name -> a list of scripted
// RunResults for each ApplyRequest coming from the Keeper.
type StubResponsesFixture struct {
	Scenarios map[string]ScenarioScript `yaml:"scenarios"`
}

// ScenarioScript — the soul-stub's response script for a particular scenario name.
type ScenarioScript struct {
	ApplyResponses []ApplyResponseScript `yaml:"apply_responses"`
}

// ApplyResponseScript — one scripted soul-stub response.
//
// TaskName — the task name (for matching: the stub responds to an
// ApplyRequest with this task_name using the chosen RunResult). RunResult —
// the payload the stub packs into FromSoul.RunResult and sends to the Keeper.
type ApplyResponseScript struct {
	TaskName  string         `yaml:"task_name"`
	RunResult map[string]any `yaml:"run_result"`
}

// ExpectationsAfter — `tests/e2e/<name>/expectations/after-<scenario>.yaml`.
// Post-apply expectations: apply_runs / incarnation.state / audit / metrics.
type ExpectationsAfter struct {
	ApplyRuns        ApplyRunsExpectation    `yaml:"apply_runs"`
	IncarnationState map[string]any          `yaml:"incarnation_state"`
	AuditEvents      []AuditEventExpectation `yaml:"audit_events"`
	Metrics          map[string]string       `yaml:"metrics"`
}

// ApplyRunsExpectation — expected shape of an apply_runs row (status is required).
type ApplyRunsExpectation struct {
	Status string `yaml:"status"`
}

// AuditEventExpectation — expectation for an audit_log row (type is required,
// Payload is a deep subset).
type AuditEventExpectation struct {
	Type    string         `yaml:"type"`
	Payload map[string]any `yaml:"payload"`
}
