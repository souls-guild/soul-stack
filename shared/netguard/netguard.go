// Package netguard is a shared SSRF egress guard for outbound HTTP to UNtrusted
// endpoints. A single point for both sides of Soul Stack: Keeper's Augur brokers
// (prom/elk, keeper/internal/augur) and Soul's core HTTP modules (core.url /
// core.http, soul/internal/coremod). Two independently evolving SSRF guards in
// security-critical code risk divergence; this package removes it.
//
// Protection model (one for all consumers):
//   - https:// only (downgrade protection; SSRF to metadata via
//     http://169.254.169.254 is a common vector);
//   - resolve-then-check-then-dial on the actually resolved IP (rebind-safe):
//     ALL resolved IPs are checked against [IsBlockedIP], and dial goes to the
//     already-checked concrete IP, not the name — there is no second resolve
//     between check and connection;
//   - block redirects to non-https and chains longer than the limit
//     ([NewCheckRedirect]).
//
// Soul-safe ([ADR-011]): net/http stdlib only, no server-only dependencies and no
// keeper-/soul-internal import. Named netguard after shared/tlsx — an infra utility
// whose suffix describes the domain (network guard); no conflict with stdlib net.
//
// [ADR-011]: docs/adr/0011-go-layout.md
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// extraBlockedNets — CIDR ranges outside the stdlib classifiers
// (net.IP.IsPrivate/IsLoopback/…), blocked symmetrically as "routable into internal
// networks":
//
//   - 100.64.0.0/10 — CGNAT / Shared Address Space (RFC 6598): carrier-grade NAT,
//     often visible in internal networks and not caught by IsPrivate.
//   - fec0::/10 — deprecated IPv6 site-local (RFC 3879): deprecated, but may still
//     resolve/route in legacy networks; not covered by IsLinkLocalUnicast
//     (fe80::/10).
//
// net.IPNet.Contains normalizes IPv4-mapped IPv6 (::ffff:100.64.0.1) to v4 form, so
// the CGNAT check automatically closes the v6-mapped bypass too. The other
// already-blocked classes (loopback/RFC1918/ULA/link-local) also work in v4-mapped
// form, since the stdlib classifiers use To4().
var extraBlockedNets = []*net.IPNet{
	mustCIDR("100.64.0.0/10"),
	mustCIDR("fec0::/10"),
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("netguard: неверный CIDR %q в блок-листе SSRF-guard: %v", s, err))
	}
	return n
}

// IsBlockedIP classifies an IP as disallowed for outbound HTTP to an UNtrusted
// endpoint: loopback (127.0.0.0/8, ::1), private RFC1918/ULA (10/172.16/192.168,
// fc00::/7), link-local (169.254.0.0/16 — includes cloud-metadata 169.254.169.254 —
// and fe80::/10), CGNAT (100.64.0.0/10, RFC 6598), deprecated IPv6 site-local
// (fec0::/10, RFC 3879) and unspecified (0.0.0.0, ::). Returning true means
// "connection forbidden".
//
// IPv4-mapped IPv6 (::ffff:0:0/96) gives no bypass: the stdlib classifiers and
// net.IPNet.Contains normalize such addresses to v4 form, so a blocked IPv4 stays
// blocked in v6 form too.
//
// Exported for unit-testing the classifier in isolation (no network egress, no
// client build).
func IsBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	for _, n := range extraBlockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateHTTPSURL accepts https:// only. http:// is a downgrade risk, file:// reads
// the local FS bypassing the access model; everything but https is rejected.
//
// Parsed via url.Parse (not a string prefix): the scheme is compared
// case-insensitively (a valid `HTTPS://` passes) and properly (`https://\nhttp://evil`
// does not slip through a naive HasPrefix).
//
// A literal IP in host is NOT checked here — that is done by [ValidateEndpoint] (fast
// reject before the client build) and in any case by the dial phase on the resolved
// IP ([GuardedDialContext]).
func ValidateHTTPSURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("netguard: invalid url %q", rawURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("netguard: only https:// is allowed, got scheme %q", u.Scheme)
	}
	return nil
}

// ValidateEndpoint — strict endpoint check before an HTTP request:
// [ValidateHTTPSURL] (https only) + non-empty host + a literal IP in host is checked
// against [IsBlockedIP] here (fast reject before the client build). DNS names are
// checked in the dial phase on the resolved IP ([GuardedDialContext]).
func ValidateEndpoint(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("netguard: invalid endpoint url %q", rawURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("netguard: only https:// endpoints are allowed, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("netguard: endpoint %q has no host", rawURL)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && IsBlockedIP(ip) {
		return blockedErr(host, ip)
	}
	return nil
}

// NewCheckRedirect returns an http.Client.CheckRedirect: it rejects any redirect to
// non-https and a chain longer than maxRedirects. Returning an error aborts the
// request (downgrade/MITM protection is non-disableable): a 302 https→http must not
// download a payload over an unprotected channel and leak sensitive headers.
//
// SSRF protection of redirects is done NOT here but in the client's DialContext
// ([GuardedDialContext]): a hop to a host resolving to metadata/loopback/RFC1918 is
// rejected in the dial phase on the actually resolved IP — this closes both direct
// access and DNS-rebind, which CheckRedirect (seeing only the hop URL) cannot.
func NewCheckRedirect(maxRedirects int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("netguard: redirect to non-https blocked: %s://%s", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("netguard: stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

// Resolver — the minimum net.Resolver the SSRF guard needs. An interface (not a
// concrete *net.Resolver) so a guard unit test can inject a fake resolver
// (DNS-rebind / multi-IP cases) without a real DNS. In production — [DefaultResolver].
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DefaultResolver — the system DNS resolver for production clients.
var DefaultResolver Resolver = net.DefaultResolver

// DialFunc — the net.Dialer.DialContext signature; a guard parameter so a test can
// verify which address (IP) the dial went to (rebind-safety), without a real TCP
// connection.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// GuardedDialContext builds a DialContext with resolve-then-check-then-dial: the
// address is resolved via resolver, ALL returned IPs are checked against
// [IsBlockedIP] (if any one is blocked — reject entirely, so a host with a "one
// public + one metadata" pair of A-records can't bypass the guard), and dial goes to
// the first checked IP, not the name. Dialing the IP itself (not a repeat resolve
// inside the dialer) is the key to DNS-rebind protection: there is no second resolve
// between check and connection that could return a different address.
func GuardedDialContext(resolver Resolver, dial DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("netguard: ssrf-guard bad address %q: %w", addr, err)
		}

		// Literal IP in the URL — check without resolving.
		if ip := net.ParseIP(host); ip != nil {
			if IsBlockedIP(ip) {
				return nil, blockedErr(host, ip)
			}
			return dial(ctx, network, addr)
		}

		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("netguard: ssrf-guard resolve %q: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("netguard: ssrf-guard %q resolved to no addresses", host)
		}
		for _, ipa := range ips {
			if IsBlockedIP(ipa.IP) {
				return nil, blockedErr(host, ipa.IP)
			}
		}

		// Dial the already-checked concrete IP (rebind-safe): take the first,
		// keep network/port. No repeat name resolve.
		return dial(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// blockedErr — the single SSRF-guard rejection wording (host for diagnostics,
// address class via the IP itself; nothing extra leaked out).
func blockedErr(host string, ip net.IP) error {
	return fmt.Errorf("netguard: ssrf-guard blocked address for %q: %s (loopback/private/link-local/cgnat/site-local)", host, ip)
}
