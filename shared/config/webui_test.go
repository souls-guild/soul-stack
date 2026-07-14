package config

import "testing"

// TestWebUIMounted_FootgunGuard — ADR-055 default-ON: unset (nil config /
// nil field) → true (UI out of the box); explicit true → true; explicit false → false
// (opt-out). Parallels TempoEnabled/Toll, but without an infrastructure dependency.
func TestWebUIMounted_FootgunGuard(t *testing.T) {
	tru, fal := true, false
	cases := []struct {
		name string
		cfg  *KeeperConfig
		want bool
	}{
		{"nil config → ON", nil, true},
		{"nil field → ON", &KeeperConfig{}, true},
		{"explicit true → ON", &KeeperConfig{WebUIEnabled: &tru}, true},
		{"explicit false → OFF", &KeeperConfig{WebUIEnabled: &fal}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.WebUIMounted(); got != tc.want {
				t.Errorf("WebUIMounted() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWebUIEnabled_StrictWalkerKnowsKey — the strict unknown-key walker (walk.go)
// knows `web_ui_enabled` (a field in KeeperConfig). A regression (missing field /
// yaml-tag typo) would yield unknown_key, as with reactor:/rbac:. Also checks that
// an explicit false parses and resolves to WebUIMounted()=false.
func TestWebUIEnabled_StrictWalkerKnowsKey(t *testing.T) {
	src := []byte(`kid: keeper-eu-west-01
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
web_ui_enabled: false
`)
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatal("web_ui_enabled породил unknown_key — strict-walker не знает поле")
	}
	if cfg == nil || cfg.WebUIEnabled == nil || *cfg.WebUIEnabled {
		t.Fatalf("web_ui_enabled: false не распарсился в *bool=false; cfg=%+v", cfg)
	}
	if cfg.WebUIMounted() {
		t.Error("WebUIMounted() = true при явном web_ui_enabled: false")
	}
}
