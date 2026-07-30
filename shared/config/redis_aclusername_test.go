package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Guard for NIM-398, config side: `username` / `sentinel_username` must be
// accepted keys and must land in the struct.
//
// Two failure modes this pins down. A missing or misspelled yaml tag parses
// silently to the empty string, and empty username is exactly the pre-ACL
// behavior — so the config would look fine and keeper would still fail to
// authenticate with -WRONGPASS. And if the schema ever grows a key allowlist
// for the redis block, adding it without these two would reject a valid config.
func TestRedis_ACLUsername_Parsed(t *testing.T) {
	src := keeperWithRedis(`  mode: sentinel
  master_name: master
  username: keeper
  sentinel_username: sentinel-keeper
  sentinels:
    - "s1:26379"
  password_ref: vault:secret/keeper/redis
  sentinel_password_ref: vault:secret/keeper/redis`)

	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("redis.username / redis.sentinel_username should be valid keys")
	}
	if cfg.Redis.Username != "keeper" {
		t.Errorf("Redis.Username = %q, want %q", cfg.Redis.Username, "keeper")
	}
	if cfg.Redis.SentinelUsername != "sentinel-keeper" {
		t.Errorf("Redis.SentinelUsername = %q, want %q", cfg.Redis.SentinelUsername, "sentinel-keeper")
	}
}

// Omitting both keys stays valid and yields empty strings — old configs against
// a non-ACL Redis must keep working untouched.
func TestRedis_ACLUsername_OptionalAndBackwardCompatible(t *testing.T) {
	src := keeperWithRedis(`  addr: "redis:6379"
  password_ref: vault:secret/keeper/redis`)

	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("redis without username keys should stay valid")
	}
	if cfg.Redis.Username != "" || cfg.Redis.SentinelUsername != "" {
		t.Errorf("expected empty usernames, got %q / %q", cfg.Redis.Username, cfg.Redis.SentinelUsername)
	}
}
