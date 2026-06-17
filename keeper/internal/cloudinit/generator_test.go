package cloudinit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/cloudinit"
	"github.com/souls-guild/soul-stack/shared/config"

	"gopkg.in/yaml.v3"
)

const testCAPem = `-----BEGIN CERTIFICATE-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAcXamPlexamplePEMblock
ForUnitTestsOnlyNotARealCertificateThisIsJustForRenderCheckLength
PaddingPaddingPaddingPaddingPaddingPaddingPaddingPaddingPadding==
-----END CERTIFICATE-----`

func validConfig() cloudinit.Config {
	return cloudinit.Config{
		BootstrapEndpoint: "lb.keeper.example:9442",
		TLSCAPem:          testCAPem,
		SoulBinaryURL:     "https://artifacts.example/soul/v1.0.0/soul",
		SoulVersion:       "v1.0.0",
	}
}

func TestGenerateUserdata_HappyPath(t *testing.T) {
	out, err := cloudinit.GenerateUserdata(validConfig())
	if err != nil {
		t.Fatalf("GenerateUserdata: %v", err)
	}
	if !strings.HasPrefix(out, "#cloud-config") {
		t.Errorf("output does not start with #cloud-config header: %q", out[:min(80, len(out))])
	}
	for _, want := range []string{
		"/etc/soul/tls/keeper-ca.pem",
		"/etc/soul/soul.yml",
		"/etc/systemd/system/soul.service",
		"lb.keeper.example",
		"https://artifacts.example/soul/v1.0.0/soul",
		"--cacert /etc/soul/tls/keeper-ca.pem",
		"systemctl enable soul.service",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing substring %q", want)
		}
	}
}

func TestGenerateUserdata_EmbedsTLSCA(t *testing.T) {
	out, err := cloudinit.GenerateUserdata(validConfig())
	if err != nil {
		t.Fatalf("GenerateUserdata: %v", err)
	}
	// PEM-блок должен попасть в YAML под write_files c indentation.
	if !strings.Contains(out, "-----BEGIN CERTIFICATE-----") || !strings.Contains(out, "-----END CERTIFICATE-----") {
		t.Errorf("PEM block not present in output")
	}
	// Каждая строка PEM сдвинута indentation-prefix-ом (6 пробелов под `content: |`).
	for _, line := range strings.Split(testCAPem, "\n") {
		want := "      " + line
		if !strings.Contains(out, want) {
			t.Errorf("PEM line not indented as expected: %q not found", want)
		}
	}
}

func TestGenerateUserdata_NoSecrets(t *testing.T) {
	out, err := cloudinit.GenerateUserdata(validConfig())
	if err != nil {
		t.Fatalf("GenerateUserdata: %v", err)
	}
	for _, banned := range []string{
		"bootstrap_token",
		"vault:",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("userdata must not contain %q (B-flat invariant), full output:\n%s", banned, out)
		}
	}
}

func TestGenerateUserdata_ValidYAML(t *testing.T) {
	out, err := cloudinit.GenerateUserdata(validConfig())
	if err != nil {
		t.Fatalf("GenerateUserdata: %v", err)
	}
	// cloud-config — `#cloud-config` header + YAML mapping. yaml.Unmarshal
	// игнорирует comment-header, парсит остаток.
	var v map[string]any
	if err := yaml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("rendered userdata is not valid YAML: %v\noutput:\n%s", err, out)
	}
	if _, ok := v["write_files"]; !ok {
		t.Errorf("rendered YAML has no top-level write_files key")
	}
	if _, ok := v["runcmd"]; !ok {
		t.Errorf("rendered YAML has no top-level runcmd key")
	}
}

func TestGenerateUserdata_Validate_Errors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*cloudinit.Config)
		wantSub string
	}{
		{"empty endpoint", func(c *cloudinit.Config) { c.BootstrapEndpoint = "" }, "bootstrap_endpoint"},
		{"bad endpoint", func(c *cloudinit.Config) { c.BootstrapEndpoint = "no-port" }, "host:port"},
		{"bad CA", func(c *cloudinit.Config) { c.TLSCAPem = "garbage" }, "PEM"},
		{"empty URL", func(c *cloudinit.Config) { c.SoulBinaryURL = "" }, "soul_binary_url"},
		{"plain http URL", func(c *cloudinit.Config) { c.SoulBinaryURL = "http://insecure" }, "https"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			c.mutate(&cfg)
			_, err := cloudinit.GenerateUserdata(cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

// fakeVault — стаб VaultReader для unit-тестов Resolver.
type fakeVault struct {
	kv  map[string]any
	err error
}

func (f *fakeVault) ReadKV(_ context.Context, _ string) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.kv, nil
}

func TestResolver_HappyPath(t *testing.T) {
	r := cloudinit.NewResolver(&fakeVault{kv: map[string]any{"ca": testCAPem}})
	cfg, err := r.Resolve(context.Background(), &config.KeeperCloudInit{
		BootstrapEndpoint: "lb.keeper.example:9442",
		TLSCARef:          "vault:secret/keeper/ca",
		SoulBinaryURL:     "https://artifacts.example/soul",
		SoulVersion:       "v1.0.0",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.TLSCAPem != testCAPem {
		t.Errorf("CA not propagated: %q", cfg.TLSCAPem[:min(40, len(cfg.TLSCAPem))])
	}
	if cfg.BootstrapEndpoint != "lb.keeper.example:9442" {
		t.Errorf("BootstrapEndpoint=%q", cfg.BootstrapEndpoint)
	}
}

func TestResolver_NilBlock_Fails(t *testing.T) {
	r := cloudinit.NewResolver(&fakeVault{})
	_, err := r.Resolve(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected error 'missing', got %v", err)
	}
}

func TestResolver_VaultError_Masked(t *testing.T) {
	r := cloudinit.NewResolver(&fakeVault{err: errors.New("vault: secret/keeper/ca not found")})
	_, err := r.Resolve(context.Background(), &config.KeeperCloudInit{
		BootstrapEndpoint: "lb.example:9442",
		TLSCARef:          "vault:secret/keeper/ca",
		SoulBinaryURL:     "https://artifacts.example/soul",
	})
	if err == nil {
		t.Fatal("expected error from vault read")
	}
	// Не утекает внутренняя ошибка Vault (содержит путь к секрету).
	if strings.Contains(err.Error(), "secret/keeper/ca not found") {
		t.Errorf("vault internal error leaked: %v", err)
	}
}

func TestResolver_InvalidVaultRef(t *testing.T) {
	r := cloudinit.NewResolver(&fakeVault{kv: map[string]any{"ca": testCAPem}})
	_, err := r.Resolve(context.Background(), &config.KeeperCloudInit{
		BootstrapEndpoint: "lb.example:9442",
		TLSCARef:          "not-a-vault-ref",
		SoulBinaryURL:     "https://artifacts.example/soul",
	})
	if err == nil {
		t.Fatal("expected error on malformed vault-ref")
	}
}

func TestResolver_MissingCAField(t *testing.T) {
	r := cloudinit.NewResolver(&fakeVault{kv: map[string]any{"other": "value"}})
	_, err := r.Resolve(context.Background(), &config.KeeperCloudInit{
		BootstrapEndpoint: "lb.example:9442",
		TLSCARef:          "vault:secret/keeper/ca",
		SoulBinaryURL:     "https://artifacts.example/soul",
	})
	if err == nil || !strings.Contains(err.Error(), "no field") {
		t.Fatalf("expected 'no field' error, got %v", err)
	}
}

// Идемпотентность: тот же config → тот же байт-выход.
func TestGenerateUserdata_Deterministic(t *testing.T) {
	cfg := validConfig()
	out1, err := cloudinit.GenerateUserdata(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := cloudinit.GenerateUserdata(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out1 != out2 {
		t.Errorf("non-deterministic render")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
