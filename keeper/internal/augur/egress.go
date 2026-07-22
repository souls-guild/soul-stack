package augur

// SSRF egress guard for the prom/elk brokers (augur.md §4.1 / §7 / §9 —
// endpoint from an Omen DB row is UNtrusted input, Keeper issues outbound
// HTTP against it).
//
// The shared SSRF-guard logic (resolve-then-check-then-dial against the
// actual IP, rebind-safe; CheckRedirect downgrade protection; https-only; the
// blocked-IP classifier) lives in shared/netguard and is reused by Soul's
// core HTTP modules (core.url / core.http). What remains here is augur
// specifics: broker request timeouts, the body limit, and the client
// constructor on top of netguard.

import (
	"net"
	"net/http"
	"time"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

// maxEgressRedirects — a hard redirect limit for a broker HTTP fetch. Every
// hop is checked for https and against its actual IP (see netguard); the
// limit guards against an endless chain.
const maxEgressRedirects = 10

// egressDialTimeout — TCP connection-establishment timeout. Matches
// http.DefaultTransport.
const egressDialTimeout = 30 * time.Second

// egressRequestTimeout — overall timeout for one broker HTTP request (dial +
// TLS + reading the size-limited body). endpoint is UNtrusted — a slow/hung
// external host must not hold the handling goroutine forever.
const egressRequestTimeout = 15 * time.Second

// maxResponseBytes — size limit for an external system's response body
// (10 MiB). Protects against raw DoS: an UNtrusted endpoint must not force
// Keeper to allocate an arbitrary amount. The body is read through
// io.LimitReader at this boundary; exceeding it → error (see broker_http.go).
const maxResponseBytes = 10 << 20

// validateEndpoint checks an Omen's endpoint before an HTTP request
// (https-only + non-empty host + literal IP against the block-list, a fast
// rejection before building the client). Delegates to shared/netguard; the
// wrapper keeps the augur-facing name for the brokers (broker_prom/broker_elk).
func validateEndpoint(rawURL string) error {
	return netguard.ValidateEndpoint(rawURL)
}

// newEgressClient returns an *http.Client for broker HTTP against an
// UNtrusted endpoint: the system TLS trust store (no InsecureSkipVerify),
// the overall request timeout, redirect-downgrade protection + limit, and
// an SSRF guard at the dial phase against the actual IP (rebind-safe, deny
// metadata/loopback/private) — all from shared/netguard.
//
// resolver is injected for the guard's testability; production passes
// netguard.DefaultResolver (see NewEgressClient).
func newEgressClient(resolver netguard.Resolver) *http.Client {
	dialer := &net.Dialer{Timeout: egressDialTimeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = netguard.GuardedDialContext(resolver, dialer.DialContext)
	return &http.Client{
		Timeout:       egressRequestTimeout,
		CheckRedirect: netguard.NewCheckRedirect(maxEgressRedirects),
		Transport:     transport,
	}
}

// NewEgressClient — a production SSRF-guarded *http.Client for broker HTTP
// against an Omen's UNtrusted endpoint (prom/elk). Uses the system DNS
// resolver; implements [HTTPDoer]. The grpc handler creates one instance and
// passes it to the brokers.
func NewEgressClient() *http.Client {
	return newEgressClient(netguard.DefaultResolver)
}
