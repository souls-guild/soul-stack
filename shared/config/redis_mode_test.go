package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// keeperWithRedis assembles a minimally valid keeper.yml, injecting the given
// redis block (already indented 2 spaces per line). All other top-level blocks
// are fixed and valid — only redis varies.
func keeperWithRedis(redisBlock string) []byte {
	return []byte(`kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
` + redisBlock + `
vault:
  addr: "https://v:8200"
  token: "root"
  pki_mount: pki/x
`)
}

// --- standalone (default / explicit) ---

func TestRedis_Standalone_Implicit_OK(t *testing.T) {
	// Without mode at all — forward-compat: treated as standalone, addr required.
	src := keeperWithRedis(`  addr: "r:6379"
  password_ref: vault:secret/keeper/redis`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("standalone без mode (addr задан) должен быть валиден")
	}
	if cfg.Redis.Mode != "" {
		t.Errorf("Mode = %q, want empty (implicit standalone)", cfg.Redis.Mode)
	}
	if cfg.Redis.Addr != "r:6379" {
		t.Errorf("Addr = %q", cfg.Redis.Addr)
	}
}

func TestRedis_Standalone_Explicit_OK(t *testing.T) {
	src := keeperWithRedis(`  mode: standalone
  addr: "r:6379"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("mode=standalone + addr должен быть валиден")
	}
}

func TestRedis_Standalone_MissingAddr_Rejected(t *testing.T) {
	src := keeperWithRedis(`  mode: standalone`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.redis.addr") {
		dump(t, diags)
		t.Fatalf("ожидался missing_required_field на redis.addr (standalone без addr)")
	}
}

func TestRedis_ImplicitStandalone_MissingAddr_Rejected(t *testing.T) {
	// mode omitted, addr too — must complain (old "addr required" semantics).
	src := keeperWithRedis(`  password_ref: vault:secret/keeper/redis`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.redis.addr") {
		dump(t, diags)
		t.Fatalf("ожидался missing_required_field на redis.addr (implicit standalone без addr)")
	}
}

// --- sentinel ---

func TestRedis_Sentinel_OK(t *testing.T) {
	src := keeperWithRedis(`  mode: sentinel
  master_name: mymaster
  sentinels:
    - "s1:26379"
    - "s2:26379"
  password_ref: vault:secret/keeper/redis
  sentinel_password_ref: vault:secret/keeper/redis-sentinel`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("sentinel + master_name + sentinels должен быть валиден")
	}
	if cfg.Redis.Mode != "sentinel" || cfg.Redis.MasterName != "mymaster" {
		t.Errorf("Mode/MasterName = %q/%q", cfg.Redis.Mode, cfg.Redis.MasterName)
	}
	if len(cfg.Redis.Sentinels) != 2 {
		t.Errorf("Sentinels = %v", cfg.Redis.Sentinels)
	}
}

func TestRedis_Sentinel_MissingMasterName_Rejected(t *testing.T) {
	src := keeperWithRedis(`  mode: sentinel
  sentinels:
    - "s1:26379"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.redis.master_name") {
		dump(t, diags)
		t.Fatalf("ожидался missing_required_field на redis.master_name (sentinel без него)")
	}
}

func TestRedis_Sentinel_MissingSentinels_Rejected(t *testing.T) {
	src := keeperWithRedis(`  mode: sentinel
  master_name: mymaster`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.redis.sentinels") {
		dump(t, diags)
		t.Fatalf("ожидался missing_required_field на redis.sentinels (sentinel без них)")
	}
}

func TestRedis_Sentinel_BadSentinelAddr_Rejected(t *testing.T) {
	src := keeperWithRedis(`  mode: sentinel
  master_name: mymaster
  sentinels:
    - "no-port"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "host_port_invalid") {
		dump(t, diags)
		t.Fatalf("ожидался host_port_invalid на sentinel-адрес без порта")
	}
}

func TestRedis_Sentinel_EmptySentinelEntry_Rejected(t *testing.T) {
	// An empty entry in a non-empty list (`["", "s2:26379"]`) is not "omitted"
	// but an error: it must be caught as host_port_invalid, not silently skipped.
	src := keeperWithRedis(`  mode: sentinel
  master_name: mymaster
  sentinels:
    - ""
    - "s2:26379"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "host_port_invalid", "$.redis.sentinels[0]") {
		dump(t, diags)
		t.Fatalf("ожидался host_port_invalid на пустой sentinel-элемент")
	}
}

func TestRedis_Sentinel_UnusedNodes_Warn(t *testing.T) {
	src := keeperWithRedis(`  mode: sentinel
  master_name: mymaster
  sentinels:
    - "s1:26379"
  nodes:
    - "n1:6379"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("лишние nodes при sentinel — warn, не error")
	}
	if !hasCode(diags, "redis_unused_field") {
		dump(t, diags)
		t.Fatalf("ожидался warning redis_unused_field на nodes при sentinel")
	}
}

// --- cluster ---

func TestRedis_Cluster_OK(t *testing.T) {
	src := keeperWithRedis(`  mode: cluster
  nodes:
    - "n1:6379"
    - "n2:6379"
    - "n3:6379"
  password_ref: vault:secret/keeper/redis`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("cluster + nodes должен быть валиден")
	}
	if cfg.Redis.Mode != "cluster" || len(cfg.Redis.Nodes) != 3 {
		t.Errorf("Mode/Nodes = %q/%v", cfg.Redis.Mode, cfg.Redis.Nodes)
	}
}

func TestRedis_Cluster_MissingNodes_Rejected(t *testing.T) {
	src := keeperWithRedis(`  mode: cluster`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.redis.nodes") {
		dump(t, diags)
		t.Fatalf("ожидался missing_required_field на redis.nodes (cluster без них)")
	}
}

func TestRedis_Cluster_BadNodeAddr_Rejected(t *testing.T) {
	src := keeperWithRedis(`  mode: cluster
  nodes:
    - "n1:6379"
    - "broken"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "host_port_invalid") {
		dump(t, diags)
		t.Fatalf("ожидался host_port_invalid на cluster-узел без порта")
	}
}

func TestRedis_Cluster_UnusedSentinelFields_Warn(t *testing.T) {
	src := keeperWithRedis(`  mode: cluster
  nodes:
    - "n1:6379"
  master_name: mymaster
  sentinels:
    - "s1:26379"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("лишние master_name/sentinels при cluster — warn, не error")
	}
	if !hasCode(diags, "redis_unused_field") {
		dump(t, diags)
		t.Fatalf("ожидался warning redis_unused_field на sentinel-поля при cluster")
	}
}

// --- enum / ref ---

func TestRedis_InvalidMode_Rejected(t *testing.T) {
	src := keeperWithRedis(`  mode: galaxy
  addr: "r:6379"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "enum_invalid", "$.redis.mode") {
		dump(t, diags)
		t.Fatalf("ожидался enum_invalid на redis.mode=galaxy")
	}
}

func TestRedis_PasswordRef_Plaintext_Rejected(t *testing.T) {
	// password_ref must be a vault ref (semantic phase).
	src := keeperWithRedis(`  addr: "r:6379"
  password_ref: plaintextpw`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "vault_ref_invalid_format", "$.redis.password_ref") {
		dump(t, diags)
		t.Fatalf("ожидался vault_ref_invalid_format на plaintext password_ref")
	}
}

func TestRedis_SentinelPasswordRef_FieldOverride_OK(t *testing.T) {
	// A vault ref with `#field` must be accepted by the semantic phase (reVaultRef).
	src := keeperWithRedis(`  mode: sentinel
  master_name: mymaster
  sentinels:
    - "s1:26379"
  sentinel_password_ref: vault:secret/keeper/redis#sentinel_pw`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("sentinel_password_ref с #field должен быть валиден")
	}
}
