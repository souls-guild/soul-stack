package util

import "context"

// Firewall is a closed set of firewall tools for core.firewall ([ADR-015]).
// MVP: ufw and firewalld; iptables is deliberately deferred (needs
// ip(6)tables-save + chain semantics not covered by a simple add/delete pair).
type Firewall string

const (
	FirewallUnknown   Firewall = ""
	FirewallUFW       Firewall = "ufw"
	FirewallFirewalld Firewall = "firewalld"
)

// DetectFirewall determines the firewall tool by which control binary is
// installed (NOT by Soulprint): `ufw` for ufw, `firewall-cmd` for firewalld.
// Mirrors DetectInitSystem/DetectPkgMgr: checked via `--version`. ufw first
// (more common on the debian fleet); firewalld for the redhat fleet.
//
// Takes a Runner — substituted with fakeRunner in unit tests.
func DetectFirewall(ctx context.Context, r Runner) Firewall {
	if r.Run(ctx, "ufw", "--version").OK() {
		return FirewallUFW
	}
	if r.Run(ctx, "firewall-cmd", "--version").OK() {
		return FirewallFirewalld
	}
	return FirewallUnknown
}
