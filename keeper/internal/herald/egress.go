package herald

// SSRF egress guard for Herald webhook delivery (ADR-052(e): channel URL is
// operator-specified → outgoing HTTP from keeper → SSRF vector).
//
// Common SSRF guard logic (resolve-then-check-then-dial by actual IP,
// rebind-safe; CheckRedirect downgrade guard; https-only; blocked IP
// classifier) is in shared/netguard (same guard as augur/core.url).
// Here — Herald-specific: per-Herald opt-out (http_allowed/allow_private),
// configurable timeout, client constructor for specific channel.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

const (
	// maxDeliveryRedirects is hard limit on webhook POST redirects. Each hop
	// checked for https and actual IP (netguard).
	maxDeliveryRedirects = 5

	// deliveryDialTimeout is TCP connection setup timeout.
	deliveryDialTimeout = 10 * time.Second

	// DefaultDeliveryTimeout is default total timeout for one webhook POST
	// (dial + TLS + write + read response). Operator-specified endpoint
	// is untrusted: slow/malicious host shouldn't hold worker goroutine longer.
	// Configurable (keeper.yml::herald.delivery_timeout, ADR-052(e) "timeout").
	DefaultDeliveryTimeout = 10 * time.Second
)

// validateDeliveryEndpoint checks channel URL BEFORE request (ADR-052(e)):
// config may have changed after create (or create passed with different opt-out),
// so validate on each delivery, not trusting CRUD time.
//
// allowPrivate / httpAllowed are per-Herald opt-outs (config.allow_private /
// config.http_allowed). Default guard (both false): https-only + literal
// private IP in host blocked (netguard.ValidateEndpoint). DNS resolve to
// private IP caught at dial phase (guardedDeliveryClient).
//
// When allow_private, dial guard not set at all (see guardedDeliveryClient),
// so literal private IP with allow_private not rejected here.
func validateDeliveryEndpoint(rawURL string, httpAllowed, allowPrivate bool) error {
	if !httpAllowed && !allowPrivate {
		return netguard.ValidateEndpoint(rawURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("herald: invalid webhook url %q", rawURL)
	}
	if u.Host == "" {
		return fmt.Errorf("herald: webhook url %q has no host", rawURL)
	}
	if !httpAllowed && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("herald: only https:// webhook allowed (set http_allowed), got scheme %q", u.Scheme)
	}
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return fmt.Errorf("herald: unsupported webhook url scheme %q", u.Scheme)
	}
	// allow_private=false + http_allowed=true: still block private (dial-guard
	// in guardedDeliveryClient). Literal private IP not validated here —
	// dial phase will cover (both literal and DNS resolve).
	return nil
}

// guardedDeliveryClient constructs *http.Client for specific channel:
//   - system TLS trust store (no InsecureSkipVerify);
//   - total request timeout;
//   - redirect downgrade guard + limit (netguard.NewCheckRedirect);
//   - SSRF dial guard by actual IP (netguard.GuardedDialContext), IF
//     allowPrivate=false. When allowPrivate=true (explicit operator opt-out)
//     dial guard NOT set — private IPs allowed (at own risk, ADR-052(e)
//     "allow_private — explicit opt-out like core.url").
//
// resolver injected for guard testability (DNS-rebind/multi-IP without
// real DNS); in production — netguard.DefaultResolver.
func guardedDeliveryClient(resolver netguard.Resolver, allowPrivate bool, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultDeliveryTimeout
	}
	dialer := &net.Dialer{Timeout: deliveryDialTimeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !allowPrivate {
		transport.DialContext = netguard.GuardedDialContext(resolver, dialer.DialContext)
	} else {
		transport.DialContext = dialer.DialContext
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: netguard.NewCheckRedirect(maxDeliveryRedirects),
		Transport:     transport,
	}
}
