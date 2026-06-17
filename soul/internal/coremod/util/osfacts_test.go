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

	// Только `service --version` доступен → SysV.
	r3 := internaltest.NewRunner()
	r3.Fallback = util.Result{ExitCode: 1}
	r3.On("service --version", util.Result{ExitCode: 0})
	if got := util.DetectInitSystem(context.Background(), r3); got != util.InitSystemSysV {
		t.Fatalf("DetectInitSystem=%q want sysv", got)
	}

	// Ничего не доступно → Unknown.
	r4 := internaltest.NewRunner()
	r4.Fallback = util.Result{ExitCode: 127}
	if got := util.DetectInitSystem(context.Background(), r4); got != util.InitSystemUnknown {
		t.Fatalf("DetectInitSystem=%q want unknown", got)
	}
}

// TestResolvePkgMgr_FactPrimary — при непустом soulprint-факте резолв возвращает
// факт и НЕ зовёт runtime-детект (Runner отдаёт fail на всё). Это BUG-B:
// единый источник истины с CEL, без зависимости от наличия бинаря в PATH.
func TestResolvePkgMgr_FactPrimary(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127} // любой `command -v`/`which` упадёт
	got := util.ResolvePkgMgr(context.Background(), r, util.PkgMgrApk)
	if got != util.PkgMgrApk {
		t.Fatalf("ResolvePkgMgr=%q want apk (из факта)", got)
	}
	if len(r.Calls) != 0 {
		t.Fatalf("факт primary: runtime-детект не должен вызываться, calls=%v", r.Calls)
	}
}

// TestResolvePkgMgr_FallbackOnEmpty — пустой/unknown факт → runtime-детект.
func TestResolvePkgMgr_FallbackOnEmpty(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	got := util.ResolvePkgMgr(context.Background(), r, util.PkgMgrUnknown)
	if got != util.PkgMgrApk {
		t.Fatalf("ResolvePkgMgr=%q want apk (fallback-детект)", got)
	}
}

// TestResolveInitSystem_FactPrimary — зеркало для init-системы: факт primary,
// детект не вызывается. Это и есть alpine-сценарий BUG-B (soulprint=openrc, но
// `rc-service --version` отсутствует — детект провалился бы).
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

// TestResolveInitSystem_FallbackOnEmpty — пустой факт → runtime-детект.
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
