//go:build e2e_live

// L3b E2E: SoulBeacon-plugin full-loop (ADR-030 V5-2, 2026-05-26).
//
// Scenario:
//  1. harness starts Keeper + PG + Vault + Redis (testcontainers) + Soul container.
//  2. Soul Docker image mounts/copies test plugin soul-beacon-echo
//     (testdata/beacon-plugin), with its manifest + Sigil seal preloaded through
//     keeper.plugin.allow.
//  3. harness creates incarnation + Vigil with `check: acme.echo` (plugin-beacon
//     address) and Decree reacting to portent through named-scenario.
//  4. ASSERT (direct PG queries, real DB, real flows, no mocks):
//     - Soul host spawns plugin on first tick, Sigil verify passes;
//     - Portent with payload.custom reaches Keeper;
//     - Oracle matched Decree -> apply_runs(planned) under subject SID;
//     - audit_log: `oracle.fired` event.
//
// DEFERRED (skip): MVP harness does not provide helpers for delivering plugin
// binary into Soul container, preloading Sigil seal, and registering Vigil/Decree
// through Operator API. These harness extensions are a separate slice, outside
// V5-2 (V5-2 scope = proto + SDK + pluginhost + L0/L1 coverage; L3b is a
// placeholder for harness extension following V5-1 oracle_typed_portent_test.go).
//
// L1 cross-side integration already covers full handler pipeline:
//   - sdk/beacon (L0): TestBaseBeaconValidateOk / TestBaseBeaconCheckUnknown /
//     TestServerAdapterDelegates;
//   - soul/internal/pluginhost (L1): TestSpawnBeaconHappyPath /
//     TestSpawnBeaconValidationFailure / TestSpawnBeaconRejectsKindMismatch /
//     TestSpawnBeaconDigestMismatchRejected.
package e2e_live_test

import "testing"

// TestE2EBeaconPlugin_FullLoop is a placeholder for V5-N harness extension.
// Uncomment body when harness.DeployBeaconPlugin /
// PreSealSigil / RegisterVigil / RegisterDecree / AssertPortentReceived.
//
// Full assertion coverage for now is L0/L1 (see file header).
func TestE2EBeaconPlugin_FullLoop(t *testing.T) {
	t.Skip("L3b SoulBeacon-plugin full-loop is deferred until harness extension (DeployBeaconPlugin/PreSealSigil/RegisterVigil/RegisterDecree). Current coverage is L0 sdk/beacon + L1 soul/internal/pluginhost beacon_integration_test.go.")
}
