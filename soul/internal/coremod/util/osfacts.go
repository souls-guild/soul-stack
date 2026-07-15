package util

import (
	"context"
	"strings"
)

// PkgMgr — closed set of package managers supported by MVP core ([ADR-015]).
// Values match SoulprintFacts.os.pkg_mgr (ADR-018): the soulprint fact
// collected by the Soul agent is the primary source for backend selection
// (see HostFacts / SoulprintAware below), DetectPkgMgr is the fallback for an
// empty/unknown fact.
type PkgMgr string

const (
	PkgMgrUnknown PkgMgr = ""
	PkgMgrApt     PkgMgr = "apt"
	PkgMgrDnf     PkgMgr = "dnf"
	PkgMgrYum     PkgMgr = "yum"
	PkgMgrApk     PkgMgr = "apk"
)

// InitSystem — closed set of init systems for core.service.* (ADR-015).
// Parallels PkgMgr — synced with SoulprintFacts.os.init_system (ADR-018):
// primary source is the collected soulprint fact (HostFacts), DetectInitSystem
// is the fallback.
type InitSystem string

const (
	InitSystemUnknown InitSystem = ""
	InitSystemSystemd InitSystem = "systemd"
	InitSystemOpenRC  InitSystem = "openrc"
	InitSystemSysV    InitSystem = "sysv"
)

// HostFacts — narrow snapshot of host soulprint facts needed by core modules
// to pick a backend (pkg-mgr / init system). Collected once by the Soul agent
// at startup (soulprint.Collector → SoulprintFacts.os) and injected into
// core modules in-process (Option A, ADR-018(b)). NOT carried over the
// proto/plugin contract — out-of-process plugins don't get the fact yet.
//
// Empty fields (factless host / soulprint returned unknown) are normal: the
// module falls back to runtime detection (DetectPkgMgr / DetectInitSystem).
type HostFacts struct {
	PkgMgr     PkgMgr
	InitSystem InitSystem
}

// SoulprintAware — optional interface for a core module that wants the
// collected host soulprint fact. ApplyRunner type-asserts before Apply and,
// on a match, calls SetHostFacts. Implemented only by in-process core modules
// (core.pkg / core.service); out-of-process plugins via sdk/module can't see
// it — the public SoulModule contract interface is NOT extended (Option A).
type SoulprintAware interface {
	SetHostFacts(HostFacts)
}

// ResolvePkgMgr — unified primary→fallback backend resolution for core.pkg:
// the actual pkg_mgr from soulprint (primary), or runtime detection via
// `command -v` (DetectPkgMgr) when the fact is empty/unknown. Closes BUG-B:
// backend selection and CEL `soulprint.self.os.pkg_mgr` see ONE source of truth.
func ResolvePkgMgr(ctx context.Context, r Runner, fact PkgMgr) PkgMgr {
	if fact != PkgMgrUnknown {
		return fact
	}
	return DetectPkgMgr(ctx, r)
}

// ResolveInitSystem — primary→fallback init-system resolution for
// core.service (mirrors ResolvePkgMgr). soulprint fact is primary,
// DetectInitSystem is the fallback.
func ResolveInitSystem(ctx context.Context, r Runner, fact InitSystem) InitSystem {
	if fact != InitSystemUnknown {
		return fact
	}
	return DetectInitSystem(ctx, r)
}

// OSFamily — closed set of OS families for PkgMgr mapping. Narrow: for core
// MVP, distinguishing alpine vs debian vs redhat is enough (pkg-mgr is the
// same within a family; ADR-018 → family→pkg_mgr table).
type OSFamily string

const (
	OSFamilyUnknown OSFamily = ""
	OSFamilyDebian  OSFamily = "debian"
	OSFamilyRedHat  OSFamily = "redhat"
	OSFamilyAlpine  OSFamily = "alpine"
)

// DetectPkgMgr — fallback pkg-mgr detection via `command -v`, used when the
// soulprint fact is empty/unknown (see ResolvePkgMgr — the primary path reads
// SoulprintFacts.os.pkg_mgr). Checked in order of prevalence (apt → dnf → yum
// → apk). On multi-mgr systems (theoretically possible on distroless
// derivatives), the first match wins.
//
// Takes a Runner — swapped for a fakeRunner in unit tests.
func DetectPkgMgr(ctx context.Context, r Runner) PkgMgr {
	for _, p := range []struct {
		bin string
		mgr PkgMgr
	}{
		{"apt-get", PkgMgrApt},
		{"dnf", PkgMgrDnf},
		{"yum", PkgMgrYum},
		{"apk", PkgMgrApk},
	} {
		if r.Run(ctx, "command", "-v", p.bin).OK() {
			return p.mgr
		}
		// command -v as a shell builtin is unreachable via exec without a shell.
		// Fallback: which (POSIX).
		if r.Run(ctx, "which", p.bin).OK() {
			return p.mgr
		}
	}
	return PkgMgrUnknown
}

// DetectInitSystem — fallback init-system detection when the soulprint fact
// is empty/unknown (see ResolveInitSystem — the primary path reads
// SoulprintFacts.os.init_system). systemd is checked first: `systemctl
// --version` exits 0 even on minimal systems where systemd is installed but
// not PID 1 (chroot, container) — in that case the module still goes to the
// systemd branch and lets systemctl sort out unit state. OpenRC via
// `rc-service` (alpine canonical).
//
// Important: on alpine with soulprint=openrc but without openrc-tools
// installed, runtime detection would fail (`rc-service --version` is
// missing) — that's exactly why the primary fact path (ResolveInitSystem)
// is needed (BUG-B).
func DetectInitSystem(ctx context.Context, r Runner) InitSystem {
	if r.Run(ctx, "systemctl", "--version").OK() {
		return InitSystemSystemd
	}
	if r.Run(ctx, "rc-service", "--version").OK() {
		return InitSystemOpenRC
	}
	if r.Run(ctx, "service", "--version").OK() {
		return InitSystemSysV
	}
	return InitSystemUnknown
}

// ParseOSRelease parses /etc/os-release content (ID= / ID_LIKE=) and returns
// an OSFamily. The source of truth is still pkg-mgr detection, but this
// helper is handy if a module needs to distinguish distros for another case.
//
// Returns OSFamilyUnknown if no family matched (including an empty or
// garbage string).
func ParseOSRelease(content string) OSFamily {
	lines := strings.Split(content, "\n")
	id, idLike := "", ""
	for _, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "ID="):
			id = trimQuoted(strings.TrimPrefix(l, "ID="))
		case strings.HasPrefix(l, "ID_LIKE="):
			idLike = trimQuoted(strings.TrimPrefix(l, "ID_LIKE="))
		}
	}
	candidates := append([]string{id}, strings.Fields(idLike)...)
	for _, c := range candidates {
		switch c {
		case "debian", "ubuntu":
			return OSFamilyDebian
		case "rhel", "centos", "fedora", "rocky", "almalinux":
			return OSFamilyRedHat
		case "alpine":
			return OSFamilyAlpine
		}
	}
	return OSFamilyUnknown
}

func trimQuoted(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
