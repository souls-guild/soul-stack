package gitauth

import (
	"testing"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

func TestIsSSHURL(t *testing.T) {
	cases := map[string]bool{
		"ssh://git@gitlab.example.com/org/repo.git":    true,
		"ssh://gitlab.example.com/org/repo.git":        true,
		"git@gitlab.example.com:org/repo.git":          true,
		"deploy@gitlab.example.com:2222/org/repo.git":  true,
		"https://gitlab.example.com/org/repo.git":      false,
		"https://user@gitlab.example.com/org/repo.git": false,
		"file:///srv/repos/repo.git":                   false,
		"/srv/repos/repo.git":                          false,
		"http://gitlab.example.com/org/repo.git":       false,
		"https://gitlab.example.com:8443/org/repo.git": false,
	}
	for url, want := range cases {
		if got := IsSSHURL(url); got != want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", url, got, want)
		}
	}
}

// TestSSHUser covers only URLs [IsSSHURL] classifies as ssh — the ones
// SSHUser is ever reached with. A scp-like string without an `@` is not one of
// them (both validateGitScheme implementations reject it), so pinning what
// SSHUser answers for it would guard nothing.
func TestSSHUser(t *testing.T) {
	cases := map[string]string{
		"ssh://deploy@gitlab.example.com/org/repo.git":      "deploy",
		"ssh://gitlab.example.com/org/repo.git":             "git",
		"ssh://deploy@gitlab.example.com:2222/org/repo.git": "deploy",
		"deploy@gitlab.example.com:org/repo.git":            "deploy",
	}
	for url, want := range cases {
		if !IsSSHURL(url) {
			t.Fatalf("precondition: IsSSHURL(%q) is false, SSHUser is unreachable for it", url)
		}
		if got := SSHUser(url); got != want {
			t.Errorf("SSHUser(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestHostPort pins the string a known_hosts lookup is keyed on — it has to be
// what go-git will hand the host-key callback, or the algorithm list comes back
// empty and the handshake negotiates whatever x/crypto prefers.
func TestHostPort(t *testing.T) {
	cases := map[string]string{
		"ssh://git@gitlab.example.com/org/repo.git":      "gitlab.example.com:22",
		"ssh://git@gitlab.example.com:2222/org/repo.git": "gitlab.example.com:2222",
		"git@gitlab.example.com:org/repo.git":            "gitlab.example.com:22",
		// go-git's scp grammar puts the port between two colons; with one
		// colon the digits are the start of the PATH, not a port.
		"git@gitlab.example.com:2222:org/repo.git": "gitlab.example.com:2222",
		"git@gitlab.example.com:2222/org/repo.git": "gitlab.example.com:22",
	}
	for url, want := range cases {
		if got := hostPort(url); got != want {
			t.Errorf("hostPort(%q) = %q, want %q", url, got, want)
		}
	}
}

// fakeSSHConfig answers `~/.ssh/config` lookups from a literal map, keyed
// "<alias>/<param>".
type fakeSSHConfig map[string]string

func (f fakeSSHConfig) Get(alias, key string) string { return f[alias+"/"+key] }

// TestHostPort_FollowsSSHConfig — go-git resolves an `~/.ssh/config` Host alias
// before it hands the address to the host-key callback, so the algorithm list
// has to be looked up under the SAME resolved address. Miss that and the list
// comes back empty for a host that is in fact pinned, which drops the
// handshake back to x/crypto's defaults — the failure the field is set to
// avoid, resurfacing only on nodes that happen to have an ssh_config.
func TestHostPort_FollowsSSHConfig(t *testing.T) {
	prev := gitssh.DefaultSSHConfig
	t.Cleanup(func() { gitssh.DefaultSSHConfig = prev })
	gitssh.DefaultSSHConfig = fakeSSHConfig{
		"gitlab.example.com/Hostname":  "git-internal.example.net",
		"gitlab.example.com/Port":      "2222",
		"plain.example.com/Port":       "2222",
		"aliased.example.com/Hostname": "git-internal.example.net",
	}

	if got, want := hostPort("ssh://git@gitlab.example.com/org/repo.git"), "git-internal.example.net:2222"; got != want {
		t.Errorf("hostPort with an aliased host = %q, want %q", got, want)
	}
	// Port alone, with no Hostname, is not an alias: go-git only consults Port
	// once Hostname matched, and reproducing that exactly is the point.
	if got, want := hostPort("ssh://git@plain.example.com/org/repo.git"), "plain.example.com:22"; got != want {
		t.Errorf("hostPort with a Port-only stanza = %q, want %q", got, want)
	}
	// Hostname without Port, against an endpoint carrying none: go-git's alias
	// branch returns before the DefaultPort floor its caller applies, so the
	// port really is 0. Pinned because AGREEING with go-git is the contract —
	// a more sensible answer here is one the callback would not be asked
	// under. The stock ssh_config reader cannot produce it (it defaults Port
	// to 22), so this only fires for a reader someone swapped in.
	if got, want := hostPort("ssh://git@aliased.example.com/org/repo.git"), "git-internal.example.net:0"; got != want {
		t.Errorf("hostPort with Hostname and no Port = %q, want %q", got, want)
	}
}

// TestHost pins what the credential lookup keys on. The port and the userinfo
// must come off: an entry is written as a bare host, so leaving either on would
// make a correctly-configured entry match nothing.
func TestHost(t *testing.T) {
	cases := map[string]string{
		"ssh://git@gitlab.example.com/org/repo.git":       "gitlab.example.com",
		"ssh://git@gitlab.example.com:2222/org/repo.git":  "gitlab.example.com",
		"git@gitlab.example.com:org/repo.git":             "gitlab.example.com",
		"https://gitlab.example.com/org/repo.git":         "gitlab.example.com",
		"https://ci@gitlab.example.com:8443/org/repo.git": "gitlab.example.com",
		"https://GitLab.Example.COM/org/repo.git":         "gitlab.example.com",
		"file:///srv/repos/repo.git":                      "",
		"/srv/repos/repo.git":                             "",
	}
	for url, want := range cases {
		if got := Host(url); got != want {
			t.Errorf("Host(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestURLUser(t *testing.T) {
	cases := map[string]string{
		"https://ci-bot@gitlab.example.com/org/repo.git": "ci-bot",
		"https://gitlab.example.com/org/repo.git":        "",
		"git@gitlab.example.com:org/repo.git":            "",
	}
	for url, want := range cases {
		if got := URLUser(url); got != want {
			t.Errorf("URLUser(%q) = %q, want %q", url, got, want)
		}
	}
}
