package util_test

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

func TestDetectPkgMgr_PrefersAptOverDnf(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("command -v dnf", util.Result{ExitCode: 0})
	if got := util.DetectPkgMgr(context.Background(), r); got != util.PkgMgrApt {
		t.Fatalf("DetectPkgMgr=%q want apt", got)
	}
}

func TestDetectPkgMgr_FallsBackToWhich(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	// command -v fails for all; which apk = 0.
	r.On("which apk", util.Result{ExitCode: 0})
	if got := util.DetectPkgMgr(context.Background(), r); got != util.PkgMgrApk {
		t.Fatalf("DetectPkgMgr=%q want apk", got)
	}
}

func TestDetectPkgMgr_Unknown(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	if got := util.DetectPkgMgr(context.Background(), r); got != util.PkgMgrUnknown {
		t.Fatalf("DetectPkgMgr=%q want unknown", got)
	}
}

func TestDetectInitSystem(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("systemctl --version", util.Result{ExitCode: 0})
	if got := util.DetectInitSystem(context.Background(), r); got != util.InitSystemSystemd {
		t.Fatalf("DetectInitSystem=%q want systemd", got)
	}

	r2 := internaltest.NewRunner()
	r2.Fallback = util.Result{ExitCode: 1}
	r2.On("rc-service --version", util.Result{ExitCode: 0})
	if got := util.DetectInitSystem(context.Background(), r2); got != util.InitSystemOpenRC {
		t.Fatalf("DetectInitSystem=%q want openrc", got)
	}

	// Only `service --version` available → SysV.
	r3 := internaltest.NewRunner()
	r3.Fallback = util.Result{ExitCode: 1}
	r3.On("service --version", util.Result{ExitCode: 0})
	if got := util.DetectInitSystem(context.Background(), r3); got != util.InitSystemSysV {
		t.Fatalf("DetectInitSystem=%q want sysv", got)
	}

	// Nothing available → Unknown.
	r4 := internaltest.NewRunner()
	r4.Fallback = util.Result{ExitCode: 127}
	if got := util.DetectInitSystem(context.Background(), r4); got != util.InitSystemUnknown {
		t.Fatalf("DetectInitSystem=%q want unknown", got)
	}
}

// TestResolvePkgMgr_FactPrimary — with a non-empty soulprint fact, resolve
// returns the fact and does NOT call runtime detection (Runner fails
// everything). This is BUG-B: single source of truth with CEL, no dependency
// on a binary being present in PATH.
func TestResolvePkgMgr_FactPrimary(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127} // any `command -v`/`which` fails
	got := util.ResolvePkgMgr(context.Background(), r, util.PkgMgrApk)
	if got != util.PkgMgrApk {
		t.Fatalf("ResolvePkgMgr=%q want apk (из факта)", got)
	}
	if len(r.Calls) != 0 {
		t.Fatalf("факт primary: runtime-детект не должен вызываться, calls=%v", r.Calls)
	}
}

// TestResolvePkgMgr_FallbackOnEmpty — empty/unknown fact → runtime detection.
func TestResolvePkgMgr_FallbackOnEmpty(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	got := util.ResolvePkgMgr(context.Background(), r, util.PkgMgrUnknown)
	if got != util.PkgMgrApk {
		t.Fatalf("ResolvePkgMgr=%q want apk (fallback-детект)", got)
	}
}

// TestResolveInitSystem_FactPrimary — mirrors the init-system case: fact
// takes priority, detection isn't called. This is exactly the alpine BUG-B
// scenario (soulprint=openrc, but `rc-service --version` is missing —
// detection would have failed).
func TestResolveInitSystem_FactPrimary(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127}
	got := util.ResolveInitSystem(context.Background(), r, util.InitSystemOpenRC)
	if got != util.InitSystemOpenRC {
		t.Fatalf("ResolveInitSystem=%q want openrc (из факта)", got)
	}
	if len(r.Calls) != 0 {
		t.Fatalf("факт primary: runtime-детект не должен вызываться, calls=%v", r.Calls)
	}
}

// TestResolveInitSystem_FallbackOnEmpty — empty fact → runtime detection.
func TestResolveInitSystem_FallbackOnEmpty(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("systemctl --version", util.Result{ExitCode: 0})
	got := util.ResolveInitSystem(context.Background(), r, util.InitSystemUnknown)
	if got != util.InitSystemSystemd {
		t.Fatalf("ResolveInitSystem=%q want systemd (fallback-детект)", got)
	}
}

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want util.OSFamily
	}{
		{"ubuntu", "ID=ubuntu\nID_LIKE=debian\nVERSION=22.04\n", util.OSFamilyDebian},
		{"rocky-quoted", "ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n", util.OSFamilyRedHat},
		{"alpine", "ID=alpine\nVERSION=3.19\n", util.OSFamilyAlpine},
		{"unknown", "ID=plan9\n", util.OSFamilyUnknown},
		{"empty", "", util.OSFamilyUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := util.ParseOSRelease(tc.in); got != tc.want {
				t.Fatalf("ParseOSRelease=%q want %q", got, tc.want)
			}
		})
	}
}
