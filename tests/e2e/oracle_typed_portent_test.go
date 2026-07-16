//go:build e2e

// L3a E2E: Oracle typed-PortentPayload full-loop (V5-1, ADR-030 amendment
// 2026-05-26).
//
// Scenario:
//  1. harness brings up Keeper + PG + Vault + Redis (testcontainers).
//  2. harness creates an incarnation (web-app + service-noop), a Vigil and a
//     Decree via the Operator API (REST POST /v1/vigils, /v1/decrees) with a
//     typed-payload where-CEL `event.file_changed.path.startsWith("/etc/")`.
//  3. Soul-stub connects over the mTLS EventStream and sends a PortentEvent
//     with typed file_changed + legacy data (dual-write hand-off period).
//  4. ASSERT (direct PG queries, real DB, real flows, no mocks):
//     - apply_runs(planned) under subject SID + scenario "noop";
//     - oracle_fires cooldown-state for (decree, subject);
//     - audit_log: `oracle.fired` event.
//
// DEFERRED (skip): the MVP harness does not provide CreateVigil /
// CreateDecree / SoulStub.SendPortent helpers. These helpers are a separate
// harness-extension slice not included in V5-1 (V5-1 spec = typed payload in
// proto + Soul-side emit + Keeper-side CEL access + L0/L1 coverage).
//
// L1 cross-side integration already covers the full handler pipeline (real
// PG + real Oracle CRUD + real where-CEL + real apply_runs):
//   - TestIntegration_OracleV5_TypedPayloadDualWriteFires
//   - TestIntegration_OracleV5_LegacyOnlyStillFires
//   - TestIntegration_OracleV5_TypedOnlyMatches
//   - TestIntegration_OracleV5_TypeMismatchNoFire
//
// The skeleton + fixtures here are a stub for the harness extension
// (RegisterVigilDecree / SoulStub / AssertOracleFired). Fixtures in
// tests/e2e/oracle_typed_portent/ describe the expected inputs/outputs; the
// harness will read them once the helpers land.
package e2e_test

import "testing"

// TestE2EOracleTypedPortent_FileChangedFireFlow -- placeholder for the V5-N
// harness extension. Uncomment the body once
// harness.RegisterVigil / RegisterDecree / SoulStub / AssertOracleFired land.
//
// Full assertion coverage lives in L1 (see file header).
func TestE2EOracleTypedPortent_FileChangedFireFlow(t *testing.T) {
	t.Skip("superseded by the working execution-e2e TestOracle_FileChanged_FiresScenario (vigil_oracle_test.go): full-loop soul-stub.SendPortent -> handlePortentEvent -> match/where/cooldown/enqueue -> apply_runs + audit oracle.fired. L1: keeper/internal/grpc/oracle_crosside_integration_test.go::TestIntegration_OracleV5_*.")
}
