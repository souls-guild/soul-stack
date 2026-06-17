package soulprint

import "strings"

// osRelease — разобранные поля /etc/os-release, нужные Soulprint-у.
type osRelease struct {
	id              string // ID= (ubuntu / rocky / alpine)
	idLike          string // ID_LIKE= (debian / "rhel centos fedora")
	versionID       string // VERSION_ID= ("22.04")
	versionCodename string // VERSION_CODENAME= (jammy)
}

// parseOSRelease разбирает содержимое /etc/os-release. Формат — KEY=VALUE
// построчно, значения опционально в кавычках (man os-release). Неизвестные
// ключи игнорируются. Парсит мягко: мусорная строка не роняет разбор.
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

// family выводит OsFacts.family из ID / ID_LIKE. Сначала ID (точное семейство),
// затем ID_LIKE (производные дистрибутивы — derivatives Ubuntu/Rocky наследуют
// семейство родителя). Возвращает "" для нераспознанного — Keeper толерантен.
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

// pkgMgrTable — маппинг (family, distro) → pkg_mgr/init_system (ADR-018,
// docs/soul/soulprint.md). Источник истины — таблица в коде Soul-агента;
// расширение покрытия = новая версия бинаря (сознательная цена централизации).
//
// Ключ — pair{family, distro}. Если точного distro-совпадения нет, действует
// family-fallback (см. pkgMgrInitSystem) — внутри семейства pkg-mgr и init
// обычно совпадают.
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

// familyDefaults — fallback по семейству, когда точного distro-ключа нет
// (например, неизвестный debian-derivative). Внутри семейства pkg-mgr/init
// одинаковы — это и есть основание для централизованной таблицы (ADR-018).
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

// pkgMgrInitSystem возвращает (pkg_mgr, init_system) для пары family+distro по
// таблице ADR-018. Порядок поиска: точная пара → family-fallback → ("", "").
// Пустые значения штатны (нераспознанная ОС) — Keeper толерантен.
func pkgMgrInitSystem(family, distro string) (pkgMgr, initSystem string) {
	if v, ok := pkgMgrTable[pair{family, distro}]; ok {
		return v.pkgMgr, v.initSystem
	}
	if v, ok := familyDefaults[family]; ok {
		return v.pkgMgr, v.initSystem
	}
	return "", ""
}

// unquote снимает обрамляющие одинарные/двойные кавычки (формат os-release).
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
