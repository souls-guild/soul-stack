package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// TrustedProxies is `listen.trusted_proxies`: the L7 proxies whose
// X-Forwarded-For Keeper believes (ADR-058 amendment NIM-1050).
type TrustedProxies []netip.Prefix

func (t TrustedProxies) trusts(a netip.Addr) bool {
	for _, p := range t {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddr is the address of the client that sent r. Without a trusted
// peer it is the peer itself. When the peer is a trusted proxy,
// X-Forwarded-For is walked from the right — each hop was appended by the
// proxy after it — and the first address that is not a trusted proxy is the
// client. A hop that does not parse ends the walk at the last address a
// trusted proxy vouched for; when every hop is trusted, the leftmost is the
// client.
func (t TrustedProxies) ClientAddr(r *http.Request) string {
	host := peerHost(r.RemoteAddr)
	peer, err := netip.ParseAddr(host)
	if err != nil || len(t) == 0 {
		return host
	}
	client := peer.Unmap()
	if !t.trusts(client) {
		return client.String()
	}
	values := r.Header.Values("X-Forwarded-For")
	for i := len(values) - 1; i >= 0; i-- {
		hops := values[i]
		for hops != "" {
			cut := strings.LastIndexByte(hops, ',')
			hop := strings.TrimSpace(hops[cut+1:])
			if cut < 0 {
				hops = ""
			} else {
				hops = hops[:cut]
			}
			a, ok := parseHop(hop)
			if !ok {
				return client.String()
			}
			client = a
			if !t.trusts(a) {
				return a.String()
			}
		}
	}
	return client.String()
}

// parseHop reads one X-Forwarded-For entry: an address, optionally bracketed
// (IPv6) and optionally with a port.
func parseHop(hop string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(hop); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(hop); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(hop, "["), "]")); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

func peerHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr // no port (rare) — as-is
	}
	return host
}

type clientAddrCtxKey struct{}

// ResolveClientAddr — a middleware that resolves the client address once per
// request with t.ClientAddr; [PeerIP] and the login limiter read it. Mounted
// first on the OpenAPI and MCP listeners.
func ResolveClientAddr(t TrustedProxies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), clientAddrCtxKey{}, t.ClientAddr(r))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
