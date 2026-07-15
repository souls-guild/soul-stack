package util

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

// SSRF egress-guard for core-HTTP modules (core.url / core.http). Shared guard
// logic (resolve-then-check-then-dial by actual IP; redirect downgrade
// protection; https-only; blocked-IP classifier) lives in shared/netguard,
// reused by Keeper's Augur brokers. This file is a thin wrapper preserving the
// core-side API (MaxRedirects, CheckRedirect, ValidateURL, IsBlockedIP,
// NewHTTPClient, HTTPDoer).

// MaxRedirects is the hard redirect-count limit for core-module HTTP fetches.
// Shared by core.url / core.http; do not duplicate locally.
const MaxRedirects = 10

// dialTimeout matches http.DefaultTransport so the custom DialContext (SSRF
// guard) doesn't change timeout behavior.
const dialTimeout = 30 * time.Second

// HTTPDoer is the minimal HTTP client interface core modules need. Exposed as
// a module field for testability (fakes in unit tests); production uses
// *http.Client via NewHTTPClient.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// CheckRedirect is http.Client.CheckRedirect: rejects any non-https hop and
// chains longer than MaxRedirects. Returning an error aborts the request
// (downgrade/MITM protection is non-optional): a 302 https→http must not
// download the payload over an unprotected channel and leak sensitive headers.
//
// SSRF protection for redirects is NOT done here but in the client's
// DialContext (see NewHTTPClient): a hop to a host resolving to
// metadata/loopback/RFC1918 is rejected at dial time by the actually-resolved
// IP. Delegates to shared/netguard (shared supply-chain protection for
// core.url / core.http and Keeper's Augur brokers).
func CheckRedirect(req *http.Request, via []*http.Request) error {
	return netguard.NewCheckRedirect(MaxRedirects)(req, via)
}

// IsBlockedIP classifies an IP as disallowed for outbound core-HTTP
// (loopback/RFC1918/ULA/link-local/CGNAT/site-local/unspecified). Delegates to
// shared/netguard; exported for isolated unit testing of the classifier and
// used by the SSRF guard via DialContext.
func IsBlockedIP(ip net.IP) bool {
	return netguard.IsBlockedIP(ip)
}

// ValidateURL accepts only https://. http:// is a downgrade risk, file:// reads
// the local FS bypassing the access model; anything but https is rejected.
// Thin wrapper over ValidateFetchURL(rawURL, false) — same behavior as the
// previous direct delegation to netguard.ValidateHTTPSURL.
//
// Shared by core-modules doing HTTP; do not duplicate locally.
func ValidateURL(rawURL string) error {
	return ValidateFetchURL(rawURL, false)
}

// ValidateFetchURL validates a fetch URL, with opt-in support for http://.
//
// allowHTTP=false (default for all consumers): delegates literally to
// netguard.ValidateHTTPSURL (same code as Augur — one supply-chain check),
// only https:// passes.
//
// allowHTTP=true (explicit operator opt-out via param allow_http): both http
// and https pass, everything else (file://, ftp://, etc.) is rejected. Scheme
// is compared strictly via url.Parse + strings.EqualFold, not a naive
// HasPrefix (a string like "https://\nhttp://evil" must not sneak through).
//
// Dropping https-only does NOT open SSRF: the dial-guard lives separately in
// NewHTTPClient (HTTPClientOpts.AllowPrivate) — http still can't reach
// metadata/loopback.
func ValidateFetchURL(rawURL string, allowHTTP bool) error {
	if !allowHTTP {
		return netguard.ValidateHTTPSURL(rawURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("util: invalid url %q", rawURL)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("util: only http(s):// is allowed, got scheme %q", u.Scheme)
	}
	return nil
}

// checkRedirectAllowingHTTP is CheckRedirect for allow_http mode: permits a
// hop to either https or http (downgrade-redirect expected under explicit
// operator opt-out), but rejects any non-http(s) scheme and chains longer than
// maxRedirects. Scheme is compared case-insensitively (EqualFold).
//
// SSRF protection for redirects is NOT done here but in the client's
// DialContext (netguard.GuardedDialContext with AllowPrivate=false): a hop to
// a host resolving to metadata/loopback/RFC1918 is rejected at dial time.
// allow_http only relaxes the scheme/downgrade check, not the SSRF guard.
func checkRedirectAllowingHTTP(maxRedirects int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, "http") && !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("util: redirect to non-http(s) blocked: %s://%s", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("util: stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

// HTTPClientOpts holds opt-out flags for NewHTTPClient. Zero value is the
// maximally safe client: SSRF dial-guard, system TLS trust store, redirect
// downgrade protection. Each flag weakens one guard and requires explicit
// operator opt-out.
type HTTPClientOpts struct {
	// AllowPrivate true disables the SSRF dial-guard (for a legitimate
	// internal endpoint, e.g. health-check on 127.0.0.1:8080/health). false
	// (default): netguard.GuardedDialContext blocks dial to
	// metadata/loopback/RFC1918.
	AllowPrivate bool
	// InsecureSkipVerify true sets transport.TLSClientConfig.InsecureSkipVerify
	// (self-signed / internal CA). MITM risk, explicit opt-out only.
	InsecureSkipVerify bool
	// AllowHTTPRedirect true lets CheckRedirect permit a downgrade hop
	// https→http (paired with allow_http at the module level). false
	// (default): non-https downgrade is rejected (netguard).
	AllowHTTPRedirect bool
}

// GuardWarnings builds a list of warning strings for weakened HTTP-fetch
// security guards (core.url / core.http). Single source of truth for wording
// and host-only masking for both modules: with an opt-out flag raised, the
// operator sees the guard was dropped in the ApplyEvent output (warnings in
// output reach the operator via RunResult).
//
// Returns one string per raised flag, in deterministic order
// (insecure_skip_verify → allow_http → allow_private); nil if no flags are
// raised. Only host goes into the message (param host is pre-extracted via
// WarnHost(rawURL)): the full URL may carry sensitive query/path data, headers
// are sensitive-by-construction ([ADR-010] §7.4) — neither belongs in a
// warning.
//
// [ADR-010]: docs/adr/0010-templating.md
func GuardWarnings(host string, opts HTTPClientOpts) []string {
	if !opts.InsecureSkipVerify && !opts.AllowHTTPRedirect && !opts.AllowPrivate {
		return nil
	}
	var w []string
	if opts.InsecureSkipVerify {
		w = append(w, fmt.Sprintf("TLS verification disabled (insecure_skip_verify) for %s", host))
	}
	if opts.AllowHTTPRedirect {
		w = append(w, fmt.Sprintf("plaintext http allowed (allow_http) for %s", host))
	}
	if opts.AllowPrivate {
		w = append(w, fmt.Sprintf("SSRF-guard disabled (allow_private) for %s", host))
	}
	return w
}

// WarnHost extracts the host from a URL for guard warnings (no scheme/path/
// query — those may carry sensitive data). Called after ValidateFetchURL, so
// an invalid URL shouldn't normally reach it; falls back to "?" rather than
// failing Apply. Shared by core.url / core.http.
func WarnHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}

// NewHTTPClient returns an *http.Client for core modules, configured by the
// HTTPClientOpts opt-out flags. Zero-value opts is the default maximally-safe
// client: system TLS trust store, redirect downgrade protection
// (CheckRedirect + limit), and an SSRF dial-guard (shared/netguard).
//
// SSRF guard (AllowPrivate=false): a custom DialContext resolves the host via
// net.Resolver and rejects the connection if ANY resolved IP matches
// IsBlockedIP. Checking after resolve and before dial closes two vectors at
// once:
//   - direct SSRF (https://169.254.169.254 — cloud metadata IAM creds,
//     https://127.0.0.1, RFC1918, [::1], link-local);
//   - DNS-rebind (a host whose DNS resolves to metadata/loopback never gets
//     through — the resolve is real, but dial uses the already-checked IP, not
//     the name, so a second "rebind" resolve can't happen).
//
// AllowPrivate=true is an opt-in for a legitimate internal health-check: the
// guard is disabled, dial proceeds normally. AllowHTTPRedirect=true permits a
// downgrade hop (paired with allow_http). InsecureSkipVerify=true disables TLS
// chain verification (self-signed). Each flag weakens an independent guard.
//
// Shared by core-modules doing HTTP; do not duplicate locally.
func NewHTTPClient(opts HTTPClientOpts) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !opts.AllowPrivate {
		dialer := &net.Dialer{Timeout: dialTimeout}
		transport.DialContext = netguard.GuardedDialContext(netguard.DefaultResolver, dialer.DialContext)
	}
	if opts.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	checkRedirect := CheckRedirect
	if opts.AllowHTTPRedirect {
		checkRedirect = checkRedirectAllowingHTTP(MaxRedirects)
	}
	return &http.Client{
		CheckRedirect: checkRedirect,
		Transport:     transport,
	}
}
