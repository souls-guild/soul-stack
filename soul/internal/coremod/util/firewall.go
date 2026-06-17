package util

import "context"

// Firewall — closed-set файрвол-инструментов для core.firewall ([ADR-015]).
// MVP: ufw и firewalld; iptables сознательно отложен (требует ip(6)tables-save
// + chain-семантику, не покрываемую парой add/delete-правил).
type Firewall string

const (
	FirewallUnknown   Firewall = ""
	FirewallUFW       Firewall = "ufw"
	FirewallFirewalld Firewall = "firewalld"
)

// DetectFirewall — определяет файрвол-инструмент по установленному управляющему
// бинарю (НЕ по Soulprint): для ufw это `ufw`, для firewalld — `firewall-cmd`.
// Параллель DetectInitSystem/DetectPkgMgr: проверка через `--version`. ufw
// первым (более распространён на debian-парке); firewalld — на redhat-парке.
//
// Принимает Runner — в unit-тестах подменяется на fakeRunner.
func DetectFirewall(ctx context.Context, r Runner) Firewall {
	if r.Run(ctx, "ufw", "--version").OK() {
		return FirewallUFW
	}
	if r.Run(ctx, "firewall-cmd", "--version").OK() {
		return FirewallFirewalld
	}
	return FirewallUnknown
}
