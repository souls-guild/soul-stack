package config

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestParseDuration pins the `duration` convention from docs/keeper/config.md
// and docs/naming-rules.md: Go `time.ParseDuration` + a `<N>d` suffix without a
// composite form.
func TestParseDuration(t *testing.T) {
	type tc struct {
		in      string
		want    time.Duration
		wantErr bool
	}
	cases := []tc{
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "0d", want: 0},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "500ms", want: 500 * time.Millisecond},
		{in: "1d2h", wantErr: true}, // composite form is not promised — explicit fail
		{in: "abc", wantErr: true},
		{in: "-1d", wantErr: true}, // a sign before `<N>d` is rejected (review.2)
		{in: "+5d", wantErr: true},
		{in: "-5d", wantErr: true},
		{in: "d", wantErr: true},
		// Overflow guard (qa.1 Bug 2): any `<N>d` above MaxDurationDays is
		// rejected; equal to the boundary is accepted. `time.Duration(N) * 24h`
		// without the guard wrapped around into garbage/negative values.
		// MaxDurationDays = MaxInt64 / int64(24h) ≈ 106 751 (≈292 years); the
		// figure 106 751 991 quoted in the delegation had an extra factor of
		// 10^3, the correct order of magnitude is 10^5.
		{in: "999999999999d", wantErr: true},
		{in: "200000000d", wantErr: true},
		{in: "106751d", want: time.Duration(106751) * 24 * time.Hour},
		{in: "106752d", wantErr: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseDuration(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ParseDuration(%q): got %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// keeperBaseRequired is the minimal set of required `keeper.yml` blocks, without
// which the semantic phase never reaches the checks: missing_required_field
// masks the picture. The tests below add their own block under test to this base.
const keeperBaseRequired = `kid: keeper-eu-west-01
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
`

// TestKeeper_RBACKeyRejected — after the hard cut of config-RBAC (ADR-028(g)) the
// `rbac:` key in keeper.yml is not allowed: the role catalog lives in Postgres,
// managed via the `role.*` API/MCP. The parser must reject it via `unknown_key`
// (there is no `rbac` field in KeeperConfig → reflect-walker in walk.go).
func TestKeeper_RBACKeyRejected(t *testing.T) {
	src := keeperBaseRequired + `rbac:
  default_policy: deny
  roles:
    - name: cluster-admin
      operators: ["archon-alice"]
      permissions: ["*"]
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key for rbac: block (config-RBAC removed, ADR-028(g))")
	}
}

// TestKeeper_NoRBACLoadsClean — keeper.yml without an `rbac:` block loads without
// errors (RBAC is no longer part of the config contract).
func TestKeeper_NoRBACLoadsClean(t *testing.T) {
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for keeper.yml without rbac:; got %d diags", len(diags))
	}
}
