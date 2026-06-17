package util

import (
	"context"
	"strings"
)

// PkgMgr — closed-set менеджеров пакетов, поддерживаемых MVP-core ([ADR-015]).
// Совпадает по значениям с полем SoulprintFacts.os.pkg_mgr (ADR-018):
// собранный Soul-агентом soulprint-факт — primary-источник выбора backend-а
// (см. HostFacts / SoulprintAware ниже), DetectPkgMgr остаётся fallback-ом при
// пустом/unknown факте.
type PkgMgr string

const (
	PkgMgrUnknown PkgMgr = ""
	PkgMgrApt     PkgMgr = "apt"
	PkgMgrDnf     PkgMgr = "dnf"
	PkgMgrYum     PkgMgr = "yum"
	PkgMgrApk     PkgMgr = "apk"
)

// InitSystem — closed-set init-системы для core.service.* (ADR-015).
// Параллель PkgMgr — синхронно с SoulprintFacts.os.init_system (ADR-018):
// primary-источник — собранный soulprint-факт (HostFacts), DetectInitSystem —
// fallback.
type InitSystem string

const (
	InitSystemUnknown InitSystem = ""
	InitSystemSystemd InitSystem = "systemd"
	InitSystemOpenRC  InitSystem = "openrc"
	InitSystemSysV    InitSystem = "sysv"
)

// HostFacts — узкий снимок soulprint-фактов хоста, нужных core-модулям для
// выбора backend-а (pkg-mgr / init-система). Собирается Soul-агентом один раз
// на старте (soulprint.Collector → SoulprintFacts.os) и инжектится в core-модули
// in-process (Вариант A, ADR-018(b)). НЕ едет через proto/plugin-контракт —
// out-of-process-плагины факт пока не получают.
//
// Пустые поля (factless / soulprint вернул unknown) — штатны: модуль откатывается
// на runtime-детект (DetectPkgMgr / DetectInitSystem).
type HostFacts struct {
	PkgMgr     PkgMgr
	InitSystem InitSystem
}

// SoulprintAware — опциональный интерфейс core-модуля, желающего получить
// собранный soulprint-факт хоста. ApplyRunner делает type-assert перед Apply и,
// при совпадении, вызывает SetHostFacts. Реализуется только in-process core-
// модулями (core.pkg / core.service); out-of-process-плагины через sdk/module
// его не видят — публичный SoulModule-контракт интерфейс НЕ расширяет (Вариант A).
type SoulprintAware interface {
	SetHostFacts(HostFacts)
}

// ResolvePkgMgr — единый primary→fallback резолв backend-а для core.pkg:
// фактический pkg_mgr из soulprint (primary) либо, при пустом/unknown факте,
// runtime-детект через `command -v` (DetectPkgMgr). Это закрывает BUG-B: выбор
// backend-а и CEL `soulprint.self.os.pkg_mgr` видят ОДИН источник истины.
func ResolvePkgMgr(ctx context.Context, r Runner, fact PkgMgr) PkgMgr {
	if fact != PkgMgrUnknown {
		return fact
	}
	return DetectPkgMgr(ctx, r)
}

// ResolveInitSystem — primary→fallback резолв init-системы для core.service
// (зеркало ResolvePkgMgr). soulprint-факт primary, DetectInitSystem fallback.
func ResolveInitSystem(ctx context.Context, r Runner, fact InitSystem) InitSystem {
	if fact != InitSystemUnknown {
		return fact
	}
	return DetectInitSystem(ctx, r)
}

// OSFamily — closed-set OS-семейств для PkgMgr-маппинга. Узкий: для core MVP
// различение alpine vs debian vs redhat достаточно (внутри семейства pkg-mgr
// одинаков; ADR-018 → таблица family→pkg_mgr).
type OSFamily string

const (
	OSFamilyUnknown OSFamily = ""
	OSFamilyDebian  OSFamily = "debian"
	OSFamilyRedHat  OSFamily = "redhat"
	OSFamilyAlpine  OSFamily = "alpine"
)

// DetectPkgMgr — fallback-детект pkg-mgr через `command -v`, когда soulprint-факт
// пуст/unknown (см. ResolvePkgMgr — primary-путь читает SoulprintFacts.os.pkg_mgr).
// Порядок проверки — в порядке распространённости (apt → dnf → yum → apk). На
// multi-mgr системах (теоретически возможны на distroless-derivatives) выигрывает
// первый.
//
// Принимает Runner — в unit-тестах подменяется на fakeRunner.
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
		// command -v как builtin недоступен через exec без shell.
		// Fallback: which (POSIX).
		if r.Run(ctx, "which", p.bin).OK() {
			return p.mgr
		}
	}
	return PkgMgrUnknown
}

// DetectInitSystem — fallback-детект init-системы, когда soulprint-факт пуст/
// unknown (см. ResolveInitSystem — primary читает SoulprintFacts.os.init_system).
// systemd проверяется первым: `systemctl --version` отрабатывает с exit 0 и на
// minimal-системах, где systemd установлен но не PID 1 (chroot, container) — в
// таких случаях модуль всё равно идёт в systemd-ветку и пусть systemctl сам
// разбирается с unit-state-ом. OpenRC через `rc-service` (alpine canonical).
//
// Важно: на alpine с soulprint=openrc, но без установленных openrc-tools,
// runtime-детект провалился бы (`rc-service --version` отсутствует) — поэтому
// primary-путь через факт (ResolveInitSystem) и нужен (BUG-B).
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

// ParseOSRelease парсит содержимое /etc/os-release (ID= / ID_LIKE=) и
// возвращает OSFamily. Источник истины пока — pkg-mgr detection, но если
// модуль захочет различать distro для другого случая, этот helper к месту.
//
// Возвращает OSFamilyUnknown, если ни одно семейство не подошло (в т.ч.
// пустая или мусорная строка).
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
