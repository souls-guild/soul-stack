package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestLoadServiceManifest_Certificate — the `certificate:` section (NIM-99, nested
// by NIM-745): the Vault PKI role the service's certs are issued with, plus an
// optional `rotate:` block carrying the opt-in auto-rotation policy. Format/duration
// are checked structurally (independent of enable); scenario and pki_role are
// required only when rotate.enable:true (inert opt-in).
func TestLoadServiceManifest_Certificate(t *testing.T) {
	const base = "state_schema: {}\n"

	cases := []struct {
		name     string
		section  string // YAML of the section (empty = no section at all)
		wantCode string // expected code; empty = expect 0 errors
		wantAt   string // expected YAMLPath; empty = path not checked
	}{
		{
			name: "rotate enabled with scenario+pki_role is valid",
			section: `certificate:
  pki_role: redis-server
  rotate:
    enable: true
    scenario: rotate_tls
`,
		},
		{
			// NIM-745, the point of the nesting: the PKI role is what a cert is ISSUED
			// with, and issuance is not only rotation. Under `certificate_rotation:` this
			// was inexpressible — pki_role lived inside the rotation block.
			name: "pki_role alone, no rotate block, is valid",
			section: `certificate:
  pki_role: redis-server
`,
		},
		{
			name: "rotate enabled without pki_role",
			section: `certificate:
  rotate:
    enable: true
    scenario: rotate_tls
`,
			wantCode: "missing_required_field",
			wantAt:   "$.certificate.pki_role",
		},
		{
			name: "rotate enabled without scenario",
			section: `certificate:
  pki_role: redis-server
  rotate:
    enable: true
`,
			wantCode: "missing_required_field",
			wantAt:   "$.certificate.rotate.scenario",
		},
		{
			name: "bad threshold",
			section: `certificate:
  pki_role: redis-server
  rotate:
    enable: true
    scenario: rotate_tls
    threshold: 30x
`,
			wantCode: "duration_invalid",
			wantAt:   "$.certificate.rotate.threshold",
		},
		{
			name: "good threshold 30d is valid",
			section: `certificate:
  pki_role: redis-server
  rotate:
    enable: true
    scenario: rotate_tls
    threshold: 30d
`,
		},
		{
			name: "scenario not snake/kebab",
			section: `certificate:
  pki_role: redis-server
  rotate:
    enable: true
    scenario: Bad_Name
`,
			wantCode: "name_invalid_format",
			wantAt:   "$.certificate.rotate.scenario",
		},
		{
			// enable:false → required fields not needed, the block is inert (opt-in).
			name: "disabled rotate requires nothing",
			section: `certificate:
  rotate:
    enable: false
`,
		},
		{
			// enable omitted = false by zero-value → also inert.
			name: "enable omitted is inert",
			section: `certificate:
  rotate:
    threshold: 30d
`,
		},
		{
			name:    "no certificate section at all",
			section: "",
		},
		{
			name: "empty certificate section is valid",
			section: `certificate: {}
`,
		},
		{
			name: "unknown key inside the section",
			section: `certificate:
  pki_role: redis-server
  bogus: 1
`,
			wantCode: "unknown_key",
			wantAt:   "$.certificate.bogus",
		},
		{
			name: "unknown key inside rotate",
			section: `certificate:
  pki_role: redis-server
  rotate:
    enable: true
    scenario: rotate_tls
    bogus: 1
`,
			wantCode: "unknown_key",
			wantAt:   "$.certificate.rotate.bogus",
		},
		{
			// The old flat key is REFUSED, not read: nothing would have consumed it, and a
			// manifest whose rotation policy the engine silently ignores is the worse of
			// the two failures. Three manifests carried it and all three were ours, so
			// there is no transition window (NIM-745).
			name: "retired certificate_rotation key is refused",
			section: `certificate_rotation:
  enable: true
  scenario: rotate_tls
  pki_role: redis-server
`,
			wantCode: "unknown_key",
			wantAt:   "$.certificate_rotation",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(base+tc.section), ValidateOptions{})
			if tc.wantCode == "" {
				if diag.HasErrors(diags) {
					dump(t, diags)
					t.Fatalf("expected 0 errors")
				}
				return
			}
			ok := hasCode(diags, tc.wantCode)
			if tc.wantAt != "" {
				ok = hasCodeAt(diags, tc.wantCode, tc.wantAt)
			}
			if !ok {
				dump(t, diags)
				t.Fatalf("expected %s @ %s", tc.wantCode, tc.wantAt)
			}
		})
	}
}

// TestLoadServiceManifest_CertificateRotationKeyHint — GUARD (NIM-745): the retired
// `certificate_rotation:` key is refused WITH a hint naming the new form, not
// silently. A bare `unknown_key` would leave the author guessing which of the four
// sub-keys moved where; the whole reason the key is in deprecatedServiceKeys rather
// than merely absent from the struct is that it can say so.
func TestLoadServiceManifest_CertificateRotationKeyHint(t *testing.T) {
	src := "state_schema: {}\n" + `certificate_rotation:
  enable: true
  scenario: rotate_tls
  pki_role: redis-server
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})

	var hint string
	var found int
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.certificate_rotation" {
			found++
			hint = d.Hint
		}
	}
	if found == 0 {
		dump(t, diags)
		t.Fatal("expected unknown_key @ $.certificate_rotation")
	}
	// The reflect walker suppresses its own hint-less duplicate for a deprecated key
	// (walk.go serviceManifestType) — one diagnostic, not two at the same line/col.
	if found != 1 {
		dump(t, diags)
		t.Errorf("expected exactly 1 diagnostic for the retired key, got %d", found)
	}
	if hint == "" {
		t.Fatal("retired certificate_rotation key must carry a hint, not be refused silently")
	}
	for _, want := range []string{"certificate:", "rotate:", "pki_role"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint does not point at the new form (missing %q): %s", want, hint)
		}
	}
}

// TestLoadServiceManifest_CertificateDecode — the section's yaml tags decode into a
// typed block: pki_role → PKIRole on the section itself, the rest under Rotate.
func TestLoadServiceManifest_CertificateDecode(t *testing.T) {
	src := "state_schema: {}\n" + `certificate:
  pki_role: redis-server
  rotate:
    enable: true
    scenario: rotate_tls
    threshold: 30d
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("unexpected errors")
	}
	crt := cfg.Certificate
	if crt == nil {
		t.Fatal("Certificate nil, expected a parsed block")
	}
	if crt.PKIRole != "redis-server" {
		t.Errorf("PKIRole = %q, want redis-server", crt.PKIRole)
	}
	if crt.Rotate == nil {
		t.Fatal("Certificate.Rotate nil, expected a parsed block")
	}
	if !crt.Rotate.Enable || crt.Rotate.Scenario != "rotate_tls" || crt.Rotate.Threshold != "30d" {
		t.Errorf("decode mismatch: %#v", crt.Rotate)
	}
}

// TestLoadServiceManifest_CertificateDecodeRoleOnly — a section with no `rotate:`
// decodes to a role and a nil Rotate, which is what every rotation gate reads as
// "off" (NIM-745).
func TestLoadServiceManifest_CertificateDecodeRoleOnly(t *testing.T) {
	src := "state_schema: {}\n" + `certificate:
  pki_role: redis-server
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("unexpected errors")
	}
	if cfg.Certificate == nil {
		t.Fatal("Certificate nil, expected a parsed block")
	}
	if cfg.Certificate.PKIRole != "redis-server" {
		t.Errorf("PKIRole = %q, want redis-server", cfg.Certificate.PKIRole)
	}
	if cfg.Certificate.Rotate != nil {
		t.Errorf("Rotate = %#v, want nil (no rotate: block)", cfg.Certificate.Rotate)
	}
}

// TestLoadServiceManifest_CertificateExamples — golden examples of redis and
// dragonfly with the `certificate:` section load without errors.
func TestLoadServiceManifest_CertificateExamples(t *testing.T) {
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
				t.Fatalf("expected 0 errors on golden %s", svc)
			}
			if cfg.Certificate == nil {
				t.Fatalf("%s: certificate section not parsed", svc)
			}
			if cfg.Certificate.PKIRole == "" {
				t.Errorf("%s: certificate.pki_role empty", svc)
			}
			if cfg.Certificate.Rotate == nil {
				t.Fatalf("%s: certificate.rotate block not parsed", svc)
			}
			if !cfg.Certificate.Rotate.Enable || cfg.Certificate.Rotate.Scenario != "rotate_tls" {
				t.Errorf("%s: certificate.rotate = %#v", svc, cfg.Certificate.Rotate)
			}
		})
	}
}
