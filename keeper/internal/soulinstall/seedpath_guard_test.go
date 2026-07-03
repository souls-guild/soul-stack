package soulinstall_test

// Sync-guard layout-а SoulSeed: SeedCertPath (guard идемпотентности `soul init`
// в core.bootstrap.delivered) обязан совпадать с тем, куда soul-агент реально
// пишет seed — `<paths.seed из soul.yml>/<currentLink>/<CertFile>` из
// soul/internal/seed. Прямой import невозможен (internal чужого Go-модуля),
// поэтому константы извлекаются из исходника.

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

	// paths.seed из генерённого soul.yml — то, что soul init получит в конфиге.
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
		t.Fatalf("SeedCertPath = %q, want %q (layout soul/internal/seed или paths.seed soul.yml разъехались)", soulinstall.SeedCertPath, want)
	}
}

func extractStringConst(t *testing.T, src []byte, name string) string {
	t.Helper()
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("константа %s не найдена в soul/internal/seed/seed.go", name)
	}
	return string(m[1])
}
