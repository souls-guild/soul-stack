package config

import (
	"testing"
)

// TestTempo_EnabledFootgunGuard — ADR-050 default-ON (footgun-guard like
// Conductor/Toll): unset (nil block / nil field) → ON; explicit true → ON;
// explicit false → OFF (opt-out).
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

// TestTempo_ResolvedVoyageCreate — nil block / zero / omitted rate|burst →
// defaults [DefaultTempoVoyageCreate*]; explicit → override. Per-field resolve:
// only rate can be overridden while burst stays default (and vice versa).
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

// TestTempo_ResolvedVoyagePreview — nil block / zero / omitted rate|burst →
// defaults [DefaultTempoVoyagePreview*] (30/60, softer than create); explicit →
// override. Per-field resolve, symmetric with ResolvedVoyageCreate. Guards the
// "preview default = 30/60" invariant (ADR-050 amendment 2026-06-17).
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

// TestTempo_PreviewDefaultsAre30_60 — targeted guard on the preview bucket's
// numeric defaults (ADR-050 amendment 2026-06-17: rate=30, burst=60). Protects
// against accidental constant edits that would drift from the ADR.
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

// TestTempo_PreviewSofterThanCreate — preview limit softer than create (ADR-050
// amendment: preview is read-like but resolver-heavy → wider, not unlimited).
// Guards "defaults not swapped / preview no stricter than create".
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

// TestLoadKeeper_Tempo_PreviewIndependentOfCreate — config level: voyage_preview
// and voyage_create resolve INDEPENDENTLY. Only preview overridden → create stays
// at its default, and vice versa. Guarantees the blocks don't share values.
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
	// create is untouched by the preview override — stays at default.
	cr, cb := cfg.Tempo.ResolvedVoyageCreate()
	if cr != DefaultTempoVoyageCreateRate || cb != DefaultTempoVoyageCreateBurst {
		t.Errorf("ResolvedVoyageCreate() = (%v, %v) — override preview не должен трогать create-дефолты (%v, %v)",
			cr, cb, DefaultTempoVoyageCreateRate, DefaultTempoVoyageCreateBurst)
	}
}

// TestLoadKeeper_Tempo_PreviewNegativeRate — negative preview.rate is rejected
// by the schema phase (value_out_of_range at $.tempo.voyage_preview.rate).
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

// TestLoadKeeper_Tempo_PreviewNegativeBurst — negative preview.burst is rejected
// by the schema phase (value_out_of_range at $.tempo.voyage_preview.burst).
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

// TestLoadKeeper_Tempo_NegativeRate — explicit negative rate is rejected by the
// schema phase (value_out_of_range at $.tempo.voyage_create.rate).
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

// TestLoadKeeper_Tempo_NegativeBurst — explicit negative burst is rejected by the
// schema phase (value_out_of_range at $.tempo.voyage_create.burst).
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

// TestLoadKeeper_Tempo_ZeroResolvesToDefault — explicit 0 in rate/burst is NOT an
// error: the validator only rejects < 0, and 0 resolves to the default in
// ResolvedVoyageCreate.
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

// TestLoadKeeper_Tempo_ValidBlock — a valid block (rate=10, burst=20) parses and
// yields no error diagnostics; resolve returns the given values.
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
