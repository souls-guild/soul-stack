//go:build e2e

// L3a E2E: Oracle typed-PortentPayload full-loop (V5-1, ADR-030 amendment
// 2026-05-26).
//
// Сценарий:
//  1. harness поднимает Keeper + PG + Vault + Redis (testcontainers).
//  2. harness создаёт incarnation (web-app + service-noop), Vigil и Decree
//     через Operator-API (REST POST /v1/vigils, /v1/decrees) с typed-payload
//     where-CEL `event.file_changed.path.startsWith("/etc/")`.
//  3. Soul-stub подключается mTLS-EventStream-ом и шлёт PortentEvent с
//     typed file_changed + legacy data (dual-write hand-off-период).
//  4. ASSERT (прямыми PG-queries, real DB, real flows, no mocks):
//     - apply_runs(planned) под subject SID + scenario "noop";
//     - oracle_fires-cooldown-state для (decree, subject);
//     - audit_log: событие `oracle.fired`.
//
// ОТЛОЖЕНО (skip): harness в MVP не предоставляет хелперы CreateVigil /
// CreateDecree / SoulStub.SendPortent. Эти хелперы — отдельный slice
// расширения harness, который не входит в V5-1 (ТЗ V5-1 = typed payload в
// proto + Soul-side emit + Keeper-side CEL-access + L0/L1 покрытие).
//
// L1 cross-side integration уже покрывает full handler-pipeline (real PG +
// real Oracle CRUD + real where-CEL + real apply_runs):
//   - TestIntegration_OracleV5_TypedPayloadDualWriteFires
//   - TestIntegration_OracleV5_LegacyOnlyStillFires
//   - TestIntegration_OracleV5_TypedOnlyMatches
//   - TestIntegration_OracleV5_TypeMismatchNoFire
//
// Skeleton + fixtures здесь — заготовка под harness-расширение
// (RegisterVigilDecree / SoulStub / AssertOracleFired). Fixtures в
// tests/e2e/oracle_typed_portent/ описывают ожидаемые входы/выходы; их
// читает harness, когда хелперы появятся.
package e2e_test

import "testing"

// TestE2EOracleTypedPortent_FileChangedFireFlow — placeholder для V5-N
// harness-расширения. Тело раскомментировать, когда появятся
// harness.RegisterVigil / RegisterDecree / SoulStub / AssertOracleFired.
//
// Полное assertion-покрытие лежит в L1 (см. шапку файла).
func TestE2EOracleTypedPortent_FileChangedFireFlow(t *testing.T) {
	t.Skip("заменён рабочим execution-e2e TestOracle_FileChanged_FiresScenario (vigil_oracle_test.go): full-loop soul-stub.SendPortent → handlePortentEvent → match/where/cooldown/enqueue → apply_runs + audit oracle.fired. L1: keeper/internal/grpc/oracle_crosside_integration_test.go::TestIntegration_OracleV5_*.")
}
