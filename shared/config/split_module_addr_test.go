package config

import "testing"

func TestSplitModuleAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantName  string
		wantState string
		wantOK    bool
	}{
		{"core.pkg.installed", "core.pkg", "installed", true},
		{"core.file.absent", "core.file", "absent", true},
		{"core.cloud.created", "core.cloud", "created", true},
		{"core.vault.kv-read", "core.vault", "kv-read", true},
		{"vendor.foo.bar.baz", "vendor.foo.bar", "baz", true},
		{"core", "core", "", true},
		{"", "", "", false},
		{".installed", "", "", false},
		{"core.", "", "", false},
	}
	for _, c := range cases {
		name, state, ok := SplitModuleAddr(c.in)
		if name != c.wantName || state != c.wantState || ok != c.wantOK {
			t.Errorf("SplitModuleAddr(%q) = (%q,%q,%v); want (%q,%q,%v)",
				c.in, name, state, ok, c.wantName, c.wantState, c.wantOK)
		}
	}
}
