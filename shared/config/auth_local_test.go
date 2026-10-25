package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestKeeperAuthLocal_ResolvedMinPasswordLength(t *testing.T) {
	var absent *KeeperAuthLocal
	if got := absent.ResolvedMinPasswordLength(); got != DefaultLocalMinPasswordLength {
		t.Fatalf("nil block: %d, want %d", got, DefaultLocalMinPasswordLength)
	}
	if got := (&KeeperAuthLocal{}).ResolvedMinPasswordLength(); got != DefaultLocalMinPasswordLength {
		t.Fatalf("local: {}: %d, want %d", got, DefaultLocalMinPasswordLength)
	}
	if got := (&KeeperAuthLocal{MinPasswordLength: 16}).ResolvedMinPasswordLength(); got != 16 {
		t.Fatalf("explicit 16: %d", got)
	}
}

// `local: {}` turns the login on; a minimum outside [8, 128] is refused at load.
func TestKeeperAuthLocal_Load(t *testing.T) {
	for _, c := range []struct {
		name    string
		block   string
		wantErr bool
	}{
		{"empty block", "  local: {}\n", false},
		{"explicit minimum", "  local:\n    min_password_length: 16\n", false},
		{"below the floor", "  local:\n    min_password_length: 6\n", true},
		{"above the maximum", "  local:\n    min_password_length: 129\n", true},
	} {
		cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired+"auth:\n"+c.block), ValidateOptions{})
		if err != nil {
			t.Fatalf("%s: load: %v", c.name, err)
		}
		gotErr := false
		for _, d := range diags {
			if d.Level == diag.LevelError && d.Code == "local_min_password_length_out_of_range" {
				gotErr = true
			}
		}
		if gotErr != c.wantErr {
			t.Errorf("%s: out-of-range error = %v, want %v; diags=%v", c.name, gotErr, c.wantErr, diags)
		}
		if !c.wantErr && (cfg.Auth == nil || cfg.Auth.Local == nil) {
			t.Errorf("%s: auth.local not parsed", c.name)
		}
	}
}
