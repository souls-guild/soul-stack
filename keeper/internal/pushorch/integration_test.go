//go:build integration

package pushorch

import "testing"

// TestIntegration_S6_PushDispatcherWireUp — end-to-end pilot (S6, 2026-05-26):
// поднять полный daemon с testcontainers PG+Redis+Vault, mock SshProvider-плагин
// (in-process gRPC fake возвращающий SignReply{certificate, ca_public_key}),
// положить fake host-CA-PEM в Vault, прокинуть `push.targets[]` + `push.providers[]` +
// `push.host_ca_ref` в keeper.yml, вставить SID в souls + presence в Redis,
// POST /v1/push/apply с inventory из 1 SID → 202 → executeAsync → status=succeeded.
//
// СТАТУС: skeleton. Требует:
//  1. testcontainers-Vault helper с записью KV (fakeCAPublicKey в Vault KV
//     `secret/test/host-ca` поле `public_key`);
//  2. in-process gRPC fake SshProvider-плагин (mock-ssh-binary, exec-friendly:
//     stdout handshake + sock-listen + Sign/Authorize-implementations);
//  3. mock-soul-binary (или http-mock-applier) на target-хосте,
//     возвращающий валидный NDJSON RunResult.
//
// Полноценная реализация выходит за scope S6 pilot-slice (требует new shared
// test-infra). Импл — отдельный slice по запросу.
func TestIntegration_S6_PushDispatcherWireUp(t *testing.T) {
	t.Skip("S6 pilot integration test skeleton — требует mock-ssh-plugin + " +
		"testcontainers-vault-with-kv helper-ов (отдельный slice). " +
		"Unit-coverage: keeper/internal/push (target_config, host_ca_vault) + " +
		"keeper/cmd/keeper (push_dispatchers_test).")
}
