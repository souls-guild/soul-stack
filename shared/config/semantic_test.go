package config

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestParseDuration фиксирует convention `duration` из docs/keeper/config.md
// и docs/naming-rules.md: Go-`time.ParseDuration` + суффикс `<N>d` без
// композитной формы.
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
		{in: "1d2h", wantErr: true}, // композитная форма не обещана — explicit fail
		{in: "abc", wantErr: true},
		{in: "-1d", wantErr: true}, // знак перед `<N>d` отвергается (review.2)
		{in: "+5d", wantErr: true},
		{in: "-5d", wantErr: true},
		{in: "d", wantErr: true},
		// Overflow guard (qa.1 Bug 2): любое `<N>d` свыше MaxDurationDays
		// отвергается; равное границе принимается. `time.Duration(N) * 24h`
		// без guard-а оборачивался в мусор/отрицательное значение.
		// MaxDurationDays = MaxInt64 / int64(24h) ≈ 106 751 (≈292 года);
		// фигурирующая в делегации цифра 106 751 991 содержала лишний
		// фактор 10^3, правильный порядок — 10^5.
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

// keeperBaseRequired — минимальный набор обязательных блоков `keeper.yml`,
// без которых semantic-фаза не доходит до проверок: missing_required_field
// перекрывает картину. Тесты ниже добавляют к этой базе свой проверяемый блок.
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

// TestKeeper_RBACKeyRejected — после hard-cut-а config-RBAC (ADR-028(g))
// ключ `rbac:` в keeper.yml недопустим: каталог ролей живёт в Postgres,
// управление — через `role.*` API/MCP. Парсер обязан отвергнуть его через
// `unknown_key` (поля `rbac` в KeeperConfig нет → reflect-walker в walk.go).
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

// TestKeeper_NoRBACLoadsClean — keeper.yml без блока `rbac:` грузится без
// ошибок (RBAC больше не часть конфиг-контракта).
func TestKeeper_NoRBACLoadsClean(t *testing.T) {
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for keeper.yml without rbac:; got %d diags", len(diags))
	}
}
