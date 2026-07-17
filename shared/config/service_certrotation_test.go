package config

import (
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestLoadServiceManifest_CertificateRotation — секция certificate_rotation
// (NIM-99): opt-in политика авто-ротации TLS-сертов и её валидация. Формат/
// duration проверяются структурно (независимо от enable); обязательность
// scenario/pki_role — только при enable:true (инертный opt-in).
func TestLoadServiceManifest_CertificateRotation(t *testing.T) {
	const base = "name: svc-golden\nstate_schema_version: 1\nstate_schema:\n  type: object\n"

	cases := []struct {
		name     string
		section  string // YAML секции (пусто = секции нет вовсе)
		wantCode string // ожидаемый code; пусто = ожидаем 0 ошибок
		wantAt   string // ожидаемый YAMLPath; пусто = путь не проверяем
	}{
		{
			name: "enable with scenario+pki_role is valid",
			section: `certificate_rotation:
  enable: true
  scenario: rotate_tls
  pki_role: redis-server
`,
		},
		{
			name: "enable without pki_role",
			section: `certificate_rotation:
  enable: true
  scenario: rotate_tls
`,
			wantCode: "missing_required_field",
			wantAt:   "$.certificate_rotation.pki_role",
		},
		{
			name: "enable without scenario",
			section: `certificate_rotation:
  enable: true
  pki_role: redis-server
`,
			wantCode: "missing_required_field",
			wantAt:   "$.certificate_rotation.scenario",
		},
		{
			name: "bad threshold",
			section: `certificate_rotation:
  enable: true
  scenario: rotate_tls
  pki_role: redis-server
  threshold: 30x
`,
			wantCode: "duration_invalid",
			wantAt:   "$.certificate_rotation.threshold",
		},
		{
			name: "good threshold 30d is valid",
			section: `certificate_rotation:
  enable: true
  scenario: rotate_tls
  pki_role: redis-server
  threshold: 30d
`,
		},
		{
			name: "scenario not snake/kebab",
			section: `certificate_rotation:
  enable: true
  scenario: Bad_Name
  pki_role: redis-server
`,
			wantCode: "name_invalid_format",
			wantAt:   "$.certificate_rotation.scenario",
		},
		{
			// enable:false → required-поля не нужны, секция инертна (opt-in).
			name: "disabled section requires nothing",
			section: `certificate_rotation:
  enable: false
`,
		},
		{
			// enable опущен = false по zero-value → тоже инертна.
			name: "enable omitted is inert",
			section: `certificate_rotation:
  threshold: 30d
`,
		},
		{
			name:    "no certificate_rotation section at all",
			section: "",
		},
		{
			name: "unknown key inside section",
			section: `certificate_rotation:
  enable: true
  scenario: rotate_tls
  pki_role: redis-server
  bogus: 1
`,
			wantCode: "unknown_key",
			wantAt:   "$.certificate_rotation.bogus",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(base+tc.section), ValidateOptions{})
			if tc.wantCode == "" {
				if diag.HasErrors(diags) {
					dump(t, diags)
					t.Fatalf("ожидалось 0 ошибок")
				}
				return
			}
			ok := hasCode(diags, tc.wantCode)
			if tc.wantAt != "" {
				ok = hasCodeAt(diags, tc.wantCode, tc.wantAt)
			}
			if !ok {
				dump(t, diags)
				t.Fatalf("ожидался %s @ %s", tc.wantCode, tc.wantAt)
			}
		})
	}
}

// TestLoadServiceManifest_CertificateRotationDecode — yaml-теги секции декодятся
// в типизированный блок (в т.ч. pki_role → PKIRole).
func TestLoadServiceManifest_CertificateRotationDecode(t *testing.T) {
	src := "name: svc-golden\nstate_schema_version: 1\nstate_schema:\n  type: object\n" + `certificate_rotation:
  enable: true
  scenario: rotate_tls
  threshold: 30d
  pki_role: redis-server
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("неожиданные ошибки")
	}
	crt := cfg.CertificateRotation
	if crt == nil {
		t.Fatal("CertificateRotation nil, ожидался разобранный блок")
	}
	if !crt.Enable || crt.Scenario != "rotate_tls" || crt.Threshold != "30d" || crt.PKIRole != "redis-server" {
		t.Errorf("decode mismatch: %#v", crt)
	}
}

// TestLoadServiceManifest_CertificateRotationExamples — golden-примеры redis и
// dragonfly с секцией certificate_rotation загружаются без ошибок.
func TestLoadServiceManifest_CertificateRotationExamples(t *testing.T) {
	for _, svc := range []string{"redis", "dragonfly"} {
		svc := svc
		t.Run(svc, func(t *testing.T) {
			path := filepath.FromSlash("../../examples/service/" + svc + "/service.yml")
			cfg, _, diags, err := LoadServiceManifest(path, ValidateOptions{})
			if err != nil {
				t.Fatalf("io error: %v", err)
			}
			if diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("ожидалось 0 ошибок на golden %s", svc)
			}
			if cfg.CertificateRotation == nil {
				t.Fatalf("%s: секция certificate_rotation не разобрана", svc)
			}
			if !cfg.CertificateRotation.Enable || cfg.CertificateRotation.Scenario != "rotate_tls" {
				t.Errorf("%s: certificate_rotation = %#v", svc, cfg.CertificateRotation)
			}
		})
	}
}
