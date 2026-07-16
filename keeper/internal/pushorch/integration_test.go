//go:build integration

package pushorch

import "testing"

// TestIntegration_S6_PushDispatcherWireUp is an end-to-end pilot (S6,
// 2026-05-26): bring up the full daemon with testcontainers PG+Redis+Vault,
// mock SshProvider plugin (in-process gRPC fake returning
// SignReply{certificate, ca_public_key}), put fake host-CA-PEM into Vault, pass
// `push.targets[]` + `push.providers[]` + `push.host_ca_ref` through keeper.yml,
// insert SID into souls + presence into Redis, POST /v1/push/apply with an
// inventory of 1 SID -> 202 -> executeAsync -> status=succeeded.
//
// STATUS: skeleton. Requires:
//  1. testcontainers-Vault helper with KV write (fakeCAPublicKey in Vault KV,
//     `secret/test/host-ca`, field `public_key`);
//  2. in-process gRPC fake SshProvider plugin (mock-ssh-binary, exec-friendly:
//     stdout handshake + sock-listen + Sign/Authorize-implementations);
//  3. mock-soul-binary (or http-mock-applier) on the target host, returning a
//     valid NDJSON RunResult.
//
// A full implementation is outside the S6 pilot-slice scope (requires new
// shared test infra). Implementation is a separate slice on request.
func TestIntegration_S6_PushDispatcherWireUp(t *testing.T) {
	t.Skip("S6 pilot integration test skeleton - requires mock-ssh-plugin + " +
		"testcontainers-vault-with-kv helpers (separate slice). " +
		"Unit-coverage: keeper/internal/push (target_config, host_ca_vault) + " +
		"keeper/cmd/keeper (push_dispatchers_test).")
}
