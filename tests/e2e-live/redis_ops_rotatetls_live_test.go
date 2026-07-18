//go:build e2e_live

// L3b E2E day-2: examples/service/redis::rotate_tls on a LIVE TLS-Redis - a LIVE
// proof of finding #4 (rotate_tls CA-rollover hot-swap). create brings up
// redis in connection_mode=tls (server-only-TLS, cert1/ca1 from the default essence
// Vault paths), rotate_tls moves the instance to an INDEPENDENT new CA (cert2/ca2):
//
//	create(tls) -> server serves cert1 (fp1) -> rotate_tls(cert2/key2/ca2 = NEW CA)
//	-> apply success -> server HOT-serves (without restart) cert2 (fp2).
//
// * Without the bundle fix (compute.tls_ca = BUNDLE of old+new CA on CA-rollover,
// rotate_tls/main.yml), the three CONFIG SET tls-*-file reconnects after swapping the server
// cert would hit "certificate signed by unknown authority" -> rotate-apply FAIL.
// A green test <=> the fix works: a single trust store trusts BOTH CAs during the
// straddle-reconnects.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_Day2RotateTls(t *testing.T) {
	repoURL := harness.BuildCommunityRedisPlugin(t)

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       1,
		SoulModules: []harness.SoulModuleEntry{
			{Name: "redis", Source: repoURL, Ref: harness.CommunityRedisPluginRef},
		},
	})
	defer stack.Cleanup()

	const (
		incName   = "redis"
		adminUser = "default_admin"
		adminPass = "e2e-default-admin-secret"
	)

	// Two INDEPENDENT TLS materials (CA1/CA2) - CA-rollover: rotate moves the instance to a
	// new CA not signed by the old one. The server cert carries IP-SAN 127.0.0.1 (the plugin +
	// health-probe connect via go-tls on 127.0.0.1:7379).
	ca1, cert1, key1, fp1 := harness.GenerateRedisTLSMaterial(t)
	ca2, cert2, key2, fp2 := harness.GenerateRedisTLSMaterial(t)

	// Vault seed (rel WITHOUT mount/data prefix, secret/data/ is added internally):
	//   - services/redis/tls    <- default essence tls#{cert,key,ca} branch for create;
	//   - services/redis/tls-v2 <- NEW material for rotate input;
	//   - redis/<inc>           <- incarnation main password;
	//   - redis/<inc>/users/default_admin <- admin password (rotate's CONFIG SET connects
	//     as it over TLS; same as create's deploy bodies).
	harness.SeedVaultKV(t, stack, "services/redis/tls", map[string]any{"cert": cert1, "key": key1, "ca": ca1})
	harness.SeedVaultKV(t, stack, "services/redis/tls-v2", map[string]any{"cert": cert2, "key": key2, "ca": ca2})
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "e2e-redis-main"})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+adminUser, map[string]any{"password": adminPass})

	stack.AddMember(t, 0, incName)
	stack.WaitSoulprintReported(t, 0, 60)

	stack.MaterializeDestinies(t, "v1.0.0", "redis", "node-exporter", "redis-exporter", "vector")
	stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)

	// Create TLS instance: connection_mode=tls (TLS-only, plain port closed). cert/key/ca
	// come from the default essence Vault paths (secret/services/redis/tls#{cert,key,ca}) -
	// TLS-create requires no essence override. The rest - same as adduser (standalone-equivalent).
	inc, createApply := stack.CreateIncarnationWithApplyScenario(t, incName, "redis@main", "create", map[string]any{
		"provision":           map[string]any{"enabled": false},
		"redis_type":          "sentinel",
		"version":             "7.4.1",
		"connection_mode":     "tls",
		"persistence":         "rdb",
		"replicas_per_master": 0,
		"memory_mb":           1024,
		"maxmemory_policy":    "volatile-lru",
	})
	stack.WaitApplySuccess(t, createApply, 600)
	stack.WaitIncarnationReady(t, inc, 300)

	// TLS connection as default_admin (server-only-TLS; CACertPath empty -> defaults to
	// /etc/redis/tls/ca.crt inside the container).
	tlsConn := harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 7379, User: adminUser, Pass: adminPass, TLS: true}

	// (a) BEFORE rotation the server serves cert1.
	stack.AssertRedisTLSCertServed(t, tlsConn, fp1)

	// (b) rotate_tls to the NEW material (CA-rollover): three CONFIG SET tls-*-file under
	// bundle(ca1,ca2) flip the live server to cert2 WITHOUT restart. * verifies the bundle
	// fix: without it, the post-swap reconnect would hit "unknown authority" -> apply FAIL.
	rot := stack.RunScenario(t, inc, "rotate_tls", map[string]any{
		"cert_ref": "secret/services/redis/tls-v2#cert",
		"key_ref":  "secret/services/redis/tls-v2#key",
		"ca_ref":   "secret/services/redis/tls-v2#ca",
	})
	stack.WaitApplySuccess(t, rot, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// (c) AFTER rotation the server serves cert2 - hot-swap + CA-rollover proven live.
	stack.AssertRedisTLSCertServed(t, tlsConn, fp2)

	// (d) read-model: state.tls recorded the NEW material (rotate_tls state_changes -
	// enable/only/port preserved, cert/key/ca_ref = new ones from input).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"tls": map[string]any{
			"enable":   true,
			"only":     true,
			"cert_ref": "secret/services/redis/tls-v2#cert",
			"key_ref":  "secret/services/redis/tls-v2#key",
			"ca_ref":   "secret/services/redis/tls-v2#ca",
		},
	})
}
