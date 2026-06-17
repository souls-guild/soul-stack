package soulprint

import "testing"

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantFamily string
		wantDistro string
		wantVer    string
		wantCode   string
	}{
		{
			name:       "ubuntu",
			in:         "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"22.04\"\nVERSION_CODENAME=jammy\n",
			wantFamily: "debian", wantDistro: "ubuntu", wantVer: "22.04", wantCode: "jammy",
		},
		{
			name:       "rocky-quoted-idlike",
			in:         "ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"9.3\"\n",
			wantFamily: "rhel", wantDistro: "rocky", wantVer: "9.3", wantCode: "",
		},
		{
			name:       "alpine",
			in:         "ID=alpine\nVERSION_ID=3.19\n",
			wantFamily: "alpine", wantDistro: "alpine", wantVer: "3.19", wantCode: "",
		},
		{
			name:       "debian-derivative-via-idlike",
			in:         "ID=raspbian\nID_LIKE=debian\nVERSION_ID=12\n",
			wantFamily: "debian", wantDistro: "raspbian", wantVer: "12", wantCode: "",
		},
		{
			name:       "unknown",
			in:         "ID=plan9\nVERSION_ID=4\n",
			wantFamily: "", wantDistro: "plan9", wantVer: "4", wantCode: "",
		},
		{
			name:       "garbage-lines-ignored",
			in:         "# comment\n\nNAME without eq\nID=alpine\n",
			wantFamily: "alpine", wantDistro: "alpine",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := parseOSRelease(tc.in)
			if got := r.family(); got != tc.wantFamily {
				t.Errorf("family=%q want %q", got, tc.wantFamily)
			}
			if r.id != tc.wantDistro {
				t.Errorf("distro=%q want %q", r.id, tc.wantDistro)
			}
			if r.versionID != tc.wantVer {
				t.Errorf("version=%q want %q", r.versionID, tc.wantVer)
			}
			if r.versionCodename != tc.wantCode {
				t.Errorf("codename=%q want %q", r.versionCodename, tc.wantCode)
			}
		})
	}
}

func TestPkgMgrInitSystem(t *testing.T) {
	cases := []struct {
		family, distro    string
		wantPkg, wantInit string
	}{
		{"debian", "ubuntu", "apt", "systemd"},
		{"debian", "debian", "apt", "systemd"},
		{"rhel", "rocky", "dnf", "systemd"},
		{"rhel", "fedora", "dnf", "systemd"},
		{"alpine", "alpine", "apk", "openrc"},
		{"darwin", "macos", "brew", "launchd"},
		{"arch", "arch", "pacman", "systemd"},
		// family-fallback: неизвестный distro внутри известного семейства.
		{"debian", "raspbian", "apt", "systemd"},
		{"rhel", "oraclelinux", "dnf", "systemd"},
		// полностью нераспознанное → пустые значения (Keeper толерантен).
		{"", "", "", ""},
		{"plan9", "plan9", "", ""},
	}
	for _, tc := range cases {
		pkg, ini := pkgMgrInitSystem(tc.family, tc.distro)
		if pkg != tc.wantPkg || ini != tc.wantInit {
			t.Errorf("pkgMgrInitSystem(%q,%q)=%q/%q want %q/%q",
				tc.family, tc.distro, pkg, ini, tc.wantPkg, tc.wantInit)
		}
	}
}

func TestUnquote(t *testing.T) {
	cases := map[string]string{
		`"22.04"`: "22.04",
		`'jammy'`: "jammy",
		`bare`:    "bare",
		`"`:       `"`,
		``:        ``,
		`"mixed'`: `"mixed'`,
	}
	for in, want := range cases {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q)=%q want %q", in, got, want)
		}
	}
}
