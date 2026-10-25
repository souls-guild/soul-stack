package config

import (
	"strings"
	"testing"
)

const ttlMaxKeeperYAML = `
kid: keeper-eu-west-01
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
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
__JWT__
`

// The config check refuses what `keeper run` refuses: an explicit ttl_max below
// ttl_default (NIM-1045). An unset ttl_max follows ttl_default up and is never
// below it.
func TestLoadKeeper_TTLMaxNotBelowDefault(t *testing.T) {
	cases := []struct {
		name string
		jwt  string
		want bool
	}{
		{"explicit below the default", "    ttl_default: 24h\n    ttl_max: 1h", true},
		{"explicit below the implicit 24h default", "    ttl_max: 1h", true},
		{"explicit above", "    ttl_default: 24h\n    ttl_max: 90d", false},
		{"unset with a long default", "    ttl_default: 120d", false},
	}
	for _, c := range cases {
		tmp := writeTemp(t, "keeper.yml", strings.Replace(ttlMaxKeeperYAML, "__JWT__", c.jwt, 1))
		_, _, diags, err := LoadKeeper(tmp, ValidateOptions{})
		if err != nil {
			t.Fatalf("%s: io error: %v", c.name, err)
		}
		if got := hasCodeAt(diags, "ttl_max_below_ttl_default", "$.auth.jwt.ttl_max"); got != c.want {
			dump(t, diags)
			t.Errorf("%s: ttl_max_below_ttl_default = %v, want %v", c.name, got, c.want)
		}
	}
}
