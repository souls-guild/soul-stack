package soulprint

import "strings"

// osRelease holds the /etc/os-release fields Soulprint needs.
type osRelease struct {
	id              string // ID= (ubuntu / rocky / alpine)
	idLike          string // ID_LIKE= (debian / "rhel centos fedora")
	versionID       string // VERSION_ID= ("22.04")
	versionCodename string // VERSION_CODENAME= (jammy)
}

// parseOSRelease parses /etc/os-release content. Format is KEY=VALUE per
// line, values optionally quoted (man os-release). Unknown keys are
// ignored. Parses leniently: a garbage line doesn't break parsing.
func parseOSRelease(content string) osRelease {
	var r osRelease
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		switch key {
		case "ID":
			r.id = val
		case "ID_LIKE":
			r.idLike = val
		case "VERSION_ID":
			r.versionID = val
		case "VERSION_CODENAME":
			r.versionCodename = val
		}
	}
	return r
}

// family derives OsFacts.family from ID / ID_LIKE. ID first (exact family),
// then ID_LIKE (derivative distros — Ubuntu/Rocky derivatives inherit their
// parent's family). Returns "" for unrecognized — Keeper tolerates it.
func (r osRelease) family() string {
	for _, c := range append([]string{r.id}, strings.Fields(r.idLike)...) {
		switch c {
		case "debian", "ubuntu":
			return "debian"
		case "rhel", "centos", "fedora", "rocky", "almalinux":
			return "rhel"
		case "alpine":
			return "alpine"
		case "arch":
			return "arch"
		}
	}
	return ""
}

// pkgMgrTable maps (family, distro) → pkg_mgr/init_system (ADR-018,
// docs/soul/soulprint.md). Source of truth is this table in the Soul agent
// code; extending coverage means a new binary version (a deliberate cost of
// centralization).
//
// Key is pair{family, distro}. With no exact distro match, family-fallback
// applies (see pkgMgrInitSystem) — pkg-mgr and init usually agree within a
// family.
var pkgMgrTable = map[pair]pkgInit{
	{"debian", "ubuntu"}:  {"apt", "systemd"},
	{"debian", "debian"}:  {"apt", "systemd"},
	{"rhel", "rocky"}:     {"dnf", "systemd"},
	{"rhel", "centos"}:    {"dnf", "systemd"},
	{"rhel", "fedora"}:    {"dnf", "systemd"},
	{"rhel", "almalinux"}: {"dnf", "systemd"},
	{"alpine", "alpine"}:  {"apk", "openrc"},
	{"darwin", "macos"}:   {"brew", "launchd"},
	{"arch", "arch"}:      {"pacman", "systemd"},
}

// familyDefaults is the per-family fallback when there's no exact distro key
// (e.g. an unknown debian derivative). pkg-mgr/init agree within a family —
// the rationale for a centralized table (ADR-018).
var familyDefaults = map[string]pkgInit{
	"debian": {"apt", "systemd"},
	"rhel":   {"dnf", "systemd"},
	"alpine": {"apk", "openrc"},
	"darwin": {"brew", "launchd"},
	"arch":   {"pacman", "systemd"},
}

type pair struct {
	family string
	distro string
}

type pkgInit struct {
	pkgMgr     string
	initSystem string
}

// pkgMgrInitSystem returns (pkg_mgr, init_system) for a family+distro pair per
// the ADR-018 table. Lookup order: exact pair → family-fallback → ("", "").
// Empty values are normal (unrecognized OS) — Keeper tolerates it.
func pkgMgrInitSystem(family, distro string) (pkgMgr, initSystem string) {
	if v, ok := pkgMgrTable[pair{family, distro}]; ok {
		return v.pkgMgr, v.initSystem
	}
	if v, ok := familyDefaults[family]; ok {
		return v.pkgMgr, v.initSystem
	}
	return "", ""
}

// unquote strips surrounding single/double quotes (os-release format).
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
