package soulinstall_test

// Sync guard for SoulSeed layout: SeedCertPath (idempotency guard for `soul init`
// in core.bootstrap.delivered) must match where soul agent actually writes seed:
// `<paths.seed from soul.yml>/<currentLink>/<CertFile>` from soul/internal/seed.
// Direct import is impossible (internal of another Go module), so constants are
// extracted from source.

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"

	"gopkg.in/yaml.v3"
)

func TestSeedCertPath_SyncWithSoulSeedLayout(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "soul", "internal", "seed", "seed.go"))
	if err != nil {
		t.Fatalf("read soul/internal/seed/seed.go: %v", err)
	}
	certFile := extractStringConst(t, src, "CertFile")
	current := extractStringConst(t, src, "currentLink")

	// paths.seed from generated soul.yml is what soul init receives in config.
	var cfg struct {
		Paths struct {
			Seed string `yaml:"seed"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal([]byte(soulinstall.SoulConfigYAML("h.example", 9443, 9442)), &cfg); err != nil {
		t.Fatalf("unmarshal generated soul.yml: %v", err)
	}
	if cfg.Paths.Seed == "" {
		t.Fatal("generated soul.yml has empty paths.seed")
	}

	want := path.Join(cfg.Paths.Seed, current, certFile)
	if soulinstall.SeedCertPath != want {
		t.Fatalf("SeedCertPath = %q, want %q (layout soul/internal/seed or paths.seed soul.yml drifted)", soulinstall.SeedCertPath, want)
	}
}

func extractStringConst(t *testing.T, src []byte, name string) string {
	t.Helper()
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("constant %s not found in soul/internal/seed/seed.go", name)
	}
	return string(m[1])
}
