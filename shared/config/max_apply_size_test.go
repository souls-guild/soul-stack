package config

import "testing"

// Лимит размера ApplyRequest (контракт Keeper↔Soul по EventStream, ADR-012):
// Keeper-send (`listen.grpc.event_stream.max_apply_size_mb`) и Soul-recv
// (`keeper.max_apply_size_mb`). Оба — int в МиБ, дефолт 8, валидация ≥ 1 MiB.

// keeperBaseNoEventStreamApply — keeperBaseRequired с многострочным
// event_stream-блоком, чтобы тест мог дописать `max_apply_size_mb` под него.
const keeperBaseNoEventStreamApply = `kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream:
      addr: "0.0.0.0:8443"
      tls: { cert: /c, key: /k, ca: /a }
`

const keeperBaseTailApply = `  openapi: { addr: "0.0.0.0:8080" }
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

func TestKeeperMaxApplySize_DefaultWhenOmitted(t *testing.T) {
	src := keeperBaseNoEventStreamApply + keeperBaseTailApply
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("omitted max_apply_size_mb must not trigger value_out_of_range")
	}
	if cfg.Listen.GRPC.EventStream.MaxApplySizeMB != 0 {
		t.Fatalf("omitted field must decode as 0, got %d", cfg.Listen.GRPC.EventStream.MaxApplySizeMB)
	}
	want := DefaultMaxApplySizeMB * 1024 * 1024
	if got := cfg.Listen.GRPC.EventStream.ResolvedMaxApplySize(); got != want {
		t.Fatalf("ResolvedMaxApplySize default: want %d bytes (8 MiB), got %d", want, got)
	}
}

func TestKeeperMaxApplySize_ParsedAndResolved(t *testing.T) {
	src := keeperBaseNoEventStreamApply + "      max_apply_size_mb: 16\n" + keeperBaseTailApply
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("16 MiB must not trigger value_out_of_range")
	}
	if cfg.Listen.GRPC.EventStream.MaxApplySizeMB != 16 {
		t.Fatalf("want parsed 16, got %d", cfg.Listen.GRPC.EventStream.MaxApplySizeMB)
	}
	if got := cfg.Listen.GRPC.EventStream.ResolvedMaxApplySize(); got != 16*1024*1024 {
		t.Fatalf("ResolvedMaxApplySize: want %d bytes, got %d", 16*1024*1024, got)
	}
}

func TestKeeperMaxApplySize_BelowMin(t *testing.T) {
	src := keeperBaseNoEventStreamApply + "      max_apply_size_mb: 0\n" + keeperBaseTailApply
	// 0 трактуется как «не задан» — НЕ ошибка, резолвится в дефолт.
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCodeAt(diags, "value_out_of_range", "$.listen.grpc.event_stream.max_apply_size_mb") {
		dump(t, diags)
		t.Fatalf("explicit 0 means default, must not be value_out_of_range")
	}

	srcNeg := keeperBaseNoEventStreamApply + "      max_apply_size_mb: -1\n" + keeperBaseTailApply
	_, _, diagsNeg, _ := LoadKeeperFromBytes("keeper.yml", []byte(srcNeg), ValidateOptions{})
	if !hasCodeAt(diagsNeg, "value_out_of_range", "$.listen.grpc.event_stream.max_apply_size_mb") {
		dump(t, diagsNeg)
		t.Fatalf("negative max_apply_size_mb must trigger value_out_of_range")
	}
}

func TestSoulMaxApplySize_DefaultWhenOmitted(t *testing.T) {
	src := soulRangeBase
	cfg, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("omitted max_apply_size_mb must not trigger value_out_of_range")
	}
	if cfg.Keeper.MaxApplySizeMB != 0 {
		t.Fatalf("omitted field must decode as 0, got %d", cfg.Keeper.MaxApplySizeMB)
	}
	want := DefaultMaxApplySizeMB * 1024 * 1024
	if got := cfg.Keeper.ResolvedMaxApplySize(); got != want {
		t.Fatalf("ResolvedMaxApplySize default: want %d bytes (8 MiB), got %d", want, got)
	}
}

func TestSoulMaxApplySize_ParsedAndResolved(t *testing.T) {
	src := soulRangeBase + "  max_apply_size_mb: 32\n"
	cfg, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("32 MiB must not trigger value_out_of_range")
	}
	if cfg.Keeper.MaxApplySizeMB != 32 {
		t.Fatalf("want parsed 32, got %d", cfg.Keeper.MaxApplySizeMB)
	}
	if got := cfg.Keeper.ResolvedMaxApplySize(); got != 32*1024*1024 {
		t.Fatalf("ResolvedMaxApplySize: want %d bytes, got %d", 32*1024*1024, got)
	}
}

func TestSoulMaxApplySize_Negative(t *testing.T) {
	src := soulRangeBase + "  max_apply_size_mb: -4\n"
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.keeper.max_apply_size_mb") {
		dump(t, diags)
		t.Fatalf("negative keeper.max_apply_size_mb must trigger value_out_of_range")
	}
}
