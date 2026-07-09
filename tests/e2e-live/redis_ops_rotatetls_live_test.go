//go:build e2e_live

// L3b E2E операция: examples/service/redis::rotate_tls на ЖИВОМ TLS-Redis — ЖИВОЕ
// доказательство находки #4 (rotate_tls CA-rollover hot-swap). create поднимает
// redis в connection_mode=tls (server-only-TLS, cert1/ca1 из дефолтных essence-
// Vault-путей), rotate_tls уводит инстанс на НЕЗАВИСИМЫЙ новый CA (cert2/ca2):
//
//	create(tls) → сервер отдаёт cert1 (fp1) → rotate_tls(cert2/key2/ca2 = НОВЫЙ CA)
//	→ apply success → сервер ГОРЯЧО (без рестарта) отдаёт cert2 (fp2).
//
// ★ Без bundle-фикса (compute.tls_ca = BUNDLE старого+нового CA при CA-rollover,
// rotate_tls/main.yml) три CONFIG SET tls-*-file реконнекта после подмены серверного
// cert упёрлись бы в «certificate signed by unknown authority» → rotate-apply FAIL.
// Зелёный тест ⟺ фикс работает: единый trust-store доверяет ОБОИМ CA на время
// straddle-реконнектов.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_OpsRotateTls(t *testing.T) {
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

	// Два НЕЗАВИСИМЫХ TLS-материала (CA1/CA2) — CA-rollover: rotate уводит инстанс на
	// новый CA, не подписанный старым. Серверный cert несёт IP-SAN 127.0.0.1 (плагин +
	// health-probe коннектятся go-tls на 127.0.0.1:7379).
	ca1, cert1, key1, fp1 := harness.GenerateRedisTLSMaterial(t)
	ca2, cert2, key2, fp2 := harness.GenerateRedisTLSMaterial(t)

	// Vault-seed (rel БЕЗ mount/data-префикса, secret/data/ добавляется внутри):
	//   - services/redis/tls    ← дефолтная essence-ветка tls#{cert,key,ca} для create;
	//   - services/redis/tls-v2 ← НОВЫЙ материал для rotate input;
	//   - redis/<inc>           ← главный пароль инкарнации;
	//   - redis/<inc>/users/default_admin ← пароль admin (rotate CONFIG SET коннектится
	//     под ним по TLS; те же деплой-тела create).
	harness.SeedVaultKV(t, stack, "services/redis/tls", map[string]any{"cert": cert1, "key": key1, "ca": ca1})
	harness.SeedVaultKV(t, stack, "services/redis/tls-v2", map[string]any{"cert": cert2, "key": key2, "ca": ca2})
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "e2e-redis-main"})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+adminUser, map[string]any{"password": adminPass})

	stack.AddSoulToCoven(t, 0, incName)
	stack.WaitSoulprintReported(t, 0, 60)

	stack.MaterializeDestinies(t, "v1.0.0", "redis", "node-exporter", "redis-exporter", "vector")
	stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)

	// Create TLS-инстанс: connection_mode=tls (только-TLS, plain-порт закрыт). cert/key/ca
	// берутся из дефолтных essence Vault-путей (secret/services/redis/tls#{cert,key,ca}) —
	// TLS-create essence-override не требует. Остальное — как adduser (standalone-эквивалент).
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

	// TLS-коннект под default_admin (server-only-TLS; CACertPath пусто → дефолт
	// /etc/redis/tls/ca.crt внутри контейнера).
	tlsConn := harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 7379, User: adminUser, Pass: adminPass, TLS: true}

	// (а) ДО ротации сервер отдаёт cert1.
	stack.AssertRedisTLSCertServed(t, tlsConn, fp1)

	// (б) rotate_tls на НОВЫЙ материал (CA-rollover): три CONFIG SET tls-*-file под
	// bundle(ca1,ca2) перекидывают live-сервер на cert2 БЕЗ рестарта. ★ проверка bundle-
	// фикса: без него post-swap реконнект упал бы «unknown authority» → apply FAIL.
	rot := stack.RunScenario(t, inc, "rotate_tls", map[string]any{
		"cert_ref": "secret/services/redis/tls-v2#cert",
		"key_ref":  "secret/services/redis/tls-v2#key",
		"ca_ref":   "secret/services/redis/tls-v2#ca",
	})
	stack.WaitApplySuccess(t, rot, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// (в) ПОСЛЕ ротации сервер отдаёт cert2 — hot-swap + CA-rollover доказаны живьём.
	stack.AssertRedisTLSCertServed(t, tlsConn, fp2)

	// (г) read-model: state.tls зафиксировал НОВЫЙ материал (rotate_tls state_changes —
	// enable/only/port сохранены, cert/key/ca_ref = новые из input).
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
