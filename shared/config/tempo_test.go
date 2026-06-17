package config

import (
	"testing"
)

// TestTempo_EnabledFootgunGuard — ADR-050 default-ON (footgun-guard как
// Conductor/Toll): не задано (nil блок / nil поле) → ON; явный true → ON;
// явный false → OFF (opt-out).
func TestTempo_EnabledFootgunGuard(t *testing.T) {
	tru, fal := true, false
	cases := []struct {
		name  string
		tempo *KeeperTempo
		want  bool
	}{
		{"nil block → ON", nil, true},
		{"nil enabled field → ON", &KeeperTempo{}, true},
		{"explicit true → ON", &KeeperTempo{Enabled: &tru}, true},
		{"explicit false → OFF", &KeeperTempo{Enabled: &fal}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tempo.TempoEnabled(); got != tc.want {
				t.Errorf("TempoEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTempo_ResolvedVoyageCreate — nil-блок / нулевые / опущенные rate|burst →
// дефолты [DefaultTempoVoyageCreate*]; явно заданные → override. Резолв
// per-field: можно переопределить только rate, оставив burst дефолтным (и
// наоборот).
func TestTempo_ResolvedVoyageCreate(t *testing.T) {
	cases := []struct {
		name      string
		tempo     *KeeperTempo
		wantRate  float64
		wantBurst int
	}{
		{"nil block → defaults", nil, DefaultTempoVoyageCreateRate, DefaultTempoVoyageCreateBurst},
		{"zero fields → defaults", &KeeperTempo{}, DefaultTempoVoyageCreateRate, DefaultTempoVoyageCreateBurst},
		{
			"explicit override both",
			&KeeperTempo{VoyageCreate: KeeperTempoBucket{Rate: 5, Burst: 3}},
			5, 3,
		},
		{
			"only rate set → burst default",
			&KeeperTempo{VoyageCreate: KeeperTempoBucket{Rate: 5}},
			5, DefaultTempoVoyageCreateBurst,
		},
		{
			"only burst set → rate default",
			&KeeperTempo{VoyageCreate: KeeperTempoBucket{Burst: 3}},
			DefaultTempoVoyageCreateRate, 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, burst := tc.tempo.ResolvedVoyageCreate()
			if rate != tc.wantRate {
				t.Errorf("ResolvedVoyageCreate() rate = %v, want %v", rate, tc.wantRate)
			}
			if burst != tc.wantBurst {
				t.Errorf("ResolvedVoyageCreate() burst = %v, want %v", burst, tc.wantBurst)
			}
		})
	}
}

// TestTempo_ResolvedVoyagePreview — nil-блок / нулевые / опущенные rate|burst →
// дефолты [DefaultTempoVoyagePreview*] (30/60, мягче create); явно заданные →
// override. Резолв per-field, симметрично ResolvedVoyageCreate. Гарантирует
// инвариант «дефолт preview = 30/60» (ADR-050 amendment 2026-06-17).
func TestTempo_ResolvedVoyagePreview(t *testing.T) {
	cases := []struct {
		name      string
		tempo     *KeeperTempo
		wantRate  float64
		wantBurst int
	}{
		{"nil block → defaults 30/60", nil, DefaultTempoVoyagePreviewRate, DefaultTempoVoyagePreviewBurst},
		{"zero fields → defaults 30/60", &KeeperTempo{}, DefaultTempoVoyagePreviewRate, DefaultTempoVoyagePreviewBurst},
		{
			"explicit override both",
			&KeeperTempo{VoyagePreview: KeeperTempoBucket{Rate: 7, Burst: 9}},
			7, 9,
		},
		{
			"only rate set → burst default",
			&KeeperTempo{VoyagePreview: KeeperTempoBucket{Rate: 7}},
			7, DefaultTempoVoyagePreviewBurst,
		},
		{
			"only burst set → rate default",
			&KeeperTempo{VoyagePreview: KeeperTempoBucket{Burst: 9}},
			DefaultTempoVoyagePreviewRate, 9,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, burst := tc.tempo.ResolvedVoyagePreview()
			if rate != tc.wantRate {
				t.Errorf("ResolvedVoyagePreview() rate = %v, want %v", rate, tc.wantRate)
			}
			if burst != tc.wantBurst {
				t.Errorf("ResolvedVoyagePreview() burst = %v, want %v", burst, tc.wantBurst)
			}
		})
	}
}

// TestTempo_PreviewDefaultsAre30_60 — точечный guard числовых дефолтов
// preview-бакета (ADR-050 amendment 2026-06-17: rate=30, burst=60). Защищает от
// случайной правки констант, которая разъехалась бы с ADR.
func TestTempo_PreviewDefaultsAre30_60(t *testing.T) {
	if DefaultTempoVoyagePreviewRate != 30.0 {
		t.Errorf("DefaultTempoVoyagePreviewRate = %v, want 30", DefaultTempoVoyagePreviewRate)
	}
	if DefaultTempoVoyagePreviewBurst != 60 {
		t.Errorf("DefaultTempoVoyagePreviewBurst = %d, want 60", DefaultTempoVoyagePreviewBurst)
	}
	rate, burst := (*KeeperTempo)(nil).ResolvedVoyagePreview()
	if rate != 30.0 || burst != 60 {
		t.Errorf("nil-Tempo ResolvedVoyagePreview() = (%v, %v), want (30, 60)", rate, burst)
	}
}

// TestTempo_PreviewSofterThanCreate — preview-лимит мягче create-лимита (ADR-050
// amendment: preview read-like, но resolver-heavy → шире, не безлимит). Guard на
// «дефолты не перепутаны местами / preview не строже create».
func TestTempo_PreviewSofterThanCreate(t *testing.T) {
	if DefaultTempoVoyagePreviewRate <= DefaultTempoVoyageCreateRate {
		t.Errorf("preview rate (%v) должен быть строго мягче create rate (%v)",
			DefaultTempoVoyagePreviewRate, DefaultTempoVoyageCreateRate)
	}
	if DefaultTempoVoyagePreviewBurst <= DefaultTempoVoyageCreateBurst {
		t.Errorf("preview burst (%d) должен быть строго мягче create burst (%d)",
			DefaultTempoVoyagePreviewBurst, DefaultTempoVoyageCreateBurst)
	}
}

// TestLoadKeeper_Tempo_PreviewIndependentOfCreate — config-уровень: voyage_preview
// и voyage_create резолвятся НЕЗАВИСИМО. Задан только preview-override — create
// остаётся на своём дефолте, и наоборот. Гарантирует, что блоки не делят значения.
func TestLoadKeeper_Tempo_PreviewIndependentOfCreate(t *testing.T) {
	src := keeperBaseRequired + `tempo:
  voyage_preview:
    rate: 33
    burst: 66
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatal("валидный preview-override не должен давать value_out_of_range")
	}
	if cfg.Tempo == nil {
		t.Fatal("tempo не распарсился")
	}
	pr, pb := cfg.Tempo.ResolvedVoyagePreview()
	if pr != 33 || pb != 66 {
		t.Errorf("ResolvedVoyagePreview() = (%v, %v), want (33, 66)", pr, pb)
	}
	// create НЕ затронут override-ом preview — остаётся на дефолте.
	cr, cb := cfg.Tempo.ResolvedVoyageCreate()
	if cr != DefaultTempoVoyageCreateRate || cb != DefaultTempoVoyageCreateBurst {
		t.Errorf("ResolvedVoyageCreate() = (%v, %v) — override preview не должен трогать create-дефолты (%v, %v)",
			cr, cb, DefaultTempoVoyageCreateRate, DefaultTempoVoyageCreateBurst)
	}
}

// TestLoadKeeper_Tempo_PreviewNegativeRate — отрицательный preview.rate
// отвергается schema-фазой (value_out_of_range на $.tempo.voyage_preview.rate).
func TestLoadKeeper_Tempo_PreviewNegativeRate(t *testing.T) {
	src := keeperBaseRequired + `tempo:
  voyage_preview:
    rate: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.tempo.voyage_preview.rate") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range на $.tempo.voyage_preview.rate (rate < 0)")
	}
}

// TestLoadKeeper_Tempo_PreviewNegativeBurst — отрицательный preview.burst
// отвергается schema-фазой (value_out_of_range на $.tempo.voyage_preview.burst).
func TestLoadKeeper_Tempo_PreviewNegativeBurst(t *testing.T) {
	src := keeperBaseRequired + `tempo:
  voyage_preview:
    burst: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.tempo.voyage_preview.burst") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range на $.tempo.voyage_preview.burst (burst < 0)")
	}
}

// TestLoadKeeper_Tempo_NegativeRate — явно заданный отрицательный rate
// отвергается schema-фазой (value_out_of_range на $.tempo.voyage_create.rate).
func TestLoadKeeper_Tempo_NegativeRate(t *testing.T) {
	src := keeperBaseRequired + `tempo:
  voyage_create:
    rate: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.tempo.voyage_create.rate") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range на $.tempo.voyage_create.rate (rate < 0)")
	}
}

// TestLoadKeeper_Tempo_NegativeBurst — явно заданный отрицательный burst
// отвергается schema-фазой (value_out_of_range на $.tempo.voyage_create.burst).
func TestLoadKeeper_Tempo_NegativeBurst(t *testing.T) {
	src := keeperBaseRequired + `tempo:
  voyage_create:
    burst: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.tempo.voyage_create.burst") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range на $.tempo.voyage_create.burst (burst < 0)")
	}
}

// TestLoadKeeper_Tempo_ZeroResolvesToDefault — явный 0 в rate/burst НЕ ошибка:
// валидатор режет только < 0, а 0 резолвится к дефолту в ResolvedVoyageCreate.
func TestLoadKeeper_Tempo_ZeroResolvesToDefault(t *testing.T) {
	src := keeperBaseRequired + `tempo:
  voyage_create:
    rate: 0
    burst: 0
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatal("rate:0/burst:0 не должны давать value_out_of_range (резолвятся к дефолту)")
	}
	if cfg.Tempo == nil {
		t.Fatal("tempo не распарсился")
	}
	rate, burst := cfg.Tempo.ResolvedVoyageCreate()
	if rate != DefaultTempoVoyageCreateRate || burst != DefaultTempoVoyageCreateBurst {
		t.Errorf("ResolvedVoyageCreate() = (%v, %v), want (%v, %v) — 0 резолвится к дефолту",
			rate, burst, DefaultTempoVoyageCreateRate, DefaultTempoVoyageCreateBurst)
	}
}

// TestLoadKeeper_Tempo_ValidBlock — валидный блок (rate=10, burst=20) парсится и
// не даёт error-диагностик; резолв возвращает заданные значения.
func TestLoadKeeper_Tempo_ValidBlock(t *testing.T) {
	src := keeperBaseRequired + `tempo:
  enabled: true
  voyage_create:
    rate: 10
    burst: 20
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatal("валидный tempo-блок не должен давать value_out_of_range")
	}
	if cfg.Tempo == nil {
		t.Fatal("tempo не распарсился")
	}
	if !cfg.Tempo.TempoEnabled() {
		t.Error("enabled: true должно дать ON")
	}
	rate, burst := cfg.Tempo.ResolvedVoyageCreate()
	if rate != 10 || burst != 20 {
		t.Errorf("ResolvedVoyageCreate() = (%v, %v), want (10, 20)", rate, burst)
	}
}
