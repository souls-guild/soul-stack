//go:build e2e_live

// L3b E2E: SoulBeacon-plugin full-loop (ADR-030 V5-2, 2026-05-26).
//
// Сценарий:
//  1. harness поднимает Keeper + PG + Vault + Redis (testcontainers) + Soul-
//     контейнер.
//  2. В Docker-image Soul-а монтируется/копируется test-плагин soul-beacon-echo
//     (testdata/beacon-plugin), его manifest + Sigil-печать предзалита через
//     keeper.plugin.allow.
//  3. harness создаёт incarnation + Vigil c `check: wb.echo` (plugin-beacon-
//     адрес) и Decree, реагирующий на portent через named-scenario.
//  4. ASSERT (прямыми PG-queries, real DB, real flows, no mocks):
//     - Soul-host spawn-ит plugin при первом тике, Sigil-verify проходит;
//     - Portent с payload.custom доходит до Keeper;
//     - Oracle сматчил Decree → apply_runs(planned) под subject SID;
//     - audit_log: событие `oracle.fired`.
//
// ОТЛОЖЕНО (skip): harness в MVP не предоставляет хелперы доставки plugin-
// бинаря в Soul-контейнер, предзалива Sigil-печати и регистрации Vigil/Decree
// через Operator-API. Эти расширения harness — отдельный slice, не входящий
// в V5-2 (ТЗ V5-2 = proto + SDK + pluginhost + L0/L1 покрытие; L3b — placeholder
// для harness-extension по образцу V5-1 oracle_typed_portent_test.go).
//
// L1 cross-side integration уже покрывает full handler-pipeline:
//   - sdk/beacon (L0): TestBaseBeaconValidateOk / TestBaseBeaconCheckUnknown /
//     TestServerAdapterDelegates;
//   - soul/internal/pluginhost (L1): TestSpawnBeaconHappyPath /
//     TestSpawnBeaconValidationFailure / TestSpawnBeaconRejectsKindMismatch /
//     TestSpawnBeaconDigestMismatchRejected.
package e2e_live_test

import "testing"

// TestE2EBeaconPlugin_FullLoop — placeholder для V5-N harness-расширения.
// Тело раскомментировать, когда появятся harness.DeployBeaconPlugin /
// PreSealSigil / RegisterVigil / RegisterDecree / AssertPortentReceived.
//
// Полное assertion-покрытие на сейчас — L0/L1 (см. шапку файла).
func TestE2EBeaconPlugin_FullLoop(t *testing.T) {
	t.Skip("L3b SoulBeacon-plugin full-loop откладывается до harness-расширения (DeployBeaconPlugin/PreSealSigil/RegisterVigil/RegisterDecree). Покрытие на сейчас — L0 sdk/beacon + L1 soul/internal/pluginhost beacon_integration_test.go.")
}
