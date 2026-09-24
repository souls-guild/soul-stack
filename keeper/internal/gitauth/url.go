package gitauth

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// defaultSSHUser is the username git forges expect when the URL names none —
// `git@` is the universal form on GitHub/GitLab/Gitea.
const defaultSSHUser = "git"

// IsSSHURL recognizes the ssh scheme and the scp-like form
// `git@host:org/repo.git`.
//
// It is the single source of the ssh/not-ssh fork for BOTH halves of auth
// selection — which credential [Store.Lookup] hands back, and whether the
// caller falls through to the ssh-agent. Two copies of this rule that disagree
// produce a public-key auth method on an https URL, which fails at the
// transport with an error naming neither.
func IsSSHURL(gitURL string) bool {
	if strings.HasPrefix(gitURL, "ssh://") {
		return true
	}
	if strings.Contains(gitURL, "://") {
		return false
	}
	// scp form: `user@host:path`, a colon after the host, no scheme.
	at := strings.Index(gitURL, "@")
	colon := strings.Index(gitURL, ":")
	return at >= 0 && colon > at
}

// SSHUser extracts the username from an ssh URL; defaults to `git`.
func SSHUser(gitURL string) string {
	if strings.HasPrefix(gitURL, "ssh://") {
		if u, err := url.Parse(gitURL); err == nil && u.User != nil {
			if name := u.User.Username(); name != "" {
				return name
			}
		}
		return defaultSSHUser
	}
	if at := strings.Index(gitURL, "@"); at > 0 {
		return gitURL[:at]
	}
	return defaultSSHUser
}

// hostPort renders the `host:port` string a known_hosts lookup is keyed on for
// an ssh URL, reproducing go-git's `command.getHostWithPort` — including
// reading the node's `~/.ssh/config` through the same exported
// [gitssh.DefaultSSHConfig] object go-git reads it through.
//
// Agreeing with go-git is the whole point, quirks included (see
// [sshConfigHostPort] for the one that looks like a bug): this string is what
// the host-key algorithm list is looked up by, and go-git hands the host-key
// callback the string ITS version produces. An `~/.ssh/config` with
// `Host gitlab.example.com` / `HostName git-internal.example.net` on the keeper
// node makes those two different — the callback would be asked about
// `git-internal.example.net:22` while the algorithms were derived for
// `gitlab.example.com:22`, which is an empty list and therefore x/crypto's
// defaults, the failure [Store.Lookup] sets the field to avoid.
func hostPort(gitURL string) string {
	ep, err := transport.NewEndpoint(gitURL)
	if err != nil {
		return ""
	}
	if addr, found := sshConfigHostPort(ep); found {
		return addr
	}
	port := ep.Port
	if port <= 0 {
		port = gitssh.DefaultPort
	}
	return net.JoinHostPort(ep.Host, strconv.Itoa(port))
}

// sshConfigHostPort mirrors go-git's `doGetHostWithPortFromSSHConfig`,
// including the part that looks like a bug: the `Hostname` match alone decides
// the branch, and the branch returns WITHOUT the DefaultPort floor its caller
// applies otherwise — so a reader answering `Hostname` and an empty `Port`
// against an endpoint with no port yields `host:0`.
//
// Copied rather than corrected on purpose. The string this produces is only
// ever compared against the one go-git produces; a version that is more
// sensible is a version that disagrees, and disagreeing is the entire failure
// mode. The stock reader cannot reach it anyway — `kevinburke/ssh_config`
// falls back to its own `Port` default of 22.
func sshConfigHostPort(ep *transport.Endpoint) (string, bool) {
	cfgHost := sshConfigGet(ep.Host, "Hostname")
	if cfgHost == "" {
		return "", false
	}
	port := ep.Port
	if cfgPort := sshConfigGet(ep.Host, "Port"); cfgPort != "" {
		if i, err := strconv.Atoi(cfgPort); err == nil {
			port = i
		}
	}
	return net.JoinHostPort(cfgHost, strconv.Itoa(port)), true
}

// sshConfigGet reads one `~/.ssh/config` parameter for an alias, through the
// same object go-git dials with. A nil reader — go-git allows the variable to
// be cleared — means no ssh_config, which is the usual case on a keeper node.
func sshConfigGet(alias, key string) string {
	if gitssh.DefaultSSHConfig == nil {
		return ""
	}
	return gitssh.DefaultSSHConfig.Get(alias, key)
}

// URLUser returns the userinfo username a scheme-bearing URL carries, or "".
// Unlike [SSHUser] it substitutes no default: the caller needs to tell "the
// operator named an account" from "the operator named none".
func URLUser(gitURL string) string {
	if !strings.Contains(gitURL, "://") {
		return ""
	}
	u, err := url.Parse(gitURL)
	if err != nil || u.User == nil {
		return ""
	}
	return u.User.Username()
}

// CanonicalHost is the form a configured `git.credentials[].host` is keyed on,
// and the same form [Host] puts a URL's host into, so that an entry and the URL
// naming the same host land on one key.
//
// Brackets come off because `url.Hostname()` strips them. Lowercasing is for
// DNS names. An IP literal is normalized through [net.ParseIP], not textually:
// `fd00:0:0:0:0:0:0:1` and `fd00::1` are one address written two ways, and
// comparing the strings would make the entry validate and never match.
func CanonicalHost(host string) string {
	if inner, ok := strings.CutPrefix(host, "["); ok {
		if trimmed, closed := strings.CutSuffix(inner, "]"); closed {
			host = trimmed
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(host)
}

// Host extracts the bare hostname a credential entry is keyed on, lowercased
// and without userinfo or port. It answers "" for a URL carrying no host
// (`file://…`, a bare path), which [Store.Lookup] reads as "no entry can
// match".
func Host(gitURL string) string {
	if strings.Contains(gitURL, "://") {
		u, err := url.Parse(gitURL)
		if err != nil {
			return ""
		}
		return CanonicalHost(u.Hostname())
	}
	// scp form `user@host:path`: the host sits between `@` and the first colon
	// after it. What follows that colon may be a path, or go-git's optional
	// `port:` prefix (`git@host:2222:path` — the port needs the SECOND colon,
	// so in `git@host:2222/path` the digits are path). Either way the host ends
	// at the first colon; resolving the port is [hostPort]'s business.
	at := strings.Index(gitURL, "@")
	if at < 0 {
		return ""
	}
	rest := gitURL[at+1:]
	colon := strings.Index(rest, ":")
	if colon <= 0 {
		return ""
	}
	return CanonicalHost(rest[:colon])
}
